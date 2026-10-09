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
//
// It also holds no memory of when the last push actually started: Initial and Poll triggers
// reach the caller without ever going through the debouncer, so a floor tracked in here would
// miss them. The caller passes its own record of the last delivered trigger into Deadline and
// Ready instead, so the --min-interval floor binds every kind of push, not just debounced ones.
type debouncer struct {
	debounce    time.Duration
	minInterval time.Duration

	pending       bool
	pendingEvents int
	ignoreFile    bool
	sample        []string
	lastEventAt   time.Time
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
// debounce, and the last push the caller delivered (if any) must have started at least
// minInterval ago. lastPushStart and havePushed describe that last delivered trigger — of any
// reason, not only a debounced one. The zero time means nothing is pending.
func (d *debouncer) Deadline(lastPushStart time.Time, havePushed bool) time.Time {
	if !d.pending {
		return time.Time{}
	}

	quiet := d.lastEventAt.Add(d.debounce)

	if !havePushed {
		return quiet
	}

	floor := lastPushStart.Add(d.minInterval)
	if floor.After(quiet) {
		return floor
	}

	return quiet
}

// Ready reports whether a pending batch should fire at now, given the caller's record of the
// last delivered trigger (see Deadline).
func (d *debouncer) Ready(now, lastPushStart time.Time, havePushed bool) bool {
	return d.pending && !now.Before(d.Deadline(lastPushStart, havePushed))
}

// Fire returns a Trigger for the pending batch and resets for the next one. The caller is
// responsible for only calling this when Ready reports true, and for recording when the
// trigger is actually delivered — which Fire does not know, since a busy consumer can leave it
// blocked in the channel well past this call, and no longer tracks itself.
func (d *debouncer) Fire() Trigger {
	reason := ReasonChange
	if d.ignoreFile {
		reason = ReasonIgnoreFile
	}

	trigger := Trigger{Reason: reason, Events: d.pendingEvents, Sample: d.sample}

	d.pending = false
	d.pendingEvents = 0
	d.ignoreFile = false
	d.sample = nil

	return trigger
}
