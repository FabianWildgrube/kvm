package videofanout

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// annexB builds an Annex-B access unit from the given NAL units.
func annexB(nalus ...[]byte) []byte {
	var out []byte
	for _, n := range nalus {
		out = append(out, 0, 0, 0, 1)
		out = append(out, n...)
	}
	return out
}

func keyframeAU() []byte   { return annexB([]byte{0x65, 0xaa, 0xbb, 0xcc, 0xdd}) }
func interframeAU() []byte { return annexB([]byte{0x41, 0xaa, 0xbb, 0xcc, 0xdd}) }

func TestPublishWithNoSubscribersIsANoop(t *testing.T) {
	h := NewHub()

	assert.NotPanics(t, func() { h.Publish(keyframeAU(), 10*time.Millisecond) })
	assert.False(t, h.HasSubscribers())
}

// The native layer hands us a buffer it reuses for the next frame, so the
// fan-out has to copy. If it ever stops, subscribers silently receive
// whatever the next frame overwrote.
func TestPublishCopiesTheFrameBuffer(t *testing.T) {
	h := NewHub()
	sub := h.Subscribe()
	defer h.Unsubscribe(sub)

	buf := keyframeAU()
	original := append([]byte(nil), buf...)

	h.Publish(buf, 10*time.Millisecond)

	// Simulate the native reader reusing its buffer for the next frame.
	for i := range buf {
		buf[i] = 0xff
	}

	frame := <-sub.Frames()
	assert.Equal(t, original, frame.Data,
		"the delivered frame must not alias the caller's buffer")
}

func TestPublishDeliversToEverySubscriber(t *testing.T) {
	h := NewHub()
	a, b := h.Subscribe(), h.Subscribe()
	defer h.Unsubscribe(a)
	defer h.Unsubscribe(b)

	h.Publish(keyframeAU(), 20*time.Millisecond)

	for name, sub := range map[string]*Subscriber{"a": a, "b": b} {
		select {
		case frame := <-sub.Frames():
			assert.True(t, frame.Keyframe, "%s: keyframe flag", name)
			assert.Equal(t, 20*time.Millisecond, frame.Duration, "%s: duration", name)
		default:
			t.Fatalf("%s received no frame", name)
		}
	}
}

func TestKeyframeDetection(t *testing.T) {
	h := NewHub()
	sub := h.Subscribe()
	defer h.Unsubscribe(sub)

	h.Publish(interframeAU(), 10*time.Millisecond)
	assert.False(t, (<-sub.Frames()).Keyframe, "a P frame is not a keyframe")

	h.Publish(keyframeAU(), 10*time.Millisecond)
	assert.True(t, (<-sub.Frames()).Keyframe, "an IDR is a keyframe")
}

func TestUnsubscribeClosesTheChannel(t *testing.T) {
	h := NewHub()
	sub := h.Subscribe()
	require.True(t, h.HasSubscribers())

	h.Unsubscribe(sub)
	assert.False(t, h.HasSubscribers())

	_, ok := <-sub.Frames()
	assert.False(t, ok, "the frame channel must be closed on unsubscribe")

	assert.NotPanics(t, func() { h.Unsubscribe(sub) },
		"a second unsubscribe must not close the channel twice")
}

func TestUnsubscribeOneLeavesOthersRunning(t *testing.T) {
	h := NewHub()
	a, b := h.Subscribe(), h.Subscribe()
	defer h.Unsubscribe(b)

	h.Unsubscribe(a)
	require.True(t, h.HasSubscribers())

	h.Publish(keyframeAU(), 10*time.Millisecond)
	select {
	case frame := <-b.Frames():
		assert.True(t, frame.Keyframe)
	default:
		t.Fatal("the remaining subscriber stopped receiving frames")
	}
}

// A subscriber that stops reading must not stall the capture goroutine, and
// must not be left holding a queue of stale frames.
func TestSlowSubscriberIsDrainedRatherThanBlocking(t *testing.T) {
	h := NewHub()
	sub := h.Subscribe()
	defer h.Unsubscribe(sub)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < QueueDepth*3; i++ {
			h.Publish(interframeAU(), 10*time.Millisecond)
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Publish blocked on a subscriber that stopped reading")
	}

	assert.Positive(t, sub.Dropped(), "frames must be counted as dropped")
	assert.True(t, sub.NeedsResync(), "a drop must request resynchronisation")
	assert.LessOrEqual(t, sub.Queued(), QueueDepth,
		"the backlog must stay bounded by the queue depth")
}

func TestResyncSkipsUntilKeyframe(t *testing.T) {
	h := NewHub()
	sub := h.Subscribe()
	defer h.Unsubscribe(sub)

	// A fresh subscriber has no decoder state, so it starts needing a
	// keyframe just as if it had fallen behind.
	require.True(t, sub.NeedsResync())

	assert.False(t, sub.ShouldDeliver(Frame{Keyframe: false}),
		"a P frame cannot start a decoder")
	assert.True(t, sub.NeedsResync(), "still waiting for a keyframe")

	assert.True(t, sub.ShouldDeliver(Frame{Keyframe: true}),
		"a keyframe resumes delivery")
	assert.False(t, sub.NeedsResync(), "resync is satisfied by the keyframe")

	assert.True(t, sub.ShouldDeliver(Frame{Keyframe: false}),
		"once synchronised, P frames flow again")
}

// The whole point of the hub is that it is driven from the capture
// goroutine while consumers come and go.
func TestConcurrentPublishAndSubscribe(t *testing.T) {
	h := NewHub()

	stop := make(chan struct{})
	publisherDone := make(chan struct{})
	go func() {
		defer close(publisherDone)
		buf := keyframeAU()
		for {
			select {
			case <-stop:
				return
			default:
				h.Publish(buf, 10*time.Millisecond)
			}
		}
	}()

	for i := 0; i < 50; i++ {
		sub := h.Subscribe()
		select {
		case <-sub.Frames():
		default:
		}
		h.Unsubscribe(sub)
	}

	close(stop)
	select {
	case <-publisherDone:
	case <-time.After(5 * time.Second):
		t.Fatal("publisher did not stop; it is blocked")
	}
}
