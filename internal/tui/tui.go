// Package tui is the operator terminal UI, built on Bubble Tea v2.
package tui

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/table"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"quiver-playtesting/internal/core"
)

const (
	tabSessions = iota
	tabVMs
	tabLinks
)

var tabNames = []string{"Sessions", "VMs", "Links"}

type mode int

const (
	modeList mode = iota
	modeAddVM
	modeNewLink
	modeConfirm
	modeURL
)

type (
	eventMsg    core.Event
	streamEnd   struct{}
	tickMsg     time.Time
	sessionsMsg []core.Session
	vmsMsg      []core.VM
	linksMsg    []core.Link
	errMsg      struct{ err error }
	noteMsg     struct {
		text    string
		refresh bool
	}
	linkMsg struct{ token string }
)

type model struct {
	svc       core.Service
	publicURL string
	events    <-chan core.Event

	width, height int
	tab           int
	mode          mode
	tables        [3]table.Model

	sessions []core.Session
	vms      []core.VM
	links    []core.Link

	inputs  []textinput.Model
	focus   int
	vmPick  int
	confirm string
	onYes   func() tea.Cmd
	url     string
	note    string
	err     string
}

// Run starts the UI and blocks until the user quits.
func Run(svc core.Service, publicURL string) error {
	events, unsub := svc.Subscribe()
	defer unsub()
	_, err := tea.NewProgram(newModel(svc, publicURL, events)).Run()
	return err
}

func newModel(svc core.Service, publicURL string, events <-chan core.Event) *model {
	m := &model{svc: svc, publicURL: strings.TrimRight(publicURL, "/"), events: events, width: 100, height: 30}
	m.tables[tabSessions] = newTable([]table.Column{{Title: "Label", Width: 22}, {Title: "VM", Width: 18}, {Title: "Client IP", Width: 18}, {Title: "Running", Width: 10}})
	m.tables[tabVMs] = newTable([]table.Column{{Title: "ID", Width: 5}, {Title: "Name", Width: 22}, {Title: "Host", Width: 30}, {Title: "Port", Width: 7}})
	m.tables[tabLinks] = newTable([]table.Column{{Title: "ID", Width: 5}, {Title: "Label", Width: 22}, {Title: "VM", Width: 18}, {Title: "Status", Width: 10}, {Title: "Expires", Width: 14}})
	m.resize()
	return m
}

func newTable(cols []table.Column) table.Model {
	t := table.New(table.WithColumns(cols), table.WithFocused(true), table.WithStyles(tableStyles()))
	t.KeyMap = table.KeyMap{
		LineUp:     key.NewBinding(key.WithKeys("up")),
		LineDown:   key.NewBinding(key.WithKeys("down")),
		PageUp:     key.NewBinding(key.WithKeys("pgup")),
		PageDown:   key.NewBinding(key.WithKeys("pgdown")),
		GotoTop:    key.NewBinding(key.WithKeys("home")),
		GotoBottom: key.NewBinding(key.WithKeys("end")),
	}
	return t
}

func (m *model) resize() {
	h := max(m.height-9, 3)
	for i := range m.tables {
		m.tables[i].SetHeight(h)
	}
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(m.waitEvent(), m.refreshAll(), tick())
}

func tick() tea.Cmd {
	return tea.Tick(3*time.Second, func(t time.Time) tea.Msg { return tickMsg(t) })
}

func (m *model) waitEvent() tea.Cmd {
	ch := m.events
	return func() tea.Msg {
		ev, ok := <-ch
		if !ok {
			return streamEnd{}
		}
		return eventMsg(ev)
	}
}

func opCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 15*time.Second)
}

func (m *model) refreshAll() tea.Cmd {
	return tea.Batch(m.loadSessions(), m.loadVMs(), m.loadLinks())
}

func (m *model) loadSessions() tea.Cmd {
	return func() tea.Msg { return sessionsMsg(m.svc.ListSessions()) }
}

func (m *model) loadVMs() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := opCtx()
		defer cancel()
		v, err := m.svc.ListVMs(ctx)
		if err != nil {
			return errMsg{err}
		}
		return vmsMsg(v)
	}
}

