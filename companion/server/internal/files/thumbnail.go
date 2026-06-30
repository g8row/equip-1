package files

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

const thumbnailGenerateTimeout = 8 * time.Second

// Thumbnail returns the path to a cached JPEG thumbnail for the named capture
// file, generating one with ffmpeg on first request. Subsequent calls reuse
// the cached file as long as it's newer than the source (so re-recording over
// a deleted-and-recreated name regenerates it).
func (s *Store) Thumbnail(name string) (string, error) {
	srcPath, err := s.Resolve(name)
	if err != nil {
		return "", err
	}
	srcInfo, err := os.Stat(srcPath)
	if err != nil {
		return "", &ErrNotFound{}
	}

	thumbDir := filepath.Join(s.dir, ".thumbnails")
	if err := os.MkdirAll(thumbDir, 0o755); err != nil {
		return "", err
	}
	thumbPath := filepath.Join(thumbDir, name+".jpg")

	if thumbInfo, err := os.Stat(thumbPath); err == nil && thumbInfo.ModTime().After(srcInfo.ModTime()) {
		return thumbPath, nil
	}

	if err := generateThumbnail(srcPath, thumbPath); err != nil {
		return "", err
	}
	return thumbPath, nil
}

// generateThumbnail grabs a single frame via ffmpeg, writing to a temp file
// first and renaming into place so concurrent readers never see a partial
// JPEG. Falls back to the very first frame if seeking 1s in fails (very
// short clips).
func generateThumbnail(srcPath, destPath string) error {
	tmp := destPath + ".tmp"
	defer os.Remove(tmp)

	args := [][]string{
		{"-y", "-ss", "1", "-i", srcPath, "-frames:v", "1", "-vf", "scale=320:-1", "-q:v", "5", tmp},
		{"-y", "-i", srcPath, "-frames:v", "1", "-vf", "scale=320:-1", "-q:v", "5", tmp},
	}

	var lastErr error
	for _, a := range args {
		ctx, cancel := context.WithTimeout(context.Background(), thumbnailGenerateTimeout)
		cmd := exec.CommandContext(ctx, "ffmpeg", append([]string{"-hide_banner", "-loglevel", "error"}, a...)...)
		_, err := cmd.CombinedOutput()
		cancel()
		if err == nil {
			if info, statErr := os.Stat(tmp); statErr == nil && info.Size() > 0 {
				return os.Rename(tmp, destPath)
			}
		}
		lastErr = err
	}
	return lastErr
}
