package workflowx

import (
	"context"
	"errors"
	"sync"
	"testing"
)

type memStore struct {
	mu   sync.Mutex
	runs map[string]Run
}

func newMemStore() *memStore { return &memStore{runs: map[string]Run{}} }

func (m *memStore) PutRun(r Run) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.runs[r.ID] = r
	return nil
}

func (m *memStore) GetRun(id string) (Run, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[id]
	return r, ok, nil
}

func TestExecutePersistsRunAndSteps(t *testing.T) {
	st := newMemStore()
	r := NewRunner(st, RetryPolicy{MaximumAttempts: 1}, "inproc", nil)
	r.RegisterActivity("greet", func(ctx context.Context, in any) (any, error) {
		return "hello " + in.(string), nil
	})
	r.RegisterWorkflow("wf-test", func(ctx *Ctx) error {
		out, err := ctx.ExecuteActivity("greet", "world")
		if err != nil {
			return err
		}
		ctx.Run.Result = out
		return nil
	})
	run, err := r.Execute(context.Background(), "wf-test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "completed" || run.Result != "hello world" {
		t.Fatalf("run: %+v", run)
	}
	persisted, ok, _ := st.GetRun(run.ID)
	if !ok || persisted.Status != "completed" {
		t.Fatalf("run not persisted: %+v ok=%v", persisted, ok)
	}
	if len(persisted.Steps) == 0 {
		t.Fatal("expected step log")
	}
}

func TestActivityRetryThenFail(t *testing.T) {
	st := newMemStore()
	r := NewRunner(st, RetryPolicy{MaximumAttempts: 2, InitialInterval: 1}, "inproc", nil)
	calls := 0
	r.RegisterActivity("flaky", func(ctx context.Context, in any) (any, error) {
		calls++
		return nil, errors.New("boom")
	})
	r.RegisterWorkflow("wf-fail", func(ctx *Ctx) error {
		_, err := ctx.ExecuteActivity("flaky", nil)
		return err
	})
	run, err := r.Execute(context.Background(), "wf-fail", nil)
	if err == nil || run.Status != "failed" {
		t.Fatalf("expected failure: %+v err=%v", run, err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 attempts, got %d", calls)
	}
}

func TestRedriveFailedRun(t *testing.T) {
	st := newMemStore()
	r := NewRunner(st, RetryPolicy{MaximumAttempts: 1}, "inproc", nil)
	fail := true
	r.RegisterActivity("maybe", func(ctx context.Context, in any) (any, error) {
		if fail {
			return nil, errors.New("adapter outage")
		}
		return "ok", nil
	})
	r.RegisterWorkflow("wf-redrive", func(ctx *Ctx) error {
		out, err := ctx.ExecuteActivity("maybe", nil)
		if err != nil {
			return err
		}
		ctx.Run.Result = out
		return nil
	})
	run, _ := r.Execute(context.Background(), "wf-redrive", nil)
	if run.Status != "failed" {
		t.Fatalf("expected failed, got %+v", run)
	}
	fail = false // adapter recovered
	run2, err := r.Redrive(context.Background(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if run2.Status != "completed" || run2.Attempt != 2 || run2.Result != "ok" {
		t.Fatalf("redrive: %+v", run2)
	}
	// completed runs are not re-drivable
	if _, err := r.Redrive(context.Background(), run.ID); err == nil {
		t.Fatal("expected idempotent no-op error for completed run")
	}
}

func TestTemporalTargetFailsClosedWithoutDispatcher(t *testing.T) {
	st := newMemStore()
	r := NewRunner(st, RetryPolicy{MaximumAttempts: 1}, "temporal://temporal:7233", nil)
	r.RegisterWorkflow("wf-x", func(ctx *Ctx) error { return nil })
	run, err := r.Execute(context.Background(), "wf-x", nil)
	if err == nil || run.Status != "failed" {
		t.Fatalf("expected fail-closed, got %+v err=%v", run, err)
	}
}

func TestNewRunnerFromEnvProdRequiresTemporalURL(t *testing.T) {
	t.Setenv("APP_PROFILE", "prod")
	t.Setenv("TEMPORAL_URL", "")
	if _, err := NewRunnerFromEnv(newMemStore(), nil); err == nil {
		t.Fatal("expected prod profile without TEMPORAL_URL to fail")
	}
	t.Setenv("TEMPORAL_URL", "temporal:7233")
	rn, err := NewRunnerFromEnv(newMemStore(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if rn.target != "temporal://temporal:7233" {
		t.Fatalf("target = %s", rn.target)
	}
}

func TestNewRunnerFromEnvDevInproc(t *testing.T) {
	t.Setenv("APP_PROFILE", "")
	t.Setenv("AUTH_MODE", "")
	t.Setenv("TEMPORAL_URL", "")
	rn, err := NewRunnerFromEnv(newMemStore(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if rn.target != "inproc" {
		t.Fatalf("target = %s", rn.target)
	}
}
