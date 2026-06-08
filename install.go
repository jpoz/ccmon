package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// claudeHookEvents are every Claude Code hook event ccmon wires itself into.
// PreToolUse/PostToolUse matter most: Claude fires no event when you *grant* a
// permission, so a tool running again is the signal that clears a stale
// needs-input — without those two the session stays red until the turn ends.
var claudeHookEvents = []string{
	"SessionStart",
	"UserPromptSubmit",
	"Notification",
	"PreToolUse",
	"PostToolUse",
	"Stop",
	"SubagentStop",
	"SessionEnd",
}

// runInstall wires the running binary into Claude Code's hooks and Codex's
// notify program. It is idempotent (re-running just refreshes the path) and
// backs up any file it rewrites to "<file>.bak".
func runInstall(args []string) {
	dry := hasFlag(args, "--dry-run")
	bin := selfPath()
	fmt.Printf("ccmon binary: %s\n", bin)
	if dry {
		fmt.Println("(dry run — no files will be written)")
	}

	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot find home directory: %v\n", err)
		os.Exit(1)
	}

	// Claude Code hooks.
	settings := filepath.Join(home, ".claude", "settings.json")
	if err := installClaudeHooks(settings, bin, dry); err != nil {
		fmt.Fprintf(os.Stderr, "✗ Claude Code: %v\n", err)
	} else {
		fmt.Printf("✓ Claude Code hooks → %s\n", settings)
	}

	// Codex notify program (only if the user has codex set up).
	codexDir := filepath.Join(home, ".codex")
	cfg := filepath.Join(codexDir, "config.toml")
	if dirExists(codexDir) {
		switch action, err := installCodexNotify(cfg, bin, dry); {
		case err != nil:
			fmt.Fprintf(os.Stderr, "✗ Codex: %v\n", err)
		case action == "skipped":
			fmt.Printf("• Codex notify left untouched — %s already has a different notify program.\n", cfg)
			fmt.Printf("  To use ccmon, set it manually:\n    %s\n", codexNotifyLine(bin))
		default:
			fmt.Printf("✓ Codex notify → %s (%s)\n", cfg, action)
		}
	} else {
		fmt.Println("• Codex not detected (~/.codex missing) — skipped")
	}

	checkDeps()
	if !dry {
		fmt.Println("\nDone. Run `ccmon doctor` to verify, or `ccmon` to open the TUI.")
		fmt.Println("Restart any running Claude Code / Codex sessions so they pick up the hooks.")
	}
}

// runUninstall removes ccmon's wiring from Claude Code and Codex, leaving any
// other hooks / settings intact.
func runUninstall(args []string) {
	dry := hasFlag(args, "--dry-run")
	home, err := os.UserHomeDir()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot find home directory: %v\n", err)
		os.Exit(1)
	}

	settings := filepath.Join(home, ".claude", "settings.json")
	if err := uninstallClaudeHooks(settings, dry); err != nil {
		fmt.Fprintf(os.Stderr, "✗ Claude Code: %v\n", err)
	} else {
		fmt.Printf("✓ Removed ccmon hooks from %s\n", settings)
	}

	cfg := filepath.Join(home, ".codex", "config.toml")
	if err := uninstallCodexNotify(cfg, dry); err != nil {
		fmt.Fprintf(os.Stderr, "✗ Codex: %v\n", err)
	} else {
		fmt.Printf("✓ Removed ccmon notify from %s\n", cfg)
	}

	fmt.Println("\nThe state in ~/.ccmon and the binary itself were left in place.")
}

// runDoctor reports the current install status and dependency health.
func runDoctor() {
	bin := selfPath()
	fmt.Printf("ccmon binary:  %s\n", bin)

	home, _ := os.UserHomeDir()
	settings := filepath.Join(home, ".claude", "settings.json")
	wired := claudeWiredEvents(settings)
	fmt.Printf("\nClaude Code (%s):\n", settings)
	for _, ev := range claudeHookEvents {
		if cmd, ok := wired[ev]; ok {
			mark := "✓"
			note := ""
			if cmd != bin+" hook" {
				mark, note = "!", "  (points at "+cmd+")"
			}
			fmt.Printf("  %s %s%s\n", mark, ev, note)
		} else {
			fmt.Printf("  ✗ %s — not wired\n", ev)
		}
	}

	cfg := filepath.Join(home, ".codex", "config.toml")
	fmt.Printf("\nCodex (%s):\n", cfg)
	switch idx, kind, line := codexNotifyStatus(cfg); kind {
	case "ccmon":
		mark := "✓"
		if strings.TrimSpace(line) != codexNotifyLine(bin) {
			mark = "!"
		}
		fmt.Printf("  %s notify wired (line %d)\n", mark, idx+1)
	case "other":
		fmt.Printf("  ! notify points at a different program (line %d)\n", idx+1)
	default:
		fmt.Println("  ✗ notify not wired")
	}

	fmt.Println("\nDependencies:")
	for _, dep := range deps() {
		if path, err := exec.LookPath(dep.name); err == nil {
			fmt.Printf("  ✓ %s (%s)\n", dep.name, path)
		} else {
			fmt.Printf("  ✗ %s — %s\n", dep.name, dep.hint)
		}
	}

	fmt.Println("\nJump / focus:")
	mode := strings.ToLower(strings.TrimSpace(os.Getenv("CCMON_FOCUS")))
	if mode == "" {
		mode = "auto"
	}
	fmt.Printf("  terminal: %s · CCMON_FOCUS=%s · interactive split-focus %s\n",
		ternary(inGhostty(), "Ghostty", "non-Ghostty"), mode,
		ternary(ghosttyFocusEnabled(), "on", "off"))

	fmt.Printf("\nState dir:     %s (%d instance(s))\n", stateDir(), len(loadAll()))
}

