package main

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// The activity feed turns the snapshot into a story: a scrolling log of state
// transitions across every instance. It's derived purely by diffing successive
// gather() results against the previous snapshot, so it captures everything the
// TUI observes — hook-driven changes, pane reconciliation, codex activity bumps,
// and your own ack/jump — without any new hook plumbing. The buffer is in-memory
// and ephemeral: it shows what's happened since you opened the TUI.

// Event is one observed state transition for an instance.
type Event struct {
	Seq   int64  // monotonic id; anchors the scroll position across ring eviction
	Label string // source:project at the time it happened
	From  string // prior state; "" means the instance newly appeared
	To    string // new state; "" means the pane closed
	Msg   string // instance message at the moment of the transition
	At    int64  // unix secs the transition is anchored to
}

// instSnap is the minimal per-instance state we retain between polls to detect
// transitions. We store value copies (not the *Instance) so that an in-place
// mutation of a row — e.g. acking it to idle — still reads as a change on the
// next gather instead of being silently aliased away.
type instSnap struct {
	state string
	label string
}

func snapOf(r *Instance) instSnap { return instSnap{state: r.State, label: label(r)} }

// seedSnaps records the current rows as a baseline without emitting any events,
// so opening the TUI doesn't flood the feed with "appeared" lines for sessions
// that were already running.
func seedSnaps(rows []*Instance) map[string]instSnap {
	m := make(map[string]instSnap, len(rows))
	for _, r := range rows {
		m[r.ID] = snapOf(r)
	}
	return m
}

const (
	maxFeedEvents = 200 // ring-buffer cap; older events scroll off the top
	feedMaxLines  = 6    // event rows shown in the bottom (stacked) panel
	feedAgeW      = 5
	feedLabelW    = 18

	// Side-panel layout: when the terminal is wide enough, the feed moves to a
	// column on the right instead of a strip below the table.
	feedSideMinW = 34 // don't bother with a side panel narrower than this
	feedSideMaxW = 56
	sideGutter   = 2
	tableMinW    = 72 // the table needs at least this much to keep the side panel
)

func (m *model) appendEvent(e Event) {
	m.feedSeq++
	e.Seq = m.feedSeq
	m.events = append(m.events, e)
	if len(m.events) > maxFeedEvents {
		m.events = m.events[len(m.events)-maxFeedEvents:]
	}
}

// recordEvents diffs the freshly gathered rows against the previous snapshot,
// appending an event for each appearance, state change, and disappearance, then
// updates the snapshot.
func (m *model) recordEvents(rows []*Instance) {
	seen := make(map[string]bool, len(rows))
	for _, r := range rows {
		seen[r.ID] = true
		switch snap, ok := m.prev[r.ID]; {
		case !ok:
			m.appendEvent(Event{Label: label(r), To: r.State, Msg: r.Msg, At: r.Since})
		case snap.state != r.State:
			m.appendEvent(Event{Label: label(r), From: snap.state, To: r.State, Msg: r.Msg, At: r.Since})
		}
		m.prev[r.ID] = snapOf(r)
	}
	for id, snap := range m.prev {
		if !seen[id] {
			m.appendEvent(Event{Label: snap.label, From: snap.state, At: now()})
			delete(m.prev, id)
		}
	}
}

// refresh re-gathers state, folds any transitions into the feed, and keeps the
// cursor in range. Used everywhere the TUI updates its rows so every change the
// TUI causes (ack/jump/forget) or observes (tick) is recorded.
func (m *model) refresh() {
	rows := gather()
	m.recordEvents(rows)
	m.rows = rows
	if m.cur >= len(m.rows) {
		m.cur = max(0, len(m.rows)-1)
	}
}

// feedLayout decides, from the current terminal size, whether the feed should
// sit in a column to the right (wide terminals) or as a strip below the table,
// and the column widths / event-row count for that choice. It's a pure function
// of w/h so both View and the scroll handler agree on the visible window without
// storing layout state.
func (m model) feedLayout() (side bool, tableW, feedW, rows int) {
	termW, termH := m.w, m.h
	if termW == 0 {
		termW = 100
	}
	if termH == 0 {
		termH = 24
	}
	feedW = min(max((termW-sideGutter)/3, feedSideMinW), feedSideMaxW)
	if tableW = termW - sideGutter - feedW; tableW >= tableMinW {
		// Wide enough: cap the table at the card width so it doesn't smear on a
		// big monitor (the feed keeps the rest, up to feedSideMaxW). The side
		// column spans the region between the header (2 lines) and footer (1);
		// one of its rows is the panel's own title, leaving termH-4 for events.
		return true, min(tableW, maxContentWidth), feedW, max(termH-4, 1)
	}
	return false, min(termW, maxContentWidth), 0, feedMaxLines
}

