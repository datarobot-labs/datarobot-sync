// Copyright 2026 DataRobot, Inc. and its affiliates.
// All rights reserved.
// DataRobot, Inc.
// This is proprietary source code of DataRobot, Inc. and its affiliates.
// Released under the terms of DataRobot Tool and Utility Agreement.

package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/datarobot/golang-datarobot-filesync/internal/config"
	"github.com/datarobot/golang-datarobot-filesync/internal/drapi"
	"github.com/datarobot/golang-datarobot-filesync/internal/manifest"
	"github.com/datarobot/golang-datarobot-filesync/internal/progress"
	"github.com/datarobot/golang-datarobot-filesync/internal/scan"
	"github.com/datarobot/golang-datarobot-filesync/internal/syncer"
)

// repeatable collects a flag that may be given more than once.
type repeatable []string

func (r *repeatable) String() string {
	return strings.Join(*r, ",")
}

func (r *repeatable) Set(value string) error {
	*r = append(*r, value)

	return nil
}

// bytesValue parses a size with an optional binary suffix, so --pack-size 64MiB reads the way
// a person would write it.
type bytesValue struct {
	target *int64
}

func (b bytesValue) String() string {
	if b.target == nil {
		return "0"
	}

	return strconv.FormatInt(*b.target, 10)
}

func (b bytesValue) Set(value string) error {
	parsed, err := ParseBytes(value)
	if err != nil {
		return err
	}

	*b.target = parsed

	return nil
}

// ParseBytes reads a byte count with an optional suffix: 1024, 64KiB, 4MB, 2G.
func ParseBytes(value string) (int64, error) {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return 0, errors.New("empty size")
	}

	// Longest suffixes first: "KiB" has to be tried before "K" or "B".
	suffixes := []struct {
		suffix     string
		multiplier int64
	}{
		{suffix: "KiB", multiplier: 1 << 10},
		{suffix: "MiB", multiplier: 1 << 20},
		{suffix: "GiB", multiplier: 1 << 30},
		{suffix: "TiB", multiplier: 1 << 40},
		{suffix: "KB", multiplier: 1000},
		{suffix: "MB", multiplier: 1000 * 1000},
		{suffix: "GB", multiplier: 1000 * 1000 * 1000},
		{suffix: "K", multiplier: 1 << 10},
		{suffix: "M", multiplier: 1 << 20},
		{suffix: "G", multiplier: 1 << 30},
		{suffix: "T", multiplier: 1 << 40},
		{suffix: "B", multiplier: 1},
	}

	upper := strings.ToUpper(trimmed)

	for _, candidate := range suffixes {
		if !strings.HasSuffix(upper, strings.ToUpper(candidate.suffix)) {
			continue
		}

		digits := strings.TrimSpace(trimmed[:len(trimmed)-len(candidate.suffix)])

		number, err := strconv.ParseFloat(digits, 64)
		if err != nil {
			return 0, fmt.Errorf("cannot read %q as a size", value)
		}

		return int64(number * float64(candidate.multiplier)), nil
	}

	number, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("cannot read %q as a size", value)
	}

	return number, nil
}

// commonFlags are shared by every subcommand.
type commonFlags struct {
	catalogID  string
	endpoint   string
	configFile string
	metaPrefix string

	check       string
	output      string
	exclude     repeatable
	include     repeatable
	excludeFrom repeatable
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
	// insecureSet distinguishes "--insecure=false" from the flag being absent, so an explicit
	// false can override what the CLI forwarded.
	insecureSet bool
}

