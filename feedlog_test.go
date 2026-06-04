package main

import (
	"os"
	"testing"
)

// The durable feed round-trips through disk, drops anything older than the 24h
// window, and prunes the on-disk file down to just the survivors so it can't grow
// without bound. HOME is redirected to a temp dir so the test never touches the
// real ~/.ccmon.
func TestFeedLogRoundTripAndPrune(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	fresh := Event{Label: "claude:ace", From: StateWorking, To: StateNeedsInput, Msg: "needs you", At: now()}
	stale := Event{Label: "codex:infra", From: StateWorking, To: StateDone, At: now() - feedLogMaxAge - 10}
	appendFeedLog(stale)
	appendFeedLog(fresh)

	got := loadFeedLog()
	if len(got) != 1 {
		t.Fatalf("expected only the in-window event, got %d: %+v", len(got), got)
	}
	if got[0].Label != "claude:ace" || got[0].To != StateNeedsInput || got[0].Msg != "needs you" {
		t.Fatalf("event did not round-trip: %+v", got[0])
	}

	// loadFeedLog saw a stale line, so it should have rewritten the file; a second
	// read returns the same single survivor with nothing left to prune.
	again := loadFeedLog()
	if len(again) != 1 || again[0].Label != "claude:ace" {
		t.Fatalf("prune should leave exactly the survivor, got %+v", again)
	}

	// A brand-new instance logs From=="" — the feed renders that as an appearance.
	appendFeedLog(Event{Label: "claude:new", From: "", To: StateIdle, At: now()})
	if got := loadFeedLog(); len(got) != 2 {
		t.Fatalf("expected 2 events after appending an appearance, got %d", len(got))
	}
}

// When the TUI is open, a hook-driven transition is logged by both the hook and
// the TUI; load-time dedup collapses them. State changes share a timestamp so the
// full tuple matches; closes are stamped per-observer so they collapse on
// (label, from) even when the times differ. Distinct transitions survive.
func TestFeedLogDedupe(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	at := now()
	// Same state change seen twice (hook + TUI) — identical At.
	appendFeedLog(Event{Label: "claude:ace", From: StateWorking, To: StateDone, At: at})
	appendFeedLog(Event{Label: "claude:ace", From: StateWorking, To: StateDone, At: at})
	// Same close seen twice, stamped a second apart by the two observers.
	appendFeedLog(Event{Label: "claude:ace", From: StateDone, To: "", At: at + 1})
	appendFeedLog(Event{Label: "claude:ace", From: StateDone, To: "", At: at + 2})
	// A genuinely distinct transition must be kept.
	appendFeedLog(Event{Label: "codex:infra", From: StateWorking, To: StateDone, At: at})

	got := loadFeedLog()
	if len(got) != 3 {
		t.Fatalf("expected 3 events after dedup, got %d: %+v", len(got), got)
	}
	// A second load is stable (file was already pruned to the deduped set).
	if again := loadFeedLog(); len(again) != 3 {
		t.Fatalf("dedup should be idempotent, got %d", len(again))
	}
}

// A missing log is not an error — it just yields no history.
func TestFeedLogMissing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if got := loadFeedLog(); got != nil {
		t.Fatalf("missing log should load as nil, got %+v", got)
	}
	if _, err := os.Stat(feedLogPath()); !os.IsNotExist(err) {
		t.Fatalf("loading a missing log should not create it")
	}
}
