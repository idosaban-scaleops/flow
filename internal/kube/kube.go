// Package kube reads the local kubectl configuration. flow never mutates the
// user's kubeconfig: to target a different cluster it passes --kube-context to
// helm instead.
package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	flowexec "github.com/idosaban-scaleops/flow/internal/exec"
)

// Kube wraps the kubectl binary.
type Kube struct {
	Runner flowexec.Runner
	Bin    string
}

// New returns a Kube for the kubectl binary.
func New(r flowexec.Runner) *Kube { return &Kube{Runner: r, Bin: "kubectl"} }

// ErrNoContext reports that kubectl has no current context selected.
var ErrNoContext = fmt.Errorf("no current kube context is set")

// Available reports whether kubectl is on PATH. The user's shell may alias
// kubectl to something else; os/exec ignores aliases, so this checks for a real
// binary.
func (k *Kube) Available() bool {
	_, err := k.Runner.LookPath(k.Bin)
	return err == nil
}

// CurrentContext returns the active kube context name.
func (k *Kube) CurrentContext(ctx context.Context) (string, error) {
	if !k.Available() {
		return "", fmt.Errorf("kubectl is not on PATH")
	}
	res, err := k.Runner.Run(ctx, flowexec.Opts{
		Name: k.Bin, Args: []string{"config", "current-context"},
	})
	if err != nil {
		return "", ErrNoContext
	}
	name := strings.TrimSpace(res.Stdout)
	if name == "" {
		return "", ErrNoContext
	}
	return name, nil
}

// ServerURL returns the API server address for a context, for the confirmation
// header that names exactly which cluster is about to be changed.
func (k *Kube) ServerURL(ctx context.Context, contextName string) (string, error) {
	jsonPath := fmt.Sprintf(
		`{.clusters[?(@.name==%q)].cluster.server}`, clusterOf(contextName))
	res, err := k.Runner.Run(ctx, flowexec.Opts{
		Name: k.Bin,
		Args: []string{"config", "view", "-o", "jsonpath=" + jsonPath},
	})
	if err != nil {
		return "", err
	}
	if server := strings.TrimSpace(res.Stdout); server != "" {
		return server, nil
	}
	return k.serverViaContexts(ctx, contextName)
}

// clusterOf is a first guess: in most kubeconfigs the context and cluster share
// a name. serverViaContexts covers the rest.
func clusterOf(contextName string) string { return contextName }

func (k *Kube) serverViaContexts(ctx context.Context, contextName string) (string, error) {
	res, err := k.Runner.Run(ctx, flowexec.Opts{
		Name: k.Bin, Args: []string{"config", "view", "-o", "json"},
	})
	if err != nil {
		return "", err
	}

	var view struct {
		Clusters []struct {
			Name    string `json:"name"`
			Cluster struct {
				Server string `json:"server"`
			} `json:"cluster"`
		} `json:"clusters"`
		Contexts []struct {
			Name    string `json:"name"`
			Context struct {
				Cluster string `json:"cluster"`
			} `json:"context"`
		} `json:"contexts"`
	}
	if err := json.Unmarshal([]byte(res.Stdout), &view); err != nil {
		return "", fmt.Errorf("parse kubeconfig: %w", err)
	}

	var clusterName string
	for _, c := range view.Contexts {
		if c.Name == contextName {
			clusterName = c.Context.Cluster
			break
		}
	}
	for _, c := range view.Clusters {
		if c.Name == clusterName {
			return c.Cluster.Server, nil
		}
	}
	return "", fmt.Errorf("no server found for context %q", contextName)
}

// Contexts lists every configured context name.
func (k *Kube) Contexts(ctx context.Context) ([]string, error) {
	res, err := k.Runner.Run(ctx, flowexec.Opts{
		Name: k.Bin,
		Args: []string{"config", "get-contexts", "-o", "name"},
	})
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(res.Stdout), "\n") {
		if name := strings.TrimSpace(line); name != "" {
			names = append(names, name)
		}
	}
	return names, nil
}

// Reachable reports whether the cluster answers, using a short-lived probe.
func (k *Kube) Reachable(ctx context.Context, contextName string) error {
	args := []string{"version", "-o", "json", "--request-timeout=5s"}
	if contextName != "" {
		args = append(args, "--context", contextName)
	}
	_, err := k.Runner.Run(ctx, flowexec.Opts{Name: k.Bin, Args: args})
	return err
}

// Pod is the summary flow shows for a namespace.
type Pod struct {
	Name       string
	Phase      string
	Ready      int
	Total      int
	Restarts   int
	NodeName   string
	Reason     string
	Terminated bool
}

// Healthy reports a pod in a state that needs no attention.
func (p Pod) Healthy() bool {
	return (p.Phase == "Running" && p.Ready == p.Total) || p.Phase == "Succeeded"
}

// Pods returns a compact summary of the pods in a namespace.
func (k *Kube) Pods(ctx context.Context, contextName, namespace string) ([]Pod, error) {
	args := []string{"get", "pods", "-n", namespace, "-o", "json"}
	if contextName != "" {
		args = append(args, "--context", contextName)
	}
	res, err := k.Runner.Run(ctx, flowexec.Opts{Name: k.Bin, Args: args})
	if err != nil {
		return nil, err
	}
	return parsePods(res.Stdout)
}

func parsePods(out string) ([]Pod, error) {
	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Spec struct {
				NodeName string `json:"nodeName"`
			} `json:"spec"`
			Status struct {
				Phase             string `json:"phase"`
				Reason            string `json:"reason"`
				ContainerStatuses []struct {
					Ready        bool `json:"ready"`
					RestartCount int  `json:"restartCount"`
					State        struct {
						Waiting *struct {
							Reason string `json:"reason"`
						} `json:"waiting"`
						Terminated *struct {
							Reason string `json:"reason"`
						} `json:"terminated"`
					} `json:"state"`
				} `json:"containerStatuses"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		return nil, fmt.Errorf("parse pod list: %w", err)
	}

	pods := make([]Pod, 0, len(list.Items))
	for _, item := range list.Items {
		p := Pod{
			Name:     item.Metadata.Name,
			Phase:    item.Status.Phase,
			NodeName: item.Spec.NodeName,
			Reason:   item.Status.Reason,
			Total:    len(item.Status.ContainerStatuses),
		}
		for _, cs := range item.Status.ContainerStatuses {
			if cs.Ready {
				p.Ready++
			}
			p.Restarts += cs.RestartCount
			if cs.State.Waiting != nil && p.Reason == "" {
				// ImagePullBackOff and CrashLoopBackOff surface here, which is
				// exactly what a bad chart version looks like.
				p.Reason = cs.State.Waiting.Reason
			}
			if cs.State.Terminated != nil {
				p.Terminated = true
			}
		}
		pods = append(pods, p)
	}
	return pods, nil
}
