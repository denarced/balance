package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"

	"github.com/alecthomas/kong"
	kongtoml "github.com/alecthomas/kong-toml"
	"github.com/denarced/balance/lib/bal"
	"github.com/denarced/balance/lib/shared"
	"github.com/fsnotify/fsnotify"
	"github.com/gobwas/glob"
)

const (
	errCodeUnknown = iota + 10
	errCodeInitialDirRead
	errCodeNewWatcher
	errCodeAddWatcher

	ignoreFilen = ".balance.toml"
)

var CLI struct {
	Command          string   `arg:"" help:"Shell command to execute on changes."`
	CommandSeparator string   `short:"s" help:"${commandSeparatorHelp}"`
	Include          []string `short:"i" help:"Only include files matching these patterns or all."`
	Exclude          []string `short:"e" help:"Exclude files matching these patterns."`
	DebounceDelay    int64    `short:"d" default:"1000" help:"${debounceDelayHelp}"`
	LogFile          string   `help:"When empty and by default, log to STDERR."`
	LogLevel         string   `default:"ERROR" help:"Logging level."`
}

func main() {
	dieIfErr := func(err error) {
		if err == nil {
			return
		}
		if shared.Logger == nil {
			fmt.Fprintf(os.Stderr, "Error: %s\n", err)
		} else {
			shared.Logger.Error("Quitting with error", "error", err)
		}
		var coded *bal.ErrorCode
		if errors.As(err, &coded) {
			os.Exit(coded.Code)
		}
		os.Exit(errCodeUnknown)
	}

	// Setup CLI
	configFilep, err := bal.FindFileAbove(ignoreFilen)
	dieIfErr(err)
	var configOption kong.Option = &doNothingOption{}
	if configFilep != "" {
		configOption = kong.Configuration(kongtoml.Loader, configFilep)
	}
	kong.Parse(
		&CLI,
		configOption,
		kong.Vars{
			"debounceDelayHelp": "How long to wait for new events (milliseconds) " +
				"before syncing.",
			"commandSeparatorHelp": "Separator between {} when there's multiple files and " +
				"they are to be joined. If left empty, " +
				"each file will become a separate word in the executed command.",
		},
	)
	if CLI.DebounceDelay < 0 {
		dieIfErr(errors.New("--debounce-delay must be >=0"))
	}
	dieIfErr(shared.InitLogger(CLI.LogFile, CLI.LogLevel))

	includes, iErr := toGlobs(CLI.Include)
	excludes, eErr := toGlobs(CLI.Exclude)
	dieIfErr(errors.Join(iErr, eErr))

	// Setup monitors
	dirs, err := scanDirs(func(dpath string) bool {
		return matchExclude(
			bal.EventWrapper{
				Dir:  true,
				Kind: bal.EventTypeCreate,
				Name: dpath + "/",
			},
			excludes)
	})
	dieIfErr(err)
	watcher, err := setup(dirs)
	dieIfErr(err)
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt)
	syncCh := make(chan bal.SyncEvent)
	go func() {
		pieces := strings.Split(CLI.Command, " ")
		cmdName := pieces[0]
		for eachSync := range syncCh {
			runCommand(cmdName, pieces[1:], eachSync.Files, CLI.CommandSeparator)
		}
	}()

	// The main event
	dieIfErr(bal.Balance(sig, watcher, func(event bal.EventWrapper) bool {
		return shouldIgnore(includes, excludes, event)
	}, syncCh, CLI.DebounceDelay))
}

func scanDirs(shouldIgnore func(dpath string) bool) (dirs []string, err error) {
	err = filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() || shouldIgnore(path) {
			return nil
		}
		dirs = append(dirs, path)
		return nil
	})
	if err != nil {
		err = bal.NewErrorCode(
			fmt.Errorf("failed to read initial directories - %w", err),
			errCodeInitialDirRead)
		shared.Logger.Error("Failed to walk dirs.", "error", err)
		return
	}
	if shared.Logger.Enabled(context.Background(), slog.LevelDebug) {
		loggedDirs := dirs
		if len(dirs) > 5 {
			loggedDirs = dirs[:5]
		}
		shared.Logger.Debug("Dirs walked.", "dirs", loggedDirs, "more", len(loggedDirs) < len(dirs))
	}
	return
}

