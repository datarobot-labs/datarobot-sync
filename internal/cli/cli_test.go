// Copyright 2026 DataRobot, Inc. and its affiliates.
// All rights reserved.
// DataRobot, Inc.
// This is proprietary source code of DataRobot, Inc. and its affiliates.
// Released under the terms of DataRobot Tool and Utility Agreement.

package cli

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/datarobot/golang-datarobot-filesync/internal/config"
	"github.com/datarobot/golang-datarobot-filesync/internal/drapi"
	"github.com/datarobot/golang-datarobot-filesync/internal/manifest"
)

// isolate clears the variables config resolution reads, so a test never picks up the
// developer's real DataRobot credentials.
func isolate(t *testing.T) {
	t.Helper()

	for _, name := range []string{
		config.EnvEndpoint, config.EnvToken, config.EnvConfig, config.EnvCatalogID,
		config.EnvCLIDebug, config.EnvCLIVerbose, config.EnvCLISkipCertific,
		config.EnvCLICACert, config.EnvSSLCertFile, config.EnvPluginMode, "XDG_CONFIG_HOME",
	} {
		t.Setenv(name, "")
	}

	t.Setenv("HOME", t.TempDir())
}

// run executes the command tree and returns the exit code plus both streams.
func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()

	var out, errOut bytes.Buffer

	root := NewRoot()
	root.SetOut(&out)
	root.SetErr(&errOut)

	code := Execute(root, args)

	return code, out.String(), errOut.String()
}

func TestRootHelpListsEveryCommand(t *testing.T) {
	isolate(t)

	code, out, _ := run(t, "--help")
	require.Equal(t, CodeOK, code)

	for _, name := range []string{
		"init", "upload", "download", "watch", "status", "ls", "inspect", "compact", "gc", "version",
		// cobra contributes this one, and it is worth having.
		"completion",
	} {
		assert.Contains(t, out, name)
	}
}

// watch mirrors the local tree by default; upload stays additive unless asked otherwise. Both
// share the same flag registration, so this is what actually distinguishes them.
func TestWatchDefaultsToDeleteUnlikeUpload(t *testing.T) {
	isolate(t)

	_, uploadHelp, _ := run(t, "upload", "--help")
	_, watchHelp, _ := run(t, "watch", "--help")

	assert.NotContains(t, deleteFlagLine(t, uploadHelp), "default true",
		"upload must stay additive by default")
	assert.Contains(t, deleteFlagLine(t, watchHelp), "default true",
		"watch must mirror the local tree by default")
}

// deleteFlagLine finds the flag-listing line for --delete, not any mention of it in prose:
// the Long description talks about --delete too, so a plain substring search would find that
// instead of the flag definition it is meant to check.
func deleteFlagLine(t *testing.T, help string) string {
	t.Helper()

	for line := range strings.SplitSeq(help, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--delete") {
			return line
		}
	}

	t.Fatalf("--delete flag definition not found in help:\n%s", help)

	return ""
}

// A bare invocation should say what to do, not exit zero as though it worked.
func TestBareInvocationIsAUsageError(t *testing.T) {
	isolate(t)

	code, _, errOut := run(t)

	assert.Equal(t, CodeUsage, code)
	assert.Contains(t, errOut, "no subcommand given")
}

func TestUnknownCommandIsAUsageError(t *testing.T) {
	isolate(t)

	code, _, errOut := run(t, "uplaod", "./dir")

	assert.Equal(t, CodeUsage, code)
	assert.Contains(t, errOut, `unknown command "uplaod"`)
	assert.Contains(t, errOut, `did you mean "upload"`, "a near miss should be suggested")
}

// Cobra permutes arguments, so flags after the path work — the way anyone would actually type
// it. Without that, `upload ./dir --delete` fails.
func TestFlagsAfterThePathAreAccepted(t *testing.T) {
	isolate(t)

	// No credentials, so this stops at config resolution — but only after parsing succeeded,
	// which is the point being tested. A parse failure would report a different message.
	code, _, errOut := run(t, "upload", "./dir", "--delete", "--exclude", "*.log")

	assert.Equal(t, CodeUsage, code)
	assert.Contains(t, errOut, "DATAROBOT_API_TOKEN",
		"parsing must have succeeded and failed later, at credential resolution")
}

