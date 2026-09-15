package ghapi

import (
	"context"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/google/go-github/v91/github"
	"golang.org/x/oauth2"
)

// DefaultRequestTimeout bounds every individual API call. The CI wait passes a
// much longer parent context but each request is still capped.
const DefaultRequestTimeout = 15 * time.Second

// API is the narrow interface the rest of flow depends on. Everything above
// this package mocks API, never go-github itself.
type API interface {
	FindPR(ctx context.Context, owner, repo, branch string) (PRInfo, error)
	ListWorkflows(ctx context.Context, owner, repo string) ([]Workflow, error)
	ListWorkflowRuns(ctx context.Context, owner, repo, workflowFile, branch string, limit int) ([]Run, error)
	ListRunJobs(ctx context.Context, owner, repo string, runID int64) ([]Job, error)
	SuccessfulRunDurations(ctx context.Context, owner, repo, workflowFile, branch string, limit int) ([]time.Duration, error)
	AuthenticatedLogin(ctx context.Context) (string, error)
	HasToken() bool

	// LastRate reports the rate-limit budget GitHub returned with the most
	// recent call, so a long polling loop can widen its interval before it
	// runs the quota out. remaining is -1 when nothing has been observed yet.
	LastRate() (remaining int, reset time.Time)
}

// Options configure the client.
type Options struct {
	APIBase        string
	Token          string
	UserAgent      string
	RequestTimeout time.Duration
	DisableRetries bool
	DisableCaching bool
	// Transport overrides the base RoundTripper; tests use httptest.
	Transport http.RoundTripper
}

// Client is the go-github-backed implementation of API.
type Client struct {
	gh       *github.Client
	hasToken bool
	timeout  time.Duration

	rateMu    sync.Mutex
	remaining int
	resetAt   time.Time
}

var _ API = (*Client)(nil)

// New builds a client. A missing token is not an error: the client still works
// for public data, and every caller already handles the Unknown case.
func New(ctx context.Context, opts Options) *Client {
	base := opts.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	// Order matters: caching sits closest to the wire so a retried request also
	// carries its If-None-Match header.
	if !opts.DisableCaching {
		base = NewCachingTransport(base)
	}
	if !opts.DisableRetries {
		base = NewRetryTransport(base)
	}

	httpClient := &http.Client{Transport: base}
	if opts.Token != "" {
		ts := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: opts.Token})
		httpClient = &http.Client{Transport: &oauth2.Transport{Source: ts, Base: base}}
	}

	clientOpts := []github.ClientOptionsFunc{github.WithHTTPClient(httpClient)}
	if apiBase := strings.TrimSpace(opts.APIBase); apiBase != "" && !isDefaultAPIBase(apiBase) {
		// WithEnterpriseURLs is the supported way to retarget the client;
		// mutating BaseURL directly misses the upload URL and the trailing
		// slash go-github requires.
		clientOpts = append(clientOpts, github.WithEnterpriseURLs(apiBase, apiBase))
	}
	if opts.UserAgent != "" {
		clientOpts = append(clientOpts, github.WithUserAgent(opts.UserAgent))
	}

	gh, err := github.NewClient(clientOpts...)
	if err != nil {
		// The only failure mode is an unparseable base URL. Fall back to the
		// public API rather than making a missing token fatal.
		gh, _ = github.NewClient(github.WithHTTPClient(httpClient))
	}

	timeout := opts.RequestTimeout
	if timeout == 0 {
		timeout = DefaultRequestTimeout
	}
	return &Client{gh: gh, hasToken: opts.Token != "", timeout: timeout, remaining: -1}
}

// LastRate reports the most recently observed rate-limit budget.
func (c *Client) LastRate() (remaining int, reset time.Time) {
	c.rateMu.Lock()
	defer c.rateMu.Unlock()
	return c.remaining, c.resetAt
}

// recordRate stores the budget from a response. A 304 served from the ETag
// cache carries the live headers too, so the figure stays accurate.
func (c *Client) recordRate(resp *github.Response) {
	if resp == nil {
		return
	}
	c.rateMu.Lock()
	defer c.rateMu.Unlock()
	c.remaining = resp.Rate.Remaining
	c.resetAt = resp.Rate.Reset.Time
}

