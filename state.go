package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"time"
)

// Instance states, ordered by how urgently they want your attention.
const (
	StateNeedsInput = "needs-input" // Claude/Codex is blocked on you
	StateDone       = "done"        // finished a turn / task
	StateWorking    = "working"     // actively running
	StateIdle       = "idle"        // registered but not doing anything
)

// Instance is one Claude Code or Codex process living in a tmux pane.
type Instance struct {
	ID      string `json:"id"`      // claude: session_id; codex: cdx-<sockbase>-<pane>
	Source  string `json:"source"`  // "claude" | "codex"
	Project string `json:"project"` // basename(cwd)
	Cwd     string `json:"cwd"`
	State   string `json:"state"`
	Msg     string `json:"msg"`
	Since   int64  `json:"since"`   // unix secs when current state was entered
	Updated int64  `json:"updated"` // unix secs of last write

	// tmux coordinates, captured from the hook's $TMUX / $TMUX_PANE.
	Socket  string `json:"socket"`   // full socket path ("" = default)
	Session string `json:"session"`  // tmux session name
	Window  string `json:"window"`   // window index
	WinName string `json:"win_name"` // window name
	PaneID  string `json:"pane_id"`  // e.g. %15

	// inferred is true for codex panes discovered by scanning (no real event yet).
	inferred bool
}

func now() int64 { return time.Now().Unix() }

// statePriority gives the sort weight: lower sorts first / more urgent.
func statePriority(s string) int {
	switch s {
	case StateNeedsInput:
		return 0
	case StateDone:
		return 1
	case StateWorking:
		return 2
	default:
		return 3
	}
}

func stateDir() string {
	home, _ := os.UserHomeDir()
	d := filepath.Join(home, ".ccmon", "state")
	_ = os.MkdirAll(d, 0o755)
	return d
}

var fnameSafe = regexp.MustCompile(`[^A-Za-z0-9._-]`)

func (i *Instance) filePath() string {
	return filepath.Join(stateDir(), fnameSafe.ReplaceAllString(i.ID, "_")+".json")
}

// save atomically writes the instance to its state file.
func (i *Instance) save() error {
	i.Updated = now()
	b, err := json.MarshalIndent(i, "", "  ")
	if err != nil {
		return err
	}
	p := i.filePath()
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func loadInstance(id string) (*Instance, bool) {
	inst := &Instance{ID: id}
	b, err := os.ReadFile(inst.filePath())
	if err != nil {
		return nil, false
	}
	var got Instance
	if json.Unmarshal(b, &got) != nil {
		return nil, false
	}
	return &got, true
}

func removeInstance(id string) {
	inst := &Instance{ID: id}
	_ = os.Remove(inst.filePath())
}

// loadAll reads every persisted instance.
func loadAll() []*Instance {
	dir := stateDir()
	entries, _ := os.ReadDir(dir)
	var out []*Instance
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		b, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		var inst Instance
		if json.Unmarshal(b, &inst) == nil && inst.ID != "" {
			out = append(out, &inst)
		}
	}
	return out
}

// sortInstances orders by urgency, then by how long they've been waiting.
func sortInstances(xs []*Instance) {
	sort.SliceStable(xs, func(a, b int) bool {
		pa, pb := statePriority(xs[a].State), statePriority(xs[b].State)
		if pa != pb {
			return pa < pb
		}
		return xs[a].Since < xs[b].Since // oldest first
	})
}

// setState transitions, resetting Since only when the state actually changes.
func (i *Instance) setState(s string) {
	if i.State != s {
		i.State = s
		i.Since = now()
	}
}
