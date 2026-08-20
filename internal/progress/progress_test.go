// Copyright 2026 DataRobot, Inc. and its affiliates.
// All rights reserved.
// DataRobot, Inc.
// This is proprietary source code of DataRobot, Inc. and its affiliates.
// Released under the terms of DataRobot Tool and Utility Agreement.

package progress

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPhaseLinesAreAligned(t *testing.T) {
	t.Parallel()

	var human bytes.Buffer

	reporter := New(Options{Human: &human})
	reporter.Phase("scan", "12,208 files")
	reporter.Phase("upload", "412 MiB")

	lines := strings.Split(strings.TrimRight(human.String(), "\n"), "\n")
	require.Len(t, lines, 2)

	// The value column has to start at the same offset or the output is unscannable.
	assert.Equal(t, strings.Index(lines[0], "12,208"), strings.Index(lines[1], "412"))
}

// On a pipe, redrawing the same line would produce thousands of lines of noise, and the
// finished Phase line carries the same information anyway.
func TestUpdateIsDroppedWhenNotInteractive(t *testing.T) {
	t.Parallel()

	var human bytes.Buffer

	reporter := New(Options{Human: &human, Interactive: false})
	reporter.Update("pack", "3/8 packs")

	assert.Empty(t, human.String())

	reporter = New(Options{Human: &human, Interactive: true})
	reporter.Update("pack", "3/8 packs")
	assert.Contains(t, human.String(), "3/8 packs")
}

func TestUpdateIsOverwrittenInPlace(t *testing.T) {
	t.Parallel()

	var human bytes.Buffer

	reporter := New(Options{Human: &human, Interactive: true})
	reporter.Update("pack", "1/8")
	reporter.Update("pack", "2/8")
	reporter.Phase("pack", "8/8 done")

	output := human.String()
	assert.Contains(t, output, "\r\033[K", "the line must be cleared before being rewritten")
	assert.True(t, strings.HasSuffix(output, "8/8 done\n"))
}

// A silent warning is the same as no warning.
func TestWarningsSurviveQuiet(t *testing.T) {
	t.Parallel()

	var human bytes.Buffer

	reporter := New(Options{Human: &human, Quiet: true})
	reporter.Phase("scan", "suppressed")
	reporter.Warn("12 extraneous local files kept")
	reporter.Hint("suppressed hint")

	assert.NotContains(t, human.String(), "suppressed")
	assert.Contains(t, human.String(), "extraneous")
	assert.Equal(t, 1, reporter.Warnings())
}

func TestNoteRequiresVerbose(t *testing.T) {
	t.Parallel()

	var human bytes.Buffer

	reporter := New(Options{Human: &human})
	reporter.Note("hidden detail")
	assert.Empty(t, human.String())

	reporter = New(Options{Human: &human, Verbose: true})
	reporter.Note("shown detail")
	assert.Contains(t, human.String(), "shown detail")
}

// JSON goes to stdout and human text to stderr, so the two never interleave in a pipeline.
func TestJSONModeSeparatesStreams(t *testing.T) {
	t.Parallel()

	var human, machine bytes.Buffer

	reporter := New(Options{Mode: ModeJSON, Human: &human, Machine: &machine})
	reporter.Phase("scan", "12 files")
	reporter.Warn("something odd")
	reporter.Event("done", map[string]any{"generation": 42})

	assert.Empty(t, human.String(), "JSON mode must not also write human text")

	lines := strings.Split(strings.TrimRight(machine.String(), "\n"), "\n")
	require.Len(t, lines, 3)

	var first map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[0]), &first))
	assert.Equal(t, "scan", first["event"])
	assert.Equal(t, "12 files", first["message"])
	assert.NotEmpty(t, first["at"])

	var last map[string]any
	require.NoError(t, json.Unmarshal([]byte(lines[2]), &last))
	assert.EqualValues(t, 42, last["generation"])
}

func TestParseMode(t *testing.T) {
	t.Parallel()

	mode, err := ParseMode("json")
	require.NoError(t, err)
	assert.Equal(t, ModeJSON, mode)

	_, err = ParseMode("interpretive-dance")
	require.ErrorContains(t, err, "unknown output format")
}

func TestBar(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "#####.....", Bar(5, 10, 10))
	assert.Equal(t, "##########", Bar(10, 10, 10))
	assert.Equal(t, "..........", Bar(0, 10, 10))
	assert.Equal(t, "##########", Bar(99, 10, 10), "overshoot must not overflow the bar")
	assert.Equal(t, "..........", Bar(1, 0, 10), "an unknown total renders as empty")
}

func TestHumanBytesAndCount(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "0 B", HumanBytes(0))
	assert.Equal(t, "1.0 KiB", HumanBytes(1024))
	assert.Equal(t, "1.0 GiB", HumanBytes(1<<30))

	assert.Equal(t, "0", Count(0))
	assert.Equal(t, "999", Count(999))
	assert.Equal(t, "1,000", Count(1000))
	assert.Equal(t, "12,208", Count(12208))
	assert.Equal(t, "1,234,567", Count(1234567))
}

func TestRateAndETA(t *testing.T) {
	t.Parallel()

	assert.Equal(t, "1.0 MiB/s", Rate(1<<20, time.Second))
	assert.Equal(t, "—", Rate(0, time.Second))
	assert.Equal(t, "—", Rate(100, 0))

	assert.Equal(t, "10s", ETA(10, 20, 10*time.Second))
	assert.Equal(t, "—", ETA(0, 20, time.Second))
	assert.Equal(t, "—", ETA(20, 20, time.Second), "a finished transfer has no estimate")
}

func TestDiscardWritesNothing(t *testing.T) {
	t.Parallel()

	reporter := Discard()
	reporter.Phase("scan", "x")
	reporter.Warn("y")
	reporter.Close()

	assert.Equal(t, 1, reporter.Warnings())
}
