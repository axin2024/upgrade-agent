// Package snapshot captures bounded, read-only repository baselines.
package snapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"
)

const maxFile = 1 << 20
const maxRepo = 8 << 20
const maxFiles = 2_000

type File struct {
	Path    string `json:"path"`
	Content string `json:"content"`
	SHA256  string `json:"sha256"`
}

type Snapshot struct {
	Commit   string            `json:"commit,omitempty"`
	Digest   string            `json:"sha256"`
	Roots    []string          `json:"roots"`
	Files    []File            `json:"files"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

func Digest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func SafePath(name string) bool {
	// Use forward slash paths for portable bundle headers and database keys.
	if name == "." || !fs.ValidPath(name) || !filepath.IsLocal(filepath.FromSlash(name)) || len(name) > 220 ||
		strings.ContainsAny(name, "\\:<>|?*\x00\r\n\t \"'") {
		return false
	}
	for part := range strings.SplitSeq(name, "/") {
		lower := strings.ToLower(part)
		if strings.HasPrefix(part, ".") || lower == "vendor" || lower == "node_modules" ||
			strings.HasSuffix(lower, ".key") || strings.HasSuffix(lower, ".pem") || strings.HasSuffix(lower, ".pfx") {
			return false
		}
	}
	return true
}

func Within(name string, roots []string) bool {
	if !SafePath(name) {
		return false
	}
	for _, root := range roots {
		if name == root || strings.HasPrefix(name, root+"/") {
			return true
		}
	}
	return false
}

func Capture(directory, commit string, roots []string) (Snapshot, error) {
	roots = slices.Clone(roots)
	slices.Sort(roots)
	roots = slices.Compact(roots)
	if len(roots) == 0 || len(roots) > 32 {
		return Snapshot{}, errors.New("pass one to 32 explicitly approved files or subdirectories")
	}
	for index, name := range roots {
		if !SafePath(name) {
			return Snapshot{}, fmt.Errorf("invalid snapshot root %q", name)
		}
		for _, earlier := range roots[:index] {
			if Within(name, []string{earlier}) {
				return Snapshot{}, fmt.Errorf("overlapping roots %q and %q", name, earlier)
			}
		}
	}
	if len(commit) > 64 || strings.ContainsAny(commit, "\x00\r\n") {
		return Snapshot{}, errors.New("invalid commit label")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return Snapshot{}, err
	}
	defer root.Close()
	result := Snapshot{Commit: commit, Roots: roots, Files: make([]File, 0)}
	checkNames := make(map[string]bool)
	var size int
	for _, sub := range roots {
		approved := ""
		for item := range strings.SplitSeq(sub, "/") {
			approved = path.Join(approved, item)
			info, err := root.Lstat(approved)
			if err != nil {
				return Snapshot{}, err
			}
			if info.Mode()&fs.ModeSymlink != 0 {
				return Snapshot{}, fmt.Errorf("symlink in approved snapshot: %s", approved)
			}
		}
		err := fs.WalkDir(root.FS(), sub, func(name string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !SafePath(name) {
				if entry.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if entry.Type()&fs.ModeSymlink != 0 {
				return fmt.Errorf("symlink in approved snapshot: %s", name)
			}
			if entry.IsDir() {
				return nil
			}
			if !entry.Type().IsRegular() {
				return fmt.Errorf("non-regular snapshot entry: %s", name)
			}
			if !sourceFile(name) {
				return nil
			}
			lower := strings.ToLower(name)
			if checkNames[lower] {
				return fmt.Errorf("case-colliding snapshot entry: %s", name)
			}
			checkNames[lower] = true
			info, err := root.Lstat(name)
			if err != nil || !info.Mode().IsRegular() {
				return fmt.Errorf("snapshot entry changed while reading: %s", name)
			}
			if info.Size() > maxFile {
				return fmt.Errorf("file larger than 1 MiB: %s", name)
			}
			input, err := root.Open(name)
			if err != nil {
				return err
			}
			opened, err := input.Stat()
			if err != nil || !os.SameFile(info, opened) {
				input.Close()
				return fmt.Errorf("snapshot entry changed while opening: %s", name)
			}
			data, readErr := io.ReadAll(io.LimitReader(input, maxFile+1))
			if err := errors.Join(readErr, input.Close()); err != nil {
				return err
			}
			if len(data) > maxFile || !utf8.Valid(data) || strings.ContainsRune(string(data), '\x00') {
				return fmt.Errorf("file is not supported UTF-8 text: %s", name)
			}
			size += len(data)
			if size > maxRepo || len(result.Files) >= maxFiles {
				return errors.New("snapshot exceeds 8 MiB or 2000 files")
			}
			result.Files = append(result.Files, File{Path: name, Content: string(data), SHA256: Digest(data)})
			return nil
		})
		if err != nil {
			return Snapshot{}, err
		}
	}
	slices.SortFunc(result.Files, func(a, b File) int { return strings.Compare(a.Path, b.Path) })
	result.Digest = result.fingerprint()
	return result, nil
}

func (s Snapshot) fingerprint() string {
	files := make([][2]string, 0, len(s.Files))
	for _, file := range s.Files {
		files = append(files, [2]string{file.Path, file.SHA256})
	}
	canonical, err := json.Marshal(struct {
		Commit string
		Roots  []string
		Files  [][2]string
	}{s.Commit, s.Roots, files})
	if err != nil {
		return ""
	}
	return Digest(canonical)
}

func (s Snapshot) Verify() error {
	if len(s.Commit) > 64 || strings.ContainsAny(s.Commit, "\x00\r\n") ||
		len(s.Roots) == 0 || len(s.Roots) > 32 || !slices.IsSorted(s.Roots) ||
		len(s.Files) > maxFiles || len(s.Metadata) != 0 {
		return errors.New("invalid snapshot metadata")
	}
	for i, name := range s.Roots {
		if !SafePath(name) || i > 0 && name == s.Roots[i-1] {
			return fmt.Errorf("invalid snapshot root %q", name)
		}
	}
	seen := make(map[string]bool)
	size := 0
	for i, file := range s.Files {
		lower := strings.ToLower(file.Path)
		size += len(file.Content)
		if i > 0 && file.Path <= s.Files[i-1].Path || seen[lower] || !Within(file.Path, s.Roots) ||
			!sourceFile(file.Path) || len(file.Content) > maxFile || size > maxRepo ||
			!utf8.ValidString(file.Content) || strings.ContainsRune(file.Content, '\x00') ||
			Digest([]byte(file.Content)) != file.SHA256 {
			return fmt.Errorf("invalid snapshot file %q", file.Path)
		}
		seen[lower] = true
	}
	if s.Digest == "" || s.Digest != s.fingerprint() {
		return errors.New("snapshot fingerprint does not match file list")
	}
	return nil
}

func sourceFile(name string) bool {
	switch path.Ext(name) {
	case ".php", ".json", ".lock", ".md", ".xml", ".yml", ".yaml", ".txt":
		return true
	default:
		return false
	}
}

func (s Snapshot) Find(name string) (File, bool) {
	index, found := slices.BinarySearchFunc(s.Files, name, func(file File, needle string) int { return strings.Compare(file.Path, needle) })
	if !found {
		return File{}, false
	}
	return s.Files[index], true
}

// Materialize writes a separate fixture directory for a validation backend.
func (s Snapshot) Materialize(directory string, changes map[string]string) error {
	root, err := os.OpenRoot(directory)
	if err != nil {
		return err
	}
	defer root.Close()
	for _, file := range s.Files {
		if !Within(file.Path, s.Roots) {
			return errors.New("invalid persisted snapshot path")
		}
		content := file.Content
		if replacement, ok := changes[file.Path]; ok {
			content = replacement
		}
		if err := root.MkdirAll(path.Dir(file.Path), 0o755); err != nil {
			return err
		}
		output, err := root.OpenFile(file.Path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return err
		}
		_, err = io.WriteString(output, content)
		if err := errors.Join(err, output.Close()); err != nil {
			return err
		}
	}
	return nil
}
