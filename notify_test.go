package main

import "testing"

func TestSoundFor(t *testing.T) {
	const key = "CCMON_TEST_SOUND"

	// Unset → the default (assert before any Setenv so the key is clean).
	if got := soundFor(key, "Glass"); got != "Glass" {
		t.Fatalf("unset should yield the default, got %q", got)
	}

	t.Setenv(key, "Hero")
	if got := soundFor(key, "Glass"); got != "Hero" {
		t.Fatalf("env override ignored, got %q", got)
	}

	for _, v := range []string{"none", "OFF", "Silent", "mute"} {
		t.Setenv(key, v)
		if got := soundFor(key, "Glass"); got != "" {
			t.Fatalf("%q should mute (empty), got %q", v, got)
		}
	}
}

// The mute flag must survive a toggle round-trip and tolerate clearing when it's
// already absent. muteFlagPath() points at the real ~/.ccmon, so we snapshot and
// restore the user's actual state.
func TestMuteFlagRoundtrip(t *testing.T) {
	orig := isMuted()
	t.Cleanup(func() { _ = setMuted(orig) })

	if err := setMuted(true); err != nil || !isMuted() {
		t.Fatalf("setMuted(true): err=%v muted=%v", err, isMuted())
	}
	if err := setMuted(false); err != nil || isMuted() {
		t.Fatalf("setMuted(false): err=%v muted=%v", err, isMuted())
	}
	if err := setMuted(false); err != nil {
		t.Fatalf("clearing an absent flag should be a no-op, got %v", err)
	}
}

func TestNotifyBackendRoundtrip(t *testing.T) {
	orig := notifyBackend()
	t.Cleanup(func() { _ = setNotifyBackend(orig) })

	if err := setNotifyBackend(backendOSC777); err != nil || notifyBackend() != backendOSC777 {
		t.Fatalf("set osc777: err=%v backend=%q", err, notifyBackend())
	}
	if err := setNotifyBackend(backendTerminalNotifier); err != nil || notifyBackend() != backendTerminalNotifier {
		t.Fatalf("set terminal-notifier: err=%v backend=%q", err, notifyBackend())
	}
}

func TestSanitizeOSC(t *testing.T) {
	// ';' (the field separator) becomes ',', and ESC/BEL/control bytes are dropped
	// so they can't break or prematurely end the sequence.
	if got := sanitizeOSC("a;b\033c\ad\ne"); got != "a,bcde" {
		t.Fatalf("sanitizeOSC = %q", got)
	}
	if got := sanitizeOSC("Claude · ace"); got != "Claude · ace" {
		t.Fatalf("unicode should pass through, got %q", got)
	}
}

func TestTmuxPassthrough(t *testing.T) {
	// Inner ESC bytes must be doubled, wrapped in DCS tmux;…ST.
	got := tmuxPassthrough("\033]777;notify;t;b\a")
	want := "\033Ptmux;\033\033]777;notify;t;b\a\033\\"
	if got != want {
		t.Fatalf("tmuxPassthrough = %q, want %q", got, want)
	}
}
