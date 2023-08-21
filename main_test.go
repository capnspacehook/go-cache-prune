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
		// no files should be deleted, build cache is empty
		if filesDeleted != 0 {
			t.Fatalf("expected 0 files to be deleted, got %d", filesDeleted)
		}
	})

	t.Run("populate cache", func(t *testing.T) {
		doPrune := startWatching(t, ctx, buildCache, false)

		out := runGoCommand(t, ctx, "testdata/first", "go", "build", "-v", "-o", tempDir)
		cacheWasNotUsed(t, out)

		filesDeleted := doPrune()
		// no files should be deleted, the build cache should contain
		// only the results of the one watched build
		if filesDeleted != 0 {
			t.Fatalf("expected 0 files to be deleted, got %d", filesDeleted)
		}
	})

	t.Run("prune cache", func(t *testing.T) {
		out := runGoCommand(t, ctx, "testdata/first", "go", "build", "-v", "-o", tempDir)
		cacheWasUsed(t, out)

		doPrune := startWatching(t, ctx, buildCache, false)

		out = runGoCommand(t, ctx, "testdata/second", "go", "build", "-v", "-o", tempDir)
		cacheWasNotUsed(t, out)

		filesDeleted := doPrune()
		// cached build files of the 'first' module should be deleted,
		// it's build was not watched
		if filesDeleted == 0 {
			t.Fatalf("expected some files to be deleted, got %d", filesDeleted)
		}

		out = runGoCommand(t, ctx, "testdata/second", "go", "build", "-v", "-o", tempDir)
		cacheWasUsed(t, out)

		out = runGoCommand(t, ctx, "testdata/first", "go", "build", "-v", "-o", tempDir)
		cacheWasNotUsed(t, out)
	})

	t.Run("prune unneeded files", func(t *testing.T) {
		doPrune := startWatching(t, ctx, buildCache, false)

		out := runGoCommand(t, ctx, "testdata/first", "go", "build", "-v", "-o", tempDir)
		cacheWasUsed(t, out)

		out = runGoCommand(t, ctx, "testdata/second", "go", "build", "-v", "-o", tempDir)
		cacheWasUsed(t, out)

		// Even though both modules were built while go-cache-prune was
		// watching, there are still apparently unneeded files that when
		// removed don't cause subsequent builds to incur cache misses.
		// I'm honestly not sure why this is yet.
		filesDeleted := doPrune()
		if filesDeleted == 0 {
			t.Fatalf("expected some files to be deleted, got %d", filesDeleted)
		}

		out = runGoCommand(t, ctx, "testdata/first", "go", "build", "-v", "-o", tempDir)
		cacheWasUsed(t, out)

		out = runGoCommand(t, ctx, "testdata/second", "go", "build", "-v", "-o", tempDir)
		cacheWasUsed(t, out)
	})
}

// 'go' is always passed for command, but it makes calls much easier to read
//
//nolint:unparam
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

func cacheWasNotUsed(t *testing.T, output []byte) {
	t.Helper()

	// output of 'go build -v' will be empty if all modules were read
	// from the module cache and not downloaded and if all packages
	// were read from the build cache and not compiled
	if len(output) == 0 {
		t.Fatalf("cache was used, expected it not to be used")
	}
}

func cacheWasUsed(t *testing.T, output []byte) {
	t.Helper()

	// output of 'go build -v' will be non-empty if any modules were
	// downloaded and not read from the module cache or if any packages
	// were compiled and not read from the build cache
	if len(output) != 0 {
		t.Fatalf("cache was not used, expected it to be used")
	}
}
