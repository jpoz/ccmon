package main

import (
	"fmt"
	"path/filepath"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// gather merges persisted instances (rich state from hooks) with a live tmux
// scan (to discover codex panes that haven't fired an event yet), prunes panes
// that have closed, and returns the list sorted by urgency.
func gather() []*Instance {
	byKey := map[string]*Instance{}
	for _, inst := range loadAll() {
		if inst.PaneID != "" && !paneExists(inst.Socket, inst.PaneID) {
			removeInstance(inst.ID) // pane closed → forget it
			continue
		}
		byKey[inst.Socket+"|"+inst.PaneID] = inst
	}
	for _, p := range scanPanes() {
		if !isCodexCommand(p.command) {
			continue
		}
		key := p.socket + "|" + p.paneID
		if ex, ok := byKey[key]; ok {
			// Codex emits no "started" event, so infer renewed activity from
			// tmux: output newer than the last known state means it's working
			// again. (Claude reports its own state, so leave it alone.)
			if ex.Source == "codex" && ex.State == StateDone && p.activity > ex.Since+3 {
				ex.State = StateWorking
				ex.Since = p.activity
			}
			continue // already have a richer, event-backed record
		}
		st := StateWorking
		if now()-p.activity > 30 {
			st = StateIdle
		}
		byKey[key] = &Instance{
			ID:      "cdx-" + filepath.Base(p.socket) + "-" + strings.TrimPrefix(p.paneID, "%"),
			Source:  "codex",
			Project: filepath.Base(p.path),
			Cwd:     p.path,
			State:   st,
			Since:   p.activity,
			Socket:  p.socket,
			Session: p.session,
			Window:  p.window,
			WinName: p.winName,
			PaneID:  p.paneID,
			inferred: true,
		}
	}
	out := make([]*Instance, 0, len(byKey))
	for _, v := range byKey {
		out = append(out, v)
	}
	sortInstances(out)
	return out
}

type tickMsg struct{}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return tickMsg{} })
}

type model struct {
	rows   []*Instance
	cur    int
	w, h   int
	status string
}

func (m model) Init() tea.Cmd { return tick() }

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
	case tickMsg:
		m.rows = gather()
		if m.cur >= len(m.rows) {
			m.cur = max(0, len(m.rows)-1)
		}
		return m, tick()
	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case "up", "k":
			if m.cur > 0 {
				m.cur--
			}
		case "down", "j":
			if m.cur < len(m.rows)-1 {
				m.cur++
			}
		case "g":
			m.cur = 0
		case "G":
			m.cur = len(m.rows) - 1
		case "enter":
			if m.cur < len(m.rows) {
				inst := m.rows[m.cur]
				if err := jumpTo(inst); err != nil {
					m.status = "jump failed: " + err.Error()
				} else {
					m.status = "→ " + label(inst)
				}
				m.rows = gather()
			}
		case "c": // acknowledge: clear the alert
			if m.cur < len(m.rows) {
				inst := m.rows[m.cur]
				inst.setState(StateIdle)
				_ = inst.save()
				tagPane(inst)
				m.rows = gather()
			}
		case "x": // forget this instance
			if m.cur < len(m.rows) {
				removeInstance(m.rows[m.cur].ID)
				m.rows = gather()
			}
		case "r":
			m.rows = gather()
		}
	}
	return m, nil
}

// ---- styling (Nord palette, to match the user's ghostty/tmux theme) ----

var (
	cRed    = lipgloss.Color("#bf616a")
	cGreen  = lipgloss.Color("#a3be8c")
	cYellow = lipgloss.Color("#ebcb8b")
	cGray   = lipgloss.Color("#6c7689")
	cFg     = lipgloss.Color("#eceff4")
	cDim    = lipgloss.Color("#8893a5")
	cSelBg  = lipgloss.Color("#434c5e")
	cBarBg  = lipgloss.Color("#3b4252")

	headStyle = lipgloss.NewStyle().Bold(true).Foreground(cFg).Background(cBarBg).Padding(0, 1)
	footStyle = lipgloss.NewStyle().Foreground(cDim).Padding(0, 1)
	selStyle  = lipgloss.NewStyle().Background(cSelBg).Bold(true)
)

func stateColor(s string) lipgloss.Color {
	switch s {
	case StateNeedsInput:
		return cRed
	case StateDone:
		return cGreen
	case StateWorking:
		return cYellow
	default:
		return cGray
	}
}

func stateIcon(s string) string {
	switch s {
	case StateNeedsInput:
		return "🔴"
	case StateDone:
		return "🟢"
	case StateWorking:
		return "🟡"
	default:
		return "⚪"
	}
}

func label(i *Instance) string { return i.Source + ":" + i.Project }

func dur(since int64) string {
	d := now() - since
	if d < 0 {
		d = 0
	}
	switch {
	case d < 60:
		return fmt.Sprintf("%ds", d)
	case d < 3600:
		return fmt.Sprintf("%dm%02ds", d/60, d%60)
	default:
		return fmt.Sprintf("%dh%02dm", d/3600, (d%3600)/60)
	}
}

func (m model) View() string {
	var counts [4]int
	for _, r := range m.rows {
		counts[statePriority(r.State)]++
	}
	head := headStyle.Render(fmt.Sprintf(
		" CC MISSION CONTROL   🔴 %d need input   🟢 %d done   🟡 %d working   ⚪ %d idle ",
		counts[0], counts[1], counts[2], counts[3]))

	var b strings.Builder
	b.WriteString(head + "\n\n")

	if len(m.rows) == 0 {
		b.WriteString(footStyle.Render("  no claude/codex instances detected\n"))
	}

	width := m.w
	if width == 0 {
		width = 100
	}
	for idx, r := range m.rows {
		marker := "  "
		if idx == m.cur {
			marker = "▌ "
		}
		tool := strings.ToUpper(r.Source[:1]) + r.Source[1:]
		inferred := ""
		if r.inferred {
			inferred = " ~"
		}
		row := stateIcon(r.State) + marker +
			padRight(tool, 5) + " " +
			padRight(truncate(r.Project, 12), 12) + " " +
			padRight(dur(r.Since), 8) + " " +
			truncate(r.Msg, max(10, width-42)) + inferred
		if idx == m.cur {
			row = selStyle.Render(padRight(row, width-1))
		} else {
			row = lipgloss.NewStyle().Foreground(stateColor(r.State)).Render(row)
		}
		b.WriteString(row + "\n")
	}

	b.WriteString("\n")
	foot := "↑/↓ move · enter jump · c ack · x forget · r refresh · q quit"
	if m.status != "" {
		foot = m.status + "   " + foot
	}
	b.WriteString(footStyle.Render(foot))
	return b.String()
}

func padRight(s string, n int) string {
	r := []rune(s)
	if len(r) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(r))
}

func runTUI() {
	p := tea.NewProgram(model{rows: gather()}, tea.WithAltScreen())
	_, _ = p.Run()
}

func runList() {
	for _, r := range gather() {
		fmt.Printf("%-12s %-6s %-12s %-8s %s\n", r.State, r.Source, r.Project, dur(r.Since), r.Msg)
	}
}
