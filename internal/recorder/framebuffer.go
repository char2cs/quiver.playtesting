package recorder

import (
	"errors"
	"sync"
)

const (
	maxDim    = 8192
	maxPixels = 16_000_000
)

var (
	errBounds = errors.New("recorder: rectangle outside the screen")
	errSize   = errors.New("recorder: unsupported screen size")
)

// Framebuffer is the VM screen as BGRX pixels (4 bytes each), written by the capture
// goroutine and read by the encoder.
type Framebuffer struct {
	mu   sync.Mutex
	w, h int
	pix  []byte
}

func checkSize(w, h int) error {
	if w < 1 || h < 1 || w > maxDim || h > maxDim || w*h > maxPixels {
		return errSize
	}
	return nil
}

func NewFramebuffer(w, h int) (*Framebuffer, error) {
	if err := checkSize(w, h); err != nil {
		return nil, err
	}
	return &Framebuffer{w: w, h: h, pix: make([]byte, w*h*4)}, nil
}

// Resize replaces the screen with a black one of the new size.
func (f *Framebuffer) Resize(w, h int) error {
	if err := checkSize(w, h); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.w, f.h, f.pix = w, h, make([]byte, w*h*4)
	return nil
}

func (f *Framebuffer) Size() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.w, f.h
}

// Snapshot copies the screen into dst (reusing its capacity) and returns it with the size it was taken at.
func (f *Framebuffer) Snapshot(dst []byte) ([]byte, int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	dst = append(dst[:0], f.pix...)
	return dst, f.w, f.h
}

func (f *Framebuffer) inside(x, y, w, h int) bool {
	return x >= 0 && y >= 0 && w >= 0 && h >= 0 && x <= f.w && y <= f.h && w <= f.w-x && h <= f.h-y
}

// PutRaw draws w*h BGRX pixels at (x, y).
func (f *Framebuffer) PutRaw(x, y, w, h int, src []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.inside(x, y, w, h) {
		return errBounds
	}
	if len(src) != w*h*4 {
		return errors.New("recorder: raw rectangle has the wrong length")
	}
	for row := 0; row < h; row++ {
		copy(f.pix[((y+row)*f.w+x)*4:], src[row*w*4:(row+1)*w*4])
	}
	return nil
}

// Copy moves a rectangle within the screen. The source is read fully first, so overlapping moves are safe.
func (f *Framebuffer) Copy(sx, sy, w, h, dx, dy int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.inside(sx, sy, w, h) || !f.inside(dx, dy, w, h) {
		return errBounds
	}
	tmp := make([]byte, w*h*4)
	for row := 0; row < h; row++ {
		copy(tmp[row*w*4:], f.pix[((sy+row)*f.w+sx)*4:((sy+row)*f.w+sx+w)*4])
	}
	for row := 0; row < h; row++ {
		copy(f.pix[((dy+row)*f.w+dx)*4:], tmp[row*w*4:(row+1)*w*4])
	}
	return nil
}
