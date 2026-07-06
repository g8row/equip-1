package provisioning

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeApplier records ApplyCredentials calls and returns errs[callIndex] (or
// the last entry once exhausted) so tests can script attempt-by-attempt
// outcomes.
type fakeApplier struct {
	calls []struct{ ssid, psk string }
	errs  []error
}

func (f *fakeApplier) ApplyCredentials(ctx context.Context, ssid, psk string) error {
	f.calls = append(f.calls, struct{ ssid, psk string }{ssid, psk})
	if len(f.errs) == 0 {
		return nil
	}
	i := len(f.calls) - 1
	if i >= len(f.errs) {
		i = len(f.errs) - 1
	}
	return f.errs[i]
}

func withFastRetry(t *testing.T) {
	t.Helper()
	orig := bootProvisionRetryDelay
	bootProvisionRetryDelay = time.Millisecond
	t.Cleanup(func() { bootProvisionRetryDelay = orig })
}

func TestApplyBootProvisionFile_MissingFileIsNoop(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "provision.json")
	applier := &fakeApplier{}

	if err := ApplyBootProvisionFile(context.Background(), applier, path); err != nil {
		t.Fatalf("expected nil error for a missing file, got %v", err)
	}
	if len(applier.calls) != 0 {
		t.Fatalf("expected ApplyCredentials not to be called, got %d calls", len(applier.calls))
	}
}

func TestApplyBootProvisionFile_Success(t *testing.T) {
	withFastRetry(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "provision.json")
	if err := os.WriteFile(path, []byte(`{"ssid":"MyNet","psk":"secret123"}`), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	applier := &fakeApplier{}

	err := ApplyBootProvisionFile(context.Background(), applier, path)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}

	if len(applier.calls) != 1 {
		t.Fatalf("expected exactly one ApplyCredentials call, got %d", len(applier.calls))
	}
	if applier.calls[0].ssid != "MyNet" || applier.calls[0].psk != "secret123" {
		t.Fatalf("unexpected credentials applied: %+v", applier.calls[0])
	}

	assertRenamedAndGone(t, path)
}

func TestApplyBootProvisionFile_ParseErrorStillRenames(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "provision.json")
	if err := os.WriteFile(path, []byte(`{not valid json`), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	applier := &fakeApplier{}

	err := ApplyBootProvisionFile(context.Background(), applier, path)
	if err == nil {
		t.Fatal("expected a parse error, got nil")
	}
	if len(applier.calls) != 0 {
		t.Fatalf("expected ApplyCredentials not to be called on parse failure, got %d calls", len(applier.calls))
	}

	assertRenamedAndGone(t, path)
}

func TestApplyBootProvisionFile_MissingSSIDStillRenames(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "provision.json")
	if err := os.WriteFile(path, []byte(`{"psk":"secret123"}`), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	applier := &fakeApplier{}

	err := ApplyBootProvisionFile(context.Background(), applier, path)
	if err == nil {
		t.Fatal("expected an error for a missing ssid field, got nil")
	}
	if !strings.Contains(err.Error(), "ssid") {
		t.Fatalf("expected error to mention the missing ssid field, got %v", err)
	}
	if len(applier.calls) != 0 {
		t.Fatalf("expected ApplyCredentials not to be called, got %d calls", len(applier.calls))
	}

	assertRenamedAndGone(t, path)
}

func TestApplyBootProvisionFile_ApplyFailsAllAttemptsThenRenames(t *testing.T) {
	withFastRetry(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "provision.json")
	if err := os.WriteFile(path, []byte(`{"ssid":"BadNet","psk":"wrongpass"}`), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	wantErr := errors.New("connect failed")
	applier := &fakeApplier{errs: []error{wantErr, wantErr, wantErr}}

	err := ApplyBootProvisionFile(context.Background(), applier, path)
	if err == nil {
		t.Fatal("expected an error after exhausting retries, got nil")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected the underlying apply error to surface, got %v", err)
	}
	if len(applier.calls) != bootProvisionMaxAttempts {
		t.Fatalf("expected exactly %d attempts, got %d", bootProvisionMaxAttempts, len(applier.calls))
	}

	assertRenamedAndGone(t, path)
}

func TestApplyBootProvisionFile_SucceedsOnLastAttempt(t *testing.T) {
	withFastRetry(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "provision.json")
	if err := os.WriteFile(path, []byte(`{"ssid":"FlakyNet","psk":"secret123"}`), 0o644); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	applier := &fakeApplier{errs: []error{errors.New("timeout"), errors.New("timeout"), nil}}

	err := ApplyBootProvisionFile(context.Background(), applier, path)
	if err != nil {
		t.Fatalf("expected eventual success, got %v", err)
	}
	if len(applier.calls) != bootProvisionMaxAttempts {
		t.Fatalf("expected all %d attempts to have been made, got %d", bootProvisionMaxAttempts, len(applier.calls))
	}

	assertRenamedAndGone(t, path)
}

func TestAppliedBootProvisionPath(t *testing.T) {
	cases := map[string]string{
		"/boot/provision.json": "/boot/provision.applied.json",
		"/boot/provision":      "/boot/provision.applied",
	}
	for in, want := range cases {
		if got := appliedBootProvisionPath(in); got != want {
			t.Errorf("appliedBootProvisionPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBootProvisionPath(t *testing.T) {
	t.Setenv(BootProvisionEnvVar, "")
	if got := BootProvisionPath(); got != DefaultBootProvisionPath {
		t.Fatalf("expected default path %q, got %q", DefaultBootProvisionPath, got)
	}

	t.Setenv(BootProvisionEnvVar, "/data/custom-provision.json")
	if got := BootProvisionPath(); got != "/data/custom-provision.json" {
		t.Fatalf("expected overridden path, got %q", got)
	}
}

func assertRenamedAndGone(t *testing.T, originalPath string) {
	t.Helper()
	if _, err := os.Stat(originalPath); !os.IsNotExist(err) {
		t.Fatalf("expected original file %s to be gone after rename, stat err=%v", originalPath, err)
	}
	applied := appliedBootProvisionPath(originalPath)
	if _, err := os.Stat(applied); err != nil {
		t.Fatalf("expected renamed file %s to exist: %v", applied, err)
	}
}
