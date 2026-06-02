package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const ghosttyBundle = "com.mitchellh.ghostty"

// Per-kind notification sounds, overridable by env var (set them in your shell
// profile, then restart Claude/Codex sessions so the hooks inherit them). A
// value of none/off/silent/mute mutes that kind. Preview with
// `ccmon test-notification [--done|--nag]`.
const (
	envSoundNeeds = "CCMON_SOUND_NEEDS" // needs-input alert
	envSoundDone  = "CCMON_SOUND_DONE"  // turn/task finished
	envSoundNag   = "CCMON_SOUND_NAG"   // still-waiting reminder

	soundNeedsDefault = "Bottle"    // /System/Library/Sounds/Bottle.aiff
	soundDoneDefault  = "Blow"      // /System/Library/Sounds/Blow.aiff
	soundNagDefault   = "Submarine" // /System/Library/Sounds/Submarine.aiff
)

// validSounds are the bundled macOS alert sounds terminal-notifier accepts via
// -sound (from /System/Library/Sounds). macOS silently ignores any other name,
// so test-notification lists these.
var validSounds = []string{
	"Basso", "Blow", "Bottle", "Frog", "Funk", "Glass", "Hero",
	"Morse", "Ping", "Pop", "Purr", "Sosumi", "Submarine", "Tink",
}

func soundOrNone(s string) string {
	if s == "" {
		return "(none)"
	}
	return s
}

// soundFor resolves the sound name for a kind: the env override if set, else the
// default. none/off/silent/mute (any case) resolve to "" — no sound.
func soundFor(envKey, def string) string {
	v := strings.TrimSpace(os.Getenv(envKey))
	if v == "" {
		return def
	}
	switch strings.ToLower(v) {
	case "none", "off", "silent", "mute":
		return ""
	}
	return v
}

// notify raises the initial notification for a state change, via whichever
// backend is selected (see deliver).
func notify(i *Instance, kind string) {
	switch kind {
	case StateNeedsInput:
		deliver(i, "⚠ needs your input", soundFor(envSoundNeeds, soundNeedsDefault), false)
	case StateDone:
		deliver(i, "✓ done", soundFor(envSoundDone, soundDoneDefault), false)
	default:
		deliver(i, kind, soundFor(envSoundNeeds, soundNeedsDefault), false)
	}
}

// renotify re-alerts a session still stuck in needs-input. It removes the prior
// banner first so the new one reliably re-shows and re-plays its sound — posting
// to an existing group can otherwise update silently.
func renotify(i *Instance) {
	deliver(i, "⏰ still waiting · "+dur(i.Since), soundFor(envSoundNag, soundNagDefault), true)
}

// deliver routes a notification to the selected backend. terminal-notifier is
// the reliable default (independent of tmux/attach state); OSC-777 is the
// classic escape-sequence path — lighter, but it only reaches you when the
// target pane is live in the attached client and the terminal supports it.
func deliver(i *Instance, subtitle, sound string, replace bool) {
	if notifyBackend() == backendOSC777 {
		sendOSC777(i, subtitle)
		return
	}
	sendNotify(i, subtitle, sound, replace)
}

// clearNotification dismisses any banner for an instance (used on ack / jump).
func clearNotification(id string) {
	if bin, err := exec.LookPath("terminal-notifier"); err == nil {
		_ = exec.Command(bin, "-remove", "ccmon-"+id).Run()
	}
}

// muteFlagPath is the sentinel file whose presence means "silence notification
// sounds". It lives in ~/.ccmon so it's shared between the TUI (which toggles it
// with `m`) and the separate hook processes that actually post notifications.
func muteFlagPath() string {
	return filepath.Join(filepath.Dir(stateDir()), "muted")
}

func isMuted() bool {
	_, err := os.Stat(muteFlagPath())
	return err == nil
}

