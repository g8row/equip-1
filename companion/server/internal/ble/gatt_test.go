package ble

import (
	"bytes"
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

func newTestCharacteristic(value []byte) *characteristic {
	c := newCharacteristic("test-uuid", "/test/service0", "test_char", []string{"read", "notify"})
	c.setValue(value)
	return c
}

func TestReadValueOffsetPaging(t *testing.T) {
	full := []byte("0123456789ABCDEFGHIJ") // 20 bytes
	c := newTestCharacteristic(full)

	got, derr := c.ReadValue(map[string]dbus.Variant{})
	if derr != nil {
		t.Fatalf("offset 0 read: %v", derr)
	}
	if !bytes.Equal(got, full) {
		t.Fatalf("offset 0: got %q, want %q", got, full)
	}

	got, derr = c.ReadValue(map[string]dbus.Variant{"offset": dbus.MakeVariant(uint16(10))})
	if derr != nil {
		t.Fatalf("offset 10 read: %v", derr)
	}
	if !bytes.Equal(got, full[10:]) {
		t.Fatalf("offset 10: got %q, want %q", got, full[10:])
	}

	// offset == length is valid (end of value, empty result).
	got, derr = c.ReadValue(map[string]dbus.Variant{"offset": dbus.MakeVariant(uint16(len(full)))})
	if derr != nil {
		t.Fatalf("offset==len read: %v", derr)
	}
	if len(got) != 0 {
		t.Fatalf("offset==len: got %q, want empty", got)
	}

	// offset past the end is InvalidOffset.
	_, derr = c.ReadValue(map[string]dbus.Variant{"offset": dbus.MakeVariant(uint16(len(full) + 1))})
	if derr == nil {
		t.Fatal("offset past end: expected an error, got nil")
	}
	if derr.Name != "org.bluez.Error.InvalidOffset" {
		t.Fatalf("offset past end: got error name %q, want org.bluez.Error.InvalidOffset", derr.Name)
	}
}

// TestReadValueSnapshotConsistency verifies that a multi-part read sequence
// (offset 0, then offset>0) is served from the snapshot taken at offset 0,
// not a value that changed mid-sequence — otherwise a client paging through
// a value with a dynamic `read` callback (e.g. wifi_scan) could see a
// Frankenstein concatenation of two different snapshots.
func TestReadValueSnapshotConsistency(t *testing.T) {
	c := newCharacteristic("test-uuid", "/test/service0", "test_char", []string{"read"})
	calls := 0
	c.read = func(ctx context.Context) ([]byte, *dbus.Error) {
		calls++
		if calls == 1 {
			return []byte("first-snapshot-data-000"), nil
		}
		return []byte("SECOND-SNAPSHOT-DIFFERENT"), nil
	}

	first, derr := c.ReadValue(map[string]dbus.Variant{})
	if derr != nil {
		t.Fatalf("offset 0: %v", derr)
	}
	if calls != 1 {
		t.Fatalf("expected exactly one read callback at offset 0, got %d", calls)
	}

	rest, derr := c.ReadValue(map[string]dbus.Variant{"offset": dbus.MakeVariant(uint16(6))})
	if derr != nil {
		t.Fatalf("offset 6: %v", derr)
	}
	if calls != 1 {
		t.Fatalf("offset>0 must not re-invoke the read callback, got %d calls", calls)
	}
	if !bytes.Equal(rest, first[6:]) {
		t.Fatalf("offset 6 slice mismatch: got %q, want %q", rest, first[6:])
	}
}

func TestReadValueReturnsCopyNotBackingSlice(t *testing.T) {
	c := newTestCharacteristic([]byte("hello"))
	got, derr := c.ReadValue(map[string]dbus.Variant{})
	if derr != nil {
		t.Fatalf("read: %v", derr)
	}
	got[0] = 'X'
	if v := c.Value(); v[0] != 'h' {
		t.Fatalf("mutating the returned read slice affected stored value: %q", v)
	}
}

func TestWriteValueDefaultStoresCopy(t *testing.T) {
	c := newCharacteristic("test-uuid", "/test/service0", "test_char", []string{"write"})
	src := []byte("payload")
	if derr := c.WriteValue(src, nil); derr != nil {
		t.Fatalf("write: %v", derr)
	}
	if !bytes.Equal(c.Value(), src) {
		t.Fatalf("stored value mismatch: got %q, want %q", c.Value(), src)
	}
	src[0] = 'X' // mutate the caller's slice
	if v := c.Value(); v[0] == 'X' {
		t.Fatal("WriteValue aliased the caller's slice instead of copying it")
	}
}

func TestStartStopNotify(t *testing.T) {
	c := newCharacteristic("test-uuid", "/test/service0", "test_char", []string{"notify"})
	if c.IsNotifying() {
		t.Fatal("expected not notifying initially")
	}
	if derr := c.StartNotify(); derr != nil {
		t.Fatalf("StartNotify: %v", derr)
	}
	if !c.IsNotifying() {
		t.Fatal("expected notifying after StartNotify")
	}
	if derr := c.StopNotify(); derr != nil {
		t.Fatalf("StopNotify: %v", derr)
	}
	if c.IsNotifying() {
		t.Fatal("expected not notifying after StopNotify")
	}
}

func TestStartNotifyRejectsNonNotifiable(t *testing.T) {
	c := newCharacteristic("test-uuid", "/test/service0", "test_char", []string{"read"})
	if derr := c.StartNotify(); derr == nil {
		t.Fatal("expected an error starting notify on a non-notifiable characteristic")
	}
}

func TestTruncateForNotify(t *testing.T) {
	short := []byte("short")
	if got := truncateForNotify(short); !bytes.Equal(got, short) {
		t.Fatalf("short payload should pass through unchanged: got %q", got)
	}

	long := bytes.Repeat([]byte("x"), 200)
	got := truncateForNotify(long)
	if len(got) != notifyMaxBytes {
		t.Fatalf("long payload: got %d bytes, want %d", len(got), notifyMaxBytes)
	}
	if !bytes.Equal(got, long[:notifyMaxBytes]) {
		t.Fatal("truncated payload is not a prefix of the original")
	}
}

// TestEmitValueKeepsFullValueDespiteTruncatedNotify is the protocol contract
// this task establishes: a notify only signals "something changed" (and may
// be truncated to notifyMaxBytes for MTU safety), but the full value must
// always remain available via ReadValue's offset-paged multi-part read.
func TestEmitValueKeepsFullValueDespiteTruncatedNotify(t *testing.T) {
	c := newCharacteristic("test-uuid", "/test/service0", "test_char", []string{"read", "notify"})

	// A minimal, unauthenticated dbus.Conn wired to a net.Pipe so Emit has
	// somewhere to write; the other end is drained in the background since
	// net.Pipe is synchronous and Emit must not block the test.
	clientEnd, serverEnd := net.Pipe()
	t.Cleanup(func() { clientEnd.Close(); serverEnd.Close() })
	go io.Copy(io.Discard, serverEnd)

	conn, err := dbus.NewConn(clientEnd)
	if err != nil {
		t.Fatalf("dbus.NewConn: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	long := bytes.Repeat([]byte("y"), 200)
	c.emitValue(conn, long)

	if v := c.Value(); !bytes.Equal(v, long) {
		t.Fatalf("emitValue must store the full value: got %d bytes, want %d", len(v), len(long))
	}

	// The full value must still be readable via multi-part ReadValue.
	var reassembled []byte
	for {
		chunk, derr := c.ReadValue(map[string]dbus.Variant{"offset": dbus.MakeVariant(uint16(len(reassembled)))})
		if derr != nil {
			t.Fatalf("paged read at offset %d: %v", len(reassembled), derr)
		}
		if len(chunk) == 0 {
			break
		}
		reassembled = append(reassembled, chunk...)
	}
	if !bytes.Equal(reassembled, long) {
		t.Fatalf("reassembled paged read mismatch: got %d bytes, want %d", len(reassembled), len(long))
	}
}

// TestConcurrentReadWriteNotifyRace exercises ReadValue/WriteValue/emitValue/
// StartNotify/StopNotify concurrently. Run with -race: godbus dispatches
// D-Bus method calls concurrently, so this must not race on value/notifying.
func TestConcurrentReadWriteNotifyRace(t *testing.T) {
	c := newTestCharacteristic([]byte("initial-value"))

	clientEnd, serverEnd := net.Pipe()
	t.Cleanup(func() { clientEnd.Close(); serverEnd.Close() })
	go io.Copy(io.Discard, serverEnd)
	conn, err := dbus.NewConn(clientEnd)
	if err != nil {
		t.Fatalf("dbus.NewConn: %v", err)
	}
	t.Cleanup(func() { conn.Close() })

	const iterations = 200
	var wg sync.WaitGroup
	wg.Add(4)

	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			_, _ = c.ReadValue(map[string]dbus.Variant{})
			_, _ = c.ReadValue(map[string]dbus.Variant{"offset": dbus.MakeVariant(uint16(2))})
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			_ = c.WriteValue([]byte(strings.Repeat("w", i%10+1)), nil)
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			c.emitValue(conn, []byte(strings.Repeat("n", i%30+1)))
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			if i%2 == 0 {
				_ = c.StartNotify()
			} else {
				_ = c.StopNotify()
			}
			_ = c.IsNotifying()
		}
	}()

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("timed out waiting for concurrent operations")
	}
}
