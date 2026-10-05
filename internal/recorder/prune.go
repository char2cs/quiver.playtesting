package recorder

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// Prune deletes recordings older than keep and folders left empty. It only looks inside link-* folders
// and only removes regular .mp4 files, so a symlink or a stray file can never lead it elsewhere.
func Prune(dir string, keep time.Duration, now time.Time) (int, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	cutoff := now.Add(-keep)
	removed := 0
	for _, d := range entries {
		if !d.IsDir() || !strings.HasPrefix(d.Name(), "link-") {
			continue
		}
		sub := filepath.Join(dir, d.Name())
		di, err := d.Info()
		if err != nil {
			continue
		}
		files, err := os.ReadDir(sub)
		if err != nil {
			continue
		}
		for _, f := range files {
			if !f.Type().IsRegular() || !strings.HasSuffix(f.Name(), ".mp4") {
				continue
			}
			info, err := f.Info()
			if err != nil || !info.ModTime().Before(cutoff) {
				continue
			}
			if os.Remove(filepath.Join(sub, f.Name())) == nil {
				removed++
			}
		}
		if di.ModTime().Before(cutoff) {
			_ = os.Remove(sub)
		}
	}
	return removed, nil
}
