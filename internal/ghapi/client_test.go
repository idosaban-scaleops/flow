package ghapi_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/idosaban-scaleops/flow/internal/ghapi"
)

// newClient points a real go-github client at an httptest server. The trailing
// slash on the base URL is required; omitting it is a common and confusing
// failure mode.
func newClient(t *testing.T, h http.Handler) (*ghapi.Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	c := ghapi.New(context.Background(), ghapi.Options{
		APIBase:   srv.URL + "/",
		Token:     "test-token",
		UserAgent: "flow/test",
	})
	return c, srv
}

func TestFindPROpen(t *testing.T) {
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("head"); got != "o:RD-1-a" {
			t.Errorf("head = %q, want o:RD-1-a", got)
		}
		if got := r.URL.Query().Get("state"); got != "all" {
			t.Errorf("state = %q, want all", got)
		}
		fmt.Fprint(w, `[{"number":123,"state":"open","merged":false,
			"html_url":"https://github.com/o/r/pull/123"}]`)
	}))

	got, err := c.FindPR(context.Background(), "o", "r", "RD-1-a")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != ghapi.PROpen || got.Number != 123 {
		t.Errorf("FindPR = %+v", got)
	}
	if got.URL != "https://github.com/o/r/pull/123" {
		t.Errorf("URL = %q", got.URL)
	}
}

func TestFindPRMerged(t *testing.T) {
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[{"number":7,"state":"closed","merged":true,
			"merged_at":"2026-09-10T08:00:00Z","html_url":"u"}]`)
	}))

	got, err := c.FindPR(context.Background(), "o", "r", "b")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != ghapi.PRMerged {
		t.Fatalf("State = %v, want merged", got.State)
	}
	if got.MergedAt == nil || !got.MergedAt.Equal(time.Date(2026, 9, 10, 8, 0, 0, 0, time.UTC)) {
		t.Errorf("MergedAt = %v", got.MergedAt)
	}
}

func TestFindPRClosedUnmerged(t *testing.T) {
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[{"number":8,"state":"closed","merged":false,"html_url":"u"}]`)
	}))
	got, err := c.FindPR(context.Background(), "o", "r", "b")
	if err != nil {
		t.Fatal(err)
	}
	if got.State != ghapi.PRClosed {
		t.Errorf("State = %v, want closed", got.State)
	}
}

func TestFindPRNoneIsNotAnError(t *testing.T) {
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `[]`)
	}))

	got, err := c.FindPR(context.Background(), "o", "r", "b")
	if err != nil {
		t.Fatalf("a branch with no pull request is a normal state, not an error: %v", err)
	}
	if got.State != ghapi.PRNone {
		t.Errorf("State = %v, want none", got.State)
	}
	if got.State == ghapi.PRUnknown {
		t.Error("none and unknown must never collapse")
	}
}

func TestFindPR404MapsToNotFound(t *testing.T) {
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"Not Found"}`)
	}))

	got, err := c.FindPR(context.Background(), "o", "r", "b")
	if !errors.Is(err, ghapi.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
	if got.State != ghapi.PRUnknown {
		t.Errorf("a failed lookup must be Unknown, got %v", got.State)
	}
	if got.Reason == "" {
		t.Error("Unknown must carry a displayable reason")
	}
}

func TestFindPR401MentionsTokenOrder(t *testing.T) {
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"message":"Bad credentials"}`)
	}))

	_, err := c.FindPR(context.Background(), "o", "r", "b")
	if !errors.Is(err, ghapi.ErrUnauthorized) {
		t.Fatalf("err = %v, want ErrUnauthorized", err)
	}
	if !strings.Contains(err.Error(), "GITHUB_TOKEN") {
		t.Errorf("the error must name the token resolution order, got %q", err)
	}
}