func (m *model) loadLinks() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := opCtx()
		defer cancel()
		l, err := m.svc.ListLinks(ctx)
		if err != nil {
			return errMsg{err}
		}
		return linksMsg(l)
	}
}

// op runs fn and reports its outcome, refreshing the lists on success.
func op(okText string, fn func(ctx context.Context) error) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := opCtx()
		defer cancel()
		if err := fn(ctx); err != nil {
			return errMsg{err}
		}
		return noteMsg{okText, true}
	}
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.resize()
	case tickMsg:
		return m, tea.Batch(tick(), m.loadSessions(), m.loadLinks())
	case eventMsg:
		m.applyEvent(core.Event(msg))
		return m, m.waitEvent()
	case streamEnd:
		m.err = "event stream closed, press ctrl+r to refresh"
	case sessionsMsg:
		m.sessions = sortSessions(msg)
		m.syncRows()
	case vmsMsg:
		m.vms = msg
		m.syncRows()
	case linksMsg:
		m.links = msg
		m.syncRows()
	case errMsg:
		m.err, m.note = msg.err.Error(), ""
	case noteMsg:
		m.note, m.err = msg.text, ""
		if msg.refresh {
			return m, m.refreshAll()
		}
	case linkMsg:
		m.url = m.publicURL + "/s/" + msg.token
		m.mode = modeURL
		m.note, m.err = "", ""
		return m, m.loadLinks()
	case tea.KeyPressMsg:
		return m.handleKey(msg)
	case tea.PasteMsg:
		if m.mode == modeAddVM || m.mode == modeNewLink {
			return m, m.updateInput(msg)
		}
	}
	return m, nil
}

func sortSessions(s []core.Session) []core.Session {
	s = append([]core.Session(nil), s...)
	sort.Slice(s, func(i, j int) bool {
		if !s[i].StartedAt.Equal(s[j].StartedAt) {
			return s[i].StartedAt.Before(s[j].StartedAt)
		}
		return s[i].ID < s[j].ID
	})
	return s
}

func (m *model) applyEvent(ev core.Event) {
	m.sessions = removeSession(m.sessions, ev.Session.ID)
	switch ev.Type {
	case "start":
		m.sessions = sortSessions(append(m.sessions, ev.Session))
		m.note = "session started: " + ev.Session.Label
	case "end":
		m.note = fmt.Sprintf("session ended: %s (%s)", ev.Session.Label, ev.Reason)
	}
	m.syncRows()
}

func removeSession(s []core.Session, id string) []core.Session {
	out := make([]core.Session, 0, len(s))
	for _, x := range s {
		if x.ID != id {
			out = append(out, x)
		}
	}
	return out
}

func (m *model) syncRows() {
	now := time.Now()
	rows := make([]table.Row, 0, len(m.sessions))
	for _, s := range m.sessions {
		rows = append(rows, table.Row{s.Label, s.VMName, s.ClientIP, age(now.Sub(s.StartedAt))})
	}
	m.setRows(tabSessions, rows)

	rows = make([]table.Row, 0, len(m.vms))
	for _, v := range m.vms {
		rows = append(rows, table.Row{strconv.FormatInt(v.ID, 10), v.Name, v.Host, strconv.Itoa(v.Port)})
	}
	m.setRows(tabVMs, rows)

	rows = make([]table.Row, 0, len(m.links))
	for _, l := range m.links {
		rows = append(rows, table.Row{strconv.FormatInt(l.ID, 10), l.Label, l.VMName, linkStatus(l, now), l.ExpiresAt.Local().Format("Jan 02 15:04")})
	}
	m.setRows(tabLinks, rows)
}

func (m *model) setRows(tab int, rows []table.Row) {
	t := &m.tables[tab]
	c := t.Cursor()
	t.SetRows(rows)
	t.SetCursor(min(c, len(rows)-1))
}

func linkStatus(l core.Link, now time.Time) string {
	switch {
	case l.Revoked:
		return "revoked"
	case l.Flagged:
		return "flagged"
	case !l.Active(now):
		return "expired"
	}
	return "active"
}

