package stream

import (
	"bytes"
	"fmt"
	"testing"
)

// buildMjpegFrame returns one ffmpeg "-f mpjpeg" multipart unit, framing
// payload with an in-body Content-length header the way ffmpeg actually
// emits it (verified against the same framing web/src/lib/mjpeg.js parses).
func buildMjpegFrame(boundary string, payload []byte) []byte {
	return []byte(fmt.Sprintf("--%s\r\nContent-type: image/jpeg\r\nContent-length: %d\r\n\r\n%s\r\n",
		boundary, len(payload), payload))
}

// TestHubDropOnFull verifies that a full subscriber channel drops frames rather
// than blocking the broadcaster.
func TestHubDropOnFull(t *testing.T) {
	h := NewHub("test")
	_, ch := h.Subscribe()

	if h.SubscriberCount() != 1 {
		t.Fatalf("expected 1 subscriber, got %d", h.SubscriberCount())
	}

	// Broadcast more frames than the channel buffer (clientQueueDepth). This
	// must NOT block — excess frames are dropped.
	total := clientQueueDepth + 25
	done := make(chan struct{})
	go func() {
		for i := 0; i < total; i++ {
			h.Broadcast([]byte{byte(i)})
		}
		close(done)
	}()
	<-done // would deadlock if Broadcast blocked on a full channel

	if got := len(ch); got != clientQueueDepth {
		t.Fatalf("expected channel filled to %d, got %d", clientQueueDepth, got)
	}
}

// TestHubCloseAllSentinel verifies CloseAll sends the nil sentinel and clears
// subscribers.
func TestHubCloseAllSentinel(t *testing.T) {
	h := NewHub("test")
	id, ch := h.Subscribe()

	h.CloseAll()

	select {
	case v := <-ch:
		if v != nil {
			t.Fatalf("expected nil sentinel, got %v", v)
		}
	default:
		t.Fatal("expected a sentinel value on the channel")
	}

	if h.SubscriberCount() != 0 {
		t.Fatalf("expected 0 subscribers after CloseAll, got %d", h.SubscriberCount())
	}
	// Unsubscribe of a now-unknown id must be safe.
	h.Unsubscribe(id)
}

// TestHubUnsubscribe verifies subscriber removal.
func TestHubUnsubscribe(t *testing.T) {
	h := NewHub("test")
	id1, _ := h.Subscribe()
	id2, _ := h.Subscribe()
	if h.SubscriberCount() != 2 {
		t.Fatalf("expected 2 subscribers, got %d", h.SubscriberCount())
	}
	h.Unsubscribe(id1)
	if h.SubscriberCount() != 1 {
		t.Fatalf("expected 1 subscriber after unsubscribe, got %d", h.SubscriberCount())
	}
	h.Unsubscribe(id2)
	if h.SubscriberCount() != 0 {
		t.Fatalf("expected 0 subscribers, got %d", h.SubscriberCount())
	}
}

// TestFrameSplitterSplitsOnFrameBoundaries verifies frameSplitter (T4.2)
// reassembles complete mpjpeg units from a byte stream delivered in
// arbitrarily small chunks (as a pipe/socket read would), and that the
// extracted units are lossless: concatenated back together they reproduce
// the original stream exactly, and each unit ends with its own untorn JPEG
// payload.
func TestFrameSplitterSplitsOnFrameBoundaries(t *testing.T) {
	payloads := [][]byte{
		bytes.Repeat([]byte("A"), 5),
		bytes.Repeat([]byte("B"), 130), // larger than a single tiny chunk
		bytes.Repeat([]byte("C"), 1),
		bytes.Repeat([]byte("D"), 42),
	}
	var full []byte
	for _, p := range payloads {
		full = append(full, buildMjpegFrame("ffmpeg", p)...)
	}

	var fp frameSplitter
	var units [][]byte
	// Feed the stream 3 bytes at a time to force frames (and even headers)
	// to split across multiple push() calls.
	for i := 0; i < len(full); i += 3 {
		end := i + 3
		if end > len(full) {
			end = len(full)
		}
		units = append(units, fp.push(full[i:end])...)
	}

	if len(units) != len(payloads) {
		t.Fatalf("expected %d units, got %d", len(payloads), len(units))
	}
	for i, u := range units {
		if !bytes.HasSuffix(u, payloads[i]) {
			t.Fatalf("unit %d does not end with its payload (torn frame): %q", i, u)
		}
	}

	// Lossless: every byte of the original stream reappears, in order,
	// across the extracted units plus whatever's left buffered. The
	// trailing "\r\n" that separates a frame from the *next* boundary is
	// attached to that next unit's leading bytes (see frameSplitter's doc
	// comment), so after the last frame there is no following unit to claim
	// it — it simply sits unconsumed in fp.buf until more data (or another
	// frame) arrives. Include it here so the check still proves no byte is
	// ever dropped or reordered, only deferred.
	if got := bytes.Join(append(append([][]byte{}, units...), fp.buf), nil); !bytes.Equal(got, full) {
		t.Fatalf("reassembled units do not match the original stream\nwant: %q\ngot:  %q", full, got)
	}
}

// TestHubReadLoopStalledSubscriberGetsCleanFrames verifies that when a
// subscriber's queue is full, ReadLoop drops whole frames rather than
// arbitrary chunks: the frames that DO make it through are always complete
// and untorn, so a stalled client resumes on a clean frame boundary instead
// of a desynced parser (T4.2).
func TestHubReadLoopStalledSubscriberGetsCleanFrames(t *testing.T) {
	var payloads [][]byte
	for i := 0; i < 6; i++ {
		payloads = append(payloads, bytes.Repeat([]byte{byte('0' + i)}, 20+i))
	}
	var full []byte
	for _, p := range payloads {
		full = append(full, buildMjpegFrame("ffmpeg", p)...)
	}

	h := NewHub("test")
	_, ch := h.Subscribe() // never drained during ReadLoop — simulates a stalled client

	// Run synchronously: bytes.Reader hands the whole stream back in one
	// Read, so all 6 frames are parsed and broadcast before this returns.
	h.ReadLoop(bytes.NewReader(full))

	if got := len(ch); got != clientQueueDepth {
		t.Fatalf("expected %d buffered frames (queue depth), got %d", clientQueueDepth, got)
	}

	// The frames that survived must be the first clientQueueDepth ones,
	// each intact — not a torn merge of neighboring frames.
	for i := 0; i < clientQueueDepth; i++ {
		frame := <-ch
		if frame == nil {
			t.Fatalf("frame %d: unexpected nil sentinel", i)
		}
		if !bytes.HasSuffix(frame, payloads[i]) {
			t.Fatalf("frame %d does not end with its payload (torn frame): %q", i, frame)
		}
	}
}
