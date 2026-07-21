package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

// The open-PRs panel sits directly below the sessions table: every open pull
// request you've authored, across all repos, with CI and review state at a
// glance. Data comes from a single `gh api graphql` search so one network
// round-trip covers the whole list (the REST search API can't include the
// status rollup). Fetches run as a bubbletea command off the UI goroutine and
// refresh on their own slow clock — GitHub state moves in minutes, not the
// 125ms the spinner ticks at.

// prInfo is one open pull request authored by the gh-authenticated user.
type prInfo struct {
	Repo    string // short repo name (owner stripped; the panel is yours anyway)
	Number  int
	Title   string
	URL     string
	Draft   bool
	CI      string // statusCheckRollup: SUCCESS/FAILURE/ERROR/PENDING/EXPECTED, "" = no checks
	Review  string // reviewDecision: APPROVED/CHANGES_REQUESTED/REVIEW_REQUIRED, "" = none
	Updated int64  // unix secs of last activity
}

// prsMsg carries a completed fetch back into Update. On error the previous
// list is kept (stale beats blank); the error surfaces in the panel rule.
type prsMsg struct {
	prs []prInfo
	err error
}

// prQuery asks for everything the panel shows in one call. `author:@me`
// resolves to whoever `gh auth` is logged in as; archived repos are noise.
const prQuery = `query {
  search(query: "is:pr is:open author:@me archived:false", type: ISSUE, first: 50) {
    nodes {
      ... on PullRequest {
        number title url isDraft updatedAt
        repository { nameWithOwner }
        reviewDecision
        commits(last: 1) { nodes { commit { statusCheckRollup { state } } } }
      }
    }
  }
}`

// fetchPRs is a tea.Cmd: it blocks on gh (network) and must only run inside
// bubbletea's command goroutine, never on the Update path.
func fetchPRs() tea.Msg {
	out, err := exec.Command("gh", "api", "graphql", "-f", "query="+prQuery).Output()
	if err != nil {
		var ee *exec.ExitError
		msg := err.Error()
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			msg = strings.SplitN(strings.TrimSpace(string(ee.Stderr)), "\n", 2)[0]
		}
		return prsMsg{err: errors.New(msg)}
	}
	prs, err := parsePRs(out)
	if err != nil {
		return prsMsg{err: err}
	}
	sortPRs(prs)
	return prsMsg{prs: prs}
}

// parsePRs decodes the GraphQL response. Nullable fields (reviewDecision on
// un-reviewed PRs, statusCheckRollup on repos with no CI, empty commits on a
// just-opened PR) all decode to their zero values.
func parsePRs(b []byte) ([]prInfo, error) {
	var resp struct {
		Data struct {
			Search struct {
				Nodes []struct {
					Number     int    `json:"number"`
					Title      string `json:"title"`
					URL        string `json:"url"`
					IsDraft    bool   `json:"isDraft"`
					UpdatedAt  string `json:"updatedAt"`
					Repository struct {
						NameWithOwner string `json:"nameWithOwner"`
					} `json:"repository"`
					ReviewDecision string `json:"reviewDecision"`
					Commits        struct {
						Nodes []struct {
							Commit struct {
								StatusCheckRollup struct {
									State string `json:"state"`
								} `json:"statusCheckRollup"`
							} `json:"commit"`
						} `json:"nodes"`
					} `json:"commits"`
				} `json:"nodes"`
			} `json:"search"`
		} `json:"data"`
	}
	if err := json.Unmarshal(b, &resp); err != nil {
		return nil, err
	}
	prs := make([]prInfo, 0, len(resp.Data.Search.Nodes))
	for _, n := range resp.Data.Search.Nodes {
		repo := n.Repository.NameWithOwner
		if i := strings.IndexByte(repo, '/'); i >= 0 {
			repo = repo[i+1:]
		}
		var updated int64
		if t, err := time.Parse(time.RFC3339, n.UpdatedAt); err == nil {
			updated = t.Unix()
		}
		ci := ""
		if len(n.Commits.Nodes) > 0 {
			ci = n.Commits.Nodes[0].Commit.StatusCheckRollup.State
		}
		prs = append(prs, prInfo{
			Repo:    repo,
			Number:  n.Number,
			Title:   n.Title,
			URL:     n.URL,
			Draft:   n.IsDraft,
			CI:      ci,
			Review:  n.ReviewDecision,
			Updated: updated,
		})
	}
	return prs, nil
}

// prPriority mirrors statePriority's spirit: what needs your hands sorts
// first. Failing CI and requested changes block the PR on you; an approved
// non-draft is a merge waiting to happen; the rest is just in flight.
func prPriority(p prInfo) int {
	switch {
	case p.CI == "FAILURE" || p.CI == "ERROR":
		return 0
	case p.Review == "CHANGES_REQUESTED":
		return 1
	case p.Review == "APPROVED" && !p.Draft:
		return 2
	default:
		return 3
	}
}

// sortPRs orders by urgency, then most recent activity first within a tier.
func sortPRs(prs []prInfo) {
	sort.SliceStable(prs, func(a, b int) bool {
		pa, pb := prPriority(prs[a]), prPriority(prs[b])
		if pa != pb {
			return pa < pb
		}
		return prs[a].Updated > prs[b].Updated
	})
}

