package main

import (
	"strings"
	"testing"
)

func TestEscapeAppleScript(t *testing.T) {
	cases := map[string]string{
		`/Users/jpoz/dev`:   `/Users/jpoz/dev`,
		`/tmp/has "quotes"`: `/tmp/has \"quotes\"`,
		`/tmp/back\slash`:   `/tmp/back\\slash`,
		`/a/"q"\and\slash`:  `/a/\"q\"\\and\\slash`,
		``:                  ``,
	}
	for in, want := range cases {
		if got := escapeAppleScript(in); got != want {
			t.Errorf("escapeAppleScript(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestGhosttyFocusScript(t *testing.T) {
	s := ghosttyFocusScript("/Users/jpoz/proj")

	// Must drive Ghostty and bail cleanly when there's nothing to focus.
	for _, want := range []string{
		`tell application "Ghostty"`,
		"activate",
		"if (count of windows) is 0 then return",
		"focused terminal of theTab",
		"if (count of sibs) is 0 then return",
		"focus (item 1 of sibs)", // sibling fallback
		"end tell",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("script missing %q\n--- script ---\n%s", want, s)
		}
	}

	// The cwd preference must embed the (escaped) target path.
	if !strings.Contains(s, `is "/Users/jpoz/proj"`) {
		t.Errorf("script missing cwd preference for the target path:\n%s", s)
	}
}

func TestGhosttyFocusScriptEscapesCwd(t *testing.T) {
	s := ghosttyFocusScript(`/tmp/a "b"`)
	if !strings.Contains(s, `is "/tmp/a \"b\""`) {
		t.Errorf("cwd not escaped into the script literal:\n%s", s)
	}
	// A stray unescaped quote would prematurely close the AppleScript literal.
	if strings.Contains(s, `is "/tmp/a "b""`) {
		t.Errorf("script contains an unescaped quote that would break parsing:\n%s", s)
	}
}

func TestInGhostty(t *testing.T) {
	t.Setenv("GHOSTTY_RESOURCES_DIR", "")
	t.Setenv("GHOSTTY_BIN_DIR", "")
	if inGhostty() {
		t.Fatal("inGhostty() = true with no Ghostty env vars set")
	}

	t.Setenv("GHOSTTY_RESOURCES_DIR", "/Applications/Ghostty.app/Contents/Resources/ghostty")
	if !inGhostty() {
		t.Error("inGhostty() = false with GHOSTTY_RESOURCES_DIR set")
	}

	t.Setenv("GHOSTTY_RESOURCES_DIR", "")
	t.Setenv("GHOSTTY_BIN_DIR", "/Applications/Ghostty.app/Contents/MacOS")
	if !inGhostty() {
		t.Error("inGhostty() = false with GHOSTTY_BIN_DIR set")
	}
}

func TestGhosttyFocusEnabled(t *testing.T) {
	// Force a known non-Ghostty baseline so "auto" is deterministic.
	clearGhosttyEnv := func() {
		t.Setenv("GHOSTTY_RESOURCES_DIR", "")
		t.Setenv("GHOSTTY_BIN_DIR", "")
	}

	tests := []struct {
		name    string
		focus   string
		ghostty bool // whether to set a Ghostty env var
		want    bool
	}{
		{"auto outside ghostty", "", false, false},
		{"auto inside ghostty", "", true, true},
		{"explicit auto inside ghostty", "auto", true, true},
		{"off forces disable in ghostty", "off", true, false},
		{"none forces disable", "none", true, false},
		{"zero forces disable", "0", true, false},
		{"ghostty forces enable outside ghostty", "ghostty", false, true},
		{"on forces enable outside ghostty", "on", false, true},
		{"unknown value falls back to auto", "wat", false, false},
		{"case insensitive OFF", "OFF", true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearGhosttyEnv()
			if tc.ghostty {
				t.Setenv("GHOSTTY_RESOURCES_DIR", "/x")
			}
			t.Setenv("CCMON_FOCUS", tc.focus)
			if got := ghosttyFocusEnabled(); got != tc.want {
				t.Errorf("ghosttyFocusEnabled() with CCMON_FOCUS=%q ghostty=%v = %v, want %v",
					tc.focus, tc.ghostty, got, tc.want)
			}
		})
	}
}
