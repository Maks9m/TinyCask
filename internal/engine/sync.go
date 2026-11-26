package engine

import (
	"context"
	"fmt"
	"time"
)

// SyncPolicy chooses when appended records are fsynced (ARCHITECTURE §7).
// Whatever the policy, a segment is fsynced before it is sealed and on Close.
type SyncPolicy uint8

const (
	// SyncInterval fsyncs the active segment from a background ticker every
	// Options.SyncInterval. A power loss loses at most about one interval.
	// It is the zero value and the default.
	SyncInterval SyncPolicy = iota
	// SyncAlways fsyncs after every append, before Put or Delete returns.
	// An acknowledged write survives a power loss.
	SyncAlways
	// SyncNever fsyncs only on roll and Close; what a power loss keeps is up
	// to the OS.
	SyncNever
)

// DefaultSyncInterval is the default fsync period for SyncInterval
// (ARCHITECTURE §12).
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

// tickerFunc starts a ticker of period d and returns its channel and a func
// that stops it. Tests inject one they drive by hand, so no test sleeps.
type tickerFunc func(d time.Duration) (ticks <-chan time.Time, stop func())

// realTicker is the production tickerFunc.
func realTicker(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTicker(d)
	return t.C, t.Stop
}

// startBackground starts the goroutines the options ask for (ARCHITECTURE
// §5.4 step 6). Each is owned by the engine: Close cancels bgCancel and
// waits on bg (invariant 11).
func (e *Engine) startBackground() {
	ctx, cancel := context.WithCancel(context.Background())
	e.bgCancel = cancel
	if e.opts.Sync == SyncInterval {
		ticks, stop := e.opts.newTicker(e.opts.SyncInterval)
		e.bg.Go(func() { e.syncLoop(ctx, ticks, stop) })
	}
}

// stopBackground cancels the background goroutines and waits for them. It
// must be called without e.mu, which a goroutine may be waiting for. Safe to
// call more than once and from concurrent Close calls.
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

// syncTick checks under the writer lock that a sync is useful, then fsyncs
// outside it so writers are not stalled by the disk (ARCHITECTURE §6). A
// failed fsync is never retried: the engine goes read-only (invariant 8).
// The error has no caller to return to, so it is logged here, once.
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

// writable reports, under the writer lock, whether the engine takes writes.
// It holds e.mu only for the flag check, never across I/O.
func (e *Engine) writable() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.writableLocked() == nil
}

// syncAfterAppendLocked applies SyncAlways after a successful append. A
// failure switches the engine to read-only (invariant 8). e.mu must be held.
func (e *Engine) syncAfterAppendLocked() error {
	if e.opts.Sync != SyncAlways {
		return nil
	}
	if err := e.store.SyncActive(); err != nil {
		return e.fail(fmt.Errorf("engine: sync: %w", err))
	}
	return nil
}
