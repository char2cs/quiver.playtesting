package main

import (
	"path/filepath"
	"testing"
)

func TestDefaultDataDirIsAbsoluteNextToBinary(t *testing.T) {
	d := defaultDataDir()
	if !filepath.IsAbs(d) || filepath.Base(d) != "data" {
		t.Fatalf("unexpected default data dir %q", d)
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]uint64{"2GiB": 2 << 30, "512MiB": 512 << 20, "1048576": 1 << 20, "1GB": 1_000_000_000} {
		got, err := parseSize(in)
		if err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "x", "-1GiB", "1TiB2"} {
		if _, err := parseSize(bad); err == nil {
			t.Errorf("parseSize(%q) accepted", bad)
		}
	}
}
