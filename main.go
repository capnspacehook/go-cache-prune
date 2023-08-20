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
	"golang.org/x/mod/semver"
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
	if err := mainErr(); err != nil {
		var exitCode *errJustExit
		if errors.As(err, &exitCode) {
			return int(*exitCode)
		}
		actions.Errorf("%v", err)
		return 1
	}
	return 0
}

type config struct {
	commit string

	moduleCache     string
	buildCache      string
	pruneModCache   bool
	pruneBuildCache bool
	usePIDFile      bool
	signalProc      bool
}

func parseFlags() (*config, error) {
	var (
		cfg          config
		printVersion bool
	)

	flag.Usage = usage
	flag.StringVar(&cfg.moduleCache, "mod-cache", "", "path to Go module cache")
	flag.StringVar(&cfg.buildCache, "build-cache", "", "path to Go build cache")
	flag.BoolVar(&cfg.pruneModCache, "prune-mod-cache", true, "prune the Go module cache")
	flag.BoolVar(&cfg.pruneBuildCache, "prune-build-cache", true, "prune the Go build cache")
	flag.BoolVar(&cfg.usePIDFile, "pid-file", false, "create a PID file")
	flag.BoolVar(&cfg.signalProc, "signal", false, "signal a running go-cache-prune to start pruning")
	flag.BoolVar(&printVersion, "version", false, "print version and build information and exit")
	flag.Parse()

	info, ok := debug.ReadBuildInfo()
	if !ok {
		return nil, errors.New("build information not found")
	}

	if printVersion {
		printVersionInfo(info)
		return nil, errJustExit(0)
	}

	if !cfg.pruneModCache && !cfg.pruneBuildCache {
		return nil, errors.New("either -prune-mod-cache or -prune-build-cache must be true")
	}
	if !cfg.pruneModCache && cfg.moduleCache != "" {
		return nil, errors.New("-mod-cache must be unset when -prune-mod-cache is false")
	}
	if !cfg.pruneBuildCache && cfg.buildCache != "" {
		return nil, errors.New("-build-cache must be unset when -prune-build-cache is false")
	}

	for _, buildSetting := range info.Settings {
		if buildSetting.Key == "vcs.revision" {
			cfg.commit = buildSetting.Value
			break
		}
	}

	return &cfg, nil
}

type errJustExit int

func (e errJustExit) Error() string { return fmt.Sprintf("exit: %d", e) }

