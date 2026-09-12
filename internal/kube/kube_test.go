package kube_test

import (
	"context"
	"errors"
	"testing"

	flowexec "github.com/idosaban-scaleops/flow/internal/exec"
	"github.com/idosaban-scaleops/flow/internal/kube"
)

func TestCurrentContext(t *testing.T) {
	f := flowexec.NewFake().Respond("kubectl config current-context", "ido-saban-dev\n")
	got, err := kube.New(f).CurrentContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if got != "ido-saban-dev" {
		t.Errorf("context = %q", got)
	}
}

func TestCurrentContextUnsetIsATypedError(t *testing.T) {
	f := flowexec.NewFake()
	f.RespondWith("kubectl config current-context", flowexec.Response{
		ExitCode: 1, Stderr: "error: current-context is not set",
	})
	if _, err := kube.New(f).CurrentContext(context.Background()); !errors.Is(err, kube.ErrNoContext) {
		t.Errorf("err = %v, want ErrNoContext", err)
	}
}

func TestCurrentContextMissingBinary(t *testing.T) {
	// The user's shell may alias kubectl; os/exec ignores aliases, so a missing
	// real binary must be detected rather than producing a confusing exec error.
	f := flowexec.NewFake()
	f.Missing["kubectl"] = true

	k := kube.New(f)
	if k.Available() {
		t.Error("kubectl must not be reported available")
	}
	if _, err := k.CurrentContext(context.Background()); err == nil {
		t.Error("a missing kubectl must produce an error")
	}
}

func TestPodsSummarizesReadiness(t *testing.T) {
	f := flowexec.NewFake().Respond("kubectl get pods", `{"items":[
		{"metadata":{"name":"ok-1"},"spec":{"nodeName":"n1"},
		 "status":{"phase":"Running","containerStatuses":[
			{"ready":true,"restartCount":0},{"ready":true,"restartCount":2}]}},
		{"metadata":{"name":"broken-1"},"spec":{"nodeName":"n2"},
		 "status":{"phase":"Pending","containerStatuses":[
			{"ready":false,"restartCount":0,"state":{"waiting":{"reason":"ImagePullBackOff"}}}]}},
		{"metadata":{"name":"job-1"},"status":{"phase":"Succeeded","containerStatuses":[]}}]}`)

	pods, err := kube.New(f).Pods(context.Background(), "", "scaleops-system")
	if err != nil {
		t.Fatal(err)
	}
	if len(pods) != 3 {
		t.Fatalf("want 3 pods, got %d", len(pods))
	}

	if pods[0].Ready != 2 || pods[0].Total != 2 || pods[0].Restarts != 2 {
		t.Errorf("healthy pod = %+v", pods[0])
	}
	if !pods[0].Healthy() {
		t.Error("a fully-ready Running pod should be healthy")
	}
	if pods[1].Healthy() {
		t.Error("a pending pod must be flagged")
	}
	if pods[1].Reason != "ImagePullBackOff" {
		t.Errorf("reason = %q — a bad chart version shows up exactly here", pods[1].Reason)
	}
	if !pods[2].Healthy() {
		t.Error("a Succeeded pod is healthy")
	}
}

func TestPodsPassesContext(t *testing.T) {
	f := flowexec.NewFake().Respond("kubectl get pods", `{"items":[]}`)
	if _, err := kube.New(f).Pods(context.Background(), "dev", "ns"); err != nil {
		t.Fatal(err)
	}
	want := "kubectl get pods -n ns -o json --context dev"
	if got := f.CommandLines()[0]; got != want {
		t.Errorf("argv = %q, want %q", got, want)
	}
}

func TestServerURLFallsBackToFullView(t *testing.T) {
	f := flowexec.NewFake()
	f.Respond("kubectl config view -o jsonpath=", "")
	f.Respond("kubectl config view -o json", `{
		"clusters":[{"name":"dev-cluster","cluster":{"server":"https://dev.example.com"}}],
		"contexts":[{"name":"ido-saban-dev","context":{"cluster":"dev-cluster"}}]}`)

	got, err := kube.New(f).ServerURL(context.Background(), "ido-saban-dev")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://dev.example.com" {
		t.Errorf("server = %q", got)
	}
}
