// Copyright 2026 DataRobot, Inc. and its affiliates.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package progress renders what a sync is doing.
//
// Human output goes to stderr and machine output to stdout, so `--output json` composes in a
// pipeline while the operator still sees what is happening.
package progress

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Mode selects the renderer.
type Mode string

const (
	// ModeText writes one labelled line per phase, updating in place on a terminal.
	ModeText Mode = "text"
	// ModeJSON writes one JSON object per event to stdout.
	ModeJSON Mode = "json"
)

// ParseMode validates an output mode name.
func ParseMode(value string) (Mode, error) {
	switch Mode(value) {
	case ModeText, ModeJSON:
		return Mode(value), nil
	default:
		return "", fmt.Errorf("unknown output format %q: want text or json", value)
	}
}

// Options configures a reporter.
type Options struct {
	Mode Mode
	// Human receives the human-readable stream; normally stderr.
	Human io.Writer
	// Machine receives JSON events; normally stdout.
	Machine io.Writer
	// Interactive enables in-place updates. False for a pipe or a CI log, where redrawing
	// the same line just produces thousands of lines of noise.
	Interactive bool
	Verbose     bool
	Quiet       bool
	Width       int
}

// Reporter renders phases, progress and warnings.
type Reporter struct {
	opts Options

	mu sync.Mutex
	// activePhase is the phase whose line is currently being rewritten in place.
	activePhase string
	dirty       bool
	warnings    int
}

// New builds a reporter.
func New(opts Options) *Reporter {
	if opts.Mode == "" {
		opts.Mode = ModeText
	}

	if opts.Width <= 0 {
		opts.Width = 80
	}

	return &Reporter{opts: opts}
}

// Discard is a reporter that renders nothing, for tests.
func Discard() *Reporter {
	return New(Options{Mode: ModeText, Human: io.Discard, Machine: io.Discard, Quiet: true})
}

// phaseLabelWidth keeps the phase column aligned, so the eye can scan down the values.
const phaseLabelWidth = 7

// Phase closes any in-place line and prints a finished phase line.
func (r *Reporter) Phase(name, message string) {
	r.emit(name, message, true)
}

// Update rewrites the current phase line in place. On a non-interactive stream it is dropped,
// because the finished Phase line will carry the same information.
func (r *Reporter) Update(name, message string) {
	if !r.opts.Interactive {
		return
	}

	r.emit(name, message, false)
}

func (r *Reporter) emit(name, message string, final bool) {
	r.event(name, map[string]any{"message": message})

	if r.opts.Quiet || r.opts.Mode == ModeJSON || r.opts.Human == nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	line := fmt.Sprintf("%-*s %s", phaseLabelWidth, name, message)

	if r.dirty {
		// Return to the start of the line and clear it, so the new text replaces the old
		// rather than trailing its remains.
		fmt.Fprint(r.opts.Human, "\r\033[K")
	}

	if final {
		fmt.Fprintln(r.opts.Human, line)
		r.dirty = false
		r.activePhase = ""

		return
	}

	fmt.Fprint(r.opts.Human, truncate(line, r.opts.Width-1))
	r.dirty = true
	r.activePhase = name
}

// Warn reports a non-fatal problem. Warnings are always shown, even when quiet: a silent
// warning is the same as no warning.
func (r *Reporter) Warn(message string) {
	r.mu.Lock()
	r.warnings++
	r.mu.Unlock()

	r.event("warn", map[string]any{"message": message})

	if r.opts.Mode == ModeJSON || r.opts.Human == nil {
		return
	}

	r.finishLine()

	fmt.Fprintf(r.opts.Human, "%-*s %s\n", phaseLabelWidth, "warn", message)
}

// Note prints detail only when verbose.
func (r *Reporter) Note(message string) {
	if !r.opts.Verbose {
		return
	}

	r.Phase("note", message)
}

