package files

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestResolvePathTraversalGuard(t *testing.T) {
	dir := t.TempDir()
	// A real capture file inside the dir.
	good := filepath.Join(dir, "capture_20260101_000000.dv")
	if err := os.WriteFile(good, []byte("dv"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A secret file outside the capture dir (sibling).
	outside := filepath.Join(filepath.Dir(dir), "secret.txt")
	_ = os.WriteFile(outside, []byte("secret"), 0o644)
	defer os.Remove(outside)

	s := New(dir)

	tests := []struct {
		name       string
		input      string
		wantErr    bool
		errInvalid bool
	}{
		{"valid file", "capture_20260101_000000.dv", false, false},
		{"empty name", "", true, true},
		{"slash traversal", "../secret.txt", true, true},
		{"nested slash", "sub/file.dv", true, true},
		{"backslash", "..\\secret.txt", true, true},
		{"missing file", "nope.dv", true, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path, err := s.Resolve(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got path %q", tt.input, path)
				}
				if tt.errInvalid {
					if _, ok := err.(*ErrInvalidName); !ok {
						t.Errorf("expected ErrInvalidName for %q, got %T", tt.input, err)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tt.input, err)
			}
			if path != good {
				t.Errorf("got %q, want %q", path, good)
			}
		})
	}
}

func TestDeleteGuard(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	// Traversal delete must be rejected before touching the filesystem.
	if err := s.Delete("../etc-passwd", ""); err == nil {
		t.Fatal("expected traversal delete to be rejected")
	}

	f := filepath.Join(dir, "x.mp4")
	_ = os.WriteFile(f, []byte("x"), 0o644)
	if err := s.Delete("x.mp4", ""); err != nil {
		t.Fatalf("valid delete failed: %v", err)
	}
	if _, err := os.Stat(f); !os.IsNotExist(err) {
		t.Fatal("file was not deleted")
	}
}

// TestDeleteRefusesActiveRecording verifies T4.4's guard: the file matching
// the recorder's current active path (its ".part" name while recording)
// cannot be deleted out from under dvgrab/ffmpeg's own open handle.
func TestDeleteRefusesActiveRecording(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	active := filepath.Join(dir, "capture_20260101_000000.dv.part")
	_ = os.WriteFile(active, []byte("x"), 0o644)

	if err := s.Delete("capture_20260101_000000.dv.part", active); err == nil {
		t.Fatal("expected active recording delete to be rejected")
	} else if _, ok := err.(*ErrActiveRecording); !ok {
		t.Fatalf("expected ErrActiveRecording, got %T: %v", err, err)
	}
	if _, err := os.Stat(active); err != nil {
		t.Fatal("active file must not be removed")
	}

	// Once idle (activePath ""), the same file is deletable.
	if err := s.Delete("capture_20260101_000000.dv.part", ""); err != nil {
		t.Fatalf("delete after recording stopped should succeed: %v", err)
	}
}

// TestListFlagsOrphanedPartFile verifies T4.4's ".part" recovery signal:
// files.List only flags a ".dv.part" file as recovered_incomplete once it's
// stale (recoverWindow) AND isn't the recorder's current active file — a
// fresh or still-active ".part" must read as a normal in-progress entry.
func TestListFlagsOrphanedPartFile(t *testing.T) {
	dir := t.TempDir()
	s := New(dir)
	path := filepath.Join(dir, "capture_20260101_000000.dv.part")
	if err := os.WriteFile(path, []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Fresh: no active recording, but not old enough yet.
	videos := s.List(100, "")
	if len(videos) != 1 || videos[0].Status != "" {
		t.Fatalf("fresh .part file should not be flagged yet: %+v", videos)
	}

	// Backdate it past recoverWindow.
	old := time.Now().Add(-2 * time.Minute)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}

	// Still the active recording: must not be flagged.
	videos = s.List(100, path)
	if len(videos) != 1 || videos[0].Status != "" {
		t.Fatalf("active .part file should never be flagged: %+v", videos)
	}

	// Stale and orphaned (no active recording claims it): flagged.
	videos = s.List(100, "")
	if len(videos) != 1 || videos[0].Status != StatusRecoveredIncomplete {
		t.Fatalf("stale orphaned .part file should be flagged recovered_incomplete: %+v", videos)
	}
}