func mainErr() error {
	cfg, err := parseFlags()
	if err != nil {
		return err
	}

	// signal a running go-cache-prune process if necessary
	pidFile := filepath.Join(os.TempDir(), pidFilename)
	if cfg.signalProc {
		pidBytes, err := os.ReadFile(pidFile)
		if err != nil {
			return fmt.Errorf("reading PID file: %w", err)
		}
		pid, err := strconv.Atoi(string(pidBytes))
		if err != nil {
			return fmt.Errorf("parsing PID from PID file: %w", err)
		}

		p, _ := os.FindProcess(pid) // always succeeds for Unix systems
		if err := p.Signal(unix.SIGHUP); err != nil {
			return fmt.Errorf("signaling go-cache-prune process: %w", err)
		}

		if _, err := p.Wait(); err != nil {
			return fmt.Errorf("waiting for signaling go-cache-prune process to complete: %w", err)
		}

		return nil
	}

	if cfg.usePIDFile {
		if _, err := os.Stat(pidFile); err == nil {
			return errors.New("go-cache-prune is already running")
		}
	}

	mainCtx, mainCancel := signal.NotifyContext(context.Background(), os.Interrupt, unix.SIGTERM)
	defer mainCancel()

	// if the caches weren't explicitly passed, get them
	if cfg.pruneModCache && cfg.moduleCache == "" {
		cfg.moduleCache, err = getGoEnv(mainCtx, "GOMODCACHE")
		if err != nil {
			return fmt.Errorf("getting GOMODCACHE: %w", err)
		}
	}
	if cfg.pruneBuildCache && cfg.buildCache == "" {
		cfg.buildCache, err = getGoEnv(mainCtx, "GOCACHE")
		if err != nil {
			return fmt.Errorf("getting GOCACHE: %w", err)
		}
	}

	if cfg.usePIDFile {
		// create PID file
		pidBytes := []byte(strconv.Itoa(os.Getpid()))
		err := os.WriteFile(pidFile, pidBytes, 0o440)
		if err != nil {
			return fmt.Errorf("creating PID file: %w", err)
		}
		defer os.Remove(pidFile)
	}

	// stop watching on SIGHUP
	watchCtx, watchCancel := signal.NotifyContext(mainCtx, unix.SIGHUP)
	defer watchCancel()

	actions.Infof("starting %s version=%s commit=%s", projectName, version, cfg.commit)

	modFiles, buildFiles, err := watchCaches(watchCtx, cfg.moduleCache, cfg.buildCache)
	if err != nil {
		return fmt.Errorf("watching caches: %w", err)
	}
	actions.EndGroup()

	if mainCtx.Err() != nil {
		actions.Infof("signal received, shutting down without pruning caches")
		return errJustExit(2)
	}

	if len(modFiles) == 0 && len(buildFiles) == 0 {
		actions.Infof("no cached files were used, nothing to do")
		return errJustExit(2)
	}

	pruneCaches(modFiles, buildFiles, cfg.moduleCache, cfg.buildCache)

	return nil
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
					// we may attempt to add descriptors for the same
					// dirs more than once due to how we match on 'go.mod',
					// but this won't adversely effect anything so ignore
					// the error
					if !errors.Is(err, fsevents.ErrDescAlreadyExists) {
						return fmt.Errorf("adding watch for %s: %w", depDir, err)
					}
				}
			}

			actions.Debugf("added watch for %q", depDir)
			return nil
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
	if d.IsDir() && strings.Contains(d.Name(), "@") {
		// if the dir name contains a valid module version, this is a dep dir
		_, ver, _ := strings.Cut(d.Name(), "@")
		if strings.HasSuffix(ver, "+incompatible") || semver.IsValid(ver) || module.IsPseudoVersion(ver) {
			return path, true
		}
	} else if !d.IsDir() && d.Name() == "go.mod" {
		// If the dir contains 'go.mod', this is a dep dir
		return filepath.Dir(path), true
	}

	return "", false
}

func pruneCaches(modFiles, buildFiles usedCacheFiles, modCache, buildCache string) {
	actions.Group("Pruning cache files")
	defer actions.EndGroup()

	newWalkFunc := func(root string, isModCache bool, deletedCounter *uint) fs.WalkDirFunc {
		return func(path string, d fs.DirEntry, err error) error {
			// ignore file not found errors, most will be because
			// module cache dirs were recursively deleted
			if err != nil && (isModCache && !errors.Is(err, os.ErrNotExist)) {
				actions.Warningf("walking %q: %v", path, err)
				return nil
			}
			if path == root {
				return nil
			}

			if isModCache {
				depDir, ok := dependencyDir(path, d)
				if !ok {
					return nil
				}
				if _, ok := modFiles[depDir]; ok {
					return nil
				}

				// allow module files to be deleted
				chmodDir(depDir)
				err := os.RemoveAll(depDir)
				if err != nil {
					actions.Warningf("deleting directory from module cache: %v", err)
					return nil
				}
				actions.Debugf("deleted directory %q from module cache", depDir)
				*deletedCounter++
			} else if !d.IsDir() {
				if _, ok := buildFiles[path]; ok {
					return nil
				}

				err := os.Remove(path)
				if err != nil {
					actions.Warningf("deleting file from build cache: %v", err)
					return nil
				}
				actions.Debugf("deleted file %q from build cache", path)
				*deletedCounter++

			}

			return nil
		}
	}

	var wg sync.WaitGroup

	if modCache != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()

			var deletedModDirs uint
			_ = filepath.WalkDir(modCache, newWalkFunc(modCache, true, &deletedModDirs))
			actions.Infof("deleted %d directories from module cache", deletedModDirs)
		}()
	}

	if buildCache != "" {
		wg.Add(1)
		go func() {
			defer wg.Done()

			var deletedBuildFiles uint
			_ = filepath.WalkDir(buildCache, newWalkFunc(buildCache, false, &deletedBuildFiles))
			actions.Infof("deleted %d files from build cache", deletedBuildFiles)
		}()
	}

	wg.Wait()
}

func chmodDir(dir string) {
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			actions.Warningf("walking %q: %v", path, err)
			return nil
		}

		if err := os.Chmod(path, 0o777); err != nil {
			actions.Warningf("changing permissions of %q: %v", path, err)
		}

		return nil
	})
}