// prRefreshSecs is how often the PR list is re-fetched while the panel is
// shown. Override with CCMON_PR_SECS.
func prRefreshSecs() int64 {
	if v := os.Getenv("CCMON_PR_SECS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return int64(n)
		}
	}
	return 60
}

// The hidden flag persists "I pressed p to hide the panel" across restarts,
// encoded like the notify-backend file: absence means the default (shown).
func prsFlagPath() string { return filepath.Join(filepath.Dir(stateDir()), "prs-hidden") }

func prsHidden() bool {
	_, err := os.Stat(prsFlagPath())
	return err == nil
}

func setPRsHidden(hidden bool) error {
	if hidden {
		return os.WriteFile(prsFlagPath(), []byte("1\n"), 0o644)
	}
	if err := os.Remove(prsFlagPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// ---- rendering ----

const (
	colPRRepo = 20 // "repo#1234", truncated
	colPRTag  = 8  // widest tag is "approved"
	prMaxRows = 10 // panel content cap; beyond this the last row is "… and N more"
)

// prPanelHeight is the total lines (title rule + content rows) the panel wants
// within maxLines. Zero when hidden or when there isn't room for at least one
// content row — a rule with nothing under it is worse than no panel.
func (m model) prPanelHeight(maxLines int) int {
	if !m.prsOn || maxLines < 2 {
		return 0
	}
	return min(1+max(len(m.prs), 1), 1+prMaxRows, maxLines)
}

// renderPRs returns exactly `lines` lines (rule + rows, blank-padded) so the
// layout never jumps as results arrive. Callers size `lines` via prPanelHeight.
func (m model) renderPRs(width, lines int) []string {
	if lines <= 0 {
		return nil
	}
	head := "── OPEN PRS "
	if len(m.prs) > 0 {
		head = fmt.Sprintf("── OPEN PRS · %d ", len(m.prs))
	}
	status, statusColor := "", cDim
	if m.prsErr != "" {
		status, statusColor = truncate("✗ "+m.prsErr+" ", max(width/2, 8)), cRed
	} else if m.prsBusy {
		status = spinFrames[(m.frame/2)%len(spinFrames)] + " "
	}
	fill := max(width-lipgloss.Width(head)-lipgloss.Width(status), 0)
	out := []string{fg(cDim, head+strings.Repeat("─", fill)) + fg(statusColor, status)}

	rows := lines - 1
	switch {
	case len(m.prs) == 0 && m.prsAt == 0 && m.prsErr == "":
		out = append(out, fg(cDim, "    fetching open PRs…"))
	case len(m.prs) == 0 && m.prsErr != "":
		out = append(out, fg(cDim, "    "+truncate(m.prsErr, max(width-4, 8))))
	case len(m.prs) == 0:
		out = append(out, fg(cDim, "    no open PRs"))
	default:
		shown := len(m.prs)
		if shown > rows {
			shown = max(rows-1, 0) // reserve a line for "… and N more"
		}
		for _, p := range m.prs[:shown] {
			out = append(out, m.renderPR(p, width))
		}
		if shown < len(m.prs) {
			out = append(out, fg(cDim, fmt.Sprintf("    … and %d more", len(m.prs)-shown)))
		}
	}
	for len(out) < lines {
		out = append(out, "")
	}
	return out[:lines]
}

// renderPR formats one PR: CI glyph, repo#num, age, review tag, title. In
// narrow mode the tag column is dropped — the CI glyph is the load-bearing
// signal and the title needs the room more.
func (m model) renderPR(p prInfo, width int) string {
	glyph, gc := m.prCIGlyph(p.CI)
	repo := padRight(truncate(fmt.Sprintf("%s#%d", p.Repo, p.Number), colPRRepo), colPRRepo)
	age := padRight(dur(p.Updated), colAge)

	narrow := width < narrowWidth
	tagPart, fixed := "", 2+2+colPRRepo+1+colAge+1
	if !narrow {
		tag, tc := prTag(p)
		tagPart = fg(tc, padRight(tag, colPRTag)+" ")
		fixed += colPRTag + 1
	}
	title := truncate(p.Title, max(width-fixed-1, 8))
	return "  " + fg(gc, glyph+" ") + fg(cFg, repo+" ") + fg(cDim, age+" ") +
		tagPart + fg(cDim, title)
}

// prCIGlyph maps a status rollup to the same visual language as the sessions
// table: green check passing, red cross failing, the working spinner while
// checks run, hollow gray for "no CI to speak of".
func (m model) prCIGlyph(state string) (string, lipgloss.Color) {
	switch state {
	case "SUCCESS":
		return "✓", cGreen
	case "FAILURE", "ERROR":
		return "✗", cRed
	case "PENDING", "EXPECTED":
		return spinFrames[(m.frame/2)%len(spinFrames)], cYellow
	default:
		return "○", cGray
	}
}

// prTag renders the review state as a short word. Draft wins over the review
// decision: a draft's "review required" is a technicality, not a call to act.
func prTag(p prInfo) (string, lipgloss.Color) {
	if p.Draft {
		return "draft", cGray
	}
	switch p.Review {
	case "APPROVED":
		return "approved", cGreen
	case "CHANGES_REQUESTED":
		return "changes", cRed
	case "REVIEW_REQUIRED":
		return "review", cDim
	}
	return "", cDim
}
