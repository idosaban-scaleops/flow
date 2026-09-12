package helmx_test

import (
	"context"
	"strings"
	"testing"

	flowexec "github.com/idosaban-scaleops/flow/internal/exec"
	"github.com/idosaban-scaleops/flow/internal/helmx"
)

func TestSearchVersionsPassesDevel(t *testing.T) {
	f := flowexec.NewFake().Respond("helm search repo", `[
		{"name":"scaleops/scaleops","version":"v1.0.199-alpha-RD-1-a-34468169597","app_version":"x"},
		{"name":"scaleops/scaleops","version":"v1.0.198","app_version":"y"}]`)

	got, err := helmx.New(f).SearchVersions(context.Background(), "scaleops", "scaleops")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 versions, got %d", len(got))
	}

	want := "helm search repo scaleops/scaleops --versions --devel -o json"
	if !f.Ran(want) {
		t.Errorf("argv = %v\nwant %q — alpha charts are pre-releases, so --devel is mandatory",
			f.CommandLines(), want)
	}
}

func TestHasVersion(t *testing.T) {
	f := flowexec.NewFake().Respond("helm search repo",
		`[{"name":"scaleops/scaleops","version":"v1.0.199-alpha-RD-1-a-34468169597"}]`)
	h := helmx.New(f)

	ok, err := h.HasVersion(context.Background(), "scaleops", "scaleops", "v1.0.199-alpha-RD-1-a-34468169597")
	if err != nil || !ok {
		t.Errorf("HasVersion = %v, %v; want true", ok, err)
	}
	ok, err = h.HasVersion(context.Background(), "scaleops", "scaleops", "v9.9.9")
	if err != nil || ok {
		t.Errorf("HasVersion for a missing version = %v, %v; want false", ok, err)
	}
}

func TestListReposEmptyIsNotAnError(t *testing.T) {
	f := flowexec.NewFake()
	f.RespondWith("helm repo list", flowexec.Response{
		ExitCode: 1, Stderr: "Error: no repositories to show",
	})

	got, err := helmx.New(f).ListRepos(context.Background())
	if err != nil {
		t.Fatalf("an empty repo list is a state, not a failure: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("want no repos, got %v", got)
	}
}

func TestUpgradeArgsOrderAndContent(t *testing.T) {
	tests := []struct {
		name string
		opts helmx.UpgradeOptions
		want string
	}{
		{
			name: "the baseline invocation",
			opts: helmx.UpgradeOptions{
				Release:     "scaleops",
				Chart:       "scaleops/scaleops",
				Namespace:   "scaleops-system",
				Version:     "v1.0.199-alpha-RD-1-a-34468169597",
				ValuesFiles: []string{"/Users/idosaban/Developer/helm-values/ido-saban-dev.yaml"},
				ExtraArgs:   []string{"--reset-then-reuse-values"},
			},
			want: "helm upgrade scaleops scaleops/scaleops --namespace scaleops-system " +
				"--reset-then-reuse-values " +
				"--values /Users/idosaban/Developer/helm-values/ido-saban-dev.yaml " +
				"--version v1.0.199-alpha-RD-1-a-34468169597",
		},
		{
			name: "install is opt-in, never the default",
			opts: helmx.UpgradeOptions{Release: "r", Chart: "c", Namespace: "ns", Install: true},
			want: "helm upgrade --install r c --namespace ns",
		},
		{
			name: "a different context is passed to helm, not switched globally",
			opts: helmx.UpgradeOptions{Release: "r", Chart: "c", Namespace: "ns", KubeContext: "other"},
			want: "helm upgrade r c --namespace ns --kube-context other",
		},
		{
			name: "repeatable values and sets",
			opts: helmx.UpgradeOptions{
				Release: "r", Chart: "c", Namespace: "ns",
				ValuesFiles: []string{"a.yaml", "b.yaml"},
				SetValues:   []string{"x=1", "y=2"},
			},
			want: "helm upgrade r c --namespace ns --values a.yaml --values b.yaml --set x=1 --set y=2",
		},
		{
			name: "rollout flags",
			opts: helmx.UpgradeOptions{
				Release: "r", Chart: "c", Namespace: "ns",
				Wait: true, Atomic: true, CreateNS: true, Timeout: "10m",
			},
			want: "helm upgrade r c --namespace ns --create-namespace --wait --atomic --timeout 10m",
		},
		{
			name: "server-side dry run",
			opts: helmx.UpgradeOptions{Release: "r", Chart: "c", Namespace: "ns", DryRun: true},
			want: "helm upgrade r c --namespace ns --dry-run",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.opts.CommandLine("helm"); got != tt.want {
				t.Errorf("CommandLine =\n  %s\nwant\n  %s", got, tt.want)
			}

			f := flowexec.NewFake()
			if err := helmx.New(f).Upgrade(context.Background(), tt.opts); err != nil {
				t.Fatal(err)
			}
			// The printed command and the executed one must never diverge.
			if got := f.CommandLines()[0]; got != tt.want {
				t.Errorf("executed\n  %s\nbut printed\n  %s", got, tt.want)
			}
		})
	}
}

