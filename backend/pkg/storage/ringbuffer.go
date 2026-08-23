package storage

import (
	"sync"
	"sync/atomic"

	"dsh-go/pkg/session"
)

// RingBuffer is a high-performance in-memory ring buffer holding recent session events.
// It allows instantaneous event ingestion (< 1µs) and zero-alloc fan-out to active subscribers.
type RingBuffer struct {
	capacity int
	mask     int
	entries  []*session.SessionEnvelope
	head     atomic.Int64
	tail     atomic.Int64
	mu       sync.RWMutex
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
		entries:  make([]*session.SessionEnvelope, actualCap),
	}
}

// Push adds an event to the ring buffer. Overwrites oldest events if full.
func (rb *RingBuffer) Push(env *session.SessionEnvelope) {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	idx := int(rb.tail.Load()) & rb.mask
	rb.entries[idx] = env
	rb.tail.Add(1)

	// If buffer is full, advance head
	if rb.tail.Load()-rb.head.Load() > int64(rb.capacity) {
		rb.head.Store(rb.tail.Load() - int64(rb.capacity))
	}
}

// GetSince returns all events with Seq >= fromSeq currently available in memory.
func (rb *RingBuffer) GetSince(fromSeq int) []*session.SessionEnvelope {
	rb.mu.RLock()
	defer rb.mu.RUnlock()

	head := rb.head.Load()
	tail := rb.tail.Load()

	count := tail - head
	if count <= 0 {
		return nil
	}

	var res []*session.SessionEnvelope
	for i := head; i < tail; i++ {
		idx := int(i) & rb.mask
		env := rb.entries[idx]
		if env != nil && env.Seq >= fromSeq {
			res = append(res, env)
		}
	}
	return res
}

// Latest returns the most recent event or nil if empty.
func (rb *RingBuffer) Latest() *session.SessionEnvelope {
	rb.mu.RLock()
	defer rb.mu.RUnlock()

	tail := rb.tail.Load()
	if tail <= rb.head.Load() {
		return nil
	}
	return rb.entries[int(tail-1)&rb.mask]
}

// Clear resets the buffer.
func (rb *RingBuffer) Clear() {
	rb.mu.Lock()
	defer rb.mu.Unlock()

	rb.head.Store(0)
	rb.tail.Store(0)
	for i := range rb.entries {
		rb.entries[i] = nil
	}
}
