package service

import (
	"context"
	"encoding/base64"
	"errors"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"quiver-playtesting/internal/core"
	"quiver-playtesting/internal/live"
	"quiver-playtesting/internal/store"
)

type env struct {
	svc *Service
	st  *store.Store
	reg *live.Registry
	now time.Time
}

func setup(t *testing.T) *env {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "q.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	reg := live.New()
	e := &env{svc: New(st, reg), st: st, reg: reg, now: time.Now()}
	e.svc.now = func() time.Time { return e.now }
	return e
}

func (e *env) vm(t *testing.T) core.VM {
	t.Helper()
	v, err := e.svc.AddVM(context.Background(), "win", "vm.example.com", 5900, "pw")
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestAddVMValidation(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	bad := []struct {
		name, host string
		port       int
		pw         string
	}{
		{"", "h.com", 5900, ""},
		{strings.Repeat("a", 65), "h.com", 5900, ""},
		{"a\nb", "h.com", 5900, ""},
		{" a", "h.com", 5900, ""},
		{"a\xff", "h.com", 5900, ""},
		{"ok", "", 5900, ""},
		{"ok", "http://h.com", 5900, ""},
		{"ok", "h.com/path", 5900, ""},
		{"ok", "h .com", 5900, ""},
		{"ok", "h.com:5900", 5900, ""},
		{"ok", "-h.com", 5900, ""},
		{"ok", "h..com", 5900, ""},
		{"ok", "h.com.", 5900, ""},
		{"ok", strings.Repeat("a", 64) + ".com", 5900, ""},
		{"ok", "h.com", 0, ""},
		{"ok", "h.com", 65536, ""},
		{"ok", "h.com", -1, ""},
		{"ok", "h.com", 5900, "123456789"},
	}
	for i, c := range bad {
		if _, err := e.svc.AddVM(ctx, c.name, c.host, c.port, c.pw); err == nil {
			t.Errorf("case %d accepted: %+v", i, c)
		}
	}
	good := []struct {
		host string
		port int
		pw   string
	}{
		{"h.com", 1, ""}, {"10.0.0.1", 65535, "12345678"}, {"::1", 5900, ""}, {"localhost", 5900, "x"},
	}
	for _, c := range good {
		if _, err := e.svc.AddVM(ctx, "ok", c.host, c.port, c.pw); err != nil {
			t.Errorf("rejected %+v: %v", c, err)
		}
	}
}

func TestAddVMErrorDoesNotLeakPassword(t *testing.T) {
	e := setup(t)
	_, err := e.svc.AddVM(context.Background(), "ok", "h.com", 0, "hunter2")
	if err != nil && strings.Contains(err.Error(), "hunter2") {
		t.Fatal("password in error")
	}
}

func TestCreateLinkAndResolve(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	v := e.vm(t)
	l, tok, err := e.svc.CreateLink(ctx, v.ID, "alice", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil || len(raw) != 32 {
		t.Fatalf("token %q %v", tok, err)
	}
	gl, gv, err := e.svc.Resolve(ctx, tok)
	if err != nil || gl.ID != l.ID || gv.ID != v.ID || gv.Password != "pw" {
		t.Fatalf("%+v %+v %v", gl, gv, err)
	}
	_, tok2, _ := e.svc.CreateLink(ctx, v.ID, "bob", time.Hour)
	if tok2 == tok {
		t.Fatal("tokens collide")
	}
	for _, bad := range []string{"", "nope", tok + "x", strings.Repeat("a", 5000)} {
		if _, _, err := e.svc.Resolve(ctx, bad); !errors.Is(err, core.ErrNotFound) {
			t.Fatalf("%.10q: %v", bad, err)
		}
	}
}

func TestTokenNotStoredInPlaintext(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	v := e.vm(t)
	_, tok, _ := e.svc.CreateLink(ctx, v.ID, "a", time.Hour)
	if _, err := e.st.LinkByTokenHash(ctx, []byte(tok)); err == nil {
		t.Fatal("plaintext token usable as hash")
	}
	if _, err := e.st.LinkByTokenHash(ctx, hashToken(tok)); err != nil {
		t.Fatal(err)
	}
}

func TestTokenInputCap(t *testing.T) {
	long := strings.Repeat("a", 128)
	if string(hashToken(long+"x")) != string(hashToken(long+"y")) {
		t.Fatal("input past cap should be ignored")
	}
	if string(hashToken(long[:127])) == string(hashToken(long)) {
		t.Fatal("input under cap must matter")
	}
}

func TestExpiry(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	v := e.vm(t)
	_, tok, _ := e.svc.CreateLink(ctx, v.ID, "a", time.Hour)
	e.now = e.now.Add(59 * time.Minute)
	if _, _, err := e.svc.Resolve(ctx, tok); err != nil {
		t.Fatal(err)
	}
	e.now = e.now.Add(2 * time.Minute)
	if _, _, err := e.svc.Resolve(ctx, tok); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("expired link resolved: %v", err)
	}
}

func TestRevokeAndKill(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	v := e.vm(t)
	l, tok, _ := e.svc.CreateLink(ctx, v.ID, "a", time.Hour)
	c, cancel := context.WithCancelCause(ctx)
	_ = e.reg.Start(core.Session{ID: "s", LinkID: l.ID, VMID: v.ID}, cancel)
	if err := e.svc.RevokeLink(ctx, l.ID); err != nil {
		t.Fatal(err)
	}
	if c.Err() == nil || context.Cause(c).Error() != "revoked" {
		t.Fatal("session not killed")
	}
	if _, _, err := e.svc.Resolve(ctx, tok); !errors.Is(err, core.ErrNotFound) {
		t.Fatal(err)
	}
	if err := e.svc.RevokeLink(ctx, 999); !errors.Is(err, core.ErrNotFound) {
		t.Fatal(err)
	}
}

func TestKillVariants(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	v := e.vm(t)
	l, tok, _ := e.svc.CreateLink(ctx, v.ID, "a", time.Hour)
	c, cancel := context.WithCancelCause(ctx)
	_ = e.reg.Start(core.Session{ID: "s", LinkID: l.ID, VMID: v.ID}, cancel)
	if err := e.svc.Kill("nope", false); !errors.Is(err, core.ErrNotFound) {
		t.Fatal(err)
	}
	if err := e.svc.Kill("s", false); err != nil {
		t.Fatal(err)
	}
	if context.Cause(c).Error() != "killed" {
		t.Fatal("cause")
	}
	if _, _, err := e.svc.Resolve(ctx, tok); err != nil {
		t.Fatalf("plain kill must not revoke: %v", err)
	}
	if err := e.svc.Kill("s", true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := e.svc.Resolve(ctx, tok); !errors.Is(err, core.ErrNotFound) {
		t.Fatal("kill+revoke did not revoke")
	}
}

func TestFlag(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	v := e.vm(t)
	l, _, _ := e.svc.CreateLink(ctx, v.ID, "a", time.Hour)
	_ = e.reg.Start(core.Session{ID: "s", LinkID: l.ID, VMID: v.ID}, nil)
	if err := e.svc.Flag("x"); !errors.Is(err, core.ErrNotFound) {
		t.Fatal(err)
	}
	if err := e.svc.Flag("s"); err != nil {
		t.Fatal(err)
	}
	g, _ := e.st.GetLink(ctx, l.ID)
	if !g.Flagged {
		t.Fatal("not flagged")
	}
}

func TestRemoveVMKillsSessionsAndRevokes(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	v := e.vm(t)
	l, tok, _ := e.svc.CreateLink(ctx, v.ID, "a", time.Hour)
	c, cancel := context.WithCancelCause(ctx)
	_ = e.reg.Start(core.Session{ID: "s", LinkID: l.ID, VMID: v.ID}, cancel)
	if err := e.svc.RemoveVM(ctx, v.ID); err != nil {
		t.Fatal(err)
	}
	if c.Err() == nil {
		t.Fatal("session survived")
	}
	if _, _, err := e.svc.Resolve(ctx, tok); !errors.Is(err, core.ErrNotFound) {
		t.Fatal(err)
	}
	if err := e.svc.RemoveVM(ctx, v.ID); !errors.Is(err, core.ErrNotFound) {
		t.Fatal(err)
	}
}

func TestCreateLinkValidation(t *testing.T) {
	e := setup(t)
	ctx := context.Background()
	v := e.vm(t)
	for _, ttl := range []time.Duration{0, -time.Hour, time.Minute - 1, MaxTTL + 1} {
		if _, _, err := e.svc.CreateLink(ctx, v.ID, "a", ttl); err == nil {
			t.Errorf("ttl %v accepted", ttl)
		}
	}
	for _, ttl := range []time.Duration{time.Minute, MaxTTL} {
		if _, _, err := e.svc.CreateLink(ctx, v.ID, "a", ttl); err != nil {
			t.Errorf("ttl %v rejected: %v", ttl, err)
		}
	}
	if _, _, err := e.svc.CreateLink(ctx, v.ID, "a\x00b", time.Hour); err == nil {
		t.Error("control char label accepted")
	}
	if _, _, err := e.svc.CreateLink(ctx, v.ID, strings.Repeat("a", 129), time.Hour); err == nil {
		t.Error("long label accepted")
	}
	if _, _, err := e.svc.CreateLink(ctx, 999, "a", time.Hour); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("unknown vm: %v", err)
	}
}

func TestTestVM(t *testing.T) {
	e := setup(t)
	v := e.vm(t)
	var gotAddr, gotPw string
	e.svc.dial = func(ctx context.Context, addr, pw string) (net.Conn, error) {
		gotAddr, gotPw = addr, pw
		a, b := net.Pipe()
		b.Close()
		return a, nil
	}
	if err := e.svc.TestVM(context.Background(), v.ID); err != nil {
		t.Fatal(err)
	}
	if gotAddr != "vm.example.com:5900" || gotPw != "pw" {
		t.Fatal(gotAddr, gotPw)
	}
	e.svc.dial = func(context.Context, string, string) (net.Conn, error) { return nil, errors.New("boom") }
	if err := e.svc.TestVM(context.Background(), v.ID); err == nil {
		t.Fatal("want error")
	}
	if err := e.svc.TestVM(context.Background(), 999); !errors.Is(err, core.ErrNotFound) {
		t.Fatal(err)
	}
}

func TestSubscribeAndLogSession(t *testing.T) {
	e := setup(t)
	ch, unsub := e.svc.Subscribe()
	defer unsub()
	_ = e.reg.Start(core.Session{ID: "s", LinkID: 1, VMID: 1}, nil)
	if ev := <-ch; ev.Type != "start" {
		t.Fatal(ev)
	}
	if got := e.svc.ListSessions(); len(got) != 1 {
		t.Fatal(got)
	}
	if err := e.svc.LogSession(context.Background(), core.Session{ID: "s"}, time.Now(), "closed"); err != nil {
		t.Fatal(err)
	}
}