func TestStatusParsesRelease(t *testing.T) {
	f := flowexec.NewFake().Respond("helm status", `{
		"name":"scaleops","namespace":"scaleops-system","version":7,
		"info":{"last_deployed":"2026-09-12T11:02:10.123456789Z","status":"deployed",
		        "description":"Upgrade complete"},
		"chart":{"metadata":{"version":"v1.0.199-alpha-RD-1-a-1","appVersion":"1.0.199"}}}`)

	got, err := helmx.New(f).Status(context.Background(), "scaleops", "scaleops-system", "")
	if err != nil {
		t.Fatal(err)
	}
	if got.Revision != 7 || got.Status != "deployed" {
		t.Errorf("release = %+v", got)
	}
	if got.ChartVersion != "v1.0.199-alpha-RD-1-a-1" {
		t.Errorf("chart version = %q", got.ChartVersion)
	}
	if got.LastDeployed.IsZero() {
		t.Error("last_deployed was not parsed")
	}
	if !f.Ran("helm status scaleops -n scaleops-system -o json") {
		t.Errorf("argv = %v", f.CommandLines())
	}
}

func TestHistoryParsesRevisions(t *testing.T) {
	f := flowexec.NewFake().Respond("helm history", `[
		{"revision":1,"updated":"2026-09-10 09:00:00.0 +0000 UTC","status":"superseded",
		 "chart":"scaleops-v1.0.198","app_version":"1.0.198","description":"Install complete"},
		{"revision":2,"updated":"2026-09-12 11:02:10.0 +0000 UTC","status":"deployed",
		 "chart":"scaleops-v1.0.199-alpha-RD-1-a-1","app_version":"1.0.199","description":"Upgrade complete"}]`)

	got, err := helmx.New(f).History(context.Background(), "scaleops", "scaleops-system", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 revisions, got %d", len(got))
	}
	if got[1].Revision != 2 || got[1].Status != "deployed" {
		t.Errorf("revision = %+v", got[1])
	}
	if got[1].Updated.IsZero() {
		t.Error("helm's space-separated timestamp format was not parsed")
	}
}

func TestRollbackDefaultsToPreviousRevision(t *testing.T) {
	f := flowexec.NewFake()
	h := helmx.New(f)

	if err := h.Rollback(context.Background(), "scaleops", "scaleops-system", "", 0); err != nil {
		t.Fatal(err)
	}
	if got := f.CommandLines()[0]; got != "helm rollback scaleops -n scaleops-system" {
		t.Errorf("argv = %q", got)
	}

	if err := h.Rollback(context.Background(), "scaleops", "scaleops-system", "dev", 5); err != nil {
		t.Fatal(err)
	}
	want := "helm rollback scaleops 5 -n scaleops-system --kube-context dev"
	if got := f.CommandLines()[1]; got != want {
		t.Errorf("argv = %q, want %q", got, want)
	}
}

func TestUninstallArgv(t *testing.T) {
	f := flowexec.NewFake()
	if err := helmx.New(f).Uninstall(context.Background(), "scaleops", "ns", "", true); err != nil {
		t.Fatal(err)
	}
	want := "helm uninstall scaleops -n ns --keep-history"
	if got := f.CommandLines()[0]; got != want {
		t.Errorf("argv = %q, want %q", got, want)
	}
}

func TestUpdateRepoScopesToOneRepo(t *testing.T) {
	f := flowexec.NewFake()
	if err := helmx.New(f).UpdateRepo(context.Background(), "scaleops"); err != nil {
		t.Fatal(err)
	}
	if got := f.CommandLines()[0]; got != "helm repo update scaleops" {
		t.Errorf("argv = %q", got)
	}
}

func TestGetValues(t *testing.T) {
	f := flowexec.NewFake().Respond("helm get values", "image:\n  tag: x\n")
	got, err := helmx.New(f).GetValues(context.Background(), "scaleops", "ns", "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "tag: x") {
		t.Errorf("values = %q", got)
	}
	if !f.Ran("helm get values scaleops -n ns -o yaml") {
		t.Errorf("argv = %v", f.CommandLines())
	}
}