func age(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", max(int(d.Seconds()), 0))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
}

// parseTTL accepts Go durations plus a "d" suffix for days.
func parseTTL(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if n, ok := strings.CutSuffix(s, "d"); ok {
		days, err := strconv.ParseFloat(n, 64)
		if err != nil || days <= 0 {
			return 0, errors.New("invalid TTL, try 4h or 2d")
		}
		return time.Duration(days * float64(24*time.Hour)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, errors.New("invalid TTL, try 4h or 2d")
	}
	return d, nil
}

func (m *model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	k := msg.String()
	if k == "ctrl+c" {
		return m, tea.Quit
	}
	switch m.mode {
	case modeAddVM, modeNewLink:
		return m.formKey(msg)
	case modeConfirm:
		m.mode = modeList
		if k == "y" {
			return m, m.onYes()
		}
		m.note = "cancelled"
		return m, nil
	case modeURL:
		if k == "c" {
			m.note = "copy requested (needs OSC52 terminal support)"
			return m, tea.SetClipboard(m.url)
		}
		m.mode, m.url, m.note = modeList, "", ""
		return m, nil
	}
	return m.listKey(msg)
}

func (m *model) listKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch k := msg.String(); k {
	case "q":
		return m, tea.Quit
	case "tab", "right":
		return m.switchTab((m.tab + 1) % 3)
	case "shift+tab", "left":
		return m.switchTab((m.tab + 2) % 3)
	case "1", "2", "3":
		return m.switchTab(int(k[0] - '1'))
	case "ctrl+r":
		m.err, m.note = "", "refreshing"
		return m, m.refreshAll()
	}
	var cmd tea.Cmd
	switch m.tab {
	case tabSessions:
		cmd = m.sessionsKey(msg.String())
	case tabVMs:
		cmd = m.vmsKey(msg.String())
	case tabLinks:
		cmd = m.linksKey(msg.String())
	}
	if cmd != nil {
		return m, cmd
	}
	var tc tea.Cmd
	m.tables[m.tab], tc = m.tables[m.tab].Update(msg)
	return m, tc
}

func (m *model) switchTab(t int) (tea.Model, tea.Cmd) {
	m.tab, m.err, m.note = t, "", ""
	return m, m.refreshAll()
}

func (m *model) sessionsKey(k string) tea.Cmd {
	i := m.tables[tabSessions].Cursor()
	if i < 0 || i >= len(m.sessions) || !strings.Contains("kfd", k) || len(k) != 1 {
		return nil
	}
	s := m.sessions[i]
	switch k {
	case "k":
		return op("killed "+s.Label, func(context.Context) error { return m.svc.Kill(s.ID, false) })
	case "f":
		return op("flagged "+s.Label, func(context.Context) error { return m.svc.Flag(s.ID) })
	}
	return op("dropped "+s.Label+" (killed and link revoked)", func(context.Context) error { return m.svc.Kill(s.ID, true) })
}

func (m *model) vmsKey(k string) tea.Cmd {
	i := m.tables[tabVMs].Cursor()
	switch k {
	case "a":
		m.openForm(modeAddVM)
		return textinput.Blink
	case "t":
		if i < 0 || i >= len(m.vms) {
			return nil
		}
		vm := m.vms[i]
		m.note, m.err = "testing "+vm.Name+"...", ""
		return op("test passed: "+vm.Name, func(ctx context.Context) error {
			if err := m.svc.TestVM(ctx, vm.ID); err != nil {
				return fmt.Errorf("test failed for %s: %w", vm.Name, err)
			}
			return nil
		})
	case "x":
		if i < 0 || i >= len(m.vms) {
			return nil
		}
		vm := m.vms[i]
		m.ask("Remove VM "+vm.Name+" and revoke its links?", op("removed "+vm.Name, func(ctx context.Context) error { return m.svc.RemoveVM(ctx, vm.ID) }))
	}
	return nil
}

