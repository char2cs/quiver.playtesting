package recorder

import (
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"quiver-playtesting/internal/core"
	"quiver-playtesting/internal/rfb/rfbtest"
)

// fakeEncoder is a shell script that swallows frames, so loop logic can be tested without ffmpeg.
func fakeEncoder(t *testing.T, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("shell script encoder")
	}
	p := filepath.Join(t.TempDir(), "ffmpeg")
	os.WriteFile(p, []byte("#!/bin/sh\n"+script+"\n"), 0o755)
	return p
}

func vmFor(t *testing.T, srv *rfbtest.FB) core.VM {
	t.Helper()
	host, port := splitAddr(t, srv.Addr())
	return core.VM{ID: 1, Name: "vm", Host: host, Port: port}
}

func newRec(t *testing.T, ffmpeg string, max int) (*Recorder, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "recordings")
	old := checkEncoders
	checkEncoders = func(string) error { return nil }
	t.Cleanup(func() { checkEncoders = old })
	r := New(Config{Dir: dir, FFmpeg: ffmpeg, FPS: 10, Max: max, MinFree: 1})
	if !r.Enabled() {
		t.Fatal("recorder should be enabled")
	}
	return r, dir
}

func session() core.Session {
	return core.Session{ID: "ab12cd34", LinkID: 5, Label: "Alpha Team", VMID: 1, StartedAt: time.Date(2026, 10, 4, 15, 0, 0, 0, time.UTC)}
}

func TestDisabledWithoutFFmpeg(t *testing.T) {
	r := New(Config{Dir: t.TempDir(), FFmpeg: "/nonexistent/ffmpeg", FPS: 10, Max: 1})
	if r.Enabled() {
		t.Fatal("must be disabled")
	}
	path, stop := r.Start(t.Context(), session(), core.VM{})
	if path != "" || stop == nil {
		t.Fatalf("path %q, stop nil: %v", path, stop == nil)
	}
	stop()
}

func TestStartCreatesFolderAndFile(t *testing.T) {
	srv := rfbtest.NewFB(t, "", 64, 48)
	r, dir := newRec(t, fakeEncoder(t, "cat >/dev/null"), 2)
	path, stop := r.Start(t.Context(), session(), vmFor(t, srv))
	if path == "" {
		t.Fatal("not recording")
	}
	want := filepath.Join(dir, "link-5-alpha-team", "20261004T150000Z_ab12cd34.mp4")
	if path != want {
		t.Fatalf("path %q want %q", path, want)
	}
	time.Sleep(300 * time.Millisecond)
	stop()
	stop() // idempotent
	if st, err := os.Stat(filepath.Dir(path)); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("folder %v %v", st, err)
	}
	if srv.Clients() != 0 {
		t.Fatal("capture connection left open")
	}
}

func TestConcurrentCap(t *testing.T) {
	srv := rfbtest.NewFB(t, "", 64, 48)
	r, _ := newRec(t, fakeEncoder(t, "cat >/dev/null"), 1)
	p1, stop1 := r.Start(t.Context(), session(), vmFor(t, srv))
	s2 := session()
	s2.ID, s2.LinkID = "ee55", 6
	p2, stop2 := r.Start(t.Context(), s2, vmFor(t, srv))
	if p1 == "" || p2 != "" {
		t.Fatalf("p1=%q p2=%q", p1, p2)
	}
	stop2()
	stop1()
	p3, stop3 := r.Start(t.Context(), s2, vmFor(t, srv))
	if p3 == "" {
		t.Fatal("slot not released after stop")
	}
	stop3()
}

func TestLowDiskSkipsRecording(t *testing.T) {
	srv := rfbtest.NewFB(t, "", 64, 48)
	r, _ := newRec(t, fakeEncoder(t, "cat >/dev/null"), 1)
	r.free = func(string) (uint64, error) { return 0, nil }
	if p, stop := r.Start(t.Context(), session(), vmFor(t, srv)); p != "" {
		t.Fatalf("recorded on a full disk: %q", p)
	} else {
		stop()
	}
}

