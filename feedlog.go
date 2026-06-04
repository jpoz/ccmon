package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
)

// The activity feed is derived live in the TUI (see feed.go), but that view only
// exists while the TUI is open. feed.jsonl is the durable companion: the hook
// processes — the only code guaranteed to run on every state change, TUI or not —
// append one line per transition here, so the panel can open with the last day of
// history instead of a blank slate.

const feedLogMaxAge = 24 * 60 * 60 // seconds of history retained on disk

// feedLogPath is the append-only activity log, kept beside the other ~/.ccmon
// runtime files (muted, notify-backend, notify.log).
func feedLogPath() string {
	return filepath.Join(filepath.Dir(stateDir()), "feed.jsonl")
}

// appendFeedLog records one transition durably. Called from the hook processes,
// so the history survives across TUI restarts. O_APPEND keeps concurrent hook
// writes from interleaving — each event is a single short line, well under the
// POSIX atomic-append limit. Best-effort: a write failure is silently dropped
// rather than disrupting the hook that triggered it.
func appendFeedLog(e Event) {
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	f, err := os.OpenFile(feedLogPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

// loadFeedLog returns the persisted events from the last feedLogMaxAge seconds,
// oldest first. If it had to skip anything (aged-out or malformed lines) it
// rewrites the file to just the survivors so the log can't grow without bound.
// The rewrite races a concurrent hook append in the worst case — acceptable for
// an ephemeral activity log, and it only happens at TUI startup.
func loadFeedLog() []Event {
	f, err := os.Open(feedLogPath())
	if err != nil {
		return nil
	}
	cutoff := now() - feedLogMaxAge
	var kept []Event
	stale := false
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e Event
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			stale = true
			continue
		}
		if e.At >= cutoff {
			kept = append(kept, e)
		} else {
			stale = true
		}
	}
	f.Close()
	deduped := dedupeEvents(kept)
	if stale || len(deduped) != len(kept) {
		pruneFeedLog(deduped)
	}
	return deduped
}

// dedupeEvents collapses events that more than one observer recorded for the same
// transition. When the TUI is open it logs what it sees and the hook process logs
// the change it drove, so a hook-driven transition is written twice. State changes
// share an exact timestamp (At == the instance's Since, read from the same state
// file by both), so the full tuple identifies the duplicate. Closes are stamped at
// observation time and so differ by up to a gather interval between observers; they
// key on (label, from) instead, which is safe because an instance closes once per
// lifetime. Order is preserved (oldest first), keeping the earliest copy.
func dedupeEvents(events []Event) []Event {
	seen := make(map[string]bool, len(events))
	out := events[:0]
	for _, e := range events {
		k := e.Label + "\x00" + e.From + "\x00" + e.To
		if e.To != "" { // not a close → include the shared timestamp
			k += "\x00" + strconv.FormatInt(e.At, 10)
		}
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, e)
	}
	return out
}

// pruneFeedLog atomically rewrites the log to exactly the given events.
func pruneFeedLog(events []Event) {
	var buf bytes.Buffer
	for _, e := range events {
		b, err := json.Marshal(e)
		if err != nil {
			continue
		}
		buf.Write(b)
		buf.WriteByte('\n')
	}
	p := feedLogPath()
	tmp := p + ".tmp"
	if os.WriteFile(tmp, buf.Bytes(), 0o644) != nil {
		return
	}
	_ = os.Rename(tmp, p)
}
