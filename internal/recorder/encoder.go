package recorder

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"time"
)

// capBuf keeps the first bytes of ffmpeg's stderr for the log and drops the rest.
type capBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (c *capBuf) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if room := 2048 - c.b.Len(); room > 0 {
		c.b.Write(p[:min(len(p), room)])
	}
	return len(p), nil
}

func (c *capBuf) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.b.String()
}

const writeWait = 5 * time.Second

type encoder struct {
	cmd    *exec.Cmd
	in     *os.File
	stderr *capBuf
	done   chan error
	out    string
}

// startEncoder runs ffmpeg reading raw BGRA frames of w*h from stdin and writing fragmented H.264 mp4 to out.
// Fragmented output stays playable if the process dies mid-session.
func startEncoder(ffmpeg, out string, w, h, fps int) (*encoder, error) {
	// Created 0600 up front: ffmpeg truncates an existing file and keeps its mode.
	f, err := os.OpenFile(out, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	f.Close()
	args := []string{
		"-hide_banner", "-loglevel", "error",
		"-f", "rawvideo", "-pixel_format", "bgra", "-video_size", fmt.Sprintf("%dx%d", w, h),
		"-use_wallclock_as_timestamps", "1", "-i", "pipe:0",
		"-vf", fmt.Sprintf("fps=%d,scale=trunc(iw/2)*2:trunc(ih/2)*2", fps),
		"-c:v", "libx264", "-preset", "ultrafast", "-crf", "30", "-pix_fmt", "yuv420p",
		"-g", strconv.Itoa(fps * 2),
		"-flush_packets", "1", "-frag_duration", "1000000",
		"-movflags", "+frag_keyframe+empty_moov+default_base_moof",
		"-y", out,
	}
	cmd := exec.Command(ffmpeg, args...)
	cmd.Env = []string{"PATH=" + os.Getenv("PATH")}
	e := &encoder{cmd: cmd, stderr: &capBuf{}, done: make(chan error, 1), out: out}
	cmd.Stderr = e.stderr
	pr, pw, err := os.Pipe()
	if err != nil {
		os.Remove(out)
		return nil, err
	}
	cmd.Stdin = pr
	cmd.WaitDelay = 2 * time.Second
	e.in = pw
	err = cmd.Start()
	pr.Close()
	if err != nil {
		pw.Close()
		os.Remove(out)
		return nil, err
	}
	lowerPriority(cmd.Process.Pid)
	go func() { e.done <- cmd.Wait() }()
	return e, nil
}

func (e *encoder) WriteFrame(p []byte) error {
	// Best effort: SetWriteDeadline is a no-op on some platforms (pipes on Windows).
	// A stalled ffmpeg must not block the recording loop, and so stop, forever.
	_ = e.in.SetWriteDeadline(time.Now().Add(writeWait))
	_, err := e.in.Write(p)
	return err
}

// Finish closes the input so ffmpeg writes the end of the file, and kills it if it takes longer than timeout.
func (e *encoder) Finish(timeout time.Duration) error {
	e.in.Close()
	select {
	case err := <-e.done:
		if err != nil {
			e.removeIfEmpty()
			return fmt.Errorf("ffmpeg: %w: %s", err, e.stderr.String())
		}
		return nil
	case <-time.After(timeout):
		e.cmd.Process.Kill()
		<-e.done
		e.removeIfEmpty()
		return fmt.Errorf("ffmpeg did not finish within %s", timeout)
	}
}

// removeIfEmpty drops a file ffmpeg never wrote to; a file with content is playable partial video.
func (e *encoder) removeIfEmpty() {
	if st, err := os.Stat(e.out); err == nil && st.Size() == 0 {
		os.Remove(e.out)
	}
}
