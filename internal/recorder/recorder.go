package recorder

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"quiver-playtesting/internal/core"
	"quiver-playtesting/internal/rfb"
)

const (
	dialWait   = 3 * time.Second
	finishWait = 10 * time.Second
	diskEvery  = 30 * time.Second
)

// checkEncoders is a variable so tests with fake encoder scripts can skip the libx264 probe.
var checkEncoders = func(bin string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "-hide_banner", "-encoders")
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	out, err := cmd.Output()
	if err != nil {
		return err
	}
	if !bytes.Contains(out, []byte("libx264")) {
		return errors.New("ffmpeg has no libx264 encoder")
	}
	return nil
}

// runCapture is a variable so tests can make the capture goroutine fail in ways the wire cannot.
var runCapture = RunPaced

var errResized = errors.New("screen size changed")

type Config struct {
	Dir     string
	FFmpeg  string
	FPS     int
	Max     int
	MinFree uint64
}

type Recorder struct {
	cfg     Config
	enabled bool
	sem     chan struct{}
	dial    func(ctx context.Context, addr, password string) (net.Conn, error)
	free    func(dir string) (uint64, error)
}

// New resolves ffmpeg and prepares the recordings directory. When either fails the recorder is
// disabled and says why in the log, so the gateway still starts.
func New(cfg Config) *Recorder {
	if cfg.FPS <= 0 {
		cfg.FPS = 10
	}
	if cfg.Max <= 0 {
		cfg.Max = 2
	}
	r := &Recorder{cfg: cfg, sem: make(chan struct{}, cfg.Max), dial: rfb.Dial, free: freeBytes}
	bin, err := exec.LookPath(cfg.FFmpeg)
	if err != nil {
		slog.Warn("recording disabled: ffmpeg not found", "ffmpeg", cfg.FFmpeg)
		return r
	}
	r.cfg.FFmpeg = bin
	if err := checkEncoders(bin); err != nil {
		slog.Warn("recording disabled: ffmpeg has no libx264 encoder", "err", err)
		return r
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		slog.Warn("recording disabled: cannot create the recordings directory", "dir", cfg.Dir, "err", err)
		return r
	}
	_ = os.Chmod(cfg.Dir, 0o700)
	r.enabled = true
	return r
}

func (r *Recorder) Enabled() bool { return r != nil && r.enabled }

func (r *Recorder) lowDisk() bool {
	free, err := r.free(r.cfg.Dir)
	return err == nil && free < r.cfg.MinFree
}

// Start begins recording the session. It returns the path of the first file, or "" when this session
// is not being recorded, and a stop function that is always safe to call and returns once the file is finished.
func (r *Recorder) Start(ctx context.Context, s core.Session, vm core.VM) (string, func()) {
	noop := func() {}
	if !r.Enabled() {
		return "", noop
	}
	select {
	case r.sem <- struct{}{}:
	default:
		slog.Warn("recording skipped: concurrent recording limit reached", "session", s.ID)
		return "", noop
	}
	release := func() { <-r.sem }
	if r.lowDisk() {
		slog.Warn("recording skipped: low disk space", "session", s.ID)
		release()
		return "", noop
	}
	dir := LinkDir(r.cfg.Dir, s.LinkID, s.Label)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		slog.Warn("recording skipped: cannot create the folder", "session", s.ID, "err", err)
		release()
		return "", noop
	}
	_ = os.Chmod(dir, 0o700)
	dctx, cancel := context.WithTimeout(ctx, dialWait)
	conn, err := r.dial(dctx, net.JoinHostPort(vm.Host, strconv.Itoa(vm.Port)), vm.Password)
	cancel()
	if err != nil {
		slog.Warn("recording skipped: cannot reach the VM", "session", s.ID, "err", err)
		release()
		return "", noop
	}

	rctx, rcancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer release()
		defer func() {
			if v := recover(); v != nil {
				slog.Error("recorder panic", "session", s.ID, "panic", v)
			}
		}()
		if err := r.record(rctx, conn, dir, s); err != nil && rctx.Err() == nil {
			slog.Warn("recording ended early", "session", s.ID, "err", err)
		}
	}()
	var once sync.Once
	stop := func() {
		once.Do(rcancel)
		<-done
	}
	return filepath.Join(dir, FileName(s.StartedAt, s.ID, 1)), stop
}

func (r *Recorder) record(ctx context.Context, conn net.Conn, dir string, s core.Session) error {
	ectx, ecancel := context.WithCancel(ctx)
	defer ecancel()
	ready := make(chan *Framebuffer, 1)
	capErr := make(chan error, 1)
	go func() {
		var err error
		defer func() {
			if v := recover(); v != nil {
				err = fmt.Errorf("capture panic: %v", v)
			}
			capErr <- err
			ecancel()
		}()
		err = runCapture(ectx, conn, time.Second/time.Duration(r.cfg.FPS), func(f *Framebuffer) { ready <- f })
	}()
	var fb *Framebuffer
	select {
	case fb = <-ready:
	case err := <-capErr:
		return err
	}
	loopErr := r.encodeLoop(ectx, fb, dir, s)
	ecancel()
	if err := <-capErr; err != nil && loopErr == nil {
		loopErr = err
	}
	return loopErr
}

// encodeLoop writes one file per screen size, sampling the framebuffer at a fixed rate.
func (r *Recorder) encodeLoop(ctx context.Context, fb *Framebuffer, dir string, s core.Session) error {
	tick := time.NewTicker(time.Second / time.Duration(r.cfg.FPS))
	defer tick.Stop()
	disk := time.NewTicker(diskEvery)
	defer disk.Stop()
	var buf []byte
	for seg := 1; ; seg++ {
		w, h := fb.Size()
		enc, err := startEncoder(r.cfg.FFmpeg, filepath.Join(dir, FileName(s.StartedAt, s.ID, seg)), w, h, r.cfg.FPS)
		if err != nil {
			return err
		}
		err = func() error {
			for {
				select {
				case <-ctx.Done():
					return nil
				case <-disk.C:
					if r.lowDisk() {
						return errors.New("low disk space")
					}
				case <-tick.C:
					var cw, ch int
					buf, cw, ch = fb.Snapshot(buf)
					if cw != w || ch != h {
						return errResized
					}
					if err := enc.WriteFrame(buf); err != nil {
						return err
					}
				}
			}
		}()
		if ferr := enc.Finish(finishWait); ferr != nil && err == nil {
			err = ferr
		}
		if !errors.Is(err, errResized) {
			return err
		}
	}
}
