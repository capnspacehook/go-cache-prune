package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"

	actions "github.com/sethvargo/go-githubactions"
	"github.com/tywkeene/go-fsevents"
	"golang.org/x/mod/module"
	"golang.org/x/sys/unix"
)

const (
	projectName = "Go Cache Prune"
	pidFilename = "go-cache-prune.pid"
)

func usage() {
	fmt.Fprintf(os.Stderr, `
Prune unused files in Go module and build caches

go-cache-prune [flags]

%s accepts the following flags:

`[1:], projectName)
	flag.PrintDefaults()
	fmt.Fprint(os.Stderr, `

For more information, see https://github.com/capnspacehook/go-cache-prune.
`[1:])
}

func main() {
	os.Exit(mainRetCode())
}

func mainRetCode() int {
	var (
		debugLogs    bool
		logPath      string
		moduleCache  string
		buildCache   string
		noModCache   bool
		noBuildCache bool
		noPIDFile    bool
		signalProc   bool
		printVersion bool
	)

	flag.Usage = usage
	flag.BoolVar(&debugLogs, "debug", false, "enable debug logging")
	flag.StringVar(&logPath, "l", "stdout", "path to log to")
	flag.StringVar(&moduleCache, "mod-cache", "", "path to Go module cache")
	flag.StringVar(&buildCache, "build-cache", "", "path to Go build cache")
	flag.BoolVar(&noBuildCache, "only-mod-cache", false, "only prune the module cache, and not the build cache")
	flag.BoolVar(&noModCache, "only-build-cache", false, "only prune the build cache, and not the module cache")
	flag.BoolVar(&noPIDFile, "no-pid-file", false, "don't create a PID file")
	flag.BoolVar(&signalProc, "signal", false, "signal a running go-cache-prune to start pruning")
	flag.BoolVar(&printVersion, "version", false, "print version and build information and exit")
	flag.Parse()

	info, ok := debug.ReadBuildInfo()
	if !ok {
		actions.Errorf("build information not found")
		return 1
	}

	if printVersion {
		printVersionInfo(info)
		return 0
	}

	if noBuildCache && noModCache {
		actions.Errorf("-only-mod-cache and -only-build-cache are mutually exclusive")
		return 1
	}
	if noModCache && moduleCache != "" {
		actions.Errorf("-mod-cache must be unset when -only-mod-cache is set")
		return 1
	}
	if noBuildCache && buildCache != "" {
		actions.Errorf("-build-cache must be unset when -only-build-cache is set")
		return 1
	}

	// signal a running go-cache-prune process if necessary
	pidFile := filepath.Join(os.TempDir(), pidFilename)
	if signalProc {
		pidBytes, err := os.ReadFile(pidFile)
		if err != nil {
			actions.Errorf("reading PID file: %v", err)
			return 1
		}
		pid, err := strconv.Atoi(string(pidBytes))
		if err != nil {
			actions.Errorf("parsing PID from PID file: %v", err)
			return 1
		}

		p, _ := os.FindProcess(pid) // always succeeds for Unix systems
		if err := p.Signal(unix.SIGHUP); err != nil {
			actions.Errorf("signaling go-cache-prune process: %v", err)
			return 1
		}

		if _, err := p.Wait(); err != nil {
			actions.Errorf("waiting for signaling go-cache-prune process to complete: %v", err)
			return 1
		}

		return 0
	}

	if _, err := os.Stat(pidFile); err == nil {
		actions.Errorf("go-cache-prune is already running")
		return 1
	}

	mainCtx, mainCancel := signal.NotifyContext(context.Background(), os.Interrupt, unix.SIGTERM)
	defer mainCancel()

	// if the caches weren't explicitly passed, get them
	var err error
	if !noModCache && moduleCache == "" {
		moduleCache, err = getGoEnv(mainCtx, "GOMODCACHE")
		if err != nil {
			actions.Errorf("getting GOMODCACHE: %v", err)
			return 1
		}
	}
	if !noBuildCache && buildCache == "" {
		buildCache, err = getGoEnv(mainCtx, "GOCACHE")
		if err != nil {
			actions.Errorf("getting GOCACHE: %v", err)
			return 1
		}
	}

	if !noPIDFile {
		// create PID file
		pidBytes := []byte(strconv.Itoa(os.Getpid()))
		err := os.WriteFile(pidFile, pidBytes, 0o440)
		if err != nil {
			actions.Errorf("creating PID file: %v", err)
			return 1
		}
		defer os.Remove(pidFile)
	}

	// stop watching on SIGHUP
	watchCtx, watchCancel := signal.NotifyContext(mainCtx, unix.SIGHUP)
	defer watchCancel()

	actions.Infof("starting %s at version %s", projectName, version)

	modFiles, buildFiles, err := watchCaches(watchCtx, moduleCache, buildCache)
	if err != nil {
		actions.Errorf("watching caches: %v", err)
		return 1
	}
	actions.EndGroup()

	if mainCtx.Err() != nil {
		actions.Infof("signal received, shutting down without pruning caches")
		return 2
	}

	if len(modFiles) == 0 && len(buildFiles) == 0 {
		actions.Infof("no cached files were used, nothing to do")
		return 2
	}

	err = pruneCaches(modFiles, buildFiles, moduleCache, buildCache)
	if err != nil {
		actions.Errorf("pruning caches: %v", err)
		return 1
	}

	return 0
}

