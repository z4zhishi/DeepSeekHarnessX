package storage

import (
	"sync/atomic"

	"dsh-go/pkg/session"
)

// RingBuffer is a lock-free in-memory ring buffer holding recent session
// events. The agent actor is the single writer (Push) while any number of
// readers (GetSince/Latest) may observe concurrently: slots are published
// with release-ordered atomic stores and consumed with acquire-ordered
// loads, so a reader can never observe a torn slot. head/tail counters are
// monotonic sequence numbers; the writer advances tail and evicts by
// pushing head, and readers only snapshot both.
type RingBuffer struct {
	capacity int
	mask     int
	entries  []atomic.Pointer[session.SessionEnvelope]
	head     atomic.Int64
	tail     atomic.Int64
}

// NewRingBuffer creates a ring buffer with power-of-two capacity (e.g. 1024, 2048, 4096).
func NewRingBuffer(capacity int) *RingBuffer {
	// Ensure power of two
	actualCap := 1
	for actualCap < capacity {
		actualCap <<= 1
	}

	return &RingBuffer{
		capacity: actualCap,
		mask:     actualCap - 1,
		entries:  make([]atomic.Pointer[session.SessionEnvelope], actualCap),
	}
}

// Push adds an event to the ring buffer. Overwrites oldest events if full.
// Single-writer: only the agent actor calls Push.
func (rb *RingBuffer) Push(env *session.SessionEnvelope) {
	idx := int(rb.tail.Load()) & rb.mask
	rb.entries[idx].Store(env)
	rb.tail.Add(1)

	// If buffer is full, advance head (never past tail).
	if rb.tail.Load()-rb.head.Load() > int64(rb.capacity) {
		rb.head.Store(rb.tail.Load() - int64(rb.capacity))
	}
}

// GetSince returns all events with Seq >= fromSeq currently available in memory.
func (rb *RingBuffer) GetSince(fromSeq int) []*session.SessionEnvelope {
	head := rb.head.Load()
	tail := rb.tail.Load()

	count := tail - head
	if count <= 0 {
		return nil
	}

	var res []*session.SessionEnvelope
	for i := head; i < tail; i++ {
		idx := int(i) & rb.mask
		env := rb.entries[idx].Load()
		if env != nil && env.Seq >= fromSeq {
			res = append(res, env)
		}
	}
	return res
}

// Latest returns the most recent event or nil if empty.
func (rb *RingBuffer) Latest() *session.SessionEnvelope {
	tail := rb.tail.Load()
	if tail <= rb.head.Load() {
		return nil
	}
	return rb.entries[int(tail-1)&rb.mask].Load()
}

// Clear resets the buffer. Call only when no concurrent reader is active
// (process shutdown path).
func (rb *RingBuffer) Clear() {
	rb.head.Store(0)
	rb.tail.Store(0)
	for i := range rb.entries {
		rb.entries[i].Store(nil)
	}
}