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
)

// videoGlobs are the capture file patterns, matching _VIDEO_GLOBS.
var videoGlobs = []string{"*.dv", "*.mkv", "*.mp4", "*.ts"}

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
}

// List returns up to limit videos sorted by modification time descending.
func (s *Store) List(limit int) []Video {
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

	videos := make([]Video, 0, len(entries))
	for _, e := range entries {
		name := filepath.Base(e.path)
		videos = append(videos, Video{
			Name:         name,
			SizeBytes:    e.info.Size(),
			ModifiedUnix: e.info.ModTime().Unix(),
			DownloadPath: fmt.Sprintf("/api/files/download/%s", name),
		})
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
// and any cached thumbnail for it.
func (s *Store) Delete(name string) error {
	path, err := s.Resolve(name)
	if err != nil {
		return err
	}
	_ = os.Remove(filepath.Join(s.dir, ".thumbnails", name+".jpg"))
	return os.Remove(path)
}