// ternary is a tiny string-only conditional for terse status lines.
func ternary(cond bool, yes, no string) string {
	if cond {
		return yes
	}
	return no
}

// --- Claude Code hooks (settings.json) -------------------------------------

func installClaudeHooks(path, bin string, dry bool) error {
	root, err := readJSONObject(path)
	if err != nil {
		return err
	}

	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		hooks = map[string]any{}
	}
	cmd := bin + " hook"
	for _, ev := range claudeHookEvents {
		arr := stripCcmonHooks(toAnySlice(hooks[ev]))
		arr = append(arr, map[string]any{
			"matcher": "",
			"hooks": []any{
				map[string]any{"type": "command", "command": cmd},
			},
		})
		hooks[ev] = arr
	}
	root["hooks"] = hooks

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	if dry {
		return nil
	}
	return writeWithBackup(path, out)
}

func uninstallClaudeHooks(path string, dry bool) error {
	root, err := readJSONObject(path)
	if err != nil {
		return err
	}
	hooks, _ := root["hooks"].(map[string]any)
	if hooks == nil {
		return nil // nothing wired
	}
	for ev := range hooks {
		arr := stripCcmonHooks(toAnySlice(hooks[ev]))
		if len(arr) == 0 {
			delete(hooks, ev)
		} else {
			hooks[ev] = arr
		}
	}
	if len(hooks) == 0 {
		delete(root, "hooks")
	} else {
		root["hooks"] = hooks
	}

	out, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	if dry {
		return nil
	}
	return writeWithBackup(path, out)
}

// claudeWiredEvents maps each event that has a ccmon hook to that hook's command.
func claudeWiredEvents(path string) map[string]string {
	out := map[string]string{}
	root, err := readJSONObject(path)
	if err != nil {
		return out
	}
	hooks, _ := root["hooks"].(map[string]any)
	for ev, v := range hooks {
		for _, g := range toAnySlice(v) {
			grp, _ := g.(map[string]any)
			for _, h := range toAnySlice(grp["hooks"]) {
				hm, _ := h.(map[string]any)
				if c, _ := hm["command"].(string); isCcmonHookCmd(c) {
					out[ev] = strings.TrimSpace(c)
				}
			}
		}
	}
	return out
}

// stripCcmonHooks drops every ccmon "<bin> hook" entry from a hook-event array,
// removing any group it leaves empty while preserving unrelated hooks.
func stripCcmonHooks(arr []any) []any {
	out := make([]any, 0, len(arr))
	for _, g := range arr {
		grp, ok := g.(map[string]any)
		if !ok {
			out = append(out, g)
			continue
		}
		inner := toAnySlice(grp["hooks"])
		kept := make([]any, 0, len(inner))
		for _, h := range inner {
			if hm, ok := h.(map[string]any); ok {
				if c, _ := hm["command"].(string); isCcmonHookCmd(c) {
					continue // this is ours — drop it
				}
			}
			kept = append(kept, h)
		}
		if len(kept) == 0 {
			continue // the whole group was ours
		}
		grp["hooks"] = kept
		out = append(out, grp)
	}
	return out
}

// isCcmonHookCmd reports whether a hook command is a ccmon hook invocation,
// matching by binary basename so it's robust across install locations.
func isCcmonHookCmd(cmd string) bool {
	cmd = strings.TrimSpace(cmd)
	if !strings.HasSuffix(cmd, " hook") {
		return false
	}
	bin := strings.Trim(strings.TrimSuffix(cmd, " hook"), `"' `)
	return filepath.Base(bin) == "ccmon"
}

// --- Codex notify (config.toml) --------------------------------------------

