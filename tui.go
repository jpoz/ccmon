package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
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
		// Self-heal: trust the live pane over possibly-stale hook state. This is
		// what clears a session stuck on needs-input after you grant permission.
		if changed, prev := reconcileClaude(inst); changed &&
			inst.State == StateNeedsInput && prev != StateNeedsInput {
			// A permission box is up that no hook told us about — alert as if the
			// Notification hook had fired.
			notify(inst, StateNeedsInput)
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
			// again. This also revives a codex pane the user attended (idle) —
			// without it, a jumped-to codex would sit idle through its next turn
			// until completion. (Claude reports its own state, so leave it alone.)
			if ex.Source == "codex" && p.activity > ex.Since+3 &&
				(ex.State == StateDone || (ex.State == StateIdle && ex.Attended)) {
				ex.State = StateWorking
				ex.Since = p.activity
				ex.Attended = false
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

// tickInterval drives both the spinner animation and (every framesPerGather
// ticks) the data refresh. A sub-second tick keeps the "working" spinner
// smooth without scanning tmux any more often than once a second.
const (
	tickInterval    = 125 * time.Millisecond
	framesPerGather = 8 // 8 * 125ms ≈ 1s between gathers
)

func tick() tea.Cmd {
	return tea.Tick(tickInterval, func(time.Time) tea.Msg { return tickMsg{} })
}

// nagInterval is how often a still-red (needs-input) session is re-notified
// while the TUI is running. Override with CCMON_NAG_SECS.
func nagInterval() int64 {
	if v := os.Getenv("CCMON_NAG_SECS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return int64(n)
		}
	}
	return 60
}

type model struct {
	rows   []*Instance
	cur    int
	w, h   int
	status string
	frame  int                 // animation frame counter (bumped every tick)
	nag    map[string]int64    // instance id -> unix secs of last notification
	events        []Event             // activity feed, oldest first (see feed.go)
	prev          map[string]instSnap // last-seen state per instance, for diffing
	feed          bool                // whether the activity-feed panel is shown
	feedSeq       int64               // monotonic event id counter
	feedBottomSeq int64               // anchored bottom event when scrolled; 0 = follow tail
}

func (m model) Init() tea.Cmd { return tick() }

// checkNags re-fires a reminder for every session that's been stuck in
// needs-input for another nagInterval. The Since timestamp anchors the first
// reminder one interval after the session went red (the hook already sent the
// initial banner). Entries are dropped once a session leaves needs-input or its
// pane disappears, so nagging stops the moment you attend to it.
func (m model) checkNags() {
	present := map[string]bool{}
	for _, r := range m.rows {
		present[r.ID] = true
		if r.State != StateNeedsInput {
			delete(m.nag, r.ID)
			continue
		}
		if _, seen := m.nag[r.ID]; !seen {
			m.nag[r.ID] = r.Since
		}
		if now()-m.nag[r.ID] >= nagInterval() {
			renotify(r)
			m.nag[r.ID] = now()
		}
	}
	for id := range m.nag {
		if !present[id] {
			delete(m.nag, id)
		}
	}
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.w, m.h = msg.Width, msg.Height
	case tickMsg:
		m.frame++
		if m.frame%framesPerGather == 0 {
			m.refresh()
			m.checkNags()
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
				m.refresh()
			}
		case "c": // acknowledge: clear the alert + dismiss its banner
			if m.cur < len(m.rows) {
				inst := m.rows[m.cur]
				inst.setState(StateIdle)
				inst.Attended = true // seen → don't let reconcile re-green it
				_ = inst.save()
				tagPane(inst)
				clearNotification(inst.ID)
				delete(m.nag, inst.ID)
				m.refresh()
			}
		case "x": // forget this instance
			if m.cur < len(m.rows) {
				removeInstance(m.rows[m.cur].ID)
				m.refresh()
			}
		case "f": // toggle the activity-feed panel
			m.feed = !m.feed
			m.feedBottomSeq = 0 // (re)open streaming live
		case "pgup", "ctrl+u": // scroll the feed toward older events
			m.scrollFeed(-1)
		case "pgdown", "ctrl+d": // scroll back toward the live tail
			m.scrollFeed(1)
		case "r":
			m.refresh()
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
	cAccent = lipgloss.Color("#88c0d0") // frost blue: title + selection cursor
	cSelBg  = lipgloss.Color("#434c5e")
	cBarBg  = lipgloss.Color("#3b4252")
)

// spinFrames is a monochrome braille spinner used for the working state. It's
// just text, so it colors like any other glyph (no emoji).
var spinFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

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

// glyph returns a single monochrome cell for a state (colored by the caller).
// Filled vs hollow distinguishes active from idle even without color, and
// "working" animates so a live session reads as alive rather than just yellow.
func (m model) glyph(s string) string {
	switch s {
	case StateNeedsInput:
		return "●"
	case StateDone:
		return "●"
	case StateWorking:
		return spinFrames[(m.frame/2)%len(spinFrames)]
	default:
		return "○"
	}
}

// fg paints s with a foreground color and nothing else.
func fg(c lipgloss.Color, s string) string {
	return lipgloss.NewStyle().Foreground(c).Render(s)
}

// onBar paints s with the header/footer bar background so adjacent segments
// keep the bar filled even across the ANSI resets between colored runs.
func onBar(c lipgloss.Color, bold bool, s string) string {
	return lipgloss.NewStyle().Background(cBarBg).Foreground(c).Bold(bold).Render(s)
}

func label(i *Instance) string { return i.Source + ":" + i.Project }

func dur(since int64) string {
	d := max(now()-since, 0)
	switch {
	case d < 60:
		return fmt.Sprintf("%ds", d)
	case d < 3600:
		return fmt.Sprintf("%dm%02ds", d/60, d%60)
	default:
		return fmt.Sprintf("%dh%02dm", d/3600, (d%3600)/60)
	}
}

// column widths for the body table; the column header lines up with these.
const (
	colTool = 6
	colProj = 16
	colAge  = 7

	// maxContentWidth caps how wide the panel grows. Past this the table stops
	// stretching and gets centered, so it reads as a card on a big monitor
	// instead of smearing edge-to-edge; narrower terminals still use full width.
	maxContentWidth = 100
)

func (m model) View() string {
	termW, termH := m.w, m.h
	if termW == 0 {
		termW = 100
	}
	if termH == 0 {
		termH = 24
	}
	if side, tableW, feedW, rows := m.feedLayout(); m.feed && side {
		return m.viewSide(termW, tableW, feedW, rows)
	}
	return m.viewStacked(termW, termH)
}

// viewStacked is the default layout: a horizontally centered card with the
// table on top and — when the feed is open and the terminal is too narrow for a
// side column — a feed strip pinned above the footer.
func (m model) viewStacked(termW, termH int) string {
	width := min(termW, maxContentWidth)
	indent := strings.Repeat(" ", max((termW-width)/2, 0))

	var feedLines []string
	if m.feed {
		feedLines = m.renderFeed(width, feedMaxLines)
	}
	// Keep the footer pinned: reserve header, blank, col-header, blank, footer
	// (5 lines) plus the feed strip; the table gets what's left.
	body := m.renderBody(width, max(termH-5-len(feedLines), 1))

	var b strings.Builder
	b.WriteString(indent + m.headerBar(width) + "\n\n")
	b.WriteString(indent + colHeadLine() + "\n")
	for _, line := range body {
		b.WriteString(indent + line + "\n")
	}
	if fill := termH - 4 - len(body) - len(feedLines); fill > 0 {
		b.WriteString(strings.Repeat("\n", fill))
	}
	for _, line := range feedLines {
		b.WriteString(indent + line + "\n")
	}
	b.WriteString(indent + m.footerBar(width))
	return b.String()
}

// viewSide puts the feed in a full-height column on the right, with the table on
// the left. The header and footer bars span both columns, and the whole pair is
// centered as a card so it doesn't smear edge-to-edge on a wide monitor.
func (m model) viewSide(termW, tableW, feedW, feedRows int) string {
	cardW := tableW + sideGutter + feedW
	indent := strings.Repeat(" ", max((termW-cardW)/2, 0))
	// Both columns fill the region between the header (2 lines) and footer (1).
	// The feed's title and the table's column header each take one of those rows,
	// so the table gets feedRows data rows just like the feed gets feedRows events.
	regionH := feedRows + 1

	// Left column: column header + table rows, padded to the region height.
	left := append([]string{colHeadLine()}, m.renderBody(tableW, feedRows)...)
	// Right column: the feed panel, filling the same height.
	right := m.renderFeed(feedW, feedRows)

	gutter := strings.Repeat(" ", sideGutter)
	var mid strings.Builder
	for i := range regionH {
		var l, r string
		if i < len(left) {
			l = left[i]
		}
		if i < len(right) {
			r = right[i]
		}
		if pad := tableW - lipgloss.Width(l); pad > 0 {
			l += strings.Repeat(" ", pad)
		}
		mid.WriteString(indent + l + gutter + r + "\n")
	}
	return indent + m.headerBar(cardW) + "\n\n" + mid.String() + indent + m.footerBar(cardW)
}

// headerBar renders the full-width title + state-count summary.
func (m model) headerBar(width int) string {
	var counts [4]int
	for _, r := range m.rows {
		counts[statePriority(r.State)]++
	}
	title := onBar(cAccent, true, " ▎ CC MISSION CONTROL ")
	summary := onBar(cRed, false, "● ") + onBar(cFg, false, fmt.Sprintf("%d need   ", counts[0])) +
		onBar(cGreen, false, "● ") + onBar(cFg, false, fmt.Sprintf("%d done   ", counts[1])) +
		onBar(cYellow, false, "● ") + onBar(cFg, false, fmt.Sprintf("%d working   ", counts[2])) +
		onBar(cGray, false, "○ ") + onBar(cFg, false, fmt.Sprintf("%d idle ", counts[3]))
	gap := max(width-lipgloss.Width(title)-lipgloss.Width(summary), 1)
	return title + onBar(cFg, false, strings.Repeat(" ", gap)) + summary
}

// footerBar renders the full-width key hints (with the current status, if any).
// The hint set swaps to surface feed scrolling while the feed is open.
func (m model) footerBar(width int) string {
	keys := "↑/↓ move · enter jump · c ack · x forget · f feed · r refresh · q quit"
	if m.feed {
		keys = "↑/↓ move · enter jump · c ack · f feed · PgUp/PgDn scroll · q quit"
	}
	var footer string
	if m.status != "" {
		footer = onBar(cAccent, true, " "+m.status+"  ") + onBar(cDim, false, keys+" ")
	} else {
		footer = onBar(cDim, false, " "+keys+" ")
	}
	if fgap := width - lipgloss.Width(footer); fgap > 0 {
		footer += onBar(cDim, false, strings.Repeat(" ", fgap))
	}
	return footer
}

// colHeadLine is the dim table column header.
func colHeadLine() string {
	return fg(cDim, "    "+padRight("TOOL", colTool+1)+
		padRight("PROJECT", colProj+1)+padRight("AGE", colAge+1)+"MESSAGE")
}

// renderBody formats the table rows for a column `width` wide, limited to
// `maxRows`; if there are more instances than fit it surfaces the dropped count
// rather than silently truncating.
func (m model) renderBody(width, maxRows int) []string {
	if len(m.rows) == 0 {
		return []string{"", fg(cDim,
			"    no claude or codex instances detected — start one and it'll show up here")}
	}
	rows, overflow := m.rows, 0
	if len(rows) > maxRows {
		overflow = len(rows) - (maxRows - 1)
		rows = rows[:maxRows-1]
	}
	body := make([]string, 0, len(rows)+1)
	for idx, r := range rows {
		body = append(body, m.renderRow(idx, r, width))
	}
	if overflow > 0 {
		body = append(body, fg(cDim, fmt.Sprintf("    … and %d more", overflow)))
	}
	return body
}

// renderRow formats one instance as a single padded line. The state glyph is
// always tinted with the state color; on the selected row everything sits on a
// highlight background (each segment carries that background so the bar fills
// cleanly across the ANSI resets between colored runs).
func (m model) renderRow(idx int, r *Instance, width int) string {
	sel := idx == m.cur
	sc := stateColor(r.State)
	glyph := m.glyph(r.State)
	tool := padRight(titleCase(r.Source), colTool)
	proj := padRight(truncate(r.Project, colProj), colProj)
	age := padRight(dur(r.Since), colAge)

	meta := "" // inferred (scan-discovered, no event yet) gets a subtle marker
	if r.inferred {
		meta = " ~"
	}
	fixed := 2 + 1 + 1 + colTool + 1 + colProj + 1 + colAge + 1
	msgMax := max(width-fixed-1-len([]rune(meta)), 8)
	msg := truncate(r.Msg, msgMax)

	if sel {
		seg := func(c lipgloss.Color, bold bool, s string) string {
			return lipgloss.NewStyle().Background(cSelBg).Foreground(c).Bold(bold).Render(s)
		}
		line := seg(cAccent, false, "▌ ") + seg(sc, false, glyph+" ") +
			seg(cFg, true, tool+" ") + seg(cFg, true, proj+" ") +
			seg(cDim, false, age+" ") + seg(cFg, false, msg) + seg(cDim, false, meta)
		used := 2 + 2 + colTool + 1 + colProj + 1 + colAge + 1 + len([]rune(msg)) + len([]rune(meta))
		if pad := width - used; pad > 0 {
			line += lipgloss.NewStyle().Background(cSelBg).Render(strings.Repeat(" ", pad))
		}
		return line
	}
	return "  " + fg(sc, glyph+" ") + fg(cDim, tool+" ") + fg(cFg, proj+" ") +
		fg(cDim, age+" ") + fg(cDim, msg) + fg(cDim, meta)
}

func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func padRight(s string, n int) string {
	r := []rune(s)
	if len(r) >= n {
		return s
	}
	return s + strings.Repeat(" ", n-len(r))
}

func runTUI() {
	rows := gather()
	m := model{rows: rows, nag: map[string]int64{}, prev: seedSnaps(rows)}
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, _ = p.Run()
}

func runList() {
	for _, r := range gather() {
		fmt.Printf("%-12s %-6s %-12s %-8s %s\n", r.State, r.Source, r.Project, dur(r.Since), r.Msg)
	}
}
