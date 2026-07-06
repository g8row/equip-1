package recorder

import (
	"os"
	"path/filepath"
	"testing"

	"equip1/companion/server/internal/config"
	"equip1/companion/server/internal/stream"
)

// newTestRecorder builds a RecorderState wired with real-but-inert
// dependencies (no ffmpeg/dvgrab/mediamtx process is ever spawned by the
// paths these tests exercise) so the state machine can be tested without
// hardware or external binaries.
func newTestRecorder(t *testing.T, dir, mode string) *RecorderState {
	t.Helper()
	cm := config.NewCaptureMode(mode)
	mediamtx := stream.NewMediamtxManager("equip1-test-nonexistent-mediamtx", "")
	return New(Deps{
		CaptureDir:  dir,
		CaptureMode: cm,
		Mediamtx:    mediamtx,
		Seamless:    stream.NewSeamlessDvHub(mediamtx, "rtsp://127.0.0.1:8554/test"),
		Preview:     stream.NewPreviewPush(cm, mediamtx, "rtsp://127.0.0.1:8554/test"),
		RTSPURL:     "rtsp://127.0.0.1:8554/test",
	})
}

// TestStartRejectsConcurrentStart verifies the T4.4 double-start fix: a
// Start() call arriving while another is still spawning (mode == "starting")
// is rejected with ErrAlreadyStarting rather than racing it into a second
// dvgrab/ffmpeg pair.
func TestStartRejectsConcurrentStart(t *testing.T) {
	r := newTestRecorder(t, t.TempDir(), "dvgrab")

	r.mu.Lock()
	r.mode = "starting"
	r.mu.Unlock()

	if err := r.Start(); err != ErrAlreadyStarting {
		t.Fatalf("expected ErrAlreadyStarting, got %v", err)
	}

	// The rejected call must not have disturbed the in-flight start's state.
	if got := r.Mode(); got != "starting" {
		t.Fatalf("expected mode to remain %q, got %q", "starting", got)
	}
}

// TestStartResetsModeOnFailure verifies a failed Start() (requirements not
// met) doesn't strand the recorder in "starting" forever — the very failure
// this test exercises (no ffmpeg in PATH) is also what the sandboxed test
// environment guarantees, so it doubles as a real negative-path test.
func TestStartResetsModeOnFailure(t *testing.T) {
	r := newTestRecorder(t, t.TempDir(), "ffmpeg-only")

	// This environment has no ffmpeg installed; Start must fail the
	// requirements check and fall back to idle, not get stuck in "starting".
	if _, err := os.Stat("/usr/bin/ffmpeg"); err == nil {
		t.Skip("ffmpeg is installed in this environment; test assumes it is not")
	}

	if err := r.Start(); err == nil {
		t.Fatal("expected Start to fail without ffmpeg installed")
	}
	if got := r.Mode(); got != "idle" {
		t.Fatalf("expected mode reset to idle after failed start, got %q", got)
	}
}

// TestStopWithReasonFinalizesOnlyOnCleanStop verifies T4.4's ".part" ->
// ".dv" rename happens for a user-initiated stop and only then; every other
// reason must leave the ".part" name in place (so files.List's recovered/
// incomplete check can flag it) and record LastStopReason/LastStopAt.
func TestStopWithReasonFinalizesOnlyOnCleanStop(t *testing.T) {
	tests := []struct {
		reason       string
		wantFinalize bool
	}{
		{ReasonUser, true},
		{ReasonDiskFull, false},
		{ReasonProcessDied, false},
		{ReasonWriteError, false},
		{ReasonShutdown, false},
	}

	for _, tt := range tests {
		t.Run(tt.reason, func(t *testing.T) {
			dir := t.TempDir()
			r := newTestRecorder(t, dir, "ffmpeg-only")

			partPath := filepath.Join(dir, "capture_20260101_000000.dv.part")
			finalPath := filepath.Join(dir, "capture_20260101_000000.dv")
			if err := os.WriteFile(partPath, []byte("dv-bytes"), 0o644); err != nil {
				t.Fatal(err)
			}

			r.mu.Lock()
			r.mode = "recording"
			r.currentFile = partPath
			r.finalFile = finalPath
			r.mu.Unlock()

			r.stopWithReason(tt.reason)

			if got := r.Mode(); got != "idle" {
				t.Errorf("mode = %q, want idle", got)
			}
			if got := r.CurrentFile(); got != "" {
				t.Errorf("CurrentFile() = %q, want cleared", got)
			}
			if got := r.LastStopReason(); got != tt.reason {
				t.Errorf("LastStopReason() = %q, want %q", got, tt.reason)
			}
			if r.LastStopAt().IsZero() {
				t.Error("LastStopAt() should be set")
			}

			_, partErr := os.Stat(partPath)
			_, finalErr := os.Stat(finalPath)
			if tt.wantFinalize {
				if partErr == nil {
					t.Error(".part file should have been renamed away")
				}
				if finalErr != nil {
					t.Errorf("finalized file should exist: %v", finalErr)
				}
			} else {
				if partErr != nil {
					t.Errorf(".part file should still exist for recovery: %v", partErr)
				}
				if finalErr == nil {
					t.Error("finalized file should not exist for a non-clean stop")
				}
			}
		})
	}
}

// TestRecordFailedTransitionsToIdleWithReason verifies the seamless hub's
// write-error callback (RecordFailed) — wired in cmd/companion-api/main.go —
// correctly drives the recorder's own state machine to idle with
// ReasonWriteError, mirroring what stopWithReason(ReasonWriteError) does
// directly.
func TestRecordFailedTransitionsToIdleWithReason(t *testing.T) {
	dir := t.TempDir()
	r := newTestRecorder(t, dir, "dvgrab")

	partPath := filepath.Join(dir, "capture_20260101_000000.dv.part")
	finalPath := filepath.Join(dir, "capture_20260101_000000.dv")
	if err := os.WriteFile(partPath, []byte("dv-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}

	r.mu.Lock()
	r.mode = "recording"
	r.currentFile = partPath
	r.finalFile = finalPath
	r.mu.Unlock()

	r.RecordFailed(ReasonWriteError)

	if got := r.Mode(); got != "idle" {
		t.Errorf("mode = %q, want idle", got)
	}
	if got := r.LastStopReason(); got != ReasonWriteError {
		t.Errorf("LastStopReason() = %q, want %q", got, ReasonWriteError)
	}
	if _, err := os.Stat(partPath); err != nil {
		t.Errorf(".part file should survive a write-error stop for recovery: %v", err)
	}
}
