package recorder

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func touch(t *testing.T, path string, age time.Duration) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	mt := time.Now().Add(-age)
	os.Chtimes(path, mt, mt)
}

func TestPrune(t *testing.T) {
	root := t.TempDir()
	touch(t, filepath.Join(root, "link-1-a", "old.mp4"), 40*24*time.Hour)
	touch(t, filepath.Join(root, "link-1-a", "new.mp4"), time.Hour)
	touch(t, filepath.Join(root, "link-2-b", "old.mp4"), 40*24*time.Hour)
	touch(t, filepath.Join(root, "link-2-b", "notes.txt"), 40*24*time.Hour)
	touch(t, filepath.Join(root, "other", "old.mp4"), 40*24*time.Hour)
	outside := filepath.Join(t.TempDir(), "secret.mp4")
	touch(t, outside, 90*24*time.Hour)
	os.Symlink(outside, filepath.Join(root, "link-1-a", "evil.mp4"))

	n, err := Prune(root, 30*24*time.Hour, time.Now())
	if err != nil || n != 2 {
		t.Fatalf("removed %d err %v", n, err)
	}
	for path, want := range map[string]bool{
		filepath.Join(root, "link-1-a", "old.mp4"):   false,
		filepath.Join(root, "link-1-a", "new.mp4"):   true,
		filepath.Join(root, "link-2-b", "old.mp4"):   false,
		filepath.Join(root, "link-2-b", "notes.txt"): true,
		filepath.Join(root, "other", "old.mp4"):      true,
		filepath.Join(root, "link-1-a", "evil.mp4"):  true,
		outside: true,
	} {
		_, err := os.Lstat(path)
		if (err == nil) != want {
			t.Errorf("%s exists=%v want %v", path, err == nil, want)
		}
	}
}

func TestPruneEmptiesFolders(t *testing.T) {
	root := t.TempDir()
	touch(t, filepath.Join(root, "link-9-z", "old.mp4"), 40*24*time.Hour)
	if _, err := Prune(root, 24*time.Hour, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "link-9-z")); !os.IsNotExist(err) {
		t.Fatal("empty folder kept")
	}
}

func TestPruneMissingDir(t *testing.T) {
	if n, err := Prune(filepath.Join(t.TempDir(), "nope"), time.Hour, time.Now()); err != nil || n != 0 {
		t.Fatalf("%d %v", n, err)
	}
}
