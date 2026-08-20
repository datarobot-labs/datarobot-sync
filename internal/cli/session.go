// Copyright 2026 DataRobot, Inc. and its affiliates.
// All rights reserved.
// DataRobot, Inc.
// This is proprietary source code of DataRobot, Inc. and its affiliates.
// Released under the terms of DataRobot Tool and Utility Agreement.

package cli

import (
	"context"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	"golang.org/x/term"

	"github.com/datarobot/golang-datarobot-filesync/internal/config"
	"github.com/datarobot/golang-datarobot-filesync/internal/drapi"
	"github.com/datarobot/golang-datarobot-filesync/internal/manifest"
	"github.com/datarobot/golang-datarobot-filesync/internal/progress"
	"github.com/datarobot/golang-datarobot-filesync/internal/scan"
	"github.com/datarobot/golang-datarobot-filesync/internal/syncer"
)

// globalFlags are the persistent flags every subcommand shares.
type globalFlags struct {
	catalogID  string
	endpoint   string
	configFile string
	metaPrefix string

	check  string
	output string

	exclude     []string
	include     []string
	excludeFrom []string
	ignoreFile  string

	jobs    int
	timeout time.Duration
	retries int

	verbose  bool
	quiet    bool
	noColor  bool
	dryRun   bool
	yes      bool
	caCert   string
	insecure bool
}

// register adds the persistent flags. The names double as viper keys, so drconfig.yaml can
// carry any of them.
func (g *globalFlags) register(flags *pflag.FlagSet) {
	flags.StringVar(&g.catalogID, config.KeyCatalogID, "",
		"container to sync (env "+config.EnvCatalogID+")")
	flags.StringVar(&g.endpoint, config.KeyEndpoint, "",
		"DataRobot API endpoint (env "+config.EnvEndpoint+")")
	flags.StringVar(&g.configFile, config.KeyConfigFile, "",
		"path to drconfig.yaml (env "+config.EnvConfig+")")
	flags.StringVar(&g.metaPrefix, "meta-prefix", manifest.DefaultMetaPrefix,
		"container directory reserved for dr-sync state")

	flags.StringVar(&g.check, "check", string(scan.CheckAuto),
		"how to decide a file changed: auto, full, size, mtime")
	flags.StringVar(&g.output, "output", string(progress.ModeText), "output format: text or json")

	flags.StringArrayVar(&g.exclude, "exclude", nil, "gitignore-style pattern to skip (repeatable)")
	flags.StringArrayVar(&g.include, "include", nil, "pattern to re-include after an --exclude (repeatable)")
	flags.StringArrayVar(&g.excludeFrom, "exclude-from", nil, "file of patterns to skip (repeatable)")
	flags.StringVar(&g.ignoreFile, "ignore-file", ".drsyncignore", "ignore file to read from the sync root")

	flags.IntVarP(&g.jobs, "jobs", "j", 0, "concurrent hashers (default: CPUs, capped at 8)")
	flags.DurationVar(&g.timeout, "timeout", 0, "overall deadline for the run (0 for none)")
	flags.IntVar(&g.retries, "retries", 0, "retries for a failed request (default 5)")

	flags.BoolVarP(&g.verbose, "verbose", "v", false, "print per-file detail")
	flags.BoolVarP(&g.quiet, "quiet", "q", false, "print only warnings and errors")
	flags.BoolVar(&g.noColor, "no-color", false, "disable in-place progress updates")
	flags.BoolVar(&g.dryRun, "dry-run", false, "plan the work and print it, writing nothing")
	flags.BoolVarP(&g.yes, "yes", "y", false, "assume yes for confirmations")
	flags.StringVar(&g.caCert, config.KeyCACert, "",
		"PEM bundle of extra certificate authorities to trust")
	flags.BoolVarP(&g.insecure, config.KeyInsecure, "k", false, "do not verify TLS certificates")
}

// session is everything a command needs to talk to a container.
type session struct {
	resolved *config.Resolved
	client   *drapi.Client
	store    *syncer.Store
	reporter *progress.Reporter
	matcher  *scan.Matcher
	check    scan.CheckMode
}