// Hint prints an actionable suggestion, such as a compaction that would pay for itself.
func (r *Reporter) Hint(message string) {
	if r.opts.Quiet {
		return
	}

	r.event("hint", map[string]any{"message": message})

	if r.opts.Mode == ModeJSON || r.opts.Human == nil {
		return
	}

	r.finishLine()

	fmt.Fprintf(r.opts.Human, "%-*s %s\n", phaseLabelWidth, "hint", message)
}

// Warnings reports how many warnings have been emitted, so a command can mention that there
// were some without repeating them.
func (r *Reporter) Warnings() int {
	r.mu.Lock()
	defer r.mu.Unlock()

	return r.warnings
}

// Event emits a machine-readable record. It is a no-op unless the mode is JSON.
func (r *Reporter) Event(kind string, fields map[string]any) {
	r.event(kind, fields)
}

func (r *Reporter) event(kind string, fields map[string]any) {
	if r.opts.Mode != ModeJSON || r.opts.Machine == nil {
		return
	}

	record := make(map[string]any, len(fields)+2)
	for key, value := range fields {
		record[key] = value
	}

	record["event"] = kind
	record["at"] = time.Now().UTC().Format(time.RFC3339Nano)

	encoded, err := json.Marshal(record)
	if err != nil {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	fmt.Fprintln(r.opts.Machine, string(encoded))
}

// finishLine ends an in-place line so the next output starts cleanly.
func (r *Reporter) finishLine() {
	r.mu.Lock()
	defer r.mu.Unlock()

	if !r.dirty {
		return
	}

	fmt.Fprint(r.opts.Human, "\r\033[K")
	r.dirty = false
	r.activePhase = ""
}

// Close flushes any pending in-place line.
func (r *Reporter) Close() {
	r.finishLine()
}

func truncate(s string, width int) string {
	if width <= 0 || len(s) <= width {
		return s
	}

	if width <= 1 {
		return s[:width]
	}

	return s[:width-1] + "…"
}

// Bar renders a fixed-width progress bar.
func Bar(current, total int64, width int) string {
	if width < 2 {
		width = 2
	}

	if total <= 0 {
		return strings.Repeat(".", width)
	}

	filled := int(float64(width) * float64(current) / float64(total))

	if filled < 0 {
		filled = 0
	}

	if filled > width {
		filled = width
	}

	return strings.Repeat("#", filled) + strings.Repeat(".", width-filled)
}

// Rate renders a transfer rate.
func Rate(bytes int64, elapsed time.Duration) string {
	if elapsed <= 0 || bytes <= 0 {
		return "—"
	}

	perSecond := float64(bytes) / elapsed.Seconds()

	return HumanBytes(int64(perSecond)) + "/s"
}

// ETA renders a remaining-time estimate.
func ETA(done, total int64, elapsed time.Duration) string {
	if done <= 0 || total <= done || elapsed <= 0 {
		return "—"
	}

	remaining := time.Duration(float64(elapsed) * float64(total-done) / float64(done))

	return remaining.Round(time.Second).String()
}

// HumanBytes renders a byte count in binary units.
func HumanBytes(n int64) string {
	const unit = 1024

	if n < unit {
		return fmt.Sprintf("%d B", n)
	}

	value := float64(n)

	for _, suffix := range []string{"KiB", "MiB", "GiB", "TiB", "PiB"} {
		value /= unit

		if value < unit {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}

	return fmt.Sprintf("%.1f EiB", value/unit)
}

// Count renders an integer with thousands separators, because a seven-digit file count is
// otherwise unreadable.
func Count(n int) string {
	text := strconv.Itoa(n)
	if len(text) <= 3 {
		return text
	}

	var out strings.Builder

	lead := len(text) % 3
	if lead > 0 {
		out.WriteString(text[:lead])
	}

	for i := lead; i < len(text); i += 3 {
		if out.Len() > 0 {
			out.WriteByte(',')
		}

		out.WriteString(text[i : i+3])
	}

	return out.String()
}
