package main

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

// A trimmed real gh response: one PR with no CI and a null review decision,
// one draft with passing checks, one approved with passing checks. Nullable
// GraphQL fields must decode to zero values, and the repo owner is stripped.
const prSampleJSON = `{"data":{"search":{"nodes":[
  {"number":3,"title":"No CI here","url":"https://github.com/o/takehome/pull/3",
   "isDraft":false,"updatedAt":"2026-07-20T19:22:40Z",
   "repository":{"nameWithOwner":"o/takehome"},"reviewDecision":null,
   "commits":{"nodes":[{"commit":{"statusCheckRollup":null}}]}},
  {"number":616,"title":"Draft work","url":"https://github.com/o/arc/pull/616",
   "isDraft":true,"updatedAt":"2026-07-20T02:16:08Z",
   "repository":{"nameWithOwner":"o/arc"},"reviewDecision":"REVIEW_REQUIRED",
   "commits":{"nodes":[{"commit":{"statusCheckRollup":{"state":"SUCCESS"}}}]}},
  {"number":615,"title":"Ready to merge","url":"https://github.com/o/arc/pull/615",
   "isDraft":false,"updatedAt":"2026-07-20T18:26:29Z",
   "repository":{"nameWithOwner":"o/arc"},"reviewDecision":"APPROVED",
   "commits":{"nodes":[{"commit":{"statusCheckRollup":{"state":"SUCCESS"}}}]}}
]}}}`

func TestParsePRs(t *testing.T) {
	prs, err := parsePRs([]byte(prSampleJSON))
	if err != nil {
		t.Fatalf("parsePRs: %v", err)
	}
	if len(prs) != 3 {
		t.Fatalf("expected 3 PRs, got %d", len(prs))
	}
	if p := prs[0]; p.Repo != "takehome" || p.Number != 3 || p.CI != "" || p.Review != "" {
		t.Errorf("null-field PR parsed wrong: %+v", p)
	}
	if p := prs[1]; !p.Draft || p.CI != "SUCCESS" || p.Review != "REVIEW_REQUIRED" {
		t.Errorf("draft PR parsed wrong: %+v", p)
	}
	if prs[2].Updated == 0 {
		t.Errorf("updatedAt should parse to unix secs, got %+v", prs[2])
	}
}

// Urgency tiers: failing CI, then changes requested, then approved-and-ready,
// then the rest; recency breaks ties within a tier.
func TestSortPRs(t *testing.T) {
	prs := []prInfo{
		{Repo: "idle", Updated: 50},
		{Repo: "approved-old", Review: "APPROVED", Updated: 10},
		{Repo: "approved-new", Review: "APPROVED", Updated: 20},
		{Repo: "approved-draft", Review: "APPROVED", Draft: true, Updated: 99},
		{Repo: "changes", Review: "CHANGES_REQUESTED", Updated: 5},
		{Repo: "failing", CI: "FAILURE", Updated: 1},
	}
	sortPRs(prs)
	want := []string{"failing", "changes", "approved-new", "approved-old", "approved-draft", "idle"}
	for i, w := range want {
		if prs[i].Repo != w {
			t.Fatalf("order[%d] = %q, want %q (full: %+v)", i, prs[i].Repo, w, prs)
		}
	}
}

func TestPRPanelHeight(t *testing.T) {
	prs := make([]prInfo, 25)
	cases := []struct {
		name     string
		on       bool
		prs      []prInfo
		maxLines int
		want     int
	}{
		{"hidden", false, prs, 20, 0},
		{"no room for a row", true, prs, 1, 0},
		{"empty list still shows placeholder row", true, nil, 20, 2},
		{"content-capped", true, prs[:3], 20, 4},
		{"prMaxRows-capped", true, prs, 40, 1 + prMaxRows},
		{"maxLines-capped", true, prs, 5, 5},
	}
	for _, c := range cases {
		m := model{prsOn: c.on, prs: c.prs}
		if got := m.prPanelHeight(c.maxLines); got != c.want {
			t.Errorf("%s: prPanelHeight(%d) = %d, want %d", c.name, c.maxLines, got, c.want)
		}
	}
}

// renderPRs must return exactly the requested number of lines (the layout
// depends on it), and fold rows that don't fit into an "… and N more" line.
func TestRenderPRsHeightAndOverflow(t *testing.T) {
	m := model{prsOn: true}
	for i := range 6 {
		m.prs = append(m.prs, prInfo{Repo: "repo", Number: i, Title: fmt.Sprintf("pr %d", i), Updated: now()})
	}

	lines := m.renderPRs(80, 5) // rule + 4 rows for 6 PRs → 3 shown + overflow
	if len(lines) != 5 {
		t.Fatalf("expected exactly 5 lines, got %d", len(lines))
	}
	if !strings.Contains(lines[4], "and 3 more") {
		t.Errorf("expected overflow line, got %q", lines[4])
	}

	lines = m.renderPRs(80, 7) // rule + 6 rows: everything fits, no overflow
	if len(lines) != 7 || strings.Contains(strings.Join(lines, "\n"), "more") {
		t.Errorf("expected all 6 PRs with no overflow line, got:\n%s", strings.Join(lines, "\n"))
	}

	empty := model{prsOn: true}
	lines = empty.renderPRs(80, 2)
	if len(lines) != 2 || !strings.Contains(lines[1], "fetching") {
		t.Errorf("never-fetched panel should show the fetching placeholder, got %q", lines)
	}
}

