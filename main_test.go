package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestBuildCache(t *testing.T) {
	tempDir := t.TempDir()
	buildCache := filepath.Join(tempDir, "build")
	if err := os.Mkdir(buildCache, 0o775); err != nil {
		t.Fatalf("creating build cache dir: %v", err)
	}
	t.Setenv("GOCACHE", buildCache)

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	runGoCommand(t, ctx, ".", "go", "clean", "-cache")

	t.Run("empty cache", func(t *testing.T) {
		doPrune := startWatching(t, ctx, buildCache, false)
		filesDeleted := doPrune()
		if filesDeleted != 0 {
			t.Fatalf("expected 0 files to be deleted, got %d", filesDeleted)
		}
	})

	t.Run("populate cache", func(t *testing.T) {
		doPrune := startWatching(t, ctx, buildCache, false)

		out := runGoCommand(t, ctx, "testdata/first", "go", "build", "-v", "-o", tempDir)
		if len(out) == 0 {
			t.Fatalf("build cache wasn't used")
		}

		filesDeleted := doPrune()
		if filesDeleted != 0 {
			t.Fatalf("expected 0 files to be deleted, got %d", filesDeleted)
		}
	})

	t.Run("prune cache", func(t *testing.T) {
		out := runGoCommand(t, ctx, "testdata/first", "go", "build", "-v", "-o", tempDir)
		if len(out) != 0 {
			t.Fatalf("build cache should be used")
		}

		doPrune := startWatching(t, ctx, buildCache, false)

		out = runGoCommand(t, ctx, "testdata/second", "go", "build", "-v", "-o", tempDir)
		if len(out) == 0 {
			t.Fatalf("build cache wasn't used")
		}

		filesDeleted := doPrune()
		if filesDeleted == 0 {
			t.Fatalf("expected some files to be deleted, got %d", filesDeleted)
		}

		out = runGoCommand(t, ctx, "testdata/second", "go", "build", "-v", "-o", tempDir)
		if len(out) != 0 {
			t.Fatalf("build cache should be used")
		}

		out = runGoCommand(t, ctx, "testdata/first", "go", "build", "-v", "-o", tempDir)
		if len(out) == 0 {
			t.Fatalf("build cache wasn't used")
		}
	})

	t.Run("prune unneeded files", func(t *testing.T) {
		doPrune := startWatching(t, ctx, buildCache, false)

		out := runGoCommand(t, ctx, "testdata/first", "go", "build", "-v", "-o", tempDir)
		if len(out) != 0 {
			t.Fatalf("build cache should be used")
		}

		out = runGoCommand(t, ctx, "testdata/second", "go", "build", "-v", "-o", tempDir)
		if len(out) != 0 {
			t.Fatalf("build cache should be used")
		}

		// Even though both modules were built while go-cache-prune was
		// watching, there are still apparently unneeded files that when
		// removed don't cause subsequent builds to incur cache misses.
		// I'm honestly not sure why this is yet.
		filesDeleted := doPrune()
		if filesDeleted == 0 {
			t.Fatalf("expected some files to be deleted, got %d", filesDeleted)
		}

		out = runGoCommand(t, ctx, "testdata/first", "go", "build", "-v", "-o", tempDir)
		if len(out) != 0 {
			t.Fatalf("build cache should be used")
		}

		out = runGoCommand(t, ctx, "testdata/second", "go", "build", "-v", "-o", tempDir)
		if len(out) != 0 {
			t.Fatalf("build cache should be used")
		}
	})
}

func runGoCommand(t *testing.T, ctx context.Context, workingDir, command string, args ...string) []byte {
	t.Helper()

	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = workingDir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("running %s: %v\n%s", cmd, err, string(out))
	}
	return out
}

func startWatching(t *testing.T, ctx context.Context, cacheDir string, isModCache bool) func() uint {
	t.Helper()

	var (
		errCh     = make(chan error)
		usedFiles usedCacheFiles
	)

	watchCtx, watchCancel := context.WithCancel(ctx)
	t.Cleanup(watchCancel)

	go func() {
		var err error
		usedFiles, err = watchCache(watchCtx, false, cacheDir)
		errCh <- err
	}()

	return func() uint {
		t.Helper()

		watchCancel()
		err := <-errCh
		if err != nil {
			t.Fatalf("watching cache: %v", err)
		}

		return pruneCache(cacheDir, isModCache, usedFiles)
	}
}
