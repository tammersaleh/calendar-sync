package daemon

import (
	"context"
	"errors"
	"testing"
)

// TestRunAuthCheck_NilCheckerProceeds: nil AuthChecker is the tests-only
// path; no probe runs and no error.
func TestRunAuthCheck_NilCheckerProceeds(t *testing.T) {
	if err := runAuthCheck(context.Background(), nil); err != nil {
		t.Errorf("nil checker should return nil error; got %v", err)
	}
}

// TestRunAuthCheck_SuccessReturnsNil: a checker returning nil propagates
// nil to the caller.
func TestRunAuthCheck_SuccessReturnsNil(t *testing.T) {
	checker := func(_ context.Context) error { return nil }
	if err := runAuthCheck(context.Background(), checker); err != nil {
		t.Errorf("success checker should return nil; got %v", err)
	}
}

// TestRunAuthCheck_FailureWrapsErrAuthFailed: a checker returning an error
// produces a wrapped ErrAuthFailed so callers can errors.Is on it.
func TestRunAuthCheck_FailureWrapsErrAuthFailed(t *testing.T) {
	cause := errors.New("gws auth status: not authenticated")
	checker := func(_ context.Context) error { return cause }
	err := runAuthCheck(context.Background(), checker)
	if err == nil {
		t.Fatalf("expected non-nil error")
	}
	if !errors.Is(err, ErrAuthFailed) {
		t.Errorf("err = %v; want errors.Is(err, ErrAuthFailed)", err)
	}
	if !errors.Is(err, cause) {
		t.Errorf("err = %v; want errors.Is(err, cause)", err)
	}
}

// TestRunAuthCheck_PassesContext: the checker receives the same ctx the
// caller provided.
func TestRunAuthCheck_PassesContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cancel() // pre-cancel

	var seen context.Context
	checker := func(c context.Context) error {
		seen = c
		return nil
	}
	if err := runAuthCheck(ctx, checker); err != nil {
		t.Fatalf("runAuthCheck = %v", err)
	}
	if seen.Err() == nil {
		t.Errorf("checker received uncancelled context; want canceled")
	}
}