func TestPRShownCount(t *testing.T) {
	cases := []struct{ n, lines, want int }{
		{0, 5, 0},  // nothing to show
		{3, 4, 3},  // all fit exactly (rule + 3 rows)
		{3, 10, 3}, // spare room doesn't invent rows
		{6, 5, 3},  // 4 content rows, one reserved for "… and N more"
		{6, 1, 0},  // rule only → nothing selectable
		{6, 0, 0},  // no panel
		{6, 2, 0},  // one content row but it must be the overflow line
	}
	for _, c := range cases {
		if got := prShownCount(c.n, c.lines); got != c.want {
			t.Errorf("prShownCount(%d, %d) = %d, want %d", c.n, c.lines, got, c.want)
		}
	}
}

// The cursor flows off the bottom of the sessions table into the PR rows:
// j/down walks into the panel and stops at the last visible PR, G lands there
// directly, enter opens the selected PR in the browser, and hiding the panel
// pulls a stranded cursor back into range.
func TestCursorFlowsIntoPRs(t *testing.T) {
	t.Setenv("HOME", t.TempDir()) // the p-toggle persists a flag file
	key := func(s string) tea.KeyMsg {
		if s == "enter" {
			return tea.KeyMsg{Type: tea.KeyEnter}
		}
		return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
	}
	m := model{
		w: 100, h: 30, prsOn: true,
		rows: []*Instance{feedInst("a", "claude", "ccmon", StateWorking)},
		prs: []prInfo{
			{Repo: "arc", Number: 1, URL: "https://example.com/1", Updated: now()},
			{Repo: "arc", Number: 2, URL: "https://example.com/2", Updated: now()},
		},
	}
	if got := m.selectableCount(); got != 3 {
		t.Fatalf("selectableCount = %d, want 3 (1 session + 2 PRs)", got)
	}

	step := func(s string) {
		next, _ := m.Update(key(s))
		m = next.(model)
	}
	step("j")
	step("j")
	step("j") // one past the end — must stick at the last PR
	if m.cur != 2 {
		t.Fatalf("cursor after walking down = %d, want 2 (last PR)", m.cur)
	}
	if p, ok := m.selectedPR(); !ok || p.Number != 2 {
		t.Fatalf("selectedPR = %+v ok=%v, want arc#2", p, ok)
	}

	var opened string
	orig := openURL
	openURL = func(url string) error { opened = url; return nil }
	defer func() { openURL = orig }()
	step("enter")
	if opened != "https://example.com/2" {
		t.Fatalf("enter opened %q, want the selected PR's URL", opened)
	}
	if !strings.Contains(m.status, "arc#2") {
		t.Fatalf("status = %q, should name the opened PR", m.status)
	}

	step("g")
	step("G")
	if m.cur != 2 {
		t.Fatalf("G should land on the last PR, got %d", m.cur)
	}
	step("p") // hide the panel with the cursor on a PR row
	if m.prsOn || m.cur != 0 {
		t.Fatalf("after hiding panel: prsOn=%v cur=%d, want false/0", m.prsOn, m.cur)
	}
}

// The selected PR row carries the accent bar so selection is visible.
func TestRenderPRsSelection(t *testing.T) {
	m := model{
		prsOn: true,
		cur:   1, // no sessions → cursor 1 = second PR
		prs: []prInfo{
			{Repo: "arc", Number: 1, Updated: now()},
			{Repo: "arc", Number: 2, Updated: now()},
		},
	}
	lines := m.renderPRs(80, 3)
	if strings.Contains(lines[1], "▌") || !strings.Contains(lines[2], "▌") {
		t.Fatalf("expected only the second PR row to carry the selection bar:\n%s",
			strings.Join(lines, "\n"))
	}
}

// The frame must stay exactly terminal-height lines with the PR panel open, in
// every layout — panel off/on, feed stacked and side — so the footer never
// drifts (the PR-panel analogue of TestViewLineCount).
func TestViewLineCountWithPRs(t *testing.T) {
	const h = 24
	base := model{
		h:     h,
		rows:  []*Instance{feedInst("a", "claude", "ace", StateWorking)},
		prsOn: true,
		prs: []prInfo{
			{Repo: "arc", Number: 615, Title: "Ready to merge", Review: "APPROVED", CI: "SUCCESS", Updated: now()},
			{Repo: "arc", Number: 616, Title: "Draft work", Draft: true, CI: "PENDING", Updated: now()},
		},
		events: []Event{{Seq: 1, Label: "claude:ace", From: StateWorking, To: StateDone, At: now()}},
	}
	for _, c := range []struct {
		name string
		w    int
		feed bool
	}{
		{"prs only", 160, false},
		{"prs + stacked feed", 80, true},
		{"prs + side feed", 160, true},
		{"prs narrow", 50, false},
	} {
		m := base
		m.w, m.feed = c.w, c.feed
		out := m.View()
		if got := strings.Count(out, "\n") + 1; got != h {
			t.Fatalf("%s: view should be %d lines, got %d\n%s", c.name, h, got, out)
		}
		if !strings.Contains(out, "OPEN PRS") {
			t.Fatalf("%s: view should render the OPEN PRS panel", c.name)
		}
		if !strings.Contains(out, "arc#615") {
			t.Fatalf("%s: view should list the PR rows", c.name)
		}
	}
}