func codexNotifyLine(bin string) string {
	return fmt.Sprintf("notify = [%q, %q]", bin, "codex-hook")
}

func installCodexNotify(path, bin string, dry bool) (string, error) {
	lines, err := readLines(path)
	if err != nil {
		return "", err
	}
	target := codexNotifyLine(bin)
	idx, kind, _ := findRootNotify(lines)

	var action string
	switch kind {
	case "ccmon":
		if strings.TrimSpace(lines[idx]) == target {
			return "unchanged", nil
		}
		lines[idx] = target
		action = "updated"
	case "other":
		return "skipped", nil
	default:
		lines = insertRootKey(lines, target)
		action = "added"
	}

	if dry {
		return action, nil
	}
	return action, writeWithBackup(path, joinLines(lines))
}

func uninstallCodexNotify(path string, dry bool) error {
	lines, err := readLines(path)
	if err != nil {
		return err
	}
	idx, kind, _ := findRootNotify(lines)
	if kind != "ccmon" {
		return nil // not ours; leave it alone
	}
	lines = append(lines[:idx], lines[idx+1:]...)
	if dry {
		return nil
	}
	return writeWithBackup(path, joinLines(lines))
}

func codexNotifyStatus(path string) (idx int, kind, line string) {
	lines, err := readLines(path)
	if err != nil {
		return -1, "", ""
	}
	return findRootNotify(lines)
}

// findRootNotify locates a root-scope `notify = ...` line (before the first
// `[table]` header) and classifies it as ours ("ccmon") or someone else's.
func findRootNotify(lines []string) (idx int, kind, line string) {
	for i, ln := range lines {
		t := strings.TrimSpace(ln)
		if strings.HasPrefix(t, "[") {
			break // entered a table — root scope is over
		}
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		if key, _, ok := splitTOMLKey(t); ok && key == "notify" {
			if strings.Contains(t, "codex-hook") && strings.Contains(t, "ccmon") {
				return i, "ccmon", ln
			}
			return i, "other", ln
		}
	}
	return -1, "", ""
}

func splitTOMLKey(line string) (key, val string, ok bool) {
	i := strings.IndexByte(line, '=')
	if i < 0 {
		return "", "", false
	}
	return strings.TrimSpace(line[:i]), strings.TrimSpace(line[i+1:]), true
}

// insertRootKey adds a key=value line in root scope: just before the first
// table header, or at the end of an all-root-keys file.
func insertRootKey(lines []string, key string) []string {
	for i, ln := range lines {
		if strings.HasPrefix(strings.TrimSpace(ln), "[") {
			res := make([]string, 0, len(lines)+1)
			res = append(res, lines[:i]...)
			res = append(res, key)
			res = append(res, lines[i:]...)
			return res
		}
	}
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	return append(lines, key)
}

// --- shared helpers ---------------------------------------------------------

type dep struct{ name, hint string }

func deps() []dep {
	return []dep{
		{"terminal-notifier", "brew install terminal-notifier  (required for notifications)"},
		{"tmux", "brew install tmux  (required for pane tracking + jumping)"},
	}
}

func checkDeps() {
	for _, d := range deps() {
		if _, err := exec.LookPath(d.name); err != nil {
			fmt.Printf("⚠ %s not found — %s\n", d.name, d.hint)
		}
	}
}

func selfPath() string {
	p, err := os.Executable()
	if err != nil {
		return "ccmon"
	}
	if rp, err := filepath.EvalSymlinks(p); err == nil {
		p = rp
	}
	return p
}

func hasFlag(args []string, flag string) bool {
	for _, a := range args {
		if a == flag {
			return true
		}
	}
	return false
}

func dirExists(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

// readJSONObject reads a JSON object file, returning an empty object if missing.
func readJSONObject(path string) (map[string]any, error) {
	root := map[string]any{}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return root, nil
		}
		return nil, err
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		return root, nil
	}
	if err := json.Unmarshal(b, &root); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	return root, nil
}

// readLines reads a file into lines, returning nil if it doesn't exist.
func readLines(path string) ([]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return strings.Split(string(b), "\n"), nil
}

func joinLines(lines []string) []byte {
	s := strings.Join(lines, "\n")
	if !strings.HasSuffix(s, "\n") {
		s += "\n"
	}
	return []byte(s)
}

func toAnySlice(v any) []any {
	if s, ok := v.([]any); ok {
		return s
	}
	return nil
}

// writeWithBackup writes data to path atomically, first copying any existing
// file to "<path>.bak" and creating parent directories as needed.
func writeWithBackup(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	if old, err := os.ReadFile(path); err == nil {
		_ = os.WriteFile(path+".bak", old, 0o644)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
