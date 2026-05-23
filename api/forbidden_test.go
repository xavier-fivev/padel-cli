package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// forbiddenPathFragments is the doctrine guard: these Playtomic API paths
// must never appear in the codebase outside this test file. Publishing an
// auto-booked match forfeits the 48h free-cancel window, which would defeat
// the "pre-grab + decide within 48h" strategy the project is built around.
//
// If a future change legitimately needs to publish, that's a deliberate
// doctrine shift and this guard should be removed in the same commit —
// alongside README + audit-event updates.
var forbiddenPathFragments = []string{
	"/v2/matches/",
	"/publish",
}

func TestNoForbiddenPlaytomicPaths(t *testing.T) {
	repoRoot, err := findRepoRoot()
	if err != nil {
		t.Fatalf("find repo root: %v", err)
	}

	selfPath, err := filepath.Abs("forbidden_test.go")
	if err != nil {
		t.Fatalf("resolve self path: %v", err)
	}

	walkErr := filepath.Walk(repoRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == ".git" || info.Name() == "result" || info.Name() == ".direnv" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(info.Name(), ".go") {
			return nil
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			return err
		}
		if abs == selfPath {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		content := string(data)
		for _, fragment := range forbiddenPathFragments {
			if strings.Contains(content, fragment) {
				t.Errorf("forbidden Playtomic path %q found in %s — see api/forbidden_test.go for the doctrine reason", fragment, path)
			}
		}
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk repo: %v", walkErr)
	}
}

func findRepoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", os.ErrNotExist
		}
		dir = parent
	}
}
