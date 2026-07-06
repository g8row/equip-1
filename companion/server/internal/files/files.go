// Package files lists, serves and deletes capture videos, and reports storage
// statistics. It ports the file/storage helpers from main.py with the same
// path-traversal guard.
package files

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// videoGlobs are the capture file patterns, matching _VIDEO_GLOBS. "*.dv.part"
// is an in-progress or interrupted capture (see recorder.Start's ".part"
// convention) — included here so an orphaned one is visible/recoverable
// rather than invisible until manually renamed.
var videoGlobs = []string{"*.dv", "*.mkv", "*.mp4", "*.ts", "*.dv.part"}

// recoverWindow is how long a ".part" file must sit untouched, with no
// recording currently claiming it, before List flags it as an orphaned,
// incomplete capture (StatusRecoveredIncomplete) rather than a normal file
// still actively being written.
const recoverWindow = time.Minute

// StatusRecoveredIncomplete marks a Video whose ".part" file is stale (see
// recoverWindow) and not the recorder's current active file — almost always
// the survivor of a crash, power loss, or disk-full auto-stop.
const StatusRecoveredIncomplete = "recovered_incomplete"

// Store provides file operations rooted at a capture directory.
type Store struct {
	dir string
}

// New returns a Store rooted at captureDir.
func New(captureDir string) *Store {
	return &Store{dir: captureDir}
}

// Dir returns the capture directory path.
func (s *Store) Dir() string { return s.dir }

// Video describes a single capture file.
type Video struct {
	Name         string `json:"name"`
	SizeBytes    int64  `json:"size_bytes"`
	ModifiedUnix int64  `json:"modified_unix"`
	DownloadPath string `json:"download_path"`
	// Status is StatusRecoveredIncomplete for an orphaned ".part" file, ""
	// otherwise (omitted from JSON).
	Status string `json:"status,omitempty"`
}

// List returns up to limit videos sorted by modification time descending.
// activePath is the recorder's current active file (RecorderState.CurrentFile()),
// or "" when idle — it keeps the in-progress capture's own ".part" file from
// being flagged as recovered/incomplete before it's even finished.
func (s *Store) List(limit int, activePath string) []Video {
	type entry struct {
		path string
		info os.FileInfo
	}
	seen := make(map[string]entry)
	for _, pattern := range videoGlobs {
		matches, _ := filepath.Glob(filepath.Join(s.dir, pattern))
		for _, m := range matches {
			resolved, err := filepath.EvalSymlinks(m)
			if err != nil {
				resolved = m
			}
			if _, ok := seen[resolved]; ok {
				continue
			}
			info, err := os.Stat(m)
			if err != nil || info.IsDir() {
				continue
			}
			seen[resolved] = entry{path: m, info: info}
		}
	}

	entries := make([]entry, 0, len(seen))
	for _, e := range seen {
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].info.ModTime().After(entries[j].info.ModTime())
	})

	videoActiveName := ""
	if activePath != "" {
		videoActiveName = filepath.Base(activePath)
	}

	videos := make([]Video, 0, len(entries))
	for _, e := range entries {
		name := filepath.Base(e.path)
		v := Video{
			Name:         name,
			SizeBytes:    e.info.Size(),
			ModifiedUnix: e.info.ModTime().Unix(),
			DownloadPath: fmt.Sprintf("/api/files/download/%s", name),
		}
		if strings.HasSuffix(name, ".part") && name != videoActiveName &&
			time.Since(e.info.ModTime()) > recoverWindow {
			v.Status = StatusRecoveredIncomplete
		}
		videos = append(videos, v)
		if len(videos) >= limit {
			break
		}
	}
	return videos
}

// Storage describes filesystem usage for the capture directory.
type Storage struct {
	TotalBytes  uint64  `json:"total_bytes"`
	UsedBytes   uint64  `json:"used_bytes"`
	FreeBytes   uint64  `json:"free_bytes"`
	UsedPercent float64 `json:"used_percent"`
}

// Stats reports storage usage for the capture directory via statfs.
func (s *Store) Stats() (Storage, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(s.dir, &st); err != nil {
		return Storage{}, err
	}
	bsize := uint64(st.Bsize)
	total := st.Blocks * bsize
	free := st.Bavail * bsize
	used := total - free
	var pct float64
	if total > 0 {
		pct = round2(float64(used) / float64(total) * 100)
	}
	return Storage{
		TotalBytes:  total,
		UsedBytes:   used,
		FreeBytes:   free,
		UsedPercent: pct,
	}, nil
}

func round2(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

// ErrInvalidName indicates a rejected (unsafe) file name.
type ErrInvalidName struct{ Detail string }

func (e *ErrInvalidName) Error() string { return e.Detail }

// ErrNotFound indicates the resolved file does not exist.
type ErrNotFound struct{}

func (e *ErrNotFound) Error() string { return "File not found" }

// ErrActiveRecording indicates name is the recorder's current in-progress
// capture: deleting (or unlinking mid-write) it would race dvgrab/ffmpeg's
// own file handle.
type ErrActiveRecording struct{}

func (e *ErrActiveRecording) Error() string { return "Cannot delete the active recording" }

// Resolve validates name and returns its absolute path inside the capture dir,
// guarding against path traversal. Mirrors _resolve_capture_file.
func (s *Store) Resolve(name string) (string, error) {
	if name == "" || strings.Contains(name, "/") || strings.Contains(name, "\\") {
		return "", &ErrInvalidName{Detail: "Invalid file name"}
	}

	base, err := filepath.Abs(s.dir)
	if err != nil {
		return "", &ErrInvalidName{Detail: "Invalid file path"}
	}
	candidate := filepath.Join(base, name)
	// Cleaned candidate must remain directly within base.
	if filepath.Dir(candidate) != base {
		return "", &ErrInvalidName{Detail: "Invalid file path"}
	}

	info, err := os.Stat(candidate)
	if err != nil || info.IsDir() {
		return "", &ErrNotFound{}
	}
	return candidate, nil
}

// Delete removes a capture file (guarded by the same path-traversal check)
// and any cached thumbnail for it. activePath is the recorder's current
// active file (RecorderState.CurrentFile()), or "" when idle; deleting that
// file is refused rather than risking a mid-write unlink race.
func (s *Store) Delete(name, activePath string) error {
	path, err := s.Resolve(name)
	if err != nil {
		return err
	}
	if activePath != "" && name == filepath.Base(activePath) {
		return &ErrActiveRecording{}
	}
	_ = os.Remove(filepath.Join(s.dir, ".thumbnails", name+".jpg"))
	return os.Remove(path)
}
