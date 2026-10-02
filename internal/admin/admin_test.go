package admin

import (
	"bufio"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"quiver-playtesting/internal/core"
)

type fake struct {
	mu    sync.Mutex
	vms   []core.VM
	links []core.Link
	sess  []core.Session
	subs  []chan core.Event
	kills []string
	flags []string
}

func (f *fake) ListVMs(context.Context) ([]core.VM, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]core.VM(nil), f.vms...), nil
}

func (f *fake) AddVM(_ context.Context, name, host string, port int, pw string) (core.VM, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	vm := core.VM{ID: int64(len(f.vms) + 1), Name: name, Host: host, Port: port, Password: pw}
	f.vms = append(f.vms, vm)
	return vm, nil
}

func (f *fake) RemoveVM(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, v := range f.vms {
		if v.ID == id {
			f.vms = append(f.vms[:i], f.vms[i+1:]...)
			return nil
		}
	}
	return core.ErrNotFound
}

func (f *fake) TestVM(_ context.Context, id int64) error {
	if id == 1 {
		return nil
	}
	if id == 2 {
		return errors.New("auth failed")
	}
	return core.ErrNotFound
}

func (f *fake) ListLinks(context.Context) ([]core.Link, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]core.Link(nil), f.links...), nil
}

func (f *fake) CreateLink(_ context.Context, vmID int64, label string, ttl time.Duration) (core.Link, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	l := core.Link{ID: int64(len(f.links) + 1), VMID: vmID, Label: label, ExpiresAt: time.Now().Add(ttl).UTC()}
	f.links = append(f.links, l)
	return l, "tok-secret", nil
}

func (f *fake) RevokeLink(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.links {
		if f.links[i].ID == id {
			f.links[i].Revoked = true
			return nil
		}
	}
	return core.ErrNotFound
}

func (f *fake) ListSessions() []core.Session {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]core.Session(nil), f.sess...)
}

func (f *fake) Kill(id string, revoke bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id == "busy" {
		return core.ErrBusy
	}
	if id != "s1" {
		return core.ErrNotFound
	}
	f.kills = append(f.kills, id)
	return nil
}

func (f *fake) Flag(id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if id != "s1" {
		return core.ErrNotFound
	}
	f.flags = append(f.flags, id)
	return nil
}

func (f *fake) Subscribe() (<-chan core.Event, func()) {
	ch := make(chan core.Event, 8)
	f.mu.Lock()
	f.subs = append(f.subs, ch)
	f.mu.Unlock()
	return ch, func() {}
}

func (f *fake) emit(ev core.Event) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, s := range f.subs {
		s <- ev
	}
}

func (f *fake) subCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.subs)
}

func start(t *testing.T, svc core.Service) (string, context.CancelFunc, chan error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "a.sock")
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, path, svc) }()
	for i := 0; i < 200; i++ {
		if _, err := os.Stat(path); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Cleanup(cancel)
	return path, cancel, done
}

