package stream

import "testing"

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