// setMuted creates or removes the mute flag; removing a missing flag is a no-op.
func setMuted(mute bool) error {
	if mute {
		return os.WriteFile(muteFlagPath(), []byte("1\n"), 0o644)
	}
	if err := os.Remove(muteFlagPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Notification backend, persisted in ~/.ccmon so the TUI (which toggles it with
// `n`) and the separate hook processes agree. The file is read live per
// notification, so a toggle takes effect without restarting sessions.
const (
	backendTerminalNotifier = "terminal-notifier"
	backendOSC777           = "osc777"
)

func backendFlagPath() string { return filepath.Join(filepath.Dir(stateDir()), "notify-backend") }

func notifyBackend() string {
	if b, err := os.ReadFile(backendFlagPath()); err == nil &&
		strings.TrimSpace(string(b)) == backendOSC777 {
		return backendOSC777
	}
	return backendTerminalNotifier
}

// setNotifyBackend writes the choice; terminal-notifier (the default) is encoded
// by removing the file so a fresh install needs no flag.
func setNotifyBackend(b string) error {
	if b == backendOSC777 {
		return os.WriteFile(backendFlagPath(), []byte(backendOSC777+"\n"), 0o644)
	}
	if err := os.Remove(backendFlagPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func backendLabel(b string) string {
	if b == backendOSC777 {
		return "OSC-777"
	}
	return "terminal-notifier"
}

// notifyTitle is the banner title for an instance, e.g. "Claude · ace".
func notifyTitle(i *Instance) string {
	tool := "Claude"
	if i.Source == "codex" {
		tool = "Codex"
	}
	if i.Project != "" {
		return tool + " · " + i.Project
	}
	return tool
}

// sendOSC777 emits a desktop notification via the OSC-777 escape sequence,
// ignoring sound (the terminal decides that). Mute can't silence it, so muting
// drops it entirely.
func sendOSC777(i *Instance, subtitle string) {
	if isMuted() {
		return
	}
	body := subtitle
	if i.Msg != "" && i.Msg != subtitle {
		body = subtitle + " — " + i.Msg
	}
	_, _ = writeOSC777(i, notifyTitle(i), body)
}

// writeOSC777 writes the OSC-777 "notify" sequence to the instance's pane tty
// (wrapped in tmux passthrough when the pane is in tmux), falling back to
// /dev/tty. It returns the tty it wrote to so the test command can report it.
func writeOSC777(i *Instance, title, body string) (string, error) {
	seq := fmt.Sprintf("\033]777;notify;%s;%s\a", sanitizeOSC(title), sanitizeOSC(body))
	tty, inTmux := "/dev/tty", false
	if i != nil && i.PaneID != "" {
		if t, err := tmux(i.Socket, "display-message", "-p", "-t", i.PaneID, "#{pane_tty}"); err == nil && t != "" {
			tty, inTmux = t, true
		}
	}
	if inTmux {
		seq = tmuxPassthrough(seq) // let tmux forward the escape to the outer terminal
	}
	f, err := os.OpenFile(tty, os.O_WRONLY, 0)
	if err != nil {
		return tty, err
	}
	defer f.Close()
	_, err = f.WriteString(seq)
	return tty, err
}

// tmuxPassthrough wraps a sequence so tmux forwards it verbatim to the outer
// terminal (requires `allow-passthrough on`). Inner ESC bytes must be doubled.
func tmuxPassthrough(s string) string {
	return "\033Ptmux;" + strings.ReplaceAll(s, "\033", "\033\033") + "\033\\"
}

// sanitizeOSC strips bytes that would prematurely terminate or break the OSC
// sequence (control chars, ESC/BEL) and swaps the field separator ';' for ','.
func sanitizeOSC(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == ';':
			b.WriteRune(',')
		case r == '\033' || r == '\a' || r < 0x20:
			// drop
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func sendNotify(i *Instance, subtitle, sound string, replace bool) {
	debugLog(i, subtitle)
	bin, err := exec.LookPath("terminal-notifier")
	if err != nil {
		return // not installed; silently skip
	}
	if isMuted() {
		sound = "" // muted: keep the banner, drop the sound
	}
	if replace {
		_ = exec.Command(bin, "-remove", "ccmon-"+i.ID).Run()
	}
	// Detach so the caller returns immediately: with -execute, terminal-notifier
	// stays alive until the banner is clicked, so we must not wait on it.
	cmd := exec.Command(bin, notifyArgs(i, subtitle, sound, true, true)...)
	_ = cmd.Start()
	go func() { _ = cmd.Wait() }()
}

// notifyArgs builds the terminal-notifier argument list for an instance.
//
// It deliberately does NOT pass `-sender <bundle>`. Masquerading as Ghostty's
// bundle id makes terminal-notifier hang and never deliver — the sender app has
// to own notification permission and the association is flaky on recent macOS —
// which was the cause of "ccmon never notifies me". `-activate` still brings
// Ghostty forward on click and `-execute` still jumps to the pane, so we keep
// the useful behavior without the masquerade. The two flags let the synchronous
// test path drop the bits that would keep terminal-notifier alive (-execute) or
// that aren't needed for a bare delivery check (-activate).
func notifyArgs(i *Instance, subtitle, sound string, activate, execute bool) []string {
	tool := "Claude"
	if i.Source == "codex" {
		tool = "Codex"
	}
	title := tool
	if i.Project != "" {
		title = tool + " · " + i.Project
	}
	msg := i.Msg
	if msg == "" {
		msg = subtitle
	}

	args := []string{
		"-title", title,
		"-subtitle", subtitle,
		"-message", msg,
		"-group", "ccmon-" + i.ID, // one entry per instance; reminders replace it
	}
	if activate {
		args = append(args, "-activate", ghosttyBundle) // click brings Ghostty forward
	}
	if self, _ := os.Executable(); execute && self != "" {
		// -execute runs via `sh -c`; quote defensively.
		args = append(args, "-execute", shellQuote(self)+" jump "+shellQuote(i.ID))
	}
	if sound != "" {
		args = append(args, "-sound", sound)
	}
	return args
}

// runTestNotification fires a single notification through the real send path so
// you can confirm delivery and audition sounds by hand. It runs terminal-notifier
// synchronously and surfaces its output/exit status, then lists the configurable
// sounds. Flags: --done / --nag pick which kind's sound to play (default
// needs-input); --plain drops the Ghostty click-activation for a bare delivery
// check. It ignores the mute flag so you can still hear a sound while muted.
func runTestNotification(args []string) {
	plain, osc, kind := false, false, StateNeedsInput
	for _, a := range args {
		switch a {
		case "--plain":
			plain = true
		case "--osc", "--osc777":
			osc = true
		case "--done":
			kind = StateDone
		case "--nag":
			kind = "nag"
		case "--needs", "--needs-input":
			kind = StateNeedsInput
		}
	}

	// Resolve subtitle + sound exactly as the live path would for this kind.
	var subtitle, sound string
	switch kind {
	case StateDone:
		subtitle, sound = "✓ done", soundFor(envSoundDone, soundDoneDefault)
	case "nag":
		subtitle, sound = "⏰ still waiting", soundFor(envSoundNag, soundNagDefault)
	default:
		subtitle, sound = "⚠ needs your input", soundFor(envSoundNeeds, soundNeedsDefault)
	}

	if osc {
		testOSC777(kind, subtitle)
		return
	}

	bin, err := exec.LookPath("terminal-notifier")
	if err != nil {
		fmt.Fprintln(os.Stderr, "✗ terminal-notifier not found on $PATH — notifications are silently skipped without it.")
		fmt.Fprintln(os.Stderr, "  install it with:  brew install terminal-notifier")
		os.Exit(1)
	}
	fmt.Printf("✓ terminal-notifier: %s\n", bin)

	i := &Instance{
		ID:      "test",
		Source:  "claude",
		Project: "ccmon",
		Msg:     "test notification — if you can see this, ccmon can reach you",
	}
	// execute=false: keep it synchronous so we can report the exit status (the
	// click-to-jump handler would otherwise keep terminal-notifier alive).
	na := notifyArgs(i, subtitle, sound, !plain, false)
	fmt.Printf("→ kind=%s  sound=%s\n", kind, soundOrNone(sound))
	if isMuted() {
		fmt.Println("  (ccmon is MUTED — live notifications are silent; this test plays anyway)")
	}

	// Bound the run: terminal-notifier should post and exit promptly. If it
	// doesn't, that hang is itself the diagnosis — masquerading as a sender that
	// lacks notification permission can wedge it — so we surface that rather than
	// blocking forever (the live path sidesteps this by never waiting on it).
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, na...).CombinedOutput()
	if s := strings.TrimSpace(string(out)); s != "" {
		fmt.Println(s)
	}

	switch {
	case ctx.Err() == context.DeadlineExceeded:
		fmt.Fprintln(os.Stderr, "✗ terminal-notifier didn't return within 6s — unexpected.")
		fmt.Fprintln(os.Stderr, "  (ccmon no longer uses the -sender masquerade that used to wedge here.)")
		os.Exit(1)
	case err != nil:
		fmt.Fprintf(os.Stderr, "✗ terminal-notifier failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Println("✓ fired and exited cleanly. You should see a banner titled “Claude · ccmon”.")

	fmt.Println("\nSounds (set in your shell profile, then restart Claude/Codex sessions):")
	fmt.Printf("  %-18s %-7s (default %s)\n", envSoundNeeds, soundOrNone(soundFor(envSoundNeeds, soundNeedsDefault)), soundNeedsDefault)
	fmt.Printf("  %-18s %-7s (default %s)\n", envSoundDone, soundOrNone(soundFor(envSoundDone, soundDoneDefault)), soundDoneDefault)
	fmt.Printf("  %-18s %-7s (default %s)\n", envSoundNag, soundOrNone(soundFor(envSoundNag, soundNagDefault)), soundNagDefault)
	fmt.Println("  values: " + strings.Join(validSounds, " "))
	fmt.Println("  or none/off to mute that kind · preview others with --done / --nag")

	fmt.Println("\nNo banner? macOS drops them silently — check, in order:")
	fmt.Println("  1. System Settings → Notifications → terminal-notifier → Allow Notifications ON")
	fmt.Println("  2. turn off Do Not Disturb / Focus (it suppresses banners but not the sound)")
	fmt.Println("  3. expand the notification stack — repeats can collapse into a summary")
}

// testOSC777 sends a test notification via the OSC-777 escape to the current
// pane, reporting where it wrote and the tmux passthrough prerequisite.
func testOSC777(kind, subtitle string) {
	sock, pane, sess, win, winName, _ := currentCoords()
	i := &Instance{
		ID: "test", Source: "claude", Project: "ccmon",
		Msg:    "OSC-777 test — if you see this, your terminal got the escape",
		Socket: sock, PaneID: pane, Session: sess, Window: win, WinName: winName,
	}
	body := subtitle + " — " + i.Msg

	fmt.Printf("→ kind=%s  via OSC-777 (sound is the terminal's job, not ccmon's)\n", kind)
	if pane != "" {
		fmt.Println("  in tmux: wrapping in passthrough, writing to the pane tty")
		if v, err := tmux(sock, "show", "-gv", "allow-passthrough"); err == nil && v != "on" && v != "all" {
			fmt.Printf("  ⚠ tmux allow-passthrough=%q — the escape won't pass through.\n", v)
			fmt.Println("    enable once with:  tmux set -g allow-passthrough on")
		}
	} else {
		fmt.Println("  not in tmux: writing OSC-777 straight to /dev/tty")
	}

	tty, err := writeOSC777(i, notifyTitle(i), body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "✗ couldn't write OSC-777 to %s: %v\n", tty, err)
		os.Exit(1)
	}
	fmt.Printf("✓ wrote OSC-777 to %s\n", tty)
	fmt.Println("\nHeads up: OSC-777 only shows when the target pane is in the attached client")
	fmt.Println("and your terminal supports it — that's why terminal-notifier is the reliable")
	fmt.Println("default. Toggle the live backend with `n` in the TUI.")
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// debugLog appends a line per notification when CCMON_DEBUG is set — handy for
// confirming the nag loop fires.
func debugLog(i *Instance, subtitle string) {
	if os.Getenv("CCMON_DEBUG") == "" {
		return
	}
	p := filepath.Join(filepath.Dir(stateDir()), "notify.log")
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "%d %-6s %-10s %s | %s\n", now(), i.Source, i.Project, subtitle, i.Msg)
}
