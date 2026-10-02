package tui

import (
	"strings"

	"charm.land/bubbles/v2/table"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

var (
	accent = lipgloss.Color("#7C5CFF")
	dim    = lipgloss.Color("#7A7A8C")
	red    = lipgloss.Color("#FF5F6D")
	green  = lipgloss.Color("#4ADE80")

	titleStyle = lipgloss.NewStyle().Bold(true).Foreground(accent)
	tabOn      = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FFFFFF")).Background(accent).Padding(0, 2)
	tabOff     = lipgloss.NewStyle().Foreground(dim).Padding(0, 2)
	dimStyle   = lipgloss.NewStyle().Foreground(dim)
	errStyle   = lipgloss.NewStyle().Foreground(red).Bold(true)
	noteStyle  = lipgloss.NewStyle().Foreground(green)
	boxStyle   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(accent).Padding(1, 2)
	labelStyle = lipgloss.NewStyle().Foreground(dim).Width(10)
	warnStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FFC857"))
	keyStyle   = lipgloss.NewStyle().Bold(true).Foreground(accent)
)

func tableStyles() table.Styles {
	return table.Styles{
		Header:   lipgloss.NewStyle().Bold(true).Foreground(dim).Padding(0, 1).BorderStyle(lipgloss.NormalBorder()).BorderBottom(true).BorderForeground(dim),
		Cell:     lipgloss.NewStyle().Padding(0, 1),
		Selected: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#FFFFFF")).Background(lipgloss.Color("#3B3566")),
	}
}

func (m *model) View() tea.View {
	v := tea.NewView(m.render())
	v.AltScreen = true
	v.WindowTitle = "quiver playtesting"
	return v
}

func (m *model) render() string {
	var tabs []string
	for i, n := range tabNames {
		if i == m.tab {
			tabs = append(tabs, tabOn.Render(n))
		} else {
			tabs = append(tabs, tabOff.Render(n))
		}
	}
	head := titleStyle.Render("quiver playtesting") + "  " + lipgloss.JoinHorizontal(lipgloss.Top, tabs...)

	var body string
	switch m.mode {
	case modeAddVM:
		body = m.addVMView()
	case modeNewLink:
		body = m.newLinkView()
	case modeURL:
		body = boxStyle.Render(titleStyle.Render("Link created") + "\n" +
			dimStyle.Render("Shown once. It cannot be recovered later.") + "\n\n" + m.url)
	default:
		body = m.tables[m.tab].View()
		if len(m.tables[m.tab].Rows()) == 0 {
			body += "\n" + dimStyle.Render("  nothing here yet")
		}
	}

	var status string
	switch {
	case m.mode == modeConfirm:
		status = warnStyle.Render(m.confirm + " (y/N)")
	case m.err != "":
		status = errStyle.Render("error: " + m.err)
	case m.note != "":
		status = noteStyle.Render(m.note)
	}
	return strings.Join([]string{head, "", body, "", status, m.footer()}, "\n")
}

func (m *model) footer() string {
	var hints [][2]string
	switch {
	case m.mode == modeAddVM || m.mode == modeNewLink:
		hints = [][2]string{{"tab", "next"}, {"enter", "next/submit"}, {"esc", "cancel"}}
		if m.mode == modeNewLink {
			hints = append([][2]string{{"left/right", "pick VM"}}, hints...)
		}
	case m.mode == modeConfirm:
		hints = [][2]string{{"y", "confirm"}, {"any", "cancel"}}
	case m.mode == modeURL:
		hints = [][2]string{{"c", "copy"}, {"any", "close"}}
	default:
		hints = [][2]string{{"tab", "switch"}, {"up/down", "move"}}
		switch m.tab {
		case tabSessions:
			hints = append(hints, [2]string{"k", "kill"}, [2]string{"f", "flag"}, [2]string{"d", "drop"})
		case tabVMs:
			hints = append(hints, [2]string{"a", "add"}, [2]string{"t", "test"}, [2]string{"x", "remove"})
		case tabLinks:
			hints = append(hints, [2]string{"n", "new"}, [2]string{"r", "revoke"})
		}
		hints = append(hints, [2]string{"ctrl+r", "refresh"}, [2]string{"q", "quit"})
	}
	parts := make([]string, len(hints))
	for i, h := range hints {
		parts[i] = keyStyle.Render(h[0]) + " " + dimStyle.Render(h[1])
	}
	return strings.Join(parts, dimStyle.Render("  |  "))
}

func (m *model) field(i int, label string) string {
	return labelStyle.Render(label) + m.inputs[i].View()
}

func (m *model) addVMView() string {
	rows := []string{titleStyle.Render("Add VM"), "",
		m.field(0, "Name"), m.field(1, "Host"), m.field(2, "Port"), m.field(3, "Password")}
	return boxStyle.Render(strings.Join(rows, "\n"))
}

func (m *model) newLinkView() string {
	pick := dimStyle.Render("< ") + m.vms[m.vmPick].Name + dimStyle.Render(" >")
	if m.focus == 0 {
		pick = keyStyle.Render("< ") + lipgloss.NewStyle().Bold(true).Render(m.vms[m.vmPick].Name) + keyStyle.Render(" >")
	}
	rows := []string{titleStyle.Render("New link"), "",
		labelStyle.Render("VM") + pick, m.field(1, "Label"), m.field(2, "TTL")}
	return boxStyle.Render(strings.Join(rows, "\n"))
}
