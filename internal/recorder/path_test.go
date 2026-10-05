package recorder

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSlug(t *testing.T) {
	cases := map[string]string{
		"Mateo's Game #1":       "mateo-s-game-1",
		"../../etc/passwd":      "etc-passwd",
		"  ":                    "",
		"日本語":                   "",
		"a/b\\c\x00d":           "a-b-c-d",
		strings.Repeat("a", 90): strings.Repeat("a", 40),
	}
	for in, want := range cases {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLinkDirStaysUnderRoot(t *testing.T) {
	root := "/data/recordings"
	for _, label := range []string{"x", "../../etc", "/abs/path", "", ".."} {
		d := LinkDir(root, 7, label)
		if filepath.Dir(d) != root || !strings.HasPrefix(filepath.Base(d), "link-7") {
			t.Errorf("label %q gave %q", label, d)
		}
	}
	if got := LinkDir(root, 7, "Alpha Team"); got != "/data/recordings/link-7-alpha-team" {
		t.Fatalf("got %q", got)
	}
}

func TestFileName(t *testing.T) {
	at := time.Date(2026, 10, 4, 15, 30, 12, 0, time.UTC)
	if got := FileName(at, "ab12", 1); got != "20261004T153012Z_ab12.mp4" {
		t.Fatalf("got %q", got)
	}
	if got := FileName(at, "ab12", 3); got != "20261004T153012Z_ab12_part3.mp4" {
		t.Fatalf("got %q", got)
	}
	if got := FileName(at, "../x/y", 1); strings.ContainsAny(got, "/\\") {
		t.Fatalf("unsafe id leaked: %q", got)
	}
}