func isDefaultAPIBase(base string) bool {
	trimmed := strings.TrimSuffix(base, "/")
	return trimmed == "https://api.github.com"
}

// HasToken reports whether the client is authenticated.
func (c *Client) HasToken() bool { return c.hasToken }

// withTimeout bounds a single request without discarding the parent's deadline.
func (c *Client) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, c.timeout)
}

// FindPR returns the newest pull request whose head is branch. A branch with no
// pull request yields PRNone, not an error — working before opening a PR is the
// normal case.
//
// The head filter is "{owner}:{branch}", so this finds same-repo pull requests
// only. A PR opened from a fork has a head of "{forkOwner}:{branch}" and reads
// as PRNone, which makes `flow delete` ask "No pull request found for branch X.
// Delete anyway?" rather than naming the open PR. That is the intended
// trade-off for flow's workflow, where branches live in the repository itself;
// it fails safe, because PRNone still prompts rather than deleting. Matching
// fork PRs would mean listing without a head filter and comparing Head.Ref,
// which would also match an unrelated fork that happens to reuse the name.
func (c *Client) FindPR(ctx context.Context, owner, repo, branch string) (PRInfo, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()

	prs, resp, err := c.gh.PullRequests.List(ctx, owner, repo, &github.PullRequestListOptions{
		Head:        owner + ":" + branch,
		State:       "all",
		Sort:        "created",
		Direction:   "desc",
		ListOptions: github.ListOptions{PerPage: 5},
	})
	c.recordRate(resp)
	if err != nil {
		return Unknown(describeError(err)), translate(err)
	}
	if len(prs) == 0 {
		return PRInfo{State: PRNone}, nil
	}

	pr := prs[0]
	info := PRInfo{Number: pr.GetNumber(), URL: pr.GetHTMLURL()}
	switch {
	case pr.GetMerged() || !pr.GetMergedAt().IsZero():
		info.State = PRMerged
		merged := pr.GetMergedAt().Time
		info.MergedAt = &merged
	case pr.GetState() == "closed":
		info.State = PRClosed
	default:
		info.State = PROpen
	}
	return info, nil
}

// Workflow is one entry from a repository's Actions workflow list.
type Workflow struct {
	// Name is the display name, e.g. "Build And Release".
	Name string `json:"name"`
	// File is the base name of the workflow file, e.g. "go.yaml". That, not
	// the display name, is what every other call here takes.
	File string `json:"file"`
}

