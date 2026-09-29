// Package pilock implements Pi's proper-lockfile directory-lock protocol for shared stores.
// Ports packages/coding-agent/src/core/auth-storage.ts
package pilock

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// Pi 0.87.1 auth-storage.ts:69-93,119-143 and settings-manager.ts:241-265.
const (
	SyncAttempts = 10
	SyncDelay    = 20 * time.Millisecond
	// proper-lockfile 4.1.2 lib/lockfile.js:208.
	SyncStale  = 10 * time.Second
	asyncStale = 30 * time.Second
	maxDelay   = 2 * time.Second
)

// ErrLocked reports contention under Pi's directory protocol.
var ErrLocked = errors.New("lock file is already being held")

// ErrLegacyLocked reports an older PiG writer; acquisition uses the same retry budget as directory contention.
var ErrLegacyLocked = fmt.Errorf("%w by an older PiG process; stop the older PiG before upgrading", ErrLocked)
var errCompromised = errors.New("lock compromised")

// Lock owns a directory and its mtime heartbeat. Compromise cancels its operation context; Release joins the heartbeat and removes only the owned directory.
type Lock struct {
	path       string
	mu         sync.Mutex
	mtime      time.Time
	err        error
	ctx        context.Context
	cancel     context.CancelCauseFunc
	stop       chan struct{}
	done       chan struct{}
	once       sync.Once
	releaseErr error
}

// AcquireSync follows Pi's bounded synchronous acquisition loop, reclaiming stale idle legacy sidecars.
func AcquireSync(path string) (*Lock, error) {
	for attempt := 1; ; attempt++ {
		lock, err := acquire(path, SyncStale)
		if !errors.Is(err, ErrLocked) || attempt == SyncAttempts {
			return lock, err
		}
		time.Sleep(SyncDelay)
	}
}

// Acquire follows Pi's cancellable auth acquisition loop, reclaiming stale idle legacy sidecars. The caller owns the returned lock until Release.
func Acquire(ctx context.Context, path string) (*Lock, error) {
	deadline := time.Now().Add(asyncStale)
	baseDelay := 10 * time.Millisecond
	for {
		if err := context.Cause(ctx); err != nil {
			return nil, err
		}
		lock, err := tryAcquire(ctx, path, asyncStale)
		if err == nil {
			if err := context.Cause(ctx); err != nil {
				return nil, errors.Join(err, lock.Release())
			}
			return lock, nil
		}
		remaining := time.Until(deadline)
		if !errors.Is(err, ErrLocked) || remaining <= 0 {
			return nil, err
		}
		jitter := time.Duration(math.Floor(float64(baseDelay/time.Millisecond)*(1+rand.Float64())+0.5)) * time.Millisecond
		baseDelay = min(baseDelay*2, maxDelay/2)
		timer := time.NewTimer(min(jitter, remaining))
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, context.Cause(ctx)
		case <-timer.C:
		}
	}
}

func acquire(path string, stale time.Duration) (*Lock, error) {
	return tryAcquire(context.Background(), path, stale)
}

func tryAcquire(ctx context.Context, path string, stale time.Duration) (*Lock, error) {
	path, err := filepath.Abs(path + ".lock")
	if err != nil {
		return nil, err
	}
	if err := mkdir(path, stale); err != nil {
		return nil, err
	}
	mtime, err := touch(path)
	if err != nil {
		return nil, errors.Join(err, syscall.Rmdir(path))
	}
	ctx, cancel := context.WithCancelCause(ctx)
	lock := &Lock{path: path, mtime: mtime, ctx: ctx, cancel: cancel, stop: make(chan struct{}), done: make(chan struct{})}
	go lock.heartbeat(stale)
	return lock, nil
}

func mkdir(path string, stale time.Duration) error {
	err := os.Mkdir(path, 0o777)
	if err == nil || !errors.Is(err, fs.ErrExist) {
		return err
	}
	if stale <= 0 {
		return ErrLocked
	}
	info, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return mkdir(path, 0)
	}
	if err != nil {
		return err
	}
	if info.Mode().IsRegular() && info.Size() == 0 {
		if !info.ModTime().Before(time.Now().Add(-stale)) {
			return ErrLocked
		}
		// pig divergence (D73): reclaim stale PiG-owned sidecars under their old OS lock before entering Pi's directory protocol.
		observed, err := observeLegacy(path, info)
		if errors.Is(err, fs.ErrNotExist) {
			return mkdir(path, 0)
		}
		if err != nil {
			return err
		}
		return replaceLegacy(path, observed, stale)
	}
	if !info.IsDir() {
		return fmt.Errorf("lock path is neither a directory nor an empty legacy sidecar: %s", path)
	}
	if !info.ModTime().Before(time.Now().Add(-stale)) {
		return ErrLocked
	}
	if err := syscall.Rmdir(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return mkdir(path, 0)
}

func touch(path string) (time.Time, error) {
	now := time.Now().Truncate(time.Millisecond)
	if err := os.Chtimes(path, now, now); err != nil {
		return time.Time{}, err
	}
	info, err := os.Stat(path)
	if err != nil {
		return time.Time{}, err
	}
	return info.ModTime(), nil
}

func (l *Lock) check() error {
	if l.err != nil {
		return l.err
	}
	info, err := os.Stat(l.path)
	if err != nil {
		l.err = fmt.Errorf("%w: %w", errCompromised, err)
	} else if !info.IsDir() || !info.ModTime().Equal(l.mtime) {
		l.err = fmt.Errorf("%w: modification time changed", errCompromised)
	}
	if l.err != nil {
		l.cancel(l.err)
	}
	return l.err
}

// Check refuses a write after cancellation or after the lock has been removed or replaced.
func (l *Lock) Check() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if err := l.check(); err != nil {
		return err
	}
	return context.Cause(l.ctx)
}

func (l *Lock) heartbeat(stale time.Duration) {
	defer close(l.done)
	lastUpdate := time.Now()
	delay := stale / 2
	for {
		timer := time.NewTimer(delay)
		select {
		case <-l.stop:
			timer.Stop()
			return
		case <-timer.C:
		}
		l.mu.Lock()
		info, err := os.Stat(l.path)
		if err == nil && (!info.IsDir() || !info.ModTime().Equal(l.mtime)) {
			l.err = fmt.Errorf("%w: modification time changed", errCompromised)
		} else {
			if err == nil {
				var next time.Time
				next, err = touch(l.path)
				if err == nil {
					l.mtime = next
				}
			}
			if err != nil {
				if errors.Is(err, fs.ErrNotExist) || time.Since(lastUpdate) > stale {
					l.err = fmt.Errorf("%w: %w", errCompromised, err)
				}
				delay = time.Second
			} else {
				lastUpdate = time.Now()
				delay = stale / 2
			}
		}
		failed := l.err != nil
		if failed {
			l.cancel(l.err)
		}
		l.mu.Unlock()
		if failed {
			return
		}
	}
}

// Context cancels with the caller or when another process compromises the lock.
func (l *Lock) Context() context.Context { return l.ctx }

// Release joins the heartbeat before removing the owned directory. Repeated calls return the same result.
func (l *Lock) Release() error {
	l.once.Do(func() {
		close(l.stop)
		<-l.done
		l.mu.Lock()
		defer l.mu.Unlock()
		l.releaseErr = l.check()
		if l.releaseErr == nil {
			l.releaseErr = errors.Join(context.Cause(l.ctx), syscall.Rmdir(l.path))
		}
		l.cancel(context.Canceled)
	})
	return l.releaseErr
}