// idxOfSeq maps an event sequence number to its slice index. Seqs are contiguous
// and ascending, so this is arithmetic; an anchor that has fallen off the front
// of the ring clamps to the oldest retained event.
func (m model) idxOfSeq(seq int64) int {
	if len(m.events) == 0 {
		return 0
	}
	return max(0, min(int(seq-m.events[0].Seq), len(m.events)-1))
}

// feedWindow returns the events currently visible given a window of `visible`
// rows, plus how many are hidden above (older) and below (newer). feedBottomSeq
// == 0 means follow mode: the window is pinned to the newest events.
func (m model) feedWindow(visible int) (window []Event, older, newer int) {
	n := len(m.events)
	if n == 0 || visible <= 0 {
		return nil, 0, 0
	}
	bottom := n - 1
	if m.feedBottomSeq != 0 {
		bottom = max(m.idxOfSeq(m.feedBottomSeq), min(visible-1, n-1)) // keep a full page on screen
	}
	start := max(bottom-visible+1, 0)
	return m.events[start : bottom+1], start, n - 1 - bottom
}

// scrollFeed moves the visible window by `deltaPages` pages (negative = older,
// positive = newer). Reaching the newest event re-engages follow mode so the
// panel resumes streaming live; new events arriving while scrolled up leave the
// anchored window put.
func (m *model) scrollFeed(deltaPages int) {
	_, _, _, visible := m.feedLayout()
	if !m.feed || len(m.events) <= visible {
		return
	}
	window, _, _ := m.feedWindow(visible)
	bottom := m.idxOfSeq(window[len(window)-1].Seq)
	bottom = max(visible-1, min(bottom+deltaPages*max(visible-1, 1), len(m.events)-1))
	if bottom >= len(m.events)-1 {
		m.feedBottomSeq = 0 // caught up to the tail → follow
	} else {
		m.feedBottomSeq = m.events[bottom].Seq
	}
}

// renderFeed returns a fixed-height block: a titled rule (with scroll/follow
// indicators) plus `rows` event lines, blank-padded so the layout never jumps.
func (m model) renderFeed(width, rows int) []string {
	window, older, newer := m.feedWindow(rows)

	status, statusColor := "", cDim
	if older > 0 {
		status += fmt.Sprintf("↑%d ", older)
	}
	if m.feedBottomSeq != 0 { // paused above the tail
		status += fmt.Sprintf("↓%d PgDn=live ", newer)
		statusColor = cYellow
	}
	head := "── ACTIVITY "
	fill := max(width-lipgloss.Width(head)-lipgloss.Width(status), 0)
	rule := fg(cDim, head+strings.Repeat("─", fill)) + fg(statusColor, status)

	lines := make([]string, 0, rows+1)
	lines = append(lines, rule)
	if len(window) == 0 {
		lines = append(lines, fg(cDim, "   nothing yet — state changes will stream here"))
	}
	for _, e := range window {
		lines = append(lines, m.renderEvent(e, width))
	}
	for len(lines) < rows+1 {
		lines = append(lines, "")
	}
	return lines
}

// renderEvent formats one transition: "<age>  <label>  <from> → <to>  <msg>".
// The destination state colors the arrow's target; a new session shows a green
// "+", a closed pane a gray "✕ closed".
func (m model) renderEvent(e Event, width int) string {
	age := padRight(dur(e.At), feedAgeW)
	lbl := padRight(truncate(e.Label, feedLabelW), feedLabelW)

	var plain, colored string
	switch {
	case e.To == "":
		plain, colored = "✕ closed", fg(cGray, "✕ closed")
	case e.From == "":
		plain, colored = "+ "+e.To, fg(cGreen, "+ ")+fg(stateColor(e.To), e.To)
	default:
		plain, colored = e.From+" → "+e.To, fg(cDim, e.From+" → ")+fg(stateColor(e.To), e.To)
	}

	out := "  " + fg(cDim, age+" ") + fg(cFg, lbl+" ") + colored
	used := 2 + feedAgeW + 1 + feedLabelW + 1 + lipgloss.Width(plain)
	if e.Msg != "" {
		if msgMax := width - used - 2; msgMax >= 6 {
			out += fg(cDim, "  "+truncate(e.Msg, msgMax))
		}
	}
	return out
}
