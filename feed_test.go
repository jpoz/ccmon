package main

import (
	"strings"
	"testing"
)

func feedInst(id, source, project, state string) *Instance {
	return &Instance{ID: id, Source: source, Project: project, State: state, Since: now()}
}

// The feed is built by diffing successive gather() results. This walks a full
// lifecycle — baseline, a transition, a close, and a fresh appearance — and
// asserts exactly one event is emitted per observed change (and none at
// baseline, since seedSnaps records the starting rows silently).
func TestRecordEvents(t *testing.T) {
	a := feedInst("a", "claude", "ace", StateWorking)
	b := feedInst("b", "codex", "infra", StateWorking)

	m := &model{prev: seedSnaps([]*Instance{a, b})}

	// Baseline re-poll with nothing changed → no events.
	m.recordEvents([]*Instance{a, b})
	if len(m.events) != 0 {
		t.Fatalf("expected no events at baseline, got %d: %+v", len(m.events), m.events)
	}

	// a goes working → needs-input.
	a.State = StateNeedsInput
	a.Msg = "needs your permission"
	m.recordEvents([]*Instance{a, b})
	if len(m.events) != 1 {
		t.Fatalf("expected 1 event after transition, got %d", len(m.events))
	}
	if e := m.events[0]; e.From != StateWorking || e.To != StateNeedsInput ||
		e.Label != "claude:ace" || e.Msg != "needs your permission" {
		t.Fatalf("unexpected transition event: %+v", e)
	}

	// b's pane closes (absent from the gather).
	m.recordEvents([]*Instance{a})
	last := m.events[len(m.events)-1]
	if last.From != StateWorking || last.To != "" || last.Label != "codex:infra" {
		t.Fatalf("expected close event for b, got %+v", last)
	}
	if _, ok := m.prev["b"]; ok {
		t.Fatalf("closed instance should be pruned from prev snapshot")
	}

	// A brand-new instance shows up → an "appeared" event (empty From).
	c := feedInst("c", "claude", "new", StateDone)
	m.recordEvents([]*Instance{a, c})
	last = m.events[len(m.events)-1]
	if last.From != "" || last.To != StateDone || last.Label != "claude:new" {
		t.Fatalf("expected appeared event for c, got %+v", last)
	}
}

// The rendered frame must always occupy exactly `height` lines so the footer
// stays pinned — across feed-off, the narrow stacked feed, and the wide side
// panel. The wide/narrow widths are chosen to land on either side of the
// feedLayout threshold.
func TestViewLineCount(t *testing.T) {
	const h = 24
	base := model{
		h:      h,
		rows:   []*Instance{feedInst("a", "claude", "ace", StateWorking)},
		events: []Event{{Seq: 1, Label: "claude:ace", From: StateWorking, To: StateDone, At: now()}},
	}
	cases := []struct {
		name       string
		w          int
		feed, side bool
	}{
		{"feed off", 160, false, false},
		{"stacked feed", 80, true, false},
		{"side feed", 160, true, true},
	}
	for _, c := range cases {
		m := base
		m.w, m.feed = c.w, c.feed
		if side, _, _, _ := m.feedLayout(); m.feed && side != c.side {
			t.Fatalf("%s: expected side=%v at width %d", c.name, c.side, c.w)
		}
		out := m.View()
		if got := strings.Count(out, "\n") + 1; got != h {
			t.Fatalf("%s: view should be %d lines, got %d", c.name, h, got)
		}
		if c.feed && !strings.Contains(out, "ACTIVITY") {
			t.Fatalf("%s: open feed should render the ACTIVITY panel", c.name)
		}
	}
}

// Scrolling anchors to a sequence number, so it survives ring-buffer eviction
// and — crucially — does not lurch when new events stream in while paused.
// Reaching the tail re-engages follow mode.
func TestFeedScroll(t *testing.T) {
	m := &model{feed: true, w: 80, h: 24} // stacked feed; height fills the region
	for range 20 {
		m.appendEvent(Event{Label: "x", To: StateDone})
	}
	_, _, _, visible := m.feedLayout()

	win, older, newer := m.feedWindow(visible)
	if len(win) != visible || newer != 0 || older != 20-visible {
		t.Fatalf("follow window: len=%d older=%d newer=%d", len(win), older, newer)
	}
	if win[len(win)-1].Seq != 20 {
		t.Fatalf("follow should end at newest seq 20, got %d", win[len(win)-1].Seq)
	}

	// Scroll up: leaves follow mode and exposes hidden-newer events.
	m.scrollFeed(-1)
	if m.feedBottomSeq == 0 {
		t.Fatal("scrolling up should pause follow mode")
	}
	if _, _, newer := m.feedWindow(visible); newer == 0 {
		t.Fatal("after scrolling up there should be hidden newer events")
	}

	// New events arriving while paused must not move the anchored window.
	before, _, _ := m.feedWindow(visible)
	m.appendEvent(Event{Label: "y", To: StateNeedsInput})
	after, _, _ := m.feedWindow(visible)
	if before[len(before)-1].Seq != after[len(after)-1].Seq {
		t.Fatal("paused feed should stay anchored when new events arrive")
	}

	// Scrolling back down to the tail resumes follow.
	m.scrollFeed(1)
	m.scrollFeed(1)
	if m.feedBottomSeq != 0 {
		t.Fatalf("scrolling to the tail should resume follow, got %d", m.feedBottomSeq)
	}
}

// A freshly-appended event flashes, the highlight fades with the animation
// clock, and an old event doesn't flash at all.
func TestEventFlash(t *testing.T) {
	m := &model{frame: 100}
	m.appendEvent(Event{To: StateDone}) // born = 100
	e := m.events[0]

	first, ok := m.eventFlash(e)
	if !ok {
		t.Fatal("a just-arrived event should flash")
	}
	// One ramp step later the shade should have advanced (faded).
	m.frame = 100 + flashStep
	if next, ok := m.eventFlash(e); !ok || next == first {
		t.Fatalf("flash should fade to a new shade after %d frames", flashStep)
	}
	// Past the full ramp it stops flashing.
	m.frame = 100 + len(flashRamp)*flashStep
	if _, ok := m.eventFlash(e); ok {
		t.Fatal("flash should have fully decayed")
	}
}

func TestAppendEventCap(t *testing.T) {
	m := &model{}
	for range maxFeedEvents + 50 {
		m.appendEvent(Event{To: StateDone})
	}
	if len(m.events) != maxFeedEvents {
		t.Fatalf("ring buffer should cap at %d, got %d", maxFeedEvents, len(m.events))
	}
}
