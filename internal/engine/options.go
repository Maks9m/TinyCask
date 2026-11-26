package engine

import (
	"fmt"
	"log/slog"
	"time"

	"github.com/Maks9m/TinyCask/internal/storage"
)

// DefaultMaxSegmentSize is the default roll threshold (64 MiB, ARCHITECTURE §12).
const DefaultMaxSegmentSize int64 = 64 << 20

// Options configures an Engine. The zero value uses the defaults.
type Options struct {
	// MaxSegmentSize is the roll threshold. A bigger record still fits, alone
	// in its own segment.
	MaxSegmentSize int64
	// MaxKeySize is the key limit in bytes (default 1 KiB).
	MaxKeySize uint32
	// MaxValueSize is the value limit in bytes (default 1 MiB).
	MaxValueSize uint32
	// Now is the timestamp clock (default time.Now); tests inject a fixed one.
	Now func() time.Time
	// Logger gets warnings such as a cut torn tail (default: discard). The
	// global logger is never used.
	Logger *slog.Logger
	// Sync chooses when appends are fsynced (default SyncInterval).
	Sync SyncPolicy
	// SyncInterval is the fsync period for SyncInterval (default 1 s).
	SyncInterval time.Duration

	// newTicker starts the sync ticker; tests swap in a hand-driven channel.
	newTicker tickerFunc
	// afterTick runs after each tick; tests use it to wait without sleeping.
	afterTick func()
}

// withDefaults fills zero fields and rejects impossible values.
func (o Options) withDefaults() (Options, error) {
	if o.MaxSegmentSize < 0 {
		return o, fmt.Errorf("engine: MaxSegmentSize %d is negative", o.MaxSegmentSize)
	}
	if o.MaxSegmentSize == 0 {
		o.MaxSegmentSize = DefaultMaxSegmentSize
	}
	if o.MaxKeySize == 0 {
		o.MaxKeySize = storage.DefaultMaxKeySize
	}
	if o.MaxValueSize == 0 {
		o.MaxValueSize = storage.DefaultMaxValueSize
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Logger == nil {
		o.Logger = slog.New(slog.DiscardHandler)
	}
	if o.Sync > SyncNever {
		return o, fmt.Errorf("engine: unknown sync policy %v", o.Sync)
	}
	if o.SyncInterval < 0 {
		return o, fmt.Errorf("engine: SyncInterval %v is negative", o.SyncInterval)
	}
	if o.SyncInterval == 0 {
		o.SyncInterval = DefaultSyncInterval
	}
	if o.newTicker == nil {
		o.newTicker = realTicker
	}
	if o.afterTick == nil {
		o.afterTick = func() {}
	}
	return o, nil
}

func (o Options) limits() storage.Limits {
	return storage.Limits{MaxKeySize: o.MaxKeySize, MaxValueSize: o.MaxValueSize}
}
