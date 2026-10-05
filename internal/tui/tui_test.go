package tui

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"

	"quiver-playtesting/internal/core"
)

type fakeSvc struct {
	core.Service
	vms     []core.VM
	killed  []string
	revoked []bool
	created []string
	addErr  error
}

func (f *fakeSvc) ListVMs(context.Context) ([]core.VM, error)     { return f.vms, nil }
func (f *fakeSvc) ListLinks(context.Context) ([]core.Link, error) { return nil, nil }
func (f *fakeSvc) ListSessions() []core.Session                   { return nil }
func (f *fakeSvc) Kill(id string, revoke bool) error {
	f.killed = append(f.killed, id)
	f.revoked = append(f.revoked, revoke)
	return nil
}
func (f *fakeSvc) AddVM(_ context.Context, name, host string, port int, pw string) (core.VM, error) {
	return core.VM{}, f.addErr
}
func (f *fakeSvc) CreateLink(_ context.Context, vmID int64, label string, ttl time.Duration) (core.Link, string, error) {
	f.created = append(f.created, label+"/"+ttl.String())
	return core.Link{}, "TOKEN", nil
}

func newTestModel(f *fakeSvc) *model {
	return newModel(f, "https://play.example/", make(chan core.Event))
}

func press(m *model, s string) tea.Cmd {
	var k tea.KeyPressMsg
	switch s {
	case "enter":
		k = tea.KeyPressMsg{Code: tea.KeyEnter}
	case "tab":
		k = tea.KeyPressMsg{Code: tea.KeyTab}
	case "esc":
		k = tea.KeyPressMsg{Code: tea.KeyEscape}
	default:
		r := []rune(s)
		k = tea.KeyPressMsg{Code: r[0], Text: s}
	}
	_, cmd := m.Update(k)
	return cmd
}

func TestEventsUpdateSessions(t *testing.T) {
	m := newTestModel(&fakeSvc{})
	s := core.Session{ID: "a", Label: "alice", VMName: "box", StartedAt: time.Now()}
	m.Update(eventMsg(core.Event{Type: "start", Session: s}))
	if len(m.sessions) != 1 || len(m.tables[tabSessions].Rows()) != 1 {
		t.Fatal("start not applied")
	}
	m.Update(eventMsg(core.Event{Type: "start", Session: s}))
	if len(m.sessions) != 1 {
		t.Fatal("duplicate start must not duplicate row")
	}
	m.Update(eventMsg(core.Event{Type: "end", Session: s, Reason: "idle"}))
	if len(m.sessions) != 0 || !strings.Contains(m.note, "idle") {
		t.Fatalf("end not applied: %q", m.note)
	}
}

func TestSessionKeys(t *testing.T) {
	f := &fakeSvc{}
	m := newTestModel(f)
	m.Update(sessionsMsg{{ID: "s1", Label: "a"}})
	cmd := press(m, "d")
	if cmd == nil {
		t.Fatal("expected cmd")
	}
	cmd()
	if len(f.killed) != 1 || f.killed[0] != "s1" || !f.revoked[0] {
		t.Fatalf("drop should kill and revoke: %+v %+v", f.killed, f.revoked)
	}
	press(m, "k")()
	if f.revoked[1] {
		t.Fatal("kill must not revoke")
	}
}

func TestTabSwitching(t *testing.T) {
	m := newTestModel(&fakeSvc{})
	press(m, "tab")
	if m.tab != tabVMs {
		t.Fatal("tab")
	}
	press(m, "3")
	if m.tab != tabLinks {
		t.Fatal("3")
	}
	_, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift})
	if m.tab != tabVMs {
		t.Fatal("shift+tab")
	}
}

func TestConfirmRemove(t *testing.T) {
	m := newTestModel(&fakeSvc{})
	m.Update(vmsMsg{{ID: 3, Name: "box"}})
	m.tab = tabVMs
	press(m, "x")
	if m.mode != modeConfirm {
		t.Fatal("want confirm")
	}
	press(m, "n")
	if m.mode != modeList || m.onYes == nil {
		t.Fatal("cancel should return to list")
	}
	press(m, "x")
	if cmd := press(m, "y"); cmd == nil {
		t.Fatal("y should run the action")
	}
}

