package planning

import (
	"fmt"
	"io"
	"strings"

	"github.com/axin2024/upgrade-agent/internal/snapshot"
)

// Diff returns a Git-compatible patch from an approved baseline.
func Diff(s snapshot.Snapshot, candidates ...Candidate) (string, error) {
	if _, err := Overlay(s, candidates...); err != nil {
		return "", err
	}
	var out strings.Builder
	seen := make(map[string]bool)
	for _, candidate := range candidates {
		for _, change := range candidate.Changes {
			if seen[change.Path] {
				continue
			}
			seen[change.Path] = true
			before, ok := s.Find(change.Path)
			if !ok || !snapshot.SafePath(change.Path) {
				return "", fmt.Errorf("unavailable diff path %q", change.Path)
			}
			writeHunk(&out, change.Path, before.Content, change.Content)
		}
	}
	return out.String(), nil
}

func writeHunk(out *strings.Builder, name, oldText, newText string) {
	before, after := lines(oldText), lines(newText)
	start, tail := 0, 0
	for start < min(len(before), len(after)) && before[start] == after[start] {
		start++
	}
	for tail < min(len(before)-start, len(after)-start) && before[len(before)-1-tail] == after[len(after)-1-tail] {
		tail++
	}
	lo, padding := max(0, start-3), min(3, tail)
	oldEnd, newEnd := len(before)-tail, len(after)-tail
	fmt.Fprintf(out, "diff --git a/%s b/%s\n--- a/%s\n+++ b/%s\n", name, name, name, name)
	fmt.Fprintf(out, "@@ -%s +%s @@\n", hunkRange(lo, oldEnd+padding), hunkRange(lo, newEnd+padding))
	writeLines(out, " ", before[lo:start])
	writeLines(out, "-", before[start:oldEnd])
	writeLines(out, "+", after[start:newEnd])
	writeLines(out, " ", before[oldEnd:oldEnd+padding])
}

func hunkRange(first, end int) string {
	count := end - first
	if count == 0 {
		return fmt.Sprintf("%d,0", first)
	}
	return fmt.Sprintf("%d,%d", first+1, count)
}

func lines(text string) []string {
	if text == "" {
		return nil
	}
	list := strings.SplitAfter(text, "\n")
	if list[len(list)-1] == "" {
		list = list[:len(list)-1]
	}
	return list
}

func writeLines(out io.StringWriter, marker string, lines []string) {
	for _, line := range lines {
		out.WriteString(marker)
		out.WriteString(line)
		if !strings.HasSuffix(line, "\n") {
			out.WriteString("\n\\ No newline at end of file\n")
		}
	}
}