// ListWorkflows returns the repository's active workflows, so flow can ask
// which one builds the chart rather than making the user look up a filename.
// Disabled workflows are left out: they cannot produce the run flow would wait
// for.
func (c *Client) ListWorkflows(ctx context.Context, owner, repo string) ([]Workflow, error) {
	var out []Workflow
	opts := &github.ListOptions{PerPage: 100}

	for {
		pageCtx, cancel := c.withTimeout(ctx)
		workflows, resp, err := c.gh.Actions.ListWorkflows(pageCtx, owner, repo, opts)
		cancel()
		c.recordRate(resp)
		if err != nil {
			return nil, translate(err)
		}
		for _, w := range workflows.Workflows {
			if w.GetState() != "active" {
				continue
			}
			// A workflow's path is ".github/workflows/go.yaml"; every other
			// Actions call in flow wants the base name alone.
			out = append(out, Workflow{Name: w.GetName(), File: path.Base(w.GetPath())})
		}
		if resp == nil || resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
}

// ListWorkflowRuns returns runs of one workflow file on one branch, newest
// first.
func (c *Client) ListWorkflowRuns(ctx context.Context, owner, repo, workflowFile, branch string, limit int) ([]Run, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()

	if limit <= 0 {
		limit = 20
	}
	runs, resp, err := c.gh.Actions.ListWorkflowRunsByFileName(ctx, owner, repo, workflowFile,
		&github.ListWorkflowRunsOptions{
			Branch:      branch,
			ListOptions: github.ListOptions{PerPage: limit},
		})
	c.recordRate(resp)
	if err != nil {
		return nil, translate(err)
	}
	return convertRuns(runs.WorkflowRuns), nil
}

// ListRunJobs returns every job of a run, following pagination: the ScaleOps
// workflow has well over one page of jobs, and a truncated list would silently
// drop a gating job.
func (c *Client) ListRunJobs(ctx context.Context, owner, repo string, runID int64) ([]Job, error) {
	var out []Job
	opts := &github.ListWorkflowJobsOptions{
		Filter:      "latest",
		ListOptions: github.ListOptions{PerPage: 100},
	}

	for {
		pageCtx, cancel := c.withTimeout(ctx)
		jobs, resp, err := c.gh.Actions.ListWorkflowJobs(pageCtx, owner, repo, runID, opts)
		cancel()
		c.recordRate(resp)
		if err != nil {
			return nil, translate(err)
		}
		out = append(out, convertJobs(jobs.Jobs)...)
		if resp == nil || resp.NextPage == 0 {
			return out, nil
		}
		opts.Page = resp.NextPage
	}
}

// SuccessfulRunDurations returns how long recent successful runs took, for the
// ETA in the progress display.
func (c *Client) SuccessfulRunDurations(ctx context.Context, owner, repo, workflowFile, branch string, limit int) ([]time.Duration, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()

	if limit <= 0 {
		limit = 10
	}
	runs, _, err := c.gh.Actions.ListWorkflowRunsByFileName(ctx, owner, repo, workflowFile,
		&github.ListWorkflowRunsOptions{
			Branch:      branch,
			Status:      "success",
			ListOptions: github.ListOptions{PerPage: limit},
		})
	if err != nil {
		return nil, translate(err)
	}

	var durations []time.Duration
	for _, r := range runs.WorkflowRuns {
		start, end := r.GetRunStartedAt().Time, r.GetUpdatedAt().Time
		if start.IsZero() || end.IsZero() || !end.After(start) {
			continue
		}
		durations = append(durations, end.Sub(start))
	}
	return durations, nil
}

// AuthenticatedLogin returns the login the token belongs to. The token itself
// is never returned or logged.
func (c *Client) AuthenticatedLogin(ctx context.Context) (string, error) {
	ctx, cancel := c.withTimeout(ctx)
	defer cancel()

	user, _, err := c.gh.Users.Get(ctx, "")
	if err != nil {
		return "", translate(err)
	}
	return user.GetLogin(), nil
}

// convertRuns flattens go-github runs, using the Get* accessors throughout:
// go-github returns pointers everywhere and a nil dereference on an absent
// conclusion is the most likely crash in this codebase.
func convertRuns(in []*github.WorkflowRun) []Run {
	out := make([]Run, 0, len(in))
	for _, r := range in {
		if r == nil {
			continue
		}
		out = append(out, Run{
			ID:         r.GetID(),
			Name:       r.GetName(),
			Status:     RunStatus(r.GetStatus()),
			Conclusion: Conclusion(r.GetConclusion()),
			Branch:     r.GetHeadBranch(),
			HTMLURL:    r.GetHTMLURL(),
			Attempt:    r.GetRunAttempt(),
			CreatedAt:  r.GetCreatedAt().Time,
			UpdatedAt:  r.GetUpdatedAt().Time,
			HeadSHA:    r.GetHeadSHA(),
		})
	}
	return out
}

func convertJobs(in []*github.WorkflowJob) []Job {
	out := make([]Job, 0, len(in))
	for _, j := range in {
		if j == nil {
			continue
		}
		job := Job{
			ID:         j.GetID(),
			Name:       j.GetName(),
			Status:     RunStatus(j.GetStatus()),
			Conclusion: Conclusion(j.GetConclusion()),
			HTMLURL:    j.GetHTMLURL(),
		}
		if t := j.GetStartedAt().Time; !t.IsZero() {
			job.StartedAt = &t
		}
		if t := j.GetCompletedAt().Time; !t.IsZero() {
			job.CompletedAt = &t
		}
		for _, s := range j.Steps {
			if s == nil {
				continue
			}
			job.Steps = append(job.Steps, Step{
				Name:       s.GetName(),
				Number:     int(s.GetNumber()),
				Status:     RunStatus(s.GetStatus()),
				Conclusion: Conclusion(s.GetConclusion()),
			})
		}
		out = append(out, job)
	}
	return out
}
