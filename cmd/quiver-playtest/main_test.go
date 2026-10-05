package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultDataDirIsAbsoluteNextToBinary(t *testing.T) {
	d := defaultDataDir()
	if !filepath.IsAbs(d) || filepath.Base(d) != "data" {
		t.Fatalf("unexpected default data dir %q", d)
	}
}

func TestParseSize(t *testing.T) {
	for in, want := range map[string]uint64{"2GiB": 2 << 30, "512MiB": 512 << 20, "1048576": 1 << 20, "1GB": 1_000_000_000, " 2GiB ": 2 << 30, "0": 0, "1KiB": 1 << 10, "1MB": 1_000_000} {
		got, err := parseSize(in)
		if err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "x", "-1GiB", "1TiB2", "99999999999GiB", "18446744073709551615KB"} {
		if _, err := parseSize(bad); err == nil {
			t.Errorf("parseSize(%q) accepted", bad)
		}
	}
	if _, err := parseSize("99999999999GiB"); err == nil || !strings.Contains(err.Error(), "99999999999GiB") {
		t.Errorf("overflow error %v does not quote the original input", err)
	}
}
