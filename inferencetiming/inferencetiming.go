// Package inferencetiming buffers per-call inference-duration samples so they
// can be produced from one component (bark_monitor) and consumed by another
// (inference-monitor) without coupling the two together or losing samples
// when their poll cadences don't line up.
package inferencetiming

import (
	"sync"
	"time"
)

// Entry is one completed inference call. StartedAt is when the inference
// call itself began — not when the underlying audio/spectrogram was
// captured by the mic.
type Entry struct {
	StartedAt  time.Time
	DurationMs float64
}

// maxBuffered bounds memory if nothing drains the buffer (e.g. the
// inference-monitor sensor isn't configured or its capture is disabled).
// Oldest samples are dropped once exceeded rather than growing unbounded.
const maxBuffered = 1000

var (
	mu      sync.Mutex
	entries []Entry
)

// Record appends a completed inference call's start time and duration. Safe
// for concurrent use.
func Record(startedAt time.Time, duration time.Duration) {
	mu.Lock()
	defer mu.Unlock()
	entries = append(entries, Entry{
		StartedAt:  startedAt,
		DurationMs: float64(duration) / float64(time.Millisecond),
	})
	if len(entries) > maxBuffered {
		entries = entries[len(entries)-maxBuffered:]
	}
}

// Drain returns every entry recorded since the last Drain and clears the
// buffer.
func Drain() []Entry {
	mu.Lock()
	defer mu.Unlock()
	out := entries
	entries = nil
	return out
}
