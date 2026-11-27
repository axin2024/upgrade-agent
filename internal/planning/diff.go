package planning

import (
	"fmt"
	"io"
	"slices"
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
	writePatch(out, name, before, after, true, true)
}

type SnapshotHunk struct {
	Path      string `json:"path"`
	Line      int    `json:"line"`
	Before    string `json:"before,omitempty"`
	After     string `json:"after,omitempty"`
	BeforeSHA string `json:"before_sha256,omitempty"`
	AfterSHA  string `json:"after_sha256,omitempty"`
	Patch     string `json:"patch"`
}

// SnapshotHunks makes reviewable Git patches for the exact differences between two frozen snapshots.
func SnapshotHunks(old, next snapshot.Snapshot, allowed []string) ([]SnapshotHunk, error) {
	paths := make([]string, 0, len(old.Files)+len(next.Files))
	for _, s := range []snapshot.Snapshot{old, next} {
		for _, file := range s.Files {
			paths = append(paths, file.Path)
		}
	}
	slices.Sort(paths)
	result := make([]SnapshotHunk, 0)
	for _, name := range slices.Compact(paths) {
		previous, before := old.Find(name)
		current, after := next.Find(name)
		if previous.SHA256 == current.SHA256 {
			continue
		}
		if !snapshot.Within(name, allowed) {
			return nil, fmt.Errorf("changed path %q is outside the repository export scope", name)
		}
		var patch strings.Builder
		first, removed, added := writePatch(&patch, name, lines(previous.Content), lines(current.Content), before, after)
		result = append(result, SnapshotHunk{Path: name, Line: first + 1, Before: removed, After: added,
			BeforeSHA: previous.SHA256, AfterSHA: current.SHA256, Patch: patch.String()})
	}
	return result, nil
}

func writePatch(out *strings.Builder, name string, before, after []string, existed, exists bool) (int, string, string) {
	start, tail := 0, 0
	for start < min(len(before), len(after)) && before[start] == after[start] {
		start++
	}
	for tail < min(len(before)-start, len(after)-start) && before[len(before)-1-tail] == after[len(after)-1-tail] {
		tail++
	}
	lo, padding := max(0, start-3), min(3, tail)
	oldEnd, newEnd := len(before)-tail, len(after)-tail
	fmt.Fprintf(out, "diff --git a/%s b/%s\n", name, name)
	from, to := "a/"+name, "b/"+name
	if !existed {
		fmt.Fprintln(out, "new file mode 100644")
		from = "/dev/null"
	}
	if !exists {
		fmt.Fprintln(out, "deleted file mode 100644")
		to = "/dev/null"
	}
	if (len(before) == 0 && !exists) || (len(after) == 0 && !existed) {
		return 0, "", ""
	}
	fmt.Fprintf(out, "--- %s\n+++ %s\n", from, to)
	fmt.Fprintf(out, "@@ -%s +%s @@\n", hunkRange(lo, oldEnd+padding), hunkRange(lo, newEnd+padding))
	writeLines(out, " ", before[lo:start])
	writeLines(out, "-", before[start:oldEnd])
	writeLines(out, "+", after[start:newEnd])
	writeLines(out, " ", before[oldEnd:oldEnd+padding])
	return start, strings.Join(before[start:oldEnd], ""), strings.Join(after[start:newEnd], "")
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
