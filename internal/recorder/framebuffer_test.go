package recorder

import (
	"bytes"
	"testing"
)

func px(b, g, r byte) []byte { return []byte{b, g, r, 0} }

func TestNewFramebufferLimits(t *testing.T) {
	for _, c := range [][2]int{{0, 10}, {10, 0}, {-1, 5}, {8193, 10}, {5000, 5000}} {
		if _, err := NewFramebuffer(c[0], c[1]); err == nil {
			t.Errorf("%v accepted", c)
		}
	}
	if _, err := NewFramebuffer(3840, 2160); err != nil {
		t.Fatal(err)
	}
}

func TestPutRawAndSnapshot(t *testing.T) {
	fb, _ := NewFramebuffer(4, 3)
	src := bytes.Repeat(px(1, 2, 3), 2*2)
	if err := fb.PutRaw(1, 1, 2, 2, src); err != nil {
		t.Fatal(err)
	}
	snap, w, h := fb.Snapshot(nil)
	if w != 4 || h != 3 || len(snap) != 4*3*4 {
		t.Fatalf("snapshot %d %d %d", w, h, len(snap))
	}
	if !bytes.Equal(snap[(1*4+1)*4:(1*4+1)*4+4], px(1, 2, 3)) || !bytes.Equal(snap[0:4], px(0, 0, 0)) {
		t.Fatal("pixels wrong")
	}
	snap[0] = 99
	if again, _, _ := fb.Snapshot(nil); again[0] != 0 {
		t.Fatal("snapshot aliases the framebuffer")
	}
}

func TestPutRawBounds(t *testing.T) {
	fb, _ := NewFramebuffer(4, 3)
	for _, c := range [][4]int{{3, 0, 2, 1}, {0, 2, 1, 2}, {-1, 0, 1, 1}, {0, 0, 5, 1}} {
		if err := fb.PutRaw(c[0], c[1], c[2], c[3], make([]byte, c[2]*c[3]*4)); err == nil {
			t.Errorf("%v accepted", c)
		}
	}
	if err := fb.PutRaw(0, 0, 2, 2, make([]byte, 3)); err == nil {
		t.Error("short data accepted")
	}
}

func TestCopyOverlap(t *testing.T) {
	fb, _ := NewFramebuffer(4, 1)
	fb.PutRaw(0, 0, 4, 1, append(append(append(px(1, 0, 0), px(2, 0, 0)...), px(3, 0, 0)...), px(4, 0, 0)...))
	if err := fb.Copy(0, 0, 3, 1, 1, 0); err != nil {
		t.Fatal(err)
	}
	snap, _, _ := fb.Snapshot(nil)
	got := []byte{snap[0], snap[4], snap[8], snap[12]}
	if !bytes.Equal(got, []byte{1, 1, 2, 3}) {
		t.Fatalf("overlapping copy gave %v", got)
	}
	if err := fb.Copy(2, 0, 3, 1, 0, 0); err == nil {
		t.Fatal("source out of bounds accepted")
	}
}

func TestResizeClears(t *testing.T) {
	fb, _ := NewFramebuffer(2, 2)
	fb.PutRaw(0, 0, 1, 1, px(9, 9, 9))
	if err := fb.Resize(3, 1); err != nil {
		t.Fatal(err)
	}
	snap, w, h := fb.Snapshot(nil)
	if w != 3 || h != 1 || len(snap) != 12 || snap[0] != 0 {
		t.Fatalf("%d %d %v", w, h, snap)
	}
	if err := fb.Resize(0, 0); err == nil {
		t.Fatal("zero size accepted")
	}
	if w, h := fb.Size(); w != 3 || h != 1 {
		t.Fatal("failed resize changed the size")
	}
}

func FuzzFramebufferOps(f *testing.F) {
	f.Add(1, 1, 2, 2, 0, 0)
	f.Fuzz(func(t *testing.T, x, y, w, h, dx, dy int) {
		fb, _ := NewFramebuffer(16, 16)
		if w >= 0 && h >= 0 && w <= 64 && h <= 64 {
			fb.PutRaw(x, y, w, h, make([]byte, w*h*4))
		}
		fb.Copy(x, y, w, h, dx, dy)
	})
}