// open resolves configuration and builds the client, store and reporter.
//
// requireCatalog is false only for `init`, which is what creates the container in the first
// place. root is the sync root when the command has one, so its ignore file can be read.
func (g *globalFlags) open(cmd *cobra.Command, requireCatalog bool, root string) (*session, error) {
	mode, err := progress.ParseMode(g.output)
	if err != nil {
		return nil, Fail(CodeUsage, err)
	}

	check, err := scan.ParseCheckMode(g.check)
	if err != nil {
		return nil, Fail(CodeUsage, err)
	}

	binding, err := config.BindFlags(cmd.Flags())
	if err != nil {
		return nil, Fail(CodeUsage, err)
	}

	resolved, err := binding.Resolve()
	if err != nil {
		return nil, Fail(CodeUsage, err)
	}

	if requireCatalog && resolved.CatalogID == "" {
		return nil, Failf(CodeUsage,
			"no container given: pass --%s, set %s, or create one with `%s init`",
			config.KeyCatalogID, config.EnvCatalogID, programName())
	}

	client, err := drapi.New(drapi.Config{
		Endpoint:           resolved.Endpoint,
		Token:              resolved.Token,
		CACertFile:         resolved.CACertFile,
		InsecureSkipVerify: resolved.InsecureSkipVerify,
		MaxRetries:         g.retries,
	})
	if err != nil {
		return nil, Fail(CodeUsage, err)
	}

	matcher, err := g.buildMatcher(root)
	if err != nil {
		return nil, Fail(CodeUsage, err)
	}

	return &session{
		resolved: resolved,
		client:   client,
		store:    syncer.NewStore(client, resolved.CatalogID, g.metaPrefix),
		reporter: g.newReporter(cmd, mode),
		matcher:  matcher,
		check:    check,
	}, nil
}

// newReporter builds the progress renderer. Human output goes to stderr and machine output to
// stdout, so `--output json` composes in a pipeline.
func (g *globalFlags) newReporter(cmd *cobra.Command, mode progress.Mode) *progress.Reporter {
	return progress.New(progress.Options{
		Mode:        mode,
		Human:       cmd.ErrOrStderr(),
		Machine:     cmd.OutOrStdout(),
		Interactive: g.interactive(cmd),
		Verbose:     g.verbose,
		Quiet:       g.quiet,
		Width:       terminalWidth(cmd),
	})
}

// buildMatcher assembles the ignore rules: the ignore file first, then --exclude-from files,
// then the flags, so a flag can always override what a file said.
func (g *globalFlags) buildMatcher(root string) (*scan.Matcher, error) {
	var patterns []string

	if root != "" && g.ignoreFile != "" {
		fromFile, err := scan.LoadIgnoreFile(joinRoot(root, g.ignoreFile))
		if err != nil {
			return nil, err
		}

		patterns = append(patterns, fromFile...)
	}

	for _, name := range g.excludeFrom {
		fromFile, err := scan.LoadIgnoreFile(name)
		if err != nil {
			return nil, err
		}

		patterns = append(patterns, fromFile...)
	}

	patterns = append(patterns, g.exclude...)

	for _, pattern := range g.include {
		patterns = append(patterns, "!"+pattern)
	}

	if len(patterns) == 0 {
		return nil, nil
	}

	return scan.NewMatcher(patterns)
}

// interactive reports whether progress should redraw in place. On a pipe or in a CI log,
// rewriting the same line just produces thousands of lines of noise.
func (g *globalFlags) interactive(cmd *cobra.Command) bool {
	if g.noColor || g.quiet || g.output != string(progress.ModeText) {
		return false
	}

	file, ok := cmd.ErrOrStderr().(*os.File)
	if !ok {
		return false
	}

	return term.IsTerminal(int(file.Fd()))
}

// withTimeout applies the run's overall deadline, if one was requested.
func (g *globalFlags) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if g.timeout <= 0 {
		return context.WithCancel(ctx)
	}

	return context.WithTimeout(ctx, g.timeout)
}

func terminalWidth(cmd *cobra.Command) int {
	file, ok := cmd.ErrOrStderr().(*os.File)
	if !ok {
		return 100
	}

	width, _, err := term.GetSize(int(file.Fd()))
	if err != nil || width <= 0 {
		return 100
	}

	return width
}

// joinRoot resolves a possibly-relative name against the sync root.
func joinRoot(root, name string) string {
	if filepath.IsAbs(name) {
		return name
	}

	return filepath.Join(root, name)
}

// syncRoot turns the positional path argument into an absolute directory.
func syncRoot(args []string) (string, error) {
	if len(args) == 0 {
		return "", Failf(CodeUsage, "a path is required")
	}

	absolute, err := filepath.Abs(args[0])
	if err != nil {
		return "", Failf(CodeUsage, "resolving %s: %v", args[0], err)
	}

	return absolute, nil
}