func TestAddVMFormValidationAndPasswordMask(t *testing.T) {
	m := newTestModel(&fakeSvc{})
	m.tab = tabVMs
	press(m, "a")
	if m.mode != modeAddVM {
		t.Fatal("form not open")
	}
	for _, s := range []string{"b", "o", "x", "enter", "h", "enter"} {
		press(m, s)
	}
	m.inputs[2].SetValue("abc")
	press(m, "enter")
	for _, c := range "secret" {
		press(m, string(c))
	}
	if !strings.Contains(m.View().Content, "******") || strings.Contains(m.View().Content, "secret") {
		t.Fatal("password must be masked")
	}
	press(m, "enter")
	if m.mode != modeAddVM || !strings.Contains(m.err, "port") {
		t.Fatalf("bad port should keep form open with error, err=%q", m.err)
	}
	m.inputs[2].SetValue("5900")
	if cmd := press(m, "enter"); cmd == nil || m.mode != modeList {
		t.Fatal("valid form should submit")
	}
}

func TestNewLinkFlowShowsURL(t *testing.T) {
	f := &fakeSvc{}
	m := newTestModel(f)
	m.tab = tabLinks
	press(m, "n")
	if m.err == "" {
		t.Fatal("no VMs should error")
	}
	m.Update(vmsMsg{{ID: 1, Name: "box"}})
	press(m, "n")
	if m.mode != modeNewLink {
		t.Fatal("form not open")
	}
	m.inputs[1].SetValue("bob")
	m.inputs[2].SetValue("2d")
	press(m, "enter")
	msg := press(m, "enter")()
	if len(f.created) != 1 || f.created[0] != "bob/48h0m0s" {
		t.Fatalf("created: %v", f.created)
	}
	m.Update(msg)
	if m.mode != modeURL || m.url != "https://play.example/s/TOKEN" {
		t.Fatalf("url: %q", m.url)
	}
	if !strings.Contains(m.View().Content, "https://play.example/s/TOKEN") {
		t.Fatal("url not rendered")
	}
	press(m, "x")
	if m.mode != modeList || m.url != "" {
		t.Fatal("url should clear on close")
	}
}

func TestErrorsInline(t *testing.T) {
	m := newTestModel(&fakeSvc{})
	m.Update(errMsg{errors.New("boom")})
	if !strings.Contains(m.View().Content, "error: boom") {
		t.Fatal("error not shown")
	}
}

func TestParseTTL(t *testing.T) {
	for in, want := range map[string]time.Duration{"4h": 4 * time.Hour, "30m": 30 * time.Minute, "2d": 48 * time.Hour} {
		if got, err := parseTTL(in); err != nil || got != want {
			t.Errorf("%s: %v %v", in, got, err)
		}
	}
	for _, in := range []string{"", "x", "-1h", "0d"} {
		if _, err := parseTTL(in); err == nil {
			t.Errorf("%q should fail", in)
		}
	}
}

func TestLinkStatus(t *testing.T) {
	now := time.Now()
	future, past := now.Add(time.Hour), now.Add(-time.Hour)
	cases := map[string]core.Link{
		"active":  {ExpiresAt: future},
		"expired": {ExpiresAt: past},
		"revoked": {ExpiresAt: future, Revoked: true, Flagged: true},
		"flagged": {ExpiresAt: future, Flagged: true},
	}
	for want, l := range cases {
		if got := linkStatus(l, now); got != want {
			t.Errorf("%s: got %s", want, got)
		}
	}
}

func TestRowsAreRendered(t *testing.T) {
	m := newModel(nil, "http://x", make(chan core.Event))
	m.Update(tea.WindowSizeMsg{Width: 110, Height: 30})
	m.Update(vmsMsg([]core.VM{{ID: 1, Name: "vm-visible", Host: "h", Port: 5900}}))
	if out := m.tables[tabVMs].View(); !strings.Contains(out, "vm-visible") {
		t.Fatalf("row missing from rendered table:\n%s", out)
	}
}

func TestSessionsTableMarksRecording(t *testing.T) {
	m := newTestModel(&fakeSvc{})
	m.sessions = []core.Session{
		{ID: "a", Label: "one", VMName: "vm", ClientIP: "1.1.1.1", StartedAt: time.Now(), Recording: "/rec/a.mp4"},
		{ID: "b", Label: "two", VMName: "vm", ClientIP: "2.2.2.2", StartedAt: time.Now()},
	}
	m.syncRows()
	rows := m.tables[tabSessions].Rows()
	if len(rows) != 2 || rows[0][4] != "REC" || rows[1][4] != "" {
		t.Fatalf("rows %v", rows)
	}
}
