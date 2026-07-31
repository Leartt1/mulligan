// Package repo holds checks about the repository itself rather than about any
// one component.
package repo

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Every Go file has to be tracked by git.
//
// This exists because one was not. .gitignore carried an unanchored
// "coverage.*", meant for Go's coverage output, which also matched
// internal/store/coverage.go — so the file was never committed, `git add -A`
// skipped it without a word, and the pushed tree did not build for a dozen
// commits while every local test passed.
//
// The failure is invisible from inside a working tree, which is exactly why it
// needs a test rather than a habit.
func TestEveryGoFileIsTrackedByGit(t *testing.T) {
	root := repoRoot(t)

	// --others lists untracked files, --exclude-standard applies .gitignore. A Go
	// file appearing here is one that a fresh clone would not have.
	out, err := run(t, root, "git", "ls-files", "--others", "--exclude-standard")
	if err != nil {
		t.Fatalf("listing untracked files: %v", err)
	}

	var missing []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasSuffix(line, ".go") {
			missing = append(missing, line)
		}
	}

	if len(missing) > 0 {
		t.Errorf("these Go files are not in the repository, so a fresh clone would not build:\n  %s\n"+
			"if one is ignored on purpose, say why here; otherwise git add it",
			strings.Join(missing, "\n  "))
	}
}

// No Go file may be matched by .gitignore.
//
// The check above catches a file that is untracked today. This one catches the
// rule that would silently drop the next one, which is the part that actually
// went wrong: a file already committed stays tracked even when a later ignore
// rule matches it, so the pattern can sit there harmlessly until someone adds a
// file with the wrong name.
func TestNoGoFileIsMatchedByGitignore(t *testing.T) {
	root := repoRoot(t)

	out, err := run(t, root, "git", "ls-files", "--cached")
	if err != nil {
		t.Fatalf("listing files: %v", err)
	}

	var goFiles []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.HasSuffix(line, ".go") {
			goFiles = append(goFiles, line)
		}
	}
	if len(goFiles) == 0 {
		t.Fatal("found no Go files to check, so this test is not testing anything")
	}

	// --no-index is essential: without it check-ignore says nothing about a file
	// already in the index, so a pattern matching a committed file would look
	// harmless right up until someone adds a new file with the same shape of name.
	args := append([]string{"check-ignore", "--no-index"}, goFiles...)
	matched, _ := run(t, root, "git", args...)

	if trimmed := strings.TrimSpace(matched); trimmed != "" {
		t.Errorf(".gitignore matches these Go files, so committing them needs -f "+
			"and forgetting to would leave the tree unbuildable:\n  %s",
			strings.ReplaceAll(trimmed, "\n", "\n  "))
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()

	out, err := run(t, ".", "git", "rev-parse", "--show-toplevel")
	if err != nil {
		t.Skipf("not a git repository: %v", err)
	}
	return filepath.Clean(strings.TrimSpace(out))
}

func run(t *testing.T, dir string, name string, args ...string) (string, error) {
	t.Helper()

	cmd := exec.Command(name, args...)
	cmd.Dir = dir

	// Only stdout: git writes diagnostics to stderr, and folding them in would
	// make a filename out of a warning.
	out, err := cmd.Output()
	return string(out), err
}