func TestVMUnreachableSkipsRecording(t *testing.T) {
	r, _ := newRec(t, fakeEncoder(t, "cat >/dev/null"), 1)
	if p, stop := r.Start(t.Context(), session(), core.VM{Host: "127.0.0.1", Port: 1}); p != "" {
		t.Fatalf("recorded without a VM: %q", p)
	} else {
		stop()
	}
}

func TestEncoderCrashDoesNotHang(t *testing.T) {
	srv := rfbtest.NewFB(t, "", 64, 48)
	r, _ := newRec(t, fakeEncoder(t, "exit 1"), 1)
	path, stop := r.Start(t.Context(), session(), vmFor(t, srv))
	if path == "" {
		t.Fatal("start should succeed, the crash comes later")
	}
	done := make(chan struct{})
	go func() { time.Sleep(500 * time.Millisecond); stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("stop hung after the encoder crashed")
	}
	// the slot is free again
	s2 := session()
	s2.ID = "ff99ee11"
	if p, s := r.Start(t.Context(), s2, vmFor(t, srv)); p == "" {
		t.Fatal("slot leaked after a crash")
	} else {
		s()
	}
}

func TestVMDiesMidRecording(t *testing.T) {
	srv := rfbtest.NewFB(t, "", 64, 48)
	r, _ := newRec(t, fakeEncoder(t, "cat >/dev/null"), 1)
	_, stop := r.Start(t.Context(), session(), vmFor(t, srv))
	time.Sleep(300 * time.Millisecond)
	srv.Close()
	done := make(chan struct{})
	go func() { time.Sleep(500 * time.Millisecond); stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("stop hung after the VM died")
	}
}

func TestResolutionChangeStartsANewFile(t *testing.T) {
	ff, fp := needFFmpeg(t)
	srv := rfbtest.NewFB(t, "", 64, 48)
	srv.Fill(10, 20, 30)
	r, _ := newRec(t, ff, 1)
	path, stop := r.Start(t.Context(), session(), vmFor(t, srv))
	if path == "" {
		t.Fatal("not recording")
	}
	time.Sleep(1200 * time.Millisecond)
	srv.Resize(96, 64)
	time.Sleep(1500 * time.Millisecond)
	stop()
	part2 := strings.TrimSuffix(path, ".mp4") + "_part2.mp4"
	w1, h1, d1 := ffprobe(t, fp, path)
	w2, h2, d2 := ffprobe(t, fp, part2)
	t.Logf("part1 %dx%d %.2fs, part2 %dx%d %.2fs", w1, h1, d1, w2, h2, d2)
	if w1 != 64 || h1 != 48 {
		t.Fatalf("first file %dx%d", w1, h1)
	}
	if w2 != 96 || h2 != 64 {
		t.Fatalf("second file %dx%d", w2, h2)
	}
}

func TestEndToEndDuration(t *testing.T) {
	ff, fp := needFFmpeg(t)
	srv := rfbtest.NewFB(t, "", 128, 96)
	srv.Fill(200, 100, 50)
	r, _ := newRec(t, ff, 1)
	path, stop := r.Start(t.Context(), session(), vmFor(t, srv))
	time.Sleep(3 * time.Second)
	stop()
	_, _, dur := ffprobe(t, fp, path)
	t.Logf("3s session recorded as %.2fs", dur)
	if dur < 2.0 || dur > 4.5 {
		t.Fatalf("duration %.2fs for a 3s session", dur)
	}
}

func splitAddr(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, p, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := strconv.Atoi(p)
	return host, n
}

func TestProbeRejectsFFmpegWithoutLibx264(t *testing.T) {
	bin := fakeEncoder(t, "echo ' V..... mpeg4  MPEG-4 part 2'")
	if New(Config{Dir: t.TempDir(), FFmpeg: bin}).Enabled() {
		t.Fatal("enabled without libx264")
	}
}

func TestProbeAcceptsFFmpegWithLibx264(t *testing.T) {
	bin := fakeEncoder(t, "echo ' V....D libx264  H.264'")
	if !New(Config{Dir: t.TempDir(), FFmpeg: bin}).Enabled() {
		t.Fatal("disabled despite libx264")
	}
}
