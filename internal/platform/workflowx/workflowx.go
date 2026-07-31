// Package workflowx provides durable, Temporal-sdkx-style workflow
// primitives for the inclusion plane (audit O1/O2): workflow definitions
// built from named activities with retry policy, a persisted run store
// (crash-safe run state + idempotent re-drive), an in-process dev runner,
// and a TEMPORAL_URL production hook.
//
// Profiles (H1 selection rule, fail-closed):
//   - profile=dev  (TEMPORAL_URL unset): DurableRunner executes in-process,
//     persisting every run + step transition so a crashed "running" run can
//     be re-driven idempotently.
//   - profile=prod (APP_PROFILE=prod|production or AUTH_MODE=keycloak):
//     TEMPORAL_URL is REQUIRED — NewRunnerFromEnv returns an error
//     otherwise (no silent dev fallback). When set, the runner records the
//     Temporal target on every run and dispatches through the
//     TemporalDispatcher hook; a binary that has not linked a real Temporal
//     client fails closed at Execute time rather than pretending to run.
package workflowx

import (
	"context"
	"fmt"
	"log"
	"math"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// RetryPolicy mirrors Temporal's activity retry policy.
type RetryPolicy struct {
	InitialInterval    time.Duration
	BackoffCoefficient float64
	MaximumInterval    time.Duration
	MaximumAttempts    int
}

// DefaultRetryPolicy is the platform default.
var DefaultRetryPolicy = RetryPolicy{
	InitialInterval:    100 * time.Millisecond,
	BackoffCoefficient: 2.0,
	MaximumInterval:    10 * time.Second,
	MaximumAttempts:    3,
}

func (p RetryPolicy) delay(attempt int) time.Duration {
	d := float64(p.InitialInterval) * math.Pow(p.BackoffCoefficient, float64(attempt-1))
	if p.MaximumInterval > 0 && time.Duration(d) > p.MaximumInterval {
		return p.MaximumInterval
	}
	return time.Duration(d)
}

// Activity is a named, idempotent unit of work.
type Activity func(ctx context.Context, input any) (any, error)

// Workflow is a durable workflow definition: a function that orchestrates
// activities via the Ctx helper.
type Workflow func(ctx *Ctx) error

// Ctx is the workflow execution context handed to a Workflow. Activity
// results are appended to the run's step log so a re-driven run can observe
// (and activities can skip) already-completed work.
type Ctx struct {
	context.Context
	Run    *Run
	policy RetryPolicy
	acts   map[string]Activity
}

// ExecuteActivity runs a named activity with the run's retry policy and
// records the step. Activities MUST be idempotent: a re-driven run
// re-executes them and they must converge to the same state.
func (c *Ctx) ExecuteActivity(name string, input any) (any, error) {
	act, ok := c.acts[name]
	if !ok {
		return nil, fmt.Errorf("activity %q not registered", name)
	}
	policy := c.policy
	if policy.MaximumAttempts <= 0 {
		policy.MaximumAttempts = 1
	}
	var out any
	var err error
	for attempt := 1; attempt <= policy.MaximumAttempts; attempt++ {
		out, err = act(c.Context, input)
		if err == nil {
			c.Run.Steps = append(c.Run.Steps, fmt.Sprintf("%s -> ok", name))
			return out, nil
		}
		c.Run.Steps = append(c.Run.Steps, fmt.Sprintf("%s attempt %d -> %v", name, attempt, err))
		if attempt < policy.MaximumAttempts {
			select {
			case <-c.Context.Done():
				return nil, c.Context.Err()
			case <-time.After(policy.delay(attempt)):
			}
		}
	}
	return nil, fmt.Errorf("activity %q failed after %d attempts: %w", name, policy.MaximumAttempts, err)
}

// Step records an informational step on the run.
func (c *Ctx) Step(format string, args ...any) {
	c.Run.Steps = append(c.Run.Steps, fmt.Sprintf(format, args...))
}

// Run is the persisted workflow execution record (durable state).
type Run struct {
	ID        string   `json:"id"`
	Workflow  string   `json:"workflow"`
	Input     any      `json:"input,omitempty"`
	Steps     []string `json:"steps"`
	Status    string   `json:"status"` // running|completed|failed
	Error     string   `json:"error,omitempty"`
	Result    any      `json:"result,omitempty"`
	Attempt   int      `json:"attempt"` // 1 = first execution, >1 = re-driven
	Target    string   `json:"target"`  // inproc|temporal://<addr>
	StartedAt string   `json:"started_at"`
	EndedAt   string   `json:"ended_at,omitempty"`
}

// RunStore persists runs (the onboarding store.Store satisfies this via an
// adapter; see Runner construction in services/onboarding).
type RunStore interface {
	PutRun(run Run) error
	GetRun(id string) (Run, bool, error)
}

// TemporalDispatcher is the production hook: a real Temporal client
// (linked via build tag in a future hardening pass) submits the workflow to
// the cluster at TEMPORAL_URL. Nil dispatcher + TEMPORAL_URL set = fail
// closed at Execute.
type TemporalDispatcher interface {
	Submit(ctx context.Context, temporalURL, name string, input any) (result any, err error)
}

// Runner executes registered workflows durably.
type Runner struct {
	mu         sync.Mutex
	wfs        map[string]Workflow
	acts       map[string]Activity
	store      RunStore
	policy     RetryPolicy
	target     string // "inproc" or "temporal://<TEMPORAL_URL>"
	dispatcher TemporalDispatcher
	seq        int
	idPrefix   string
}

// NewRunner builds a runner over the persisted run store. target is
// "inproc" for dev or "temporal://<addr>" for prod; dispatcher may be nil
// (fail-closed at Execute for the temporal target).
func NewRunner(store RunStore, policy RetryPolicy, target string, dispatcher TemporalDispatcher) *Runner {
	if policy.MaximumAttempts == 0 {
		policy = DefaultRetryPolicy
	}
	return &Runner{
		wfs: map[string]Workflow{}, acts: map[string]Activity{},
		store: store, policy: policy, target: target, dispatcher: dispatcher,
		idPrefix: "run",
	}
}

// RegisterWorkflow adds a workflow definition (panics on duplicate).
func (r *Runner) RegisterWorkflow(name string, wf Workflow) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.wfs[name]; ok {
		panic("workflowx: duplicate workflow " + name)
	}
	r.wfs[name] = wf
}

