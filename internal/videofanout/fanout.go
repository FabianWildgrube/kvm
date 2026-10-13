// Package videofanout turns the capture pipeline's single-consumer frame
// handoff into a fan-out, so the WebRTC track and the WebSocket transport can
// both receive the encoded stream.
//
// Two properties drive the design:
//
//   - Publishing must never block. It runs on the goroutine draining the
//     frame socket from the native process; stalling it stalls capture for
//     every consumer, including WebRTC.
//   - The buffer handed to Publish is reused by the caller on its next
//     iteration (see internal/native/proxy.go), so anything passed to another
//     goroutine must be copied first.
package videofanout

import (
	"sync"
	"sync/atomic"
	"time"

	"github.com/jetkvm/kvm/internal/fmp4"
)

// QueueDepth is how many encoded frames may be queued for a subscriber
// before its connection is considered too slow. At the encoder's ~0.5s IDR
// cadence this is well under one GOP, so a backlog resolves at the next
// keyframe rather than by growing latency.
const QueueDepth = 8

// Frame is an owned copy of one encoded access unit.
type Frame struct {
	Data     []byte
	Duration time.Duration
	Keyframe bool
}

// Subscriber is one consumer of the encoded stream.
type Subscriber struct {
	frames chan Frame

	// resync is set when frames were discarded. The consumer then skips
	// access units until the next keyframe, so the decoder restarts cleanly
	// instead of rendering a corrupt picture for the rest of the GOP.
	resync atomic.Bool

	dropped atomic.Uint64
}

// Frames returns the channel of encoded frames. It is closed by Unsubscribe.
func (s *Subscriber) Frames() <-chan Frame { return s.frames }

// Dropped reports how many frames have been discarded for being too slow.
func (s *Subscriber) Dropped() uint64 { return s.dropped.Load() }

// NeedsResync reports whether the subscriber is waiting for a keyframe.
func (s *Subscriber) NeedsResync() bool { return s.resync.Load() }

// Queued reports the current backlog. Exposed for diagnostics and tests.
func (s *Subscriber) Queued() int { return len(s.frames) }

// ShouldDeliver reports whether this frame may be rendered. After a drop the
// stream only resumes at a keyframe.
func (s *Subscriber) ShouldDeliver(f Frame) bool {
	if !s.resync.Load() {
		return true
	}
	if !f.Keyframe {
		return false
	}
	s.resync.Store(false)
	return true
}

// Hub distributes encoded frames to its subscribers.
type Hub struct {
	mu   sync.Mutex
	subs map[*Subscriber]struct{}

	// hasSubs mirrors len(subs) so Publish can skip the copy and the lock
	// entirely when nothing is listening, which is the case for every
	// plain WebRTC session.
	hasSubs atomic.Bool
}

// NewHub returns an empty hub.
func NewHub() *Hub {
	return &Hub{subs: map[*Subscriber]struct{}{}}
}

// Subscribe registers a new consumer.
func (h *Hub) Subscribe() *Subscriber {
	sub := &Subscriber{frames: make(chan Frame, QueueDepth)}

	// A fresh subscriber has no decoder state, so it must start at a
	// keyframe just as if it had fallen behind.
	sub.resync.Store(true)

	h.mu.Lock()
	defer h.mu.Unlock()

	h.subs[sub] = struct{}{}
	h.hasSubs.Store(true)
	return sub
}

// Unsubscribe removes a consumer and closes its frame channel. It is safe to
// call more than once.
func (h *Hub) Unsubscribe(sub *Subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if _, ok := h.subs[sub]; !ok {
		return
	}
	delete(h.subs, sub)
	h.hasSubs.Store(len(h.subs) > 0)
	close(sub.frames)
}

// HasSubscribers reports whether anything is listening.
func (h *Hub) HasSubscribers() bool { return h.hasSubs.Load() }

// Publish hands a copy of frame to every subscriber. It never blocks: a
// subscriber that cannot keep up has its backlog discarded and is told to
// resynchronise at the next keyframe.
//
// frame is only read for the duration of the call.
func (h *Hub) Publish(frame []byte, duration time.Duration) {
	if !h.hasSubs.Load() {
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	if len(h.subs) == 0 {
		return
	}

	// One copy is shared by all subscribers; nothing downstream writes to it.
	owned := Frame{
		Data:     append([]byte(nil), frame...),
		Duration: duration,
		Keyframe: fmp4.IsKeyframe(frame),
	}

	for sub := range h.subs {
		select {
		case sub.frames <- owned:
		default:
			// The queue is full. Drain it rather than dropping just this
			// frame: keeping stale frames would pin latency at the queue
			// depth forever once the link has hiccupped.
			drained := drain(sub.frames)
			sub.dropped.Add(uint64(drained) + 1)
			sub.resync.Store(true)

			select {
			case sub.frames <- owned:
			default:
				// A consumer that refilled the queue between the drain and
				// here is beyond help this round; the resync flag lets it
				// recover at the next keyframe.
			}
		}
	}
}

func drain(ch chan Frame) int {
	n := 0
	for {
		select {
		case <-ch:
			n++
		default:
			return n
		}
	}
}