func (m *model) linksKey(k string) tea.Cmd {
	i := m.tables[tabLinks].Cursor()
	switch k {
	case "n":
		if len(m.vms) == 0 {
			m.err = "add a VM first"
			return nil
		}
		m.openForm(modeNewLink)
		return textinput.Blink
	case "r":
		if i < 0 || i >= len(m.links) {
			return nil
		}
		l := m.links[i]
		m.ask("Revoke link "+l.Label+"?", op("revoked "+l.Label, func(ctx context.Context) error { return m.svc.RevokeLink(ctx, l.ID) }))
	}
	return nil
}

func (m *model) ask(prompt string, cmd tea.Cmd) {
	m.mode, m.confirm, m.err, m.note = modeConfirm, prompt, "", ""
	m.onYes = func() tea.Cmd { return cmd }
}

func (m *model) openForm(md mode) {
	m.mode, m.err, m.note, m.focus = md, "", "", 0
	mk := func(ph string, w int) textinput.Model {
		t := textinput.New()
		t.Placeholder = ph
		t.Prompt = ""
		t.SetWidth(w)
		return t
	}
	if md == modeAddVM {
		pw := mk("max 8 chars", 30)
		pw.EchoMode, pw.EchoCharacter = textinput.EchoPassword, '*'
		m.inputs = []textinput.Model{mk("name", 30), mk("host or ip", 30), mk("5900", 30), pw}
		m.inputs[2].SetValue("5900")
	} else {
		m.vmPick = 0
		m.inputs = []textinput.Model{{}, mk("who is this for", 30), mk("4h", 30)}
		m.inputs[2].SetValue("4h")
		m.focus = 1
	}
	m.inputs[m.focus].Focus()
}

func (m *model) setFocus(i int) {
	for j := range m.inputs {
		m.inputs[j].Blur()
	}
	m.focus = (i + len(m.inputs)) % len(m.inputs)
	if m.mode != modeNewLink || m.focus != 0 {
		m.inputs[m.focus].Focus()
	}
}

func (m *model) updateInput(msg tea.Msg) tea.Cmd {
	if m.mode == modeNewLink && m.focus == 0 {
		return nil
	}
	var cmd tea.Cmd
	m.inputs[m.focus], cmd = m.inputs[m.focus].Update(msg)
	return cmd
}

func (m *model) formKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode, m.inputs = modeList, nil
		return m, nil
	case "tab", "down":
		m.setFocus(m.focus + 1)
		return m, textinput.Blink
	case "shift+tab", "up":
		m.setFocus(m.focus - 1)
		return m, textinput.Blink
	case "enter":
		if m.focus < len(m.inputs)-1 {
			m.setFocus(m.focus + 1)
			return m, textinput.Blink
		}
		return m, m.submit()
	case "left", "right":
		if m.mode == modeNewLink && m.focus == 0 && len(m.vms) > 0 {
			d := 1
			if msg.String() == "left" {
				d = len(m.vms) - 1
			}
			m.vmPick = (m.vmPick + d) % len(m.vms)
			return m, nil
		}
	}
	return m, m.updateInput(msg)
}

func (m *model) submit() tea.Cmd {
	v := func(i int) string { return strings.TrimSpace(m.inputs[i].Value()) }
	if m.mode == modeAddVM {
		name, host, pw := v(0), v(1), m.inputs[3].Value()
		port, err := strconv.Atoi(v(2))
		switch {
		case name == "" || host == "":
			m.err = "name and host are required"
			return nil
		case err != nil:
			m.err = "port must be a number"
			return nil
		}
		m.mode, m.inputs = modeList, nil
		return op("added VM "+name, func(ctx context.Context) error {
			_, err := m.svc.AddVM(ctx, name, host, port, pw)
			return err
		})
	}
	ttl, err := parseTTL(v(2))
	if err != nil {
		m.err = err.Error()
		return nil
	}
	vm, label := m.vms[m.vmPick], v(1)
	m.mode, m.inputs = modeList, nil
	return func() tea.Msg {
		ctx, cancel := opCtx()
		defer cancel()
		_, tok, err := m.svc.CreateLink(ctx, vm.ID, label, ttl)
		if err != nil {
			return errMsg{err}
		}
		return linkMsg{tok}
	}
}
