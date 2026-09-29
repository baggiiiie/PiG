package pilock

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLockStaleAndCompromisedOwnership(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.Mkdir(path+".lock", 0o700); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-2 * SyncStale)
	if err := os.Chtimes(path+".lock", old, old); err != nil {
		t.Fatal(err)
	}
	lock, err := AcquireSync(path)
	if err != nil {
		t.Fatal(err)
	}
	// An external replacement must be surfaced before commit and must not be
	// removed by the former owner's release (Pi onCompromised contract).
	changed := time.Now().Add(time.Second)
	if err := os.Chtimes(path+".lock", changed, changed); err != nil {
		t.Fatal(err)
	}
	if err := lock.Check(); err == nil {
		t.Fatal("lost lock was accepted")
	}
	if err := lock.Release(); err == nil {
		t.Fatal("compromised lock released without error")
	}
	if _, err := os.Stat(path + ".lock"); err != nil {
		t.Fatalf("removed new owner's lock: %v", err)
	}
}

func TestCancelledAcquisitionLeavesOwnerIntact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	owner, err := AcquireSync(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() {
		lock, err := Acquire(ctx, path)
		if lock != nil {
			err = errors.Join(err, lock.Release())
		}
		done <- err
	}()
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled acquire: %v", err)
	}
	if err := owner.Check(); err != nil {
		t.Fatal(err)
	}
	if err := owner.Release(); err != nil {
		t.Fatal(err)
	}
}

func TestLockHeartbeatAndReleaseJoin(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	// A short injected stale interval exercises the identical heartbeat loop
	// without changing the production protocol's 10/30-second thresholds.
	const stale = 40 * time.Millisecond
	lock, err := acquire(path, stale)
	if err != nil {
		t.Fatal(err)
	}
	lock.mu.Lock()
	initial := lock.mtime
	lock.mu.Unlock()
	time.Sleep(3 * stale)
	lock.mu.Lock()
	advanced := lock.mtime.After(initial)
	lock.mu.Unlock()
	if !advanced {
		t.Error("heartbeat did not update mtime")
	}
	if err := lock.Check(); err != nil {
		t.Error(err)
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-lock.done:
	default:
		t.Fatal("release did not join heartbeat")
	}
	if _, err := os.Stat(path + ".lock"); !os.IsNotExist(err) {
		t.Fatalf("lock remains: %v", err)
	}
}

func BenchmarkLockRoundTrip(b *testing.B) {
	path := filepath.Join(b.TempDir(), "settings.json")
	b.ReportAllocs()
	for b.Loop() {
		lock, err := AcquireSync(path)
		if err != nil {
			b.Fatal(err)
		}
		if err := lock.Release(); err != nil {
			b.Fatal(err)
		}
	}
}
