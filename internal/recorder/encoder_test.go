package recorder

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func needFFmpeg(t *testing.T) (ffmpeg, ffprobe string) {
	t.Helper()
	a, err1 := exec.LookPath("ffmpeg")
	b, err2 := exec.LookPath("ffprobe")
	if err1 != nil || err2 != nil {
		t.Skip("ffmpeg and ffprobe are not installed")
	}
	return a, b
}

type probe struct {
	Streams []struct {
		Width  int `json:"width"`
		Height int `json:"height"`
	} `json:"streams"`
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

func ffprobe(t *testing.T, bin, file string) (w, h int, dur float64) {
	t.Helper()
	out, err := exec.Command(bin, "-v", "error", "-show_entries", "stream=width,height:format=duration", "-of", "json", file).Output()
	if err != nil {
		t.Fatalf("ffprobe %s: %v", file, err)
	}
	var p probe
	if err := json.Unmarshal(out, &p); err != nil || len(p.Streams) == 0 {
		t.Fatalf("probe output %s %v", out, err)
	}
	dur, _ = strconv.ParseFloat(p.Format.Duration, 64)
	return p.Streams[0].Width, p.Streams[0].Height, dur
}

func TestEncoderWritesPlayableMP4(t *testing.T) {
	ff, fp := needFFmpeg(t)
	out := filepath.Join(t.TempDir(), "a.mp4")
	enc, err := startEncoder(ff, out, 100, 60, 10)
	if err != nil {
		t.Fatal(err)
	}
	frame := make([]byte, 100*60*4)
	for i := 0; i < 30; i++ {
		frame[0] = byte(i * 8)
		if err := enc.WriteFrame(frame); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if err := enc.Finish(10 * time.Second); err != nil {
		t.Fatal(err)
	}
	w, h, dur := ffprobe(t, fp, out)
	if w != 100 || h != 60 || dur < 2.0 || dur > 4.5 {
		t.Fatalf("got %dx%d %.2fs", w, h, dur)
	}
	if st, _ := os.Stat(out); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
}

func TestEncoderOddSizeIsRoundedDown(t *testing.T) {
	ff, fp := needFFmpeg(t)
	out := filepath.Join(t.TempDir(), "odd.mp4")
	enc, err := startEncoder(ff, out, 101, 61, 10)
	if err != nil {
		t.Fatal(err)
	}
	frame := make([]byte, 101*61*4)
	for i := 0; i < 10; i++ {
		enc.WriteFrame(frame)
		time.Sleep(100 * time.Millisecond)
	}
	if err := enc.Finish(10 * time.Second); err != nil {
		t.Fatal(err)
	}
	if w, h, _ := ffprobe(t, fp, out); w != 100 || h != 60 {
		t.Fatalf("got %dx%d", w, h)
	}
}

func TestEncoderFinishKillsAHungChild(t *testing.T) {
	sh := filepath.Join(t.TempDir(), "hang.sh")
	os.WriteFile(sh, []byte("#!/bin/sh\nexec sleep 60\n"), 0o755)
	enc, err := startEncoder(sh, filepath.Join(t.TempDir(), "x.mp4"), 4, 4, 10)
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	enc.Finish(300 * time.Millisecond)
	if time.Since(start) > 3*time.Second {
		t.Fatal("Finish did not kill the hung encoder")
	}
}

func TestStartEncoderMissingBinary(t *testing.T) {
	if _, err := startEncoder("/nonexistent/ffmpeg", filepath.Join(t.TempDir(), "x.mp4"), 4, 4, 10); err == nil {
		t.Fatal("missing binary accepted")
	}
}

func TestEncoderKilledMidRecordingStaysPlayable(t *testing.T) {
	ff, fp := needFFmpeg(t)
	out := filepath.Join(t.TempDir(), "killed.mp4")
	enc, err := startEncoder(ff, out, 100, 60, 10)
	if err != nil {
		t.Fatal(err)
	}
	frame := make([]byte, 100*60*4)
	for i := 0; i < 40; i++ {
		frame[0] = byte(i * 6)
		if err := enc.WriteFrame(frame); err != nil {
			t.Fatal(err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	enc.cmd.Process.Kill()
	if err := enc.Finish(5 * time.Second); err == nil {
		t.Fatal("a killed encoder should report an error")
	}
	w, h, dur := ffprobe(t, fp, out)
	if w != 100 || h != 60 || dur < 1.0 {
		t.Fatalf("killed file reads as %dx%d %.2fs", w, h, dur)
	}
}

func TestEncoderDurationMatchesWallClock(t *testing.T) {
	ff, fp := needFFmpeg(t)
	out := filepath.Join(t.TempDir(), "wall.mp4")
	enc, err := startEncoder(ff, out, 64, 48, 10)
	if err != nil {
		t.Fatal(err)
	}
	frame := make([]byte, 64*48*4)
	start := time.Now()
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for time.Since(start) < 3*time.Second {
		<-tick.C
		if err := enc.WriteFrame(frame); err != nil {
			t.Fatal(err)
		}
	}
	wall := time.Since(start).Seconds()
	if err := enc.Finish(10 * time.Second); err != nil {
		t.Fatal(err)
	}
	_, _, dur := ffprobe(t, fp, out)
	t.Logf("wall %.2fs file %.2fs", wall, dur)
	if dur < wall-0.5 || dur > wall+0.5 {
		t.Fatalf("file %.2fs for %.2fs of wall time", dur, wall)
	}
}

func TestEncoderFailureLeavesNoFile(t *testing.T) {
	sh := filepath.Join(t.TempDir(), "fail.sh")
	os.WriteFile(sh, []byte("#!/bin/sh\nexit 1\n"), 0o755)
	out := filepath.Join(t.TempDir(), "x.mp4")
	enc, err := startEncoder(sh, out, 4, 4, 10)
	if err != nil {
		t.Fatal(err)
	}
	if enc.Finish(5*time.Second) == nil {
		t.Fatal("expected an error")
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatalf("empty file left behind: %v", err)
	}
}
