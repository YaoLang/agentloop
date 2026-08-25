package eval

import (
	"fmt"
	"os"
	"path/filepath"
)

// FindRepoRoot walks up from cwd looking for go.mod.
func FindRepoRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	dir := wd
	for i := 0; i < 10; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", fmt.Errorf("go.mod not found from %s", wd)
}

// DefaultSuite is evals/suites/basic.jsonl relative to the module root.
func DefaultSuite() (string, error) {
	root, err := FindRepoRoot()
	if err != nil {
		return "", err
	}
	p := filepath.Join(root, "evals", "suites", "basic.jsonl")
	if _, err := os.Stat(p); err != nil {
		return "", err
	}
	return p, nil
}