func setup(dirs []string) (watcher *fsnotify.Watcher, err error) {
	defer func() {
		if err != nil {
			shared.Logger.Error("Failed to setup fsnotify watcher.", "error", err)
		}
	}()
	watcher, err = fsnotify.NewWatcher()
	if err != nil {
		err = bal.NewErrorCode(
			fmt.Errorf("failed to create fsnotify watcher - %w", err),
			errCodeNewWatcher,
		)
		return
	}
	for _, each := range dirs {
		if err = watcher.Add(each); err != nil {
			err = bal.NewErrorCode(
				fmt.Errorf("failed to add watched directory (\"%s\") - %w", each, err),
				errCodeAddWatcher,
			)
			return
		}
	}
	shared.Logger.Debug("Added initial watched dirs.", "count", len(dirs))
	return
}

func shouldIgnore(includes, excludes []glob.Glob, event bal.EventWrapper) bool {
	if event.Kind == bal.EventTypeChmod {
		return true
	}
	found := matchInclude(event, includes)
	if !found {
		return true
	}
	return matchExclude(event, excludes)
}

func matchInclude(event bal.EventWrapper, includes []glob.Glob) bool {
	if len(includes) == 0 {
		return true
	}
	logger := shared.Logger.With("file", event.Name, "kind", event.Kind)
	for _, each := range includes {
		if each.Match(event.Name) {
			logger.Debug("Event included.", "pattern", each)
			return true
		}
	}
	logger.Debug("Event excluded, none of include patterns matched.")
	return false
}

func matchExclude(event bal.EventWrapper, excludes []glob.Glob) bool {
	if excludes == nil {
		return false
	}
	for _, each := range excludes {
		if each.Match(event.Name) {
			shared.Logger.Debug("Event excluded.", "file", event.Name, "pattern", each)
			return true
		}
	}
	return false
}

func runCommand(cmdName string, parameters []string, files []string, separator string) {
	cmd := exec.Command(cmdName, replacePlaceholder(parameters, files, separator)...)
	cmd.Stderr = os.Stderr
	cmd.Stdout = os.Stdout
	shared.Logger.Debug("Run command.", "command", cmdName, "args", cmd.Args)
	// If the command doesn't work, it's the CLI user's problem
	_ = cmd.Run()
}

func replacePlaceholder(source []string, replacement []string, separator string) []string {
	result := make([]string, 0, len(source))
	for _, each := range source {
		if replacement != nil {
			reset := true
			if each == "{}" && separator == "" {
				result = append(result, replacement...)
			} else if strings.Contains(each, "{}") && separator != "" {
				result = append(result, replaceJoinedPlaceholder(each, replacement, separator))
			} else {
				reset = false
			}
			if reset {
				replacement = nil
				continue
			}
		}
		result = append(result, each)
	}
	return result
}

func replaceJoinedPlaceholder(template string, replacement []string, separator string) string {
	joined := strings.Join(replacement, separator)
	return strings.Replace(template, "{}", joined, 1)
}

type doNothingOption struct{}

func (*doNothingOption) Apply(_ *kong.Kong) error {
	return nil
}

func toGlobs(patterns []string) (globs []glob.Glob, err error) {
	if patterns == nil {
		return
	}
	for _, each := range expand(patterns) {
		var g glob.Glob
		g, err = glob.Compile(each)
		if err != nil {
			return
		}
		globs = append(globs, g)
	}
	return
}

// Expand all "**/" prefixed patterns to without it.
// My guess was that "**/.hg/**" would match to ".hg/": it didn't. Instead it would match to
// "/.hg/". That makes no sense so an obvious fix is to "duplicate" the pattern: add ".hg/**".
func expand(patterns []string) []string {
	if len(patterns) == 0 {
		return patterns
	}
	expanded := make([]string, 0, len(patterns))
	prefix := "**/"
	for _, each := range patterns {
		expanded = append(expanded, each)
		without, found := strings.CutPrefix(each, prefix)
		if found {
			expanded = append(expanded, without)
		}
	}
	return expanded
}
