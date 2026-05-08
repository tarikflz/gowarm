package retry

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestExponentialBackoff_SucceedsAfterRetries(t *testing.T) {
	t.Parallel()
	rt := &ExponentialBackoff{
		BaseDelay:   1 * time.Millisecond,
		MaxDelay:    10 * time.Millisecond,
		Jitter:      0,
		Factor:      2.0,
		MaxAttempts: 5,
	}
	calls := 0
	err := rt.Execute(context.Background(), func(_ context.Context) error {
		calls++
		if calls < 3 {
			return errors.New("transient")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if calls != 3 {
		t.Errorf("expected 3 calls, got %d", calls)
	}
}

func TestExponentialBackoff_GivesUp(t *testing.T) {
	t.Parallel()
	rt := &ExponentialBackoff{
		BaseDelay:   1 * time.Millisecond,
		Factor:      2.0,
		MaxAttempts: 3,
	}
	calls := 0
	want := errors.New("fail")
	err := rt.Execute(context.Background(), func(_ context.Context) error {
		calls++
		return want
	})
	if !errors.Is(err, want) {
		t.Fatalf("expected %v, got %v", want, err)
	}
	if calls != 3 {
		t.Errorf("expected 3 calls, got %d", calls)
	}
}

func TestExponentialBackoff_StopsOnPermanent(t *testing.T) {
	t.Parallel()
	rt := &ExponentialBackoff{BaseDelay: 1 * time.Millisecond, MaxAttempts: 5, Factor: 2.0}
	calls := 0
	err := rt.Execute(context.Background(), func(_ context.Context) error {
		calls++
		return PermanentError(errors.New("404"))
	})
	if !IsPermanent(err) {
		t.Fatalf("expected permanent, got %v", err)
	}
	if calls != 1 {
		t.Errorf("expected 1 call, got %d", calls)
	}
}

func TestExponentialBackoff_RespectsContextCancel(t *testing.T) {
	t.Parallel()
	rt := &ExponentialBackoff{
		BaseDelay:   500 * time.Millisecond,
		Factor:      2.0,
		MaxAttempts: 5,
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	err := rt.Execute(ctx, func(_ context.Context) error {
		return errors.New("transient")
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	if time.Since(start) > 200*time.Millisecond {
		t.Errorf("retrier ignored cancellation; took %s", time.Since(start))
	}
}

func TestIsPermanent_Wraps(t *testing.T) {
	t.Parallel()
	base := errors.New("x")
	if !IsPermanent(PermanentError(base)) {
		t.Error("expected PermanentError to be permanent")
	}
	if IsPermanent(base) {
		t.Error("plain error should not be permanent")
	}
}
