package stream

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestSeamlessDvHubEnsureRunningConcurrentSingleSpawn verifies the T4.1
// generation-guarded lifecycle: 10 concurrent EnsureRunning callers must
// converge on exactly one dvgrab+ffmpeg spawn. Before the fix, EnsureRunning
// released its lock between the liveness check and the stop+spawn, leaving a
// window where concurrent callers could each decide the pipeline wasn't
// running and spawn a competing copy (the "stream flap / double capture" bug
// this task fixes).
//
// dvgrab/ffmpeg are stubbed via a PATH override so the test doesn't need real
// capture hardware or ffmpeg encoders: the stub ffmpeg answers the encoder
// probe ("-encoders" / "testsrc") the way a build with only mjpeg available
// would, then idles like the real long-running pipeline process would.
func TestSeamlessDvHubEnsureRunningConcurrentSingleSpawn(t *testing.T) {
	dir := t.TempDir()
	spawnLog := filepath.Join(dir, "dvgrab-spawns.log")

	writeStub(t, dir, "dvgrab", "#!/bin/sh\n"+
		`echo spawn >> "$SPAWN_LOG"`+"\n"+
		"exec sleep 3600\n")
	writeStub(t, dir, "ffmpeg", "#!/bin/sh\n"+
		`case "$*" in`+"\n"+
		`  *-encoders*) printf ' mjpeg\n'; exit 0 ;;`+"\n"+
		`  *testsrc*) exit 0 ;;`+"\n"+
		`esac`+"\n"+
		"exec sleep 3600\n")

	// Put the stub dir first so it shadows any real dvgrab/ffmpeg on PATH,
	// but keep the rest of PATH so the stub scripts can still exec `sleep`.
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("SPAWN_LOG", spawnLog)

	mediamtx := NewMediamtxManager("equip1-test-nonexistent-mediamtx", "")
	hub := NewSeamlessDvHub(mediamtx, "rtsp://127.0.0.1:0/seamless")
	t.Cleanup(hub.Stop)

	const callers = 10
	var wg sync.WaitGroup
	errs := make([]error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = hub.EnsureRunning()
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("EnsureRunning goroutine %d: %v", i, err)
		}
	}

	if !hub.IsRunning() {
		t.Fatal("expected hub to be running after EnsureRunning")
	}

	data, err := os.ReadFile(spawnLog)
	if err != nil {
		t.Fatalf("reading spawn log: %v", err)
	}
	spawns := strings.Count(string(data), "spawn")
	if spawns != 1 {
		t.Fatalf("expected exactly 1 dvgrab spawn from %d concurrent EnsureRunning calls, got %d", callers, spawns)
	}
}

func writeStub(t *testing.T, dir, name, script string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("writing stub %s: %v", name, err)
	}
}
