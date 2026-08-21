// Copyright 2026 DataRobot, Inc. and its affiliates.
// All rights reserved.
// DataRobot, Inc.
// This is proprietary source code of DataRobot, Inc. and its affiliates.
// Released under the terms of DataRobot Tool and Utility Agreement.

package watcher

import "time"

// maxSample caps how many changed paths a Trigger carries for --verbose output. A daemon that
// coalesced ten thousand events has no use for all ten thousand names.
const maxSample = 5

// debouncer decides when a burst of filesystem events should become one push.
//
// It holds no timer of its own: every method takes the current time explicitly, so the whole
// state machine is exercised by feeding it timestamps rather than waiting on real clocks. The
// caller (Watcher) is the one thing that owns a real timer, armed against Deadline.
type debouncer struct {
	debounce    time.Duration
	minInterval time.Duration

	pending       bool
	pendingEvents int
	ignoreFile    bool
	sample        []string
	lastEventAt   time.Time

	havePushed    bool
	lastPushStart time.Time
}

func newDebouncer(debounce, minInterval time.Duration) *debouncer {
	return &debouncer{debounce: debounce, minInterval: minInterval}
}

// Event records that something changed at t. relPath is recorded as a sample if there is room;
// ignoreFile marks that the change touched the ignore file itself, which takes priority over
// every other reason once the batch fires.
func (d *debouncer) Event(t time.Time, relPath string, ignoreFile bool) {
	d.pending = true
	d.pendingEvents++
	d.lastEventAt = t

	if ignoreFile {
		d.ignoreFile = true
	}

	if relPath != "" && len(d.sample) < maxSample {
		d.sample = append(d.sample, relPath)
	}
}

// Deadline is the earliest time a pending batch may fire: the tree must have been quiet for
// debounce, and the previous push (if any) must have started at least minInterval ago. The
// zero time means nothing is pending.
func (d *debouncer) Deadline() time.Time {
	if !d.pending {
		return time.Time{}
	}

	quiet := d.lastEventAt.Add(d.debounce)

	if !d.havePushed {
		return quiet
	}

	floor := d.lastPushStart.Add(d.minInterval)
	if floor.After(quiet) {
		return floor
	}

	return quiet
}

// Ready reports whether a pending batch should fire at now.
func (d *debouncer) Ready(now time.Time) bool {
	return d.pending && !now.Before(d.Deadline())
}

// Fire returns a Trigger for the pending batch and resets for the next one. The caller is
// responsible for only calling this when Ready reports true.
func (d *debouncer) Fire(now time.Time) Trigger {
	reason := ReasonChange
	if d.ignoreFile {
		reason = ReasonIgnoreFile
	}

	trigger := Trigger{Reason: reason, Events: d.pendingEvents, Sample: d.sample}

	d.pending = false
	d.pendingEvents = 0
	d.ignoreFile = false
	d.sample = nil
	d.havePushed = true
	d.lastPushStart = now

	return trigger
}