func TestMissingPathIsRejected(t *testing.T) {
	isolate(t)

	code, _, errOut := run(t, "upload")

	assert.Equal(t, CodeUsage, code)
	assert.Contains(t, errOut, "accepts 1 arg")
}

func TestCommandsThatTakeNoPathRejectOne(t *testing.T) {
	isolate(t)

	for _, name := range []string{"init", "gc", "ls", "inspect"} {
		code, _, errOut := run(t, name, "./dir")

		assert.Equal(t, CodeUsage, code, "%s should reject a path", name)
		assert.Contains(t, errOut, "unknown command", "%s: %s", name, errOut)
	}
}

func TestMissingCredentialsIsAUsageError(t *testing.T) {
	isolate(t)

	code, _, errOut := run(t, "upload", ".")

	assert.Equal(t, CodeUsage, code)
	assert.Contains(t, errOut, "DATAROBOT_ENDPOINT")
}

// A container is required for everything except init, which is what creates one.
func TestMissingContainerIsAUsageError(t *testing.T) {
	isolate(t)
	t.Setenv(config.EnvEndpoint, "https://app.datarobot.com/api/v2")
	t.Setenv(config.EnvToken, "token")

	code, _, errOut := run(t, "upload", ".")

	assert.Equal(t, CodeUsage, code)
	assert.Contains(t, errOut, "no container given")
	assert.Contains(t, errOut, "init")
}

func TestBadFlagValuesAreRejectedEarly(t *testing.T) {
	isolate(t)
	t.Setenv(config.EnvEndpoint, "https://app.datarobot.com/api/v2")
	t.Setenv(config.EnvToken, "token")
	t.Setenv(config.EnvCatalogID, "container-1")

	cases := map[string][]string{
		"unknown check mode":    {"upload", ".", "--check", "vibes"},
		"unknown output format": {"upload", ".", "--output", "interpretive-dance"},
		"cannot read":           {"upload", ".", "--slice-size", "lots"},
	}

	for want, args := range cases {
		code, _, errOut := run(t, args...)

		assert.Equal(t, CodeUsage, code, "%v", args)
		assert.Contains(t, errOut, want, "%v", args)
	}
}

func TestVersionCommand(t *testing.T) {
	isolate(t)

	code, out, _ := run(t, "version")

	require.Equal(t, CodeOK, code)
	assert.Contains(t, out, "dr-sync")
	assert.Contains(t, out, "go1.")
}

// Cobra derives Name() from the first word of Use, and every usage line is built from
// CommandPath(), which is built from Name(). Under the dr CLI the invocation is two words, so
// without the display-name annotation the help reads "dr upload <path>" — wrong, and not
// copy-pasteable.
func TestUsageTextReadsBackTheInvocation(t *testing.T) {
	isolate(t)
	t.Setenv(config.EnvPluginMode, "1")

	code, out, _ := run(t, "--help")
	require.Equal(t, CodeOK, code)

	assert.Contains(t, out, "dr sync [flags]")
	assert.Contains(t, out, "dr sync [command]")
	assert.Contains(t, out, `Use "dr sync [command] --help"`)

	// A subcommand inherits the corrected path from its parent.
	code, subcommand, _ := run(t, "upload", "--help")
	require.Equal(t, CodeOK, code)
	assert.Contains(t, subcommand, "dr sync upload <path> [flags]")

	code, versionOut, _ := run(t, "--version")
	require.Equal(t, CodeOK, code)
	assert.Contains(t, versionOut, "dr sync ")
}

func TestCompletionIsAvailable(t *testing.T) {
	isolate(t)

	code, out, _ := run(t, "completion", "bash")

	require.Equal(t, CodeOK, code)
	// The script embeds whatever the binary is called, which under `go test` is the test
	// binary, so the assertion is on the machinery rather than the name.
	assert.Contains(t, out, "bash completion")
	assert.Contains(t, out, "__complete")
}

