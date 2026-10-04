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