func getGoEnv(ctx context.Context, name string) (string, error) {
	cmd := exec.CommandContext(ctx, "go", "env", name)
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("running %s: %w", cmd, err)
	}
	if len(out) < 1 {
		return "", fmt.Errorf("'go env' output is too short: %v", out)
	}

	// trim ending newline
	return string(out[:len(out)-1]), nil
}

type usedCacheFiles map[string]struct{}

func watchCaches(ctx context.Context, modCache, buildCache string) (usedCacheFiles, usedCacheFiles, error) {
	actions.Group("Recording used cache files")
	defer actions.EndGroup()

	var (
		modFiles      usedCacheFiles
		buildFiles    usedCacheFiles
		watchModErr   error
		watchBuildErr error
		wg            sync.WaitGroup
	)

	if modCache != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			modFiles, watchModErr = watchCache(ctx, true, modCache)
			if watchModErr != nil {
				watchModErr = fmt.Errorf("watching module cache: %w", watchModErr)
			}
		}()
	}
	if buildCache != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buildFiles, watchBuildErr = watchCache(ctx, false, buildCache)
			if watchBuildErr != nil {
				watchModErr = fmt.Errorf("watching build cache: %w", watchBuildErr)
			}
		}()
	}
	wg.Wait()

	err := errors.Join(watchModErr, watchBuildErr)
	if err != nil {
		return nil, nil, err
	}

	return modFiles, buildFiles, nil
}

func watchCache(ctx context.Context, isModCache bool, dir string) (usedCacheFiles, error) {
	actions.Infof("creating watches for cache dir %q", dir)

	watcher, err := fsevents.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("creating file watcher: %w", err)
	}

	mask := fsevents.Accessed | fsevents.Create
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}

		if isModCache {
			depDir, ok := dependencyDir(path, d)
			if ok {
				_, err := watcher.AddDescriptor(depDir, mask)
				if err != nil {
					if !errors.Is(err, fsevents.ErrDescAlreadyExists) {
						return fmt.Errorf("adding watch for %s: %w", depDir, err)
					}
				}
			}

			actions.Debugf("added watch for %q", depDir)
			return filepath.SkipDir
		} else if d.IsDir() {
			_, err := watcher.AddDescriptor(path, mask)
			if err != nil {
				return fmt.Errorf("adding watch for %s: %w", path, err)
			}
			actions.Debugf("added watch for %q", path)
		}

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walking %s: %w", dir, err)
	}

	if err := watcher.StartAll(); err != nil {
		return nil, fmt.Errorf("starting to watch files: %w", err)
	}
	defer func() {
		err := watcher.StopAll()
		if err != nil {
			actions.Warningf("stopping file watchers: %v", err)
		}
	}()

	go watcher.Watch()

	usedFiles := make(usedCacheFiles)

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return nil, errors.New("file watcher event channel closed")
			}

			created := fsevents.CheckMask(fsevents.Create, event.RawEvent.Mask)
			actions.Debugf("got event: path=%q created=%t", event.Path, created)

			usedFiles[event.Path] = struct{}{}
		case err, ok := <-watcher.Errors:
			if !ok {
				return nil, errors.New("file watcher error channel closed")
			}
			actions.Errorf("file watcher: %v", err)
		case <-ctx.Done():
			return usedFiles, nil
		}
	}
}

func dependencyDir(path string, d fs.DirEntry) (string, bool) {
	if !d.IsDir() && d.Name() == "go.mod" {
		// If the dir contains 'go.mod', the subdirs don't need to be
		// watched. go will always try to read 'go.mod' when reading a
		// cached dep
		return filepath.Dir(path), true
	} else if d.IsDir() && strings.Contains(d.Name(), "@") {
		// if the dir contains a version of a module that is a
		// pseudo-version, this is a dep dir
		_, ver, _ := strings.Cut(d.Name(), "@")
		if strings.HasSuffix(ver, "+incompatible") || module.IsPseudoVersion(ver) {
			return path, true
		}
	}

	return "", false
}

func pruneCaches(modFiles, buildFiles usedCacheFiles, modCache, buildCache string) error {
	actions.Group("Pruning cache files")
	defer actions.EndGroup()

	var deletedFiles uint
	newWalkFunc := func(root string, isModCache bool) fs.WalkDirFunc {
		return func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				actions.Warningf("walking %q at %q: %v", root, path, err)
			}
			if path == root {
				return nil
			}

			if isModCache {
				depDir, ok := dependencyDir(path, d)
				if ok {
					if _, ok := modFiles[depDir]; ok {
						return nil
					}
					err := os.RemoveAll(depDir)
					if err != nil {
						actions.Warningf("deleting directory from module cache: %v", err)
						return nil
					}
					actions.Debugf("deleted directory %q from module cache", depDir)
					deletedFiles++
				}
			} else if !d.IsDir() {
				if _, ok := buildFiles[path]; !ok {
					err := os.Remove(path)
					if err != nil {
						actions.Warningf("deleting file from build cache: %v", err)
						return nil
					}
					actions.Debugf("deleted file %q from build cache", path)
					deletedFiles++
				}
			}

			return nil
		}
	}

	var walkModErr error
	if modCache != "" {
		walkModErr = filepath.WalkDir(modCache, newWalkFunc(modCache, true))
		actions.Infof("deleted %d directories from module cache", deletedFiles)
		deletedFiles = 0
	}

	var walkBuildErr error
	if buildCache != "" {
		walkBuildErr = filepath.WalkDir(buildCache, newWalkFunc(buildCache, false))
		actions.Infof("deleted %d files from build cache", deletedFiles)
	}

	return errors.Join(walkModErr, walkBuildErr)
}