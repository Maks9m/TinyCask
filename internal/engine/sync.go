package engine

import (
	"context"
	"fmt"
	"time"
)

// SyncPolicy chooses when appends are fsynced (ARCHITECTURE §7). Segments are
// always fsynced before sealing and on Close.
type SyncPolicy uint8

const (
	// SyncInterval (default) fsyncs from a background ticker; a power loss
	// loses at most about one interval.
	SyncInterval SyncPolicy = iota
	// SyncAlways fsyncs before Put/Delete returns; acked writes survive power loss.
	SyncAlways
	// SyncNever fsyncs only on roll and Close; the rest is up to the OS.
	SyncNever
)

// DefaultSyncInterval is the default fsync period (ARCHITECTURE §12).
const DefaultSyncInterval = time.Second

// String returns the policy's flag spelling.
func (p SyncPolicy) String() string {
	switch p {
	case SyncInterval:
		return "interval"
	case SyncAlways:
		return "always"
	case SyncNever:
		return "never"
	}
	return fmt.Sprintf("SyncPolicy(%d)", uint8(p))
}

// tickerFunc starts a ticker and returns its channel and stop func. Injectable
// so tests drive ticks by hand instead of sleeping.
type tickerFunc func(d time.Duration) (ticks <-chan time.Time, stop func())

func realTicker(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTicker(d)
	return t.C, t.Stop
}

// startBackground starts background goroutines (ARCHITECTURE §5.4 step 6).
// Close cancels and waits for them (invariant 11).
func (e *Engine) startBackground() {
	ctx, cancel := context.WithCancel(context.Background())
	e.bgCancel = cancel
	if e.opts.Sync == SyncInterval {
		ticks, stop := e.opts.newTicker(e.opts.SyncInterval)
		e.bg.Go(func() { e.syncLoop(ctx, ticks, stop) })
	}
}

// stopBackground cancels and waits for the goroutines. Call without e.mu (a
// goroutine may be waiting for it). Safe to call repeatedly.
func (e *Engine) stopBackground() {
	if e.bgCancel != nil {
		e.bgCancel()
	}
	e.bg.Wait()
}

// syncLoop fsyncs the active segment on every tick until ctx is canceled.
func (e *Engine) syncLoop(ctx context.Context, ticks <-chan time.Time, stop func()) {
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			e.syncTick()
			e.opts.afterTick()
		}
	}
}

// syncTick fsyncs outside the writer lock so writers aren't stalled by disk
// (ARCHITECTURE §6). Failure → read-only, no retry (invariant 8). No caller to
// return to, so the error is logged here.
func (e *Engine) syncTick() {
	if !e.writable() {
		return
	}
	if err := e.store.SyncActive(); err != nil {
		e.mu.Lock()
		err = e.fail(fmt.Errorf("engine: interval sync: %w", err))
		e.mu.Unlock()
		e.opts.Logger.Error("background fsync failed", "dir", e.dir, "read_only", e.readOnly.Load(), "err", err)
	}
}

// writable checks the flags under e.mu, never held across I/O.
func (e *Engine) writable() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.writableLocked() == nil
}

// syncAfterAppendLocked applies SyncAlways; failure → read-only (invariant 8).
// e.mu must be held.
func (e *Engine) syncAfterAppendLocked() error {
	if e.opts.Sync != SyncAlways {
		return nil
	}
	if err := e.store.SyncActive(); err != nil {
		return e.fail(fmt.Errorf("engine: sync: %w", err))
	}
	return nil
}
