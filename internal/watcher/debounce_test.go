// Copyright 2026 DataRobot, Inc. and its affiliates.
// All rights reserved.
// DataRobot, Inc.
// This is proprietary source code of DataRobot, Inc. and its affiliates.
// Released under the terms of DataRobot Tool and Utility Agreement.

package watcher

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// epoch anchors every test's timestamps, so the debouncer is exercised with fabricated instants
// rather than real sleeps.
var epoch = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

func TestDebouncerWaitsForQuiet(t *testing.T) {
	t.Parallel()

	d := newDebouncer(2*time.Second, 15*time.Second)

	d.Event(epoch, "a.txt", false)
	assert.False(t, d.Ready(epoch.Add(1*time.Second)), "not quiet yet")
	assert.True(t, d.Ready(epoch.Add(2*time.Second)), "quiet for exactly the debounce window")
}

func TestDebouncerCoalescesABurst(t *testing.T) {
	t.Parallel()

	d := newDebouncer(2*time.Second, 15*time.Second)

	d.Event(epoch, "a.txt", false)
	d.Event(epoch.Add(500*time.Millisecond), "b.txt", false)
	d.Event(epoch.Add(1*time.Second), "c.txt", false)

	// The deadline tracks the *last* event, not the first: a burst that keeps arriving must
	// keep pushing the fire time out, or a busy editor would trigger mid-save.
	assert.False(t, d.Ready(epoch.Add(2*time.Second)), "still within debounce of the last event")

	fireAt := epoch.Add(3 * time.Second)
	require.True(t, d.Ready(fireAt))

	trigger := d.Fire(fireAt)
	assert.Equal(t, ReasonChange, trigger.Reason)
	assert.Equal(t, 3, trigger.Events)
	assert.Equal(t, []string{"a.txt", "b.txt", "c.txt"}, trigger.Sample)
}

func TestDebouncerEnforcesTheMinIntervalFloor(t *testing.T) {
	t.Parallel()

	d := newDebouncer(1*time.Second, 10*time.Second)

	d.Event(epoch, "a.txt", false)
	first := epoch.Add(1 * time.Second)
	require.True(t, d.Ready(first))
	d.Fire(first)

	// A new event arrives well within the floor. Even once it has been quiet for the
	// debounce window, the floor measured from the previous push's start must still bind.
	quietAt := first.Add(2 * time.Second)
	d.Event(quietAt, "b.txt", false)
	assert.False(t, d.Ready(quietAt.Add(1*time.Second)), "the floor has not elapsed yet")

	floorElapsed := first.Add(10 * time.Second)
	assert.True(t, d.Ready(floorElapsed), "the floor has elapsed")
}

func TestDebouncerEventsDuringAPushBecomeExactlyOneFollowUp(t *testing.T) {
	t.Parallel()

	d := newDebouncer(1*time.Second, 5*time.Second)

	d.Event(epoch, "a.txt", false)
	pushStart := epoch.Add(1 * time.Second)
	require.True(t, d.Ready(pushStart))
	d.Fire(pushStart)

	// Three more events arrive while the (hypothetical) push is still running.
	d.Event(pushStart.Add(100*time.Millisecond), "b.txt", false)
	d.Event(pushStart.Add(200*time.Millisecond), "c.txt", false)
	d.Event(pushStart.Add(300*time.Millisecond), "d.txt", false)

	deadline := d.Deadline()
	require.False(t, deadline.IsZero())

	// One Ready+Fire at the deadline must drain everything coalesced so far, not fire once
	// per event.
	require.True(t, d.Ready(deadline))
	trigger := d.Fire(deadline)
	assert.Equal(t, 3, trigger.Events)

	assert.False(t, d.Ready(deadline), "nothing pending after the single follow-up fired")
}

func TestDebouncerReasonPrioritizesTheIgnoreFile(t *testing.T) {
	t.Parallel()

	d := newDebouncer(time.Second, time.Second)

	d.Event(epoch, "src/main.go", false)
	d.Event(epoch, ".drsyncignore", true)

	trigger := d.Fire(epoch.Add(time.Second))
	assert.Equal(t, ReasonIgnoreFile, trigger.Reason)
}

func TestDebouncerSampleIsCapped(t *testing.T) {
	t.Parallel()

	d := newDebouncer(time.Second, time.Second)

	for i := range maxSample + 5 {
		d.Event(epoch, "file"+string(rune('a'+i)), false)
	}

	trigger := d.Fire(epoch.Add(time.Second))
	assert.Len(t, trigger.Sample, maxSample)
	assert.Equal(t, maxSample+5, trigger.Events, "the count is not truncated, only the sample")
}

func TestDebouncerReportsNoDeadlineWhenNothingIsPending(t *testing.T) {
	t.Parallel()

	d := newDebouncer(time.Second, time.Second)

	assert.True(t, d.Deadline().IsZero())
	assert.False(t, d.Ready(epoch))
}