// RegisterActivity adds a named activity (panics on duplicate).
func (r *Runner) RegisterActivity(name string, a Activity) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.acts[name]; ok {
		panic("workflowx: duplicate activity " + name)
	}
	r.acts[name] = a
}

// WorkflowNames lists registered workflows.
func (r *Runner) WorkflowNames() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.wfs))
	for n := range r.wfs {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

func nowUTC() string { return time.Now().UTC().Format(time.RFC3339) }

// Execute runs a workflow to completion, persisting run state transitions
// (running -> completed|failed). A process crash leaves the run "running";
// Redrive re-executes it idempotently.
func (r *Runner) Execute(ctx context.Context, name string, input any) (Run, error) {
	r.mu.Lock()
	wf, ok := r.wfs[name]
	r.seq++
	id := fmt.Sprintf("%s-%d-%d", r.idPrefix, time.Now().UnixNano(), r.seq)
	r.mu.Unlock()
	if !ok {
		return Run{}, fmt.Errorf("unknown workflow %q (have %v)", name, r.WorkflowNames())
	}
	run := Run{ID: id, Workflow: name, Input: input, Status: "running", StartedAt: nowUTC(), Attempt: 1, Target: r.target}
	return r.execute(ctx, wf, run)
}

func (r *Runner) execute(ctx context.Context, wf Workflow, run Run) (Run, error) {
	if r.store != nil {
		if err := r.store.PutRun(run); err != nil {
			return run, fmt.Errorf("persist run start: %w", err)
		}
	}
	if strings.HasPrefix(r.target, "temporal://") {
		// Production hook: dispatch to the Temporal cluster. Without a
		// linked dispatcher this fails closed — never silently runs in-proc.
		if r.dispatcher == nil {
			run.Status = "failed"
			run.Error = "TEMPORAL_URL is set but no Temporal client is linked in this build; refusing in-process execution in prod profile"
			run.EndedAt = nowUTC()
			if r.store != nil {
				_ = r.store.PutRun(run)
			}
			return run, fmt.Errorf("%s", run.Error)
		}
		out, err := r.dispatcher.Submit(ctx, strings.TrimPrefix(r.target, "temporal://"), run.Workflow, run.Input)
		if err != nil {
			run.Status = "failed"
			run.Error = err.Error()
		} else {
			run.Status = "completed"
			run.Result = out
		}
		run.EndedAt = nowUTC()
		if r.store != nil {
			_ = r.store.PutRun(run)
		}
		return run, err
	}
	wctx := &Ctx{Context: ctx, Run: &run, policy: r.policy, acts: r.acts}
	err := wf(wctx)
	run = *wctx.Run
	run.EndedAt = nowUTC()
	if err != nil {
		run.Status = "failed"
		run.Error = err.Error()
	} else {
		run.Status = "completed"
	}
	if r.store != nil {
		_ = r.store.PutRun(run)
	}
	return run, err
}

// Redrive re-executes a persisted run (crash recovery / manual re-drive).
// Only runs in "running" (crashed) or "failed" state are re-drivable; the
// attempt counter is incremented. Activities must be idempotent.
func (r *Runner) Redrive(ctx context.Context, runID string) (Run, error) {
	if r.store == nil {
		return Run{}, fmt.Errorf("no run store configured")
	}
	run, ok, err := r.store.GetRun(runID)
	if err != nil || !ok {
		return Run{}, fmt.Errorf("run %s not found", runID)
	}
	if run.Status == "completed" {
		return run, fmt.Errorf("run %s already completed (idempotent no-op)", runID)
	}
	r.mu.Lock()
	wf, ok := r.wfs[run.Workflow]
	r.mu.Unlock()
	if !ok {
		return Run{}, fmt.Errorf("workflow %q not registered in this build", run.Workflow)
	}
	run.Status = "running"
	run.Error = ""
	run.Attempt++
	run.Steps = append(run.Steps, fmt.Sprintf("redrive attempt %d", run.Attempt))
	return r.execute(ctx, wf, run)
}

// IsProdProfile reports whether the process runs in the production profile
// (mirrors keyx: APP_PROFILE=prod|production or AUTH_MODE=keycloak).
func IsProdProfile() bool {
	p := strings.ToLower(os.Getenv("APP_PROFILE"))
	if p == "prod" || p == "production" {
		return true
	}
	return strings.EqualFold(os.Getenv("AUTH_MODE"), "keycloak")
}

// NewRunnerFromEnv wires the runner per profile (fail-closed in prod):
// TEMPORAL_URL set -> temporal target (dispatcher hook, may be nil);
// TEMPORAL_URL unset + prod profile -> error; unset + dev -> in-process.
func NewRunnerFromEnv(store RunStore, dispatcher TemporalDispatcher) (*Runner, error) {
	if u := os.Getenv("TEMPORAL_URL"); u != "" {
		log.Printf("workflowx: profile=prod-runner target=temporal url=%s queue=%s", u, os.Getenv("TEMPORAL_TASK_QUEUE"))
		return NewRunner(store, DefaultRetryPolicy, "temporal://"+u, dispatcher), nil
	}
	if IsProdProfile() {
		return nil, fmt.Errorf("profile=prod FATAL: TEMPORAL_URL is required (durable workflows must not fall back to the in-process runner)")
	}
	log.Printf("workflowx: profile=dev runner=inproc (durable, re-drivable)")
	return NewRunner(store, DefaultRetryPolicy, "inproc", nil), nil
}