func TestParseOverwriteRejectsTheStrategiesThatWouldOrphanAPack(t *testing.T) {
	t.Parallel()

	for _, valid := range []drapi.OverwriteStrategy{drapi.OverwriteError, drapi.OverwriteReplace} {
		got, err := parseOverwrite(string(valid))
		require.NoError(t, err)
		assert.Equal(t, valid, got)
	}

	// rename would store the pack as "p_x (1).tar.xz"; skip would silently keep whatever was
	// there. Either way the manifest points at content that is not where it says.
	for _, rejected := range []drapi.OverwriteStrategy{drapi.OverwriteRename, drapi.OverwriteSkip} {
		_, err := parseOverwrite(string(rejected))
		require.ErrorContains(t, err, "the manifest cannot find")
		assert.Equal(t, CodeUsage, CodeFor(err))
	}

	_, err := parseOverwrite("whatever")
	require.ErrorContains(t, err, "unknown --overwrite")
}

func TestParseBytes(t *testing.T) {
	t.Parallel()

	cases := map[string]int64{
		"1024":   1024,
		"64KiB":  64 << 10,
		"64kib":  64 << 10,
		"4MiB":   4 << 20,
		"2GiB":   2 << 30,
		"1TiB":   1 << 40,
		"1MB":    1000 * 1000,
		"1M":     1 << 20,
		"512B":   512,
		" 8MiB ": 8 << 20,
		"1.5MiB": 3 << 19,
	}

	for given, want := range cases {
		got, err := ParseBytes(given)
		require.NoError(t, err, "input %q", given)
		assert.Equal(t, want, got, "input %q", given)
	}

	for _, bad := range []string{"", "   ", "lots", "12XiB", "MiB"} {
		_, err := ParseBytes(bad)
		require.Error(t, err, "input %q should be rejected", bad)
	}
}

func TestExitCodeMapping(t *testing.T) {
	t.Parallel()

	assert.Equal(t, CodeOK, CodeFor(nil))
	assert.Equal(t, CodeInternal, CodeFor(errors.New("something")))
	assert.Equal(t, CodeConflict, CodeFor(Failf(CodeConflict, "held")))

	// The code survives wrapping, so a caller deep in the stack can set it.
	wrapped := errors.Join(errors.New("context"), Failf(CodeIntegrity, "bad digest"))
	assert.Equal(t, CodeIntegrity, CodeFor(wrapped))

	assert.NoError(t, Fail(CodeConflict, nil), "wrapping nil must stay nil")
}

func TestSyncRoot(t *testing.T) {
	t.Parallel()

	root, err := syncRoot([]string{"."})
	require.NoError(t, err)
	assert.NotEqual(t, ".", root, "the path must be made absolute")

	_, err = syncRoot(nil)
	require.ErrorContains(t, err, "a path is required")
	assert.Equal(t, CodeUsage, CodeFor(err))
}

func TestRenderPath(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "a/b/", renderPathFor(t, "dir", "a/b", ""))
	assert.Equal(t, "link -> ../target", renderPathFor(t, "symlink", "link", "../target"))
	assert.Equal(t, "a.txt", renderPathFor(t, "file", "a.txt", ""))
}

// renderPathFor keeps the manifest import out of the assertions above.
func renderPathFor(t *testing.T, kind, path, target string) string {
	t.Helper()

	return renderPath(entryFor(kind, path, target))
}

func TestGlobalFlagsAreRegisteredOnce(t *testing.T) {
	isolate(t)

	root := NewRoot()

	// A persistent flag defined twice would panic at registration; reaching here proves the
	// shared set is registered on the root rather than per command.
	for _, name := range []string{
		config.KeyCatalogID, config.KeyEndpoint, config.KeyConfigFile, "meta-prefix",
		"check", "output", "exclude", "jobs", "dry-run",
	} {
		assert.NotNil(t, root.PersistentFlags().Lookup(name), "missing persistent flag --%s", name)
	}

	// Shorthands people expect.
	for _, short := range []string{"j", "v", "q", "y", "k"} {
		assert.NotNil(t, root.PersistentFlags().ShorthandLookup(short), "missing shorthand -%s", short)
	}
}

// entryFor builds a manifest entry for the rendering assertions.
func entryFor(kind, path, target string) manifest.Entry {
	return manifest.Entry{Type: manifest.EntryType(kind), Path: path, Target: target}
}
