package gateway

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"quiver-playtesting/internal/core"
	"quiver-playtesting/internal/rfb/rfbtest"
)

type fakeRec struct {
	enabled bool
	path    string
	mu      sync.Mutex
	starts  int
	stops   int
	vmSeen  core.VM
}

func (f *fakeRec) Enabled() bool { return f.enabled }

func (f *fakeRec) Start(_ context.Context, _ core.Session, vm core.VM) (string, func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts++
	f.vmSeen = vm
	return f.path, func() { f.mu.Lock(); f.stops++; f.mu.Unlock() }
}

func (f *fakeRec) counts() (int, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.starts, f.stops
}

func getIndex(t *testing.T, e *env) string {
	t.Helper()
	resp, err := http.Get(e.srv.URL + "/s/anything")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b)
}

func TestRecordingNoticeFollowsEnabled(t *testing.T) {
	on := setup(t, Config{Recorder: &fakeRec{enabled: true}})
	body := getIndex(t, on)
	if !strings.Contains(body, "This session is recorded") || strings.Contains(body, "<!--rec-notice-->") {
		t.Fatalf("notice missing or placeholder left:\n%s", body)
	}
	if !strings.Contains(body, `<div class="right"><div class="rec">`) {
		t.Fatalf("notice not inside the right block:\n%s", body)
	}
	for name, cfg := range map[string]Config{"nil recorder": {}, "disabled": {Recorder: &fakeRec{}}} {
		e := setup(t, cfg)
		if body := getIndex(t, e); strings.Contains(body, "recorded") || strings.Contains(body, "<!--rec-notice-->") {
			t.Fatalf("%s: notice shown or placeholder left", name)
		}
	}
}

func TestSessionStartsAndStopsRecording(t *testing.T) {
	rec := &fakeRec{enabled: true, path: "/rec/a.mp4"}
	e := setup(t, Config{Recorder: rec})
	e.b.add("tok1", 1, rfbtest.Server(t, "pw"), "pw")
	ws, _, err := e.dial(t, "tok1")
	if err != nil {
		t.Fatal(err)
	}
	nc := handshake(t, ws)
	s := waitSessions(t, e.reg, 1)[0]
	if s.Recording != "/rec/a.mp4" {
		t.Fatalf("registry session %+v", s)
	}
	if st, _ := rec.counts(); st != 1 {
		t.Fatalf("starts %d", st)
	}
	if rec.vmSeen.Password != "pw" || rec.vmSeen.ID != 1 {
		t.Fatalf("recorder got %+v", rec.vmSeen)
	}
	nc.Close()
	waitLog(t, e.b)
	if st, sp := rec.counts(); st != 1 || sp != 1 {
		t.Fatalf("starts %d stops %d", st, sp)
	}
	if got := e.b.lastSession().Recording; got != "/rec/a.mp4" {
		t.Fatalf("LogSession saw recording %q", got)
	}
}

func TestSessionWorksWhenNotRecorded(t *testing.T) {
	rec := &fakeRec{enabled: true} // Start returns "" like a full disk or the cap
	e := setup(t, Config{Recorder: rec})
	e.b.add("tok1", 1, rfbtest.Server(t, "pw"), "pw")
	ws, _, err := e.dial(t, "tok1")
	if err != nil {
		t.Fatal(err)
	}
	nc := handshake(t, ws)
	if s := waitSessions(t, e.reg, 1)[0]; s.Recording != "" {
		t.Fatalf("session marked as recording: %+v", s)
	}
	nc.Close()
	waitLog(t, e.b)
	if st, sp := rec.counts(); st != 1 || sp != 1 {
		t.Fatalf("starts %d stops %d", st, sp)
	}
	if got := e.b.lastSession().Recording; got != "" {
		t.Fatalf("LogSession saw recording %q", got)
	}
}

func TestKillStillStopsRecording(t *testing.T) {
	rec := &fakeRec{enabled: true, path: "/rec/a.mp4"}
	e := setup(t, Config{Recorder: rec})
	e.b.add("tok1", 1, rfbtest.Server(t, "pw"), "pw")
	ws, _, _ := e.dial(t, "tok1")
	nc := handshake(t, ws)
	s := waitSessions(t, e.reg, 1)[0]
	e.reg.Kill(s.ID, "killed")
	nc.SetDeadline(time.Now().Add(5 * time.Second))
	nc.Read(make([]byte, 1)) // the close frame is only delivered to a reading peer
	waitLog(t, e.b)
	if _, sp := rec.counts(); sp != 1 {
		t.Fatalf("stops %d", sp)
	}
}