func TestRateLimitSurfacedWithResetTime(t *testing.T) {
	reset := time.Now().Add(20 * time.Minute).Truncate(time.Second)
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.Header().Set("X-RateLimit-Reset", fmt.Sprint(reset.Unix()))
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"API rate limit exceeded","documentation_url":"https://docs.github.com/"}`)
	}))

	_, err := c.FindPR(context.Background(), "o", "r", "b")
	var rate *ghapi.RateLimitError
	if !errors.As(err, &rate) {
		t.Fatalf("err = %#v, want *ghapi.RateLimitError", err)
	}
	if !rate.ResetAt.Equal(reset) {
		t.Errorf("ResetAt = %v, want %v", rate.ResetAt, reset)
	}
}

func TestListRunJobsPaginates(t *testing.T) {
	var pages atomic.Int32
	c, srv := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		if page == "" || page == "1" {
			pages.Add(1)
			w.Header().Set("Link", `<`+baseOf(r)+`?page=2>; rel="next", <`+baseOf(r)+`?page=2>; rel="last"`)
			fmt.Fprint(w, `{"total_count":3,"jobs":[
				{"id":1,"name":"Setup","status":"completed","conclusion":"success"},
				{"id":2,"name":"Pre Release Helm / Release","status":"in_progress"}]}`)
			return
		}
		pages.Add(1)
		fmt.Fprint(w, `{"total_count":3,"jobs":[
			{"id":3,"name":"Pre Release Images / Build And Push","status":"queued"}]}`)
	}))
	_ = srv

	jobs, err := c.ListRunJobs(context.Background(), "o", "r", 42)
	if err != nil {
		t.Fatal(err)
	}
	if pages.Load() != 2 {
		t.Errorf("fetched %d pages, want 2 — a truncated job list silently drops gating jobs", pages.Load())
	}
	if len(jobs) != 3 {
		t.Fatalf("want 3 jobs across both pages, got %d", len(jobs))
	}
	if jobs[2].Name != "Pre Release Images / Build And Push" {
		t.Errorf("second page job = %q", jobs[2].Name)
	}
}

func baseOf(r *http.Request) string {
	return "http://" + r.Host + r.URL.Path
}

func TestListRunJobsParsesSteps(t *testing.T) {
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"jobs":[{"id":1,"name":"Helm Build","status":"in_progress",
			"started_at":"2026-09-12T10:00:00Z","html_url":"https://gh/job/1",
			"steps":[
				{"name":"Set up job","number":1,"status":"completed","conclusion":"success"},
				{"name":"Publishing chart","number":2,"status":"in_progress"}]}]}`)
	}))

	jobs, err := c.ListRunJobs(context.Background(), "o", "r", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 1 || len(jobs[0].Steps) != 2 {
		t.Fatalf("jobs = %+v", jobs)
	}
	if jobs[0].Steps[1].Name != "Publishing chart" || jobs[0].Steps[1].Status != ghapi.StatusInProgress {
		t.Errorf("step = %+v", jobs[0].Steps[1])
	}
	if jobs[0].StartedAt == nil {
		t.Error("started_at was not parsed")
	}
	if jobs[0].CompletedAt != nil {
		t.Error("an unfinished job must not have a completion time")
	}
}

func TestRetryOn5xx(t *testing.T) {
	var attempts atomic.Int32
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		fmt.Fprint(w, `[]`)
	}))

	got, err := c.FindPR(context.Background(), "o", "r", "b")
	if err != nil {
		t.Fatalf("the request should have succeeded after retries: %v", err)
	}
	if got.State != ghapi.PRNone {
		t.Errorf("State = %v", got.State)
	}
	if attempts.Load() != 3 {
		t.Errorf("made %d attempts, want 3", attempts.Load())
	}
}

