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
	panes := scanPanes()
	paneByKey := make(map[string]paneInfo, len(panes))
	for _, p := range panes {
		paneByKey[p.socket+"|"+p.paneID] = p
	}

	byKey := map[string]*Instance{}
	for _, inst := range loadAll() {
		if inst.PaneID != "" && !paneExists(inst.Socket, inst.PaneID) {
			removeInstance(inst.ID) // pane closed → forget it
			continue
		}
		// Self-heal: trust the live pane over possibly-stale hook state. For
		// Claude this is what clears a session stuck on needs-input after you
		// grant permission; for codex it's the only mid-turn signal there is —
		// a new turn starting, an approval box, or a stale "working" (codex
		// fires no event for any of those, only the end-of-turn notify).
		var changed bool
		var prev string
		switch inst.Source {
		case "claude":
			changed, prev = reconcileClaude(inst)
		case "codex":
			changed, prev = reconcileCodex(inst, paneByKey[inst.Socket+"|"+inst.PaneID].activity)
		}
		if changed && inst.State == StateNeedsInput && prev != StateNeedsInput {
			// A permission box is up that no hook told us about — alert as if the
			// Notification hook had fired.
			notify(inst, StateNeedsInput)
		}
		byKey[inst.Socket+"|"+inst.PaneID] = inst
	}
	for _, p := range panes {
		if !isCodexCommand(p.command) {
			continue
		}
		key := p.socket + "|" + p.paneID
		if _, ok := byKey[key]; ok {
			continue // already have a richer, event-backed record
		}
		// First sighting, no event yet: read the pane for a state; if it shows
		// nothing definitive (chrome only), an event-less codex is just idle.
		// Fall back to recency of output when even the chrome isn't legible.
		st := StateWorking
		if now()-p.activity > 30 {
			st = StateIdle
		}
		if text, ok := capturePane(p.socket, p.paneID); ok {
			if s, ok := classifyCodexPane(text); ok {
				st = s
			} else if reCodexChrome.MatchString(text) {
				st = StateIdle
			}
		}
		byKey[key] = &Instance{
			ID:       "cdx-" + filepath.Base(p.socket) + "-" + strings.TrimPrefix(p.paneID, "%"),
			Source:   "codex",
			Project:  filepath.Base(p.path),
			Cwd:      p.path,
			State:    st,
			Since:    p.activity,
			Socket:   p.socket,
			Session:  p.session,
			Window:   p.window,
			WinName:  p.winName,
			PaneID:   p.paneID,
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
	rows          []*Instance
	cur           int
	w, h          int
	status        string
	frame         int                 // animation frame counter (bumped every tick)
	nag           map[string]int64    // instance id -> unix secs of last notification
	events        []Event             // activity feed, oldest first (see feed.go)
	prev          map[string]instSnap // last-seen state per instance, for diffing
	feed          bool                // whether the activity-feed panel is shown
	feedSeq       int64               // monotonic event id counter
	feedBottomSeq int64               // anchored bottom event when scrolled; 0 = follow tail
	muted         bool                // notification sounds silenced (mirrors the ~/.ccmon/muted flag)
	backend       string              // notification backend (mirrors ~/.ccmon/notify-backend)
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
				if err := jumpTo(inst, true); err != nil {
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
		case "m": // toggle mute: silence notification sounds (banners still show)
			m.muted = !m.muted
			_ = setMuted(m.muted)
			if m.muted {
				m.status = "muted — sounds off"
			} else {
				m.status = "unmuted"
			}
		case "n": // toggle notification backend: terminal-notifier ↔ OSC-777
			if m.backend == backendOSC777 {
				m.backend = backendTerminalNotifier
			} else {
				m.backend = backendOSC777
			}
			_ = setNotifyBackend(m.backend)
			m.status = "notify via " + backendLabel(m.backend)
		case "f": // toggle the activity-feed panel
			m.feed = !m.feed
			m.feedBottomSeq = 0 // (re)open streaming live
		case "pgup", "ctrl+u": // newest is at the top → scroll up toward the live tail
			m.scrollFeed(1)
		case "pgdown", "ctrl+d": // scroll down into older history
			m.scrollFeed(-1)
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

	// narrowWidth is the content width below which a single-line row leaves too
	// little room for the message, so rows wrap to two lines: tool/project/age on
	// top, the message indented beneath. Tuned so the wrap kicks in only on a
	// genuinely cramped split, not a merely smallish terminal.
	narrowWidth = 60

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
// side column — the activity panel filling the gap down to the footer.
func (m model) viewStacked(termW, termH int) string {
	width := min(termW, maxContentWidth)
	indent := strings.Repeat(" ", max((termW-width)/2, 0))

	// Region between the col-header and the footer (header, blank, col-header,
	// footer = 4 fixed lines) is shared by the table and, when open, the feed.
	region := max(termH-4, 1)

	var body, feedLines []string
	if m.feed {
		// The table takes only the rows it needs; the activity panel grows to
		// fill the rest of the region (feedLayout computes the matching split so
		// the scroll handler agrees on the visible window).
		_, _, _, feedRows := m.feedLayout()
		body = m.renderBody(width, max(region-feedRows-1, 1))
		feedLines = m.renderFeed(width, feedRows)
	} else {
		body = m.renderBody(width, region)
	}

	var b strings.Builder
	b.WriteString(indent + m.headerBar(width) + "\n\n")
	b.WriteString(indent + colHeadLine(width) + "\n")
	for _, line := range body {
		b.WriteString(indent + line + "\n")
	}
	if fill := region - len(body) - len(feedLines); fill > 0 {
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
	left := append([]string{colHeadLine(tableW)}, m.renderBody(tableW, feedRows)...)
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
	summary := ""
	if m.backend == backendOSC777 {
		summary += onBar(cAccent, true, "OSC-777  ")
	}
	if m.muted {
		summary += onBar(cYellow, true, "⊘ MUTED  ")
	}
	summary += onBar(cRed, false, "● ") + onBar(cFg, false, fmt.Sprintf("%d need   ", counts[0])) +
		onBar(cGreen, false, "● ") + onBar(cFg, false, fmt.Sprintf("%d done   ", counts[1])) +
		onBar(cYellow, false, "● ") + onBar(cFg, false, fmt.Sprintf("%d working   ", counts[2])) +
		onBar(cGray, false, "○ ") + onBar(cFg, false, fmt.Sprintf("%d idle ", counts[3]))
	gap := max(width-lipgloss.Width(title)-lipgloss.Width(summary), 1)
	return title + onBar(cFg, false, strings.Repeat(" ", gap)) + summary
}

// footerBar renders the full-width key hints (with the current status, if any).
// The hint set swaps to surface feed scrolling while the feed is open.
func (m model) footerBar(width int) string {
	mute := "m mute"
	if m.muted {
		mute = "m unmute"
	}
	backend := "n osc-777" // pressing n switches to the other backend
	if m.backend == backendOSC777 {
		backend = "n notifier"
	}
	keys := "↑/↓ move · enter jump · c ack · x forget · f feed · " + mute + " · " + backend + " · q quit"
	if m.feed {
		keys = "↑/↓ move · enter jump · c ack · f feed · scroll PgUp/PgDn · " + mute + " · " + backend + " · q quit"
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

// colHeadLine is the dim table column header. In narrow mode the message moves
// to its own row per instance, so the header drops the MESSAGE column to match.
func colHeadLine(width int) string {
	if width < narrowWidth {
		return fg(cDim, "    "+padRight("TOOL", colTool+1)+padRight("PROJECT", colProj+1)+"AGE")
	}
	return fg(cDim, "    "+padRight("TOOL", colTool+1)+
		padRight("PROJECT", colProj+1)+padRight("AGE", colAge+1)+"MESSAGE")
}

// packBody works out how the table rows fit into a `maxRows` line budget. In
// narrow mode a row with a message takes two lines and a message-less row takes
// one; in wide mode every row is a single line. When not everything fits it
// reserves a line for the "… and N more" indicator. It returns how many leading
// rows to show, how many are dropped, and the total lines the body occupies.
// renderBody and bodyLineCount share it so the feed-split math agrees with what
// is actually drawn.
func packBody(rows []*Instance, maxRows int, narrow bool) (shown, overflow, lines int) {
	height := func(r *Instance) int {
		if narrow && r.Msg != "" {
			return 2
		}
		return 1
	}
	total := 0
	for _, r := range rows {
		total += height(r)
	}
	if total <= maxRows {
		return len(rows), 0, total
	}
	budget := max(maxRows-1, 0) // reserve a line for "… and N more"
	used := 0
	for _, r := range rows {
		h := height(r)
		if used+h > budget {
			break
		}
		used += h
		shown++
	}
	return shown, len(rows) - shown, used + 1
}

// renderBody formats the table rows for a column `width` wide, limited to
// `maxRows` lines; if there are more instances than fit it surfaces the dropped
// count rather than silently truncating. When `width` is below narrowWidth a row
// wraps to a second line for its message (message-less rows stay one line).
func (m model) renderBody(width, maxRows int) []string {
	if len(m.rows) == 0 {
		return []string{"", fg(cDim,
			"    no claude or codex instances detected — start one and it'll show up here")}
	}
	narrow := width < narrowWidth
	shown, overflow, _ := packBody(m.rows, maxRows, narrow)
	body := make([]string, 0, maxRows)
	for idx, r := range m.rows[:shown] {
		body = append(body, m.renderRow(idx, r, width, narrow)...)
	}
	if overflow > 0 {
		body = append(body, fg(cDim, fmt.Sprintf("    … and %d more", overflow)))
	}
	return body
}

// bodyLineCount reports how many terminal lines renderBody will occupy for the
// current rows at `width`, capped at `maxRows`. It mirrors renderBody's packing
// so feedLayout can split the screen without rendering — both View and the
// scroll handler must agree on the feed's visible window.
func (m model) bodyLineCount(width, maxRows int) int {
	if len(m.rows) == 0 {
		return min(2, maxRows)
	}
	_, _, lines := packBody(m.rows, maxRows, width < narrowWidth)
	return lines
}

// renderRow formats one instance, returning the one or two lines it occupies.
// In narrow mode the message wraps to its own line beneath tool/project/age.
func (m model) renderRow(idx int, r *Instance, width int, narrow bool) []string {
	if narrow {
		return m.renderRowNarrow(idx, r, width)
	}
	return []string{m.renderRowWide(idx, r, width)}
}

// renderRowWide formats one instance as a single padded line. The state glyph is
// always tinted with the state color; on the selected row everything sits on a
// highlight background (each segment carries that background so the bar fills
// cleanly across the ANSI resets between colored runs).
func (m model) renderRowWide(idx int, r *Instance, width int) string {
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
		return padBg(line, width, cSelBg)
	}
	return "  " + fg(sc, glyph+" ") + fg(cDim, tool+" ") + fg(cFg, proj+" ") +
		fg(cDim, age+" ") + fg(cDim, msg) + fg(cDim, meta)
}

// renderRowNarrow lays one instance across two lines for a cramped width: the
// state glyph, tool, project, and age on the first; the message indented on the
// second. The project column is kept fixed so age still lines up under the
// header, shrinking only when the width genuinely can't hold it.
func (m model) renderRowNarrow(idx int, r *Instance, width int) []string {
	sel := idx == m.cur
	sc := stateColor(r.State)
	glyph := m.glyph(r.State)
	tool := padRight(titleCase(r.Source), colTool)
	age := dur(r.Since)

	meta := "" // inferred (scan-discovered, no event yet) gets a subtle marker
	if r.inferred {
		meta = " ~"
	}
	projW := colProj
	if room := width - (2 + 2 + colTool + 1 + 1 + colAge + len([]rune(meta))); room < projW {
		projW = max(room, 4)
	}
	proj := padRight(truncate(r.Project, projW), projW)

	if sel {
		seg := func(c lipgloss.Color, bold bool, s string) string {
			return lipgloss.NewStyle().Background(cSelBg).Foreground(c).Bold(bold).Render(s)
		}
		line1 := seg(cAccent, false, "▌ ") + seg(sc, false, glyph+" ") +
			seg(cFg, true, tool+" ") + seg(cFg, true, proj+" ") +
			seg(cDim, false, age) + seg(cDim, false, meta)
		if r.Msg == "" {
			return []string{padBg(line1, width, cSelBg)}
		}
		msg := truncate(r.Msg, max(width-4-1, 6))
		line2 := seg(cAccent, false, "▌ ") + seg(cFg, false, "  "+msg)
		return []string{padBg(line1, width, cSelBg), padBg(line2, width, cSelBg)}
	}
	line1 := "  " + fg(sc, glyph+" ") + fg(cDim, tool+" ") + fg(cFg, proj+" ") +
		fg(cDim, age) + fg(cDim, meta)
	if r.Msg == "" {
		return []string{line1}
	}
	msg := truncate(r.Msg, max(width-4-1, 6))
	line2 := fg(cDim, "    "+msg)
	return []string{line1, line2}
}

// padBg right-pads a rendered line to `width` with a background-colored run so a
// highlighted row fills cleanly across the ANSI resets between colored segments.
func padBg(line string, width int, bg lipgloss.Color) string {
	if pad := width - lipgloss.Width(line); pad > 0 {
		return line + lipgloss.NewStyle().Background(bg).Render(strings.Repeat(" ", pad))
	}
	return line
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
	m := model{rows: rows, nag: map[string]int64{}, prev: seedSnaps(rows), muted: isMuted(), backend: notifyBackend()}
	m.loadHistory()
	p := tea.NewProgram(m, tea.WithAltScreen())
	_, _ = p.Run()
}

// loadHistory seeds the in-memory feed with the persisted last-24h events so the
// panel opens with context instead of empty. Disk Seqs come from independent hook
// processes and are meaningless here, so they're renumbered contiguously (the
// scroll math in feed.go assumes ascending +1 seqs); feedSeq then continues from
// the end so live events keep climbing. born is pushed past the flash window so
// history doesn't pulse on open — only genuinely new events flash.
func (m *model) loadHistory() {
	hist := loadFeedLog()
	for i := range hist {
		m.feedSeq++
		hist[i].Seq = m.feedSeq
		hist[i].born = -len(flashRamp) * flashStep
	}
	m.events = hist
}

func runList() {
	for _, r := range gather() {
		fmt.Printf("%-12s %-6s %-12s %-8s %s\n", r.State, r.Source, r.Project, dur(r.Since), r.Msg)
	}
}
