package forge

import (
	"strings"
	"unicode/utf8"
)

// FailureKeywords are the substrings a log line has to carry, matched
// case-insensitively, to be judged worth keeping in a [FilterLog] excerpt:
// the vocabulary a test runner or a build tool uses to name its own
// failure, not the forge's decoration around it (mill#95).
var FailureKeywords = []string{"Error", "FAIL", "✘", "expected", "Received", "exit code"}

// MaxExcerptBytes is the most a [FilterLog] excerpt may be, per check: a log
// that stays this size after filtering fits inside a revise and a park body
// without approaching skift's own comment and body limits
// (internal/run/breaker.go).
const MaxExcerptBytes = 8 << 10

// contextLines is how many lines around a matching line survive with it. A
// test runner's assertion line (`Error: expect(...).toBeDisabled() failed`)
// and the test's own name and location, usually a line or two above it, are
// two lines of the same fact — filtering to the assertion alone and
// dropping the name answers a question nobody asked.
const contextLines = 3

// FilterLog reduces raw log bytes to the lines that name a failure, in the
// order they appeared, capped to [MaxExcerptBytes]. A log with no matching
// line returns empty: there is nothing here worth quoting back.
//
// Matches keep contextLines of surrounding text on each side, and runs of
// kept lines that touch or overlap are merged, so two matches close
// together read as one block rather than a repeated fragment; a gap
// between two blocks is marked with "…" so a reader knows text was cut,
// not that the log simply ended there.
//
// When the merged, kept text still exceeds the cap, the tail survives — the
// same rule internal/run/breaker.go's own trimTail applies to a park's
// findings, because the part of a log closest to where it stopped is the
// part most likely to explain why.
func FilterLog(raw []byte) string {
	text := strings.TrimRight(string(raw), "\n")
	if text == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	keep := make([]bool, len(lines))
	matched := false
	for i, line := range lines {
		if !matchesFailure(line) {
			continue
		}
		matched = true
		for j := max(0, i-contextLines); j <= min(len(lines)-1, i+contextLines); j++ {
			keep[j] = true
		}
	}
	if !matched {
		return ""
	}

	var b strings.Builder
	gap := false
	for i, line := range lines {
		if !keep[i] {
			gap = true
			continue
		}
		if gap && b.Len() > 0 {
			b.WriteString("…\n")
		}
		gap = false
		b.WriteString(line)
		b.WriteString("\n")
	}
	excerpt := strings.TrimRight(b.String(), "\n")
	return tailBytes(excerpt, MaxExcerptBytes)
}

// matchesFailure reports whether line carries any of [FailureKeywords].
func matchesFailure(line string) bool {
	lower := strings.ToLower(line)
	for _, keyword := range FailureKeywords {
		if strings.Contains(lower, strings.ToLower(keyword)) {
			return true
		}
	}
	return false
}

// tailBytes keeps the last budget bytes of text, on a rune boundary. Text
// already within budget is returned unchanged.
func tailBytes(text string, budget int) string {
	if len(text) <= budget {
		return text
	}
	cut := len(text) - budget
	for cut < len(text) && !utf8.RuneStart(text[cut]) {
		cut++
	}
	return text[cut:]
}
