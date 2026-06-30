package files

import (
	"os"
	"path/filepath"
	"testing"
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
	if err := s.Delete("../etc-passwd"); err == nil {
		t.Fatal("expected traversal delete to be rejected")
	}

	f := filepath.Join(dir, "x.mp4")
	_ = os.WriteFile(f, []byte("x"), 0o644)
	if err := s.Delete("x.mp4"); err != nil {
		t.Fatalf("valid delete failed: %v", err)
	}
	if _, err := os.Stat(f); !os.IsNotExist(err) {
		t.Fatal("file was not deleted")
	}
}