func TestNoRetryOn404(t *testing.T) {
	var attempts atomic.Int32
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"Not Found"}`)
	}))

	_, _ = c.FindPR(context.Background(), "o", "r", "b")
	if attempts.Load() != 1 {
		t.Errorf("made %d attempts; a 404 must not be retried", attempts.Load())
	}
}

func TestConditionalRequestsAvoidRateLimit(t *testing.T) {
	var full, notModified atomic.Int32
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const etag = `W/"abc123"`
		if r.Header.Get("If-None-Match") == etag {
			notModified.Add(1)
			w.Header().Set("ETag", etag)
			w.Header().Set("X-RateLimit-Remaining", "4999")
			w.WriteHeader(http.StatusNotModified)
			return
		}
		full.Add(1)
		w.Header().Set("ETag", etag)
		fmt.Fprint(w, `{"jobs":[{"id":1,"name":"Setup","status":"completed","conclusion":"success"}]}`)
	}))

	for range 3 {
		jobs, err := c.ListRunJobs(context.Background(), "o", "r", 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(jobs) != 1 || jobs[0].Name != "Setup" {
			t.Fatalf("a 304 must still yield the cached body, got %+v", jobs)
		}
	}

	if full.Load() != 1 {
		t.Errorf("%d full responses, want 1", full.Load())
	}
	if notModified.Load() != 2 {
		t.Errorf("%d conditional hits, want 2", notModified.Load())
	}
}

func TestListWorkflowRunsMapsFields(t *testing.T) {
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("branch"); got != "RD-1-a" {
			t.Errorf("branch = %q", got)
		}
		fmt.Fprint(w, `{"total_count":1,"workflow_runs":[{
			"id":34468169597,"name":"Build And Release","status":"completed",
			"conclusion":"success","head_branch":"RD-1-a","run_attempt":2,
			"created_at":"2026-09-12T10:00:00Z",
			"html_url":"https://github.com/o/r/actions/runs/34468169597"}]}`)
	}))

	runs, err := c.ListWorkflowRuns(context.Background(), "o", "r", "go.yaml", "RD-1-a", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 {
		t.Fatalf("want 1 run, got %d", len(runs))
	}
	r := runs[0]
	if r.ID != 34468169597 || !r.Succeeded() || r.Attempt != 2 {
		t.Errorf("run = %+v", r)
	}
}

func TestSuccessfulRunDurations(t *testing.T) {
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("status"); got != "success" {
			t.Errorf("status = %q, want success", got)
		}
		fmt.Fprint(w, `{"workflow_runs":[
			{"id":1,"run_started_at":"2026-09-12T10:00:00Z","updated_at":"2026-09-12T10:08:00Z"},
			{"id":2,"run_started_at":"2026-09-12T09:00:00Z","updated_at":"2026-09-12T09:06:00Z"},
			{"id":3,"run_started_at":"2026-09-12T08:00:00Z"}]}`)
	}))

	got, err := c.SuccessfulRunDurations(context.Background(), "o", "r", "go.yaml", "main", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 usable durations (the third has no end time), got %d", len(got))
	}
	if got[0] != 8*time.Minute || got[1] != 6*time.Minute {
		t.Errorf("durations = %v", got)
	}
}

func TestAuthenticatedLogin(t *testing.T) {
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/user") {
			t.Errorf("path = %q", r.URL.Path)
		}
		fmt.Fprint(w, `{"login":"idosaban"}`)
	}))

	login, err := c.AuthenticatedLogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if login != "idosaban" {
		t.Errorf("login = %q", login)
	}
}

func TestUserAgentIsSet(t *testing.T) {
	var seen string
	c, _ := newClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("User-Agent")
		fmt.Fprint(w, `[]`)
	}))
	if _, err := c.FindPR(context.Background(), "o", "r", "b"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(seen, "flow/") {
		t.Errorf("User-Agent = %q, want it to identify flow", seen)
	}
}

func TestPRStateJSONRoundTrip(t *testing.T) {
	for _, state := range []ghapi.PRState{
		ghapi.PRUnknown, ghapi.PRNone, ghapi.PROpen, ghapi.PRMerged, ghapi.PRClosed,
	} {
		t.Run(state.String(), func(t *testing.T) {
			data, err := state.MarshalJSON()
			if err != nil {
				t.Fatal(err)
			}
			if want := `"` + state.String() + `"`; string(data) != want {
				t.Errorf("marshalled to %s, want %s", data, want)
			}
			var back ghapi.PRState
			if err := back.UnmarshalJSON(data); err != nil {
				t.Fatal(err)
			}
			if back != state {
				t.Errorf("round trip gave %v, want %v", back, state)
			}
		})
	}
}
