package cli

import (
	"regexp"
	"strings"
	"unicode"
)

// maxLogLine is the longest log line printed; longer lines are cut.
const maxLogLine = 500

// logEscape matches terminal escape sequences: DCS, SOS, PM and APC strings (up to the end of the
// line), CSI, OSC and two-byte escapes.
var logEscape = regexp.MustCompile("\x1b(?:[PX^_][^\x1b\n]*(?:\x1b\\\\)?|\\[[0-?]*[ -/]*[@-~]|\\][^\x07\x1b]*(?:\x07|\x1b\\\\)?|[@-Z\\\\-_])")

// stripUnsafe removes escape sequences and characters that can rewrite what a terminal shows: control
// characters (tab is kept) and format characters such as bidi overrides and zero-width characters.
func stripUnsafe(s string) string {
	s = strings.ToValidUTF8(s, "\uFFFD")
	s = logEscape.ReplaceAllString(s, "")
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\t' || r == '\n':
			return r
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r):
			return -1
		}
		return r
	}, s)
}

// cleanLine makes untrusted text (an API error, an event message) safe to print on one line.
func cleanLine(s string) string {
	return strings.Join(strings.Fields(stripUnsafe(s)), " ")
}

// sanitizeLog splits untrusted container output into printable lines: trailing blank lines are dropped
// and long lines are cut.
func sanitizeLog(text string) []string {
	lines := strings.Split(stripUnsafe(text), "\n")
	for i, l := range lines {
		if r := []rune(l); len(r) > maxLogLine {
			lines[i] = string(r[:maxLogLine]) + "…"
		}
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}