// register adds the shared flags to a set.
func (c *commonFlags) register(set *flag.FlagSet) {
	set.StringVar(&c.catalogID, "catalog-id", "", "container to sync (env "+config.EnvCatalogID+")")
	set.StringVar(&c.endpoint, "endpoint", "", "DataRobot API endpoint (env "+config.EnvEndpoint+")")
	set.StringVar(&c.configFile, "config", "", "path to drconfig.yaml (env "+config.EnvConfig+")")
	set.StringVar(&c.metaPrefix, "meta-prefix", manifest.DefaultMetaPrefix,
		"container directory reserved for dr-sync state")

	set.StringVar(&c.check, "check", string(scan.CheckAuto),
		"how to decide a file changed: auto, full, size, mtime")
	set.StringVar(&c.output, "output", string(progress.ModeText), "output format: text or json")
	set.Var(&c.exclude, "exclude", "gitignore-style pattern to skip (repeatable)")
	set.Var(&c.include, "include", "pattern to re-include after an --exclude (repeatable)")
	set.Var(&c.excludeFrom, "exclude-from", "file of patterns to skip (repeatable)")
	set.StringVar(&c.ignoreFile, "ignore-file", ".drsyncignore", "ignore file to read from the sync root")

	set.IntVar(&c.jobs, "jobs", 0, "concurrent hashers (default: CPUs, capped at 8)")
	set.IntVar(&c.jobs, "j", 0, "shorthand for --jobs")
	set.DurationVar(&c.timeout, "timeout", 0, "overall deadline for the run (0 for none)")
	set.IntVar(&c.retries, "retries", 0, "retries for a failed request (default 5)")

	set.BoolVar(&c.verbose, "verbose", false, "print per-file detail")
	set.BoolVar(&c.verbose, "v", false, "shorthand for --verbose")
	set.BoolVar(&c.quiet, "quiet", false, "print only warnings and errors")
	set.BoolVar(&c.quiet, "q", false, "shorthand for --quiet")
	set.BoolVar(&c.noColor, "no-color", false, "disable in-place progress updates")
	set.BoolVar(&c.dryRun, "dry-run", false, "plan the work and print it, writing nothing")
	set.BoolVar(&c.yes, "yes", false, "assume yes for confirmations")
	set.StringVar(&c.caCert, "ca-cert", "", "PEM bundle of extra certificate authorities to trust")
	set.BoolVar(&c.insecure, "insecure-skip-verify", false, "do not verify TLS certificates")
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
func (c *commonFlags) open(env *Env, set *flag.FlagSet, requireCatalog bool, root string) (*session, error) {
	c.noteExplicitFlags(set)

	mode, err := progress.ParseMode(c.output)
	if err != nil {
		return nil, Fail(CodeUsage, err)
	}

	check, err := scan.ParseCheckMode(c.check)
	if err != nil {
		return nil, Fail(CodeUsage, err)
	}

	overrides := config.Overrides{
		Endpoint:   c.endpoint,
		CatalogID:  c.catalogID,
		ConfigFile: c.configFile,
		CACertFile: c.caCert,
	}

	if c.insecureSet {
		overrides.InsecureSkipVerify = &c.insecure
	}

	resolved, err := config.Resolve(overrides)
	if err != nil {
		return nil, Fail(CodeUsage, err)
	}

	if requireCatalog && resolved.CatalogID == "" {
		return nil, Failf(CodeUsage,
			"no container given: pass --catalog-id, set %s, or create one with `%s init`",
			config.EnvCatalogID, env.Program)
	}

	client, err := drapi.New(drapi.Config{
		Endpoint:           resolved.Endpoint,
		Token:              resolved.Token,
		CACertFile:         resolved.CACertFile,
		InsecureSkipVerify: resolved.InsecureSkipVerify,
		MaxRetries:         c.retries,
	})
	if err != nil {
		return nil, Fail(CodeUsage, err)
	}

	reporter := progress.New(progress.Options{
		Mode:        mode,
		Human:       env.Stderr,
		Machine:     env.Stdout,
		Interactive: c.interactive(env),
		Verbose:     c.verbose || resolved.Verbose,
		Quiet:       c.quiet,
		Width:       terminalWidth(env),
	})

	matcher, err := c.buildMatcher(root)
	if err != nil {
		return nil, Fail(CodeUsage, err)
	}

	return &session{
		resolved: resolved,
		client:   client,
		store:    syncer.NewStore(client, resolved.CatalogID, c.metaPrefix),
		reporter: reporter,
		matcher:  matcher,
		check:    check,
	}, nil
}

// noteExplicitFlags records which flags the user actually set, so "absent" and "false" can be
// told apart.
func (c *commonFlags) noteExplicitFlags(set *flag.FlagSet) {
	set.Visit(func(f *flag.Flag) {
		if f.Name == "insecure-skip-verify" {
			c.insecureSet = true
		}
	})
}

// buildMatcher assembles the ignore rules: the ignore file first, then --exclude-from files,
// then the flags, so a flag can always override what a file said.
func (c *commonFlags) buildMatcher(root string) (*scan.Matcher, error) {
	var patterns []string

	if root != "" && c.ignoreFile != "" {
		fromFile, err := scan.LoadIgnoreFile(joinRoot(root, c.ignoreFile))
		if err != nil {
			return nil, err
		}

		patterns = append(patterns, fromFile...)
	}

	for _, name := range c.excludeFrom {
		fromFile, err := scan.LoadIgnoreFile(name)
		if err != nil {
			return nil, err
		}

		patterns = append(patterns, fromFile...)
	}

	patterns = append(patterns, c.exclude...)

	for _, pattern := range c.include {
		patterns = append(patterns, "!"+pattern)
	}

	if len(patterns) == 0 {
		return nil, nil
	}

	return scan.NewMatcher(patterns)
}

// interactive reports whether progress should redraw in place.
func (c *commonFlags) interactive(env *Env) bool {
	if c.noColor || c.quiet || c.output != string(progress.ModeText) {
		return false
	}

	file, ok := env.Stderr.(*os.File)
	if !ok {
		return false
	}

	return term.IsTerminal(int(file.Fd()))
}

func terminalWidth(env *Env) int {
	file, ok := env.Stderr.(*os.File)
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

// withTimeout applies the run's overall deadline, if one was requested.
func (c *commonFlags) withTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	if c.timeout <= 0 {
		return context.WithCancel(ctx)
	}

	return context.WithTimeout(ctx, c.timeout)
}

// resolveRoot turns the positional path argument into an absolute directory.
func resolveRoot(args []string) (string, error) {
	if len(args) == 0 {
		return "", Failf(CodeUsage, "a path is required")
	}

	if len(args) > 1 {
		return "", Failf(CodeUsage, "expected one path, got %d", len(args))
	}

	absolute, err := filepath.Abs(args[0])
	if err != nil {
		return "", Failf(CodeUsage, "resolving %s: %v", args[0], err)
	}

	return absolute, nil
}