func TestRoundTrip(t *testing.T) {
	f := &fake{sess: []core.Session{{ID: "s1", Label: "alice"}}}
	path, _, _ := start(t, f)
	c, err := Dial(path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx := context.Background()

	vms, err := c.ListVMs(ctx)
	if err != nil || len(vms) != 0 {
		t.Fatalf("empty list: %v %v", vms, err)
	}
	vm, err := c.AddVM(ctx, "box", "10.0.0.1", 5900, "hunter22")
	if err != nil || vm.ID != 1 || vm.Name != "box" || vm.Password != "" {
		t.Fatalf("addvm: %+v %v", vm, err)
	}
	f.mu.Lock()
	if f.vms[0].Password != "hunter22" {
		t.Error("password did not reach the service")
	}
	f.mu.Unlock()
	if vms, _ = c.ListVMs(ctx); len(vms) != 1 || vms[0].Password != "" {
		t.Fatalf("list: %+v", vms)
	}
	if err := c.TestVM(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := c.TestVM(ctx, 2); err == nil || err.Error() != "auth failed" {
		t.Fatalf("testvm err: %v", err)
	}
	if err := c.TestVM(ctx, 9); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("want not found: %v", err)
	}

	l, tok, err := c.CreateLink(ctx, 1, "lbl", time.Hour)
	if err != nil || tok != "tok-secret" || l.Label != "lbl" || l.ID != 1 {
		t.Fatalf("createlink: %+v %q %v", l, tok, err)
	}
	if ls, _ := c.ListLinks(ctx); len(ls) != 1 {
		t.Fatalf("links: %+v", ls)
	}
	if err := c.RevokeLink(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := c.RevokeLink(ctx, 5); !errors.Is(err, core.ErrNotFound) {
		t.Fatal(err)
	}

	if ss := c.ListSessions(); len(ss) != 1 || ss[0].ID != "s1" {
		t.Fatalf("sessions: %+v", ss)
	}
	if err := c.Kill("s1", true); err != nil {
		t.Fatal(err)
	}
	if err := c.Kill("busy", false); !errors.Is(err, core.ErrBusy) {
		t.Fatal(err)
	}
	if err := c.Flag("s1"); err != nil {
		t.Fatal(err)
	}
	if err := c.Flag("x"); !errors.Is(err, core.ErrNotFound) {
		t.Fatal(err)
	}
	if err := c.RemoveVM(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveVM(ctx, 1); !errors.Is(err, core.ErrNotFound) {
		t.Fatal(err)
	}
}

func TestSubscribe(t *testing.T) {
	f := &fake{}
	path, _, _ := start(t, f)
	c, _ := Dial(path)
	ch, unsub := c.Subscribe()
	defer unsub()
	for f.subCount() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	f.emit(core.Event{Type: "start", Session: core.Session{ID: "s9", Label: "bob"}})
	f.emit(core.Event{Type: "end", Session: core.Session{ID: "s9"}, Reason: "killed"})
	for _, want := range []string{"start", "end"} {
		select {
		case ev := <-ch:
			if ev.Type != want || ev.Session.ID != "s9" {
				t.Fatalf("got %+v want %s", ev, want)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("timeout")
		}
	}
	unsub()
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("expected closed channel")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("channel not closed after unsubscribe")
	}
}

func TestSocketMode(t *testing.T) {
	path, _, _ := start(t, &fake{})
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
}

func TestStaleSocket(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.sock")
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	ln.Close()
	if _, err := os.Stat(path); err != nil {
		t.Fatal("stale socket file should remain")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go Serve(ctx, path, &fake{})
	var c *Client
	for i := 0; i < 200; i++ {
		if c, err = Dial(path); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("stale socket not replaced: %v", err)
	}
	if _, err := c.ListVMs(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestRefusesLiveOwnerAndNonSocket(t *testing.T) {
	path, _, _ := start(t, &fake{})
	if err := Serve(context.Background(), path, &fake{}); err == nil || !strings.Contains(err.Error(), "already") {
		t.Fatalf("want already listening error, got %v", err)
	}
	if _, err := Dial(path); err != nil {
		t.Fatal("first daemon must survive")
	}
	file := filepath.Join(t.TempDir(), "plain")
	os.WriteFile(file, []byte("x"), 0o600)
	if err := Serve(context.Background(), file, &fake{}); err == nil {
		t.Fatal("must not clobber a regular file")
	}
	if _, err := os.Stat(file); err != nil {
		t.Fatal("regular file removed")
	}
}

func TestOversizeLine(t *testing.T) {
	path, _, _ := start(t, &fake{})
	c, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	go c.Write([]byte(strings.Repeat("a", maxLine+1024) + "\n"))
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	line, _ := bufio.NewReader(c).ReadString('\n')
	if !strings.Contains(line, "too large") {
		t.Fatalf("got %q", line)
	}
}

func TestPasswordNeverOnWire(t *testing.T) {
	f := &fake{}
	path, _, _ := start(t, f)
	c, _ := Dial(path)
	c.AddVM(context.Background(), "box", "h", 5900, "sekret99")
	raw, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	raw.Write([]byte(`{"id":1,"method":"ListVMs"}` + "\n"))
	raw.SetReadDeadline(time.Now().Add(2 * time.Second))
	line, _ := bufio.NewReader(raw).ReadString('\n')
	if !strings.Contains(line, `"box"`) || strings.Contains(line, "sekret99") || strings.Contains(strings.ToLower(line), "password") {
		t.Fatalf("bad reply %q", line)
	}
}

func TestBadRequests(t *testing.T) {
	path, _, _ := start(t, &fake{})
	raw, _ := net.Dial("unix", path)
	defer raw.Close()
	r := bufio.NewReader(raw)
	raw.Write([]byte(`{"id":1,"method":"Nope"}` + "\n"))
	line, _ := r.ReadString('\n')
	if !strings.Contains(line, "unknown method") {
		t.Fatalf("got %q", line)
	}
	raw.Write([]byte("not json\n"))
	line, _ = r.ReadString('\n')
	if !strings.Contains(line, "bad request") {
		t.Fatalf("got %q", line)
	}
}

func TestShutdownRemovesSocket(t *testing.T) {
	path, cancel, done := start(t, &fake{})
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("serve did not return")
	}
	if _, err := os.Stat(path); err == nil {
		t.Fatal("socket not removed")
	}
}
