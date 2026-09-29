package ai

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"
)

// Pi auth/resolve.ts:149-153 retains AbortSignal.any([caller, AbortSignal.timeout(15000)]) even after the callback settles, on success or rejection.
func TestOAuthRefreshSignalSurvivesSettlement(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "success", true: "failure"}[fail], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancelCause(t.Context())
				defer cancel(nil)
				failure := errors.New("refresh rejected")
				var signal context.Context
				oauth := &OAuthAuth{Refresh: func(ctx context.Context, c Credential) (Credential, error) {
					signal = ctx
					if fail {
						return Credential{}, failure
					}
					return c, nil
				}}
				_, err := refreshOAuthWithTimeout(ctx, oauth, Credential{})
				if fail && !errors.Is(err, failure) || !fail && err != nil {
					t.Fatalf("refresh error = %v", err)
				}
				if signal.Err() != nil {
					t.Errorf("settlement canceled retained signal: %v", context.Cause(signal))
				}
				cause := errors.New("later caller cancellation")
				cancel(cause)
				if !errors.Is(context.Cause(signal), cause) {
					t.Fatalf("retained signal cause = %v, want %v", context.Cause(signal), cause)
				}
			})
		})
	}
}

func TestOAuthRefreshSignalKeepsOriginalTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var signal context.Context
		start := time.Now()
		oauth := &OAuthAuth{Refresh: func(ctx context.Context, c Credential) (Credential, error) {
			signal = ctx
			return c, nil
		}}
		if _, err := refreshOAuthWithTimeout(t.Context(), oauth, Credential{}); err != nil {
			t.Fatal(err)
		}
		if deadline, ok := signal.Deadline(); !ok || !deadline.Equal(start.Add(defaultOAuthRefreshTimeout)) {
			t.Errorf("deadline = %v, %t", deadline, ok)
		}
		if signal.Err() != nil {
			t.Fatalf("completed refresh signal already canceled: %v", signal.Err())
		}
		<-signal.Done()
		if elapsed := time.Since(start); elapsed != defaultOAuthRefreshTimeout {
			t.Errorf("timeout after %s, want %s", elapsed, defaultOAuthRefreshTimeout)
		}
		if !errors.Is(signal.Err(), context.DeadlineExceeded) || !errors.Is(context.Cause(signal), context.DeadlineExceeded) {
			t.Errorf("timeout = %v, cause = %v", signal.Err(), context.Cause(signal))
		}
	})
}

func TestOAuthRefreshSignalKeepsCallerDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		deadline, _ := ctx.Deadline()
		oauth := &OAuthAuth{Refresh: func(ctx context.Context, _ Credential) (Credential, error) {
			if got, ok := ctx.Deadline(); !ok || !got.Equal(deadline) {
				t.Errorf("callback deadline = %v, %t; want %v", got, ok, deadline)
			}
			<-ctx.Done()
			if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
				t.Errorf("callback Err = %v", ctx.Err())
			}
			return Credential{}, context.Cause(ctx)
		}}
		_, err := refreshOAuthWithTimeout(ctx, oauth, Credential{})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("caller timeout = %v", err)
		}
	})
}

func TestOAuthRefreshSignalCancelsActiveCallback(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		oauth := &OAuthAuth{Refresh: func(ctx context.Context, _ Credential) (Credential, error) {
			<-ctx.Done()
			return Credential{}, context.Cause(ctx)
		}}
		start := time.Now()
		_, err := refreshOAuthWithTimeout(t.Context(), oauth, Credential{})
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != defaultOAuthRefreshTimeout {
			t.Fatalf("active timeout = %v after %s", err, time.Since(start))
		}
	})
}
