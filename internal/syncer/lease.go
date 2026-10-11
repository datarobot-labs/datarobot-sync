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

package syncer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/user"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/datarobot-labs/datarobot-sync/internal/drapi"
	"github.com/datarobot-labs/datarobot-sync/internal/manifest"
)

// DefaultLeaseTTL is how long a lease is honoured before it is treated as abandoned.
//
// Generous on purpose: expiry depends on wall clocks that different clients do not share, and
// a lease that expires early is worse than one that lingers — the point is to catch the case
// where two CI jobs push the same tree, not to fence anything.
const DefaultLeaseTTL = 30 * time.Minute

// leaseRenewalsPerTTL is how many renewal attempts fit in one TTL.
const leaseRenewalsPerTTL = 3

// leaseReleaseTimeout bounds the cleanup that drops a lease after the run that took it ended,
// including a run that was interrupted.
const leaseReleaseTimeout = 30 * time.Second

// leaseRenewTimeout bounds one renewal, so a hung request cannot stall the end of an upload. A
// lease with a short TTL bounds it further, see renewTimeout.
const leaseRenewTimeout = 30 * time.Second

// renewTimeout is how long one renewal may run: one renewal interval. A hung attempt then ends
// before the next tick, so a second attempt starts with a third of the lease left, and no
// attempt runs past the expiry of the lease it extends.
func (h *LeaseHolder) renewTimeout() time.Duration {
	return min(leaseRenewTimeout, h.ttl/leaseRenewalsPerTTL)
}

// Lease is an advisory claim on a container, written as an object under the locks prefix.
//
// It cannot prevent a concurrent write — the API has no compare-and-swap for that — but it
// turns the common failure from "two clients each spend six minutes uploading and one silently
// loses its work" into an immediate, legible refusal.
type Lease struct {
	ClientID  string `json:"clientId"`
	Host      string `json:"host,omitempty"`
	User      string `json:"user,omitempty"`
	PID       int    `json:"pid,omitempty"`
	StartedAt string `json:"startedAt"`
	ExpiresAt string `json:"expiresAt"`
	Sequence  int    `json:"sequence"`
	// ObjectName is where this lease was read from; not part of the serialized form.
	ObjectName string `json:"-"`
}

// Expired reports whether the lease's deadline has passed.
func (l Lease) Expired(now time.Time) bool {
	deadline, err := time.Parse(time.RFC3339Nano, l.ExpiresAt)
	if err != nil {
		// An unparseable deadline is treated as expired: refusing to run because of a
		// corrupt lease would be worse than the race it guards against.
		return true
	}

	return now.After(deadline)
}

// Describe renders a lease for a refusal message, so the user knows who to go and ask.
func (l Lease) Describe() string {
	who := l.ClientID

	if l.User != "" && l.Host != "" {
		who = fmt.Sprintf("%s@%s (%s)", l.User, l.Host, l.ClientID)
	}

	return fmt.Sprintf("%s, expires %s", who, l.ExpiresAt)
}

// leaseObjectName builds the object name for one heartbeat of a lease.
//
// Each heartbeat is a new object rather than an overwrite of the old one, which keeps the
// container append-only: nothing in it is ever replaced in place.
func leaseObjectName(metaPrefix, clientID string, sequence int) string {
	return path.Join(strings.TrimSuffix(manifest.LocksPrefix(metaPrefix), "/"),
		clientID+"-"+strconv.Itoa(sequence)+".lock")
}

// parseLeaseObjectName recovers the client id and sequence from a lease object name.
func parseLeaseObjectName(metaPrefix, objectName string) (clientID string, sequence int, ok bool) {
	prefix := manifest.LocksPrefix(metaPrefix)

	if !strings.HasPrefix(objectName, prefix) || !strings.HasSuffix(objectName, ".lock") {
		return "", 0, false
	}

	stem := strings.TrimSuffix(strings.TrimPrefix(objectName, prefix), ".lock")

	// Split on the last dash, not the first: the sequence number is the suffix, and a client
	// id is allowed to contain dashes.
	dash := strings.LastIndex(stem, "-")
	if dash <= 0 || dash == len(stem)-1 {
		return "", 0, false
	}

	parsed, err := strconv.Atoi(stem[dash+1:])
	if err != nil {
		return "", 0, false
	}

	return stem[:dash], parsed, true
}

// loadLease reads one lease object.
func (s *Store) loadLease(ctx context.Context, objectName string) (Lease, error) {
	content, err := s.ReadObject(ctx, objectName)
	if err != nil {
		return Lease{}, err
	}

	var lease Lease
	if err := json.Unmarshal(content, &lease); err != nil {
		return Lease{}, fmt.Errorf("parsing lease %s: %w", objectName, err)
	}

	lease.ObjectName = objectName

	return lease, nil
}

// ForeignLeases returns the unexpired leases held by anyone other than clientID.
func ForeignLeases(leases []Lease, clientID string, now time.Time) []Lease {
	var held []Lease

	for _, lease := range leases {
		if lease.ClientID == clientID || lease.Expired(now) {
			continue
		}

		held = append(held, lease)
	}

	sort.Slice(held, func(i, j int) bool { return held[i].ObjectName < held[j].ObjectName })

	return held
}

// LeaseHeldError is a refusal to start because someone else is mid-push.
type LeaseHeldError struct {
	Leases []Lease
}

func (e *LeaseHeldError) Error() string {
	descriptions := make([]string, 0, len(e.Leases))
	for _, lease := range e.Leases {
		descriptions = append(descriptions, lease.Describe())
	}

	return fmt.Sprintf("another dr-sync run holds this container: %s (pass --force to ignore)",
		strings.Join(descriptions, "; "))
}

// LeaseHolder maintains this process's lease on a container.
type LeaseHolder struct {
	store    *Store
	clientID string
	ttl      time.Duration
	sequence int
	// current is the object name of the lease this process most recently wrote.
	current string
	// superseded holds older leases whose deletion failed. They stay visible to other clients
	// until they expire, so every later renewal and the release retry them.
	superseded []string
}

// AcquireLease claims the container, refusing if someone else holds an unexpired lease.
func AcquireLease(ctx context.Context, store *Store, state *State, clientID string, ttl time.Duration, force bool) (*LeaseHolder, error) {
	if ttl <= 0 {
		ttl = DefaultLeaseTTL
	}

	if held := ForeignLeases(state.Leases, clientID, time.Now()); len(held) > 0 && !force {
		return nil, &LeaseHeldError{Leases: held}
	}

	holder := &LeaseHolder{store: store, clientID: clientID, ttl: ttl, sequence: ownMaxSequence(state.Leases, clientID)}

	if err := holder.write(ctx); err != nil {
		// The claim may have landed despite the error. Clean up with a context that survives
		// the one that just failed.
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), leaseReleaseTimeout)
		defer cancel()

		if releaseErr := holder.Release(cleanupCtx); releaseErr != nil {
			return nil, errors.Join(err, releaseErr)
		}

		return nil, err
	}

	return holder, nil
}

// ownMaxSequence returns the highest sequence number clientID has already written among
// leases, or 0 if it has none.
//
// A fresh LeaseHolder starts from here rather than always at 0: a stable ClientID — watch's
// whole point, per SPEC.md, is one identity for its entire lifetime rather than one per push —
// means AcquireLease is called many times for the same clientID across a long run. Starting
// every LeaseHolder at sequence 0 would make every one of those calls target the same object
// name, clientId-1.lock; that is harmless while the previous call's Release ran, but a cycle
// that crashed mid-commit or whose Release itself failed leaves that name occupied, and
// Overwrite: error then rejects the next cycle's commit outright — not a foreign-lease refusal
// (this clientID is never foreign to itself), but a collision with its own leftover.
func ownMaxSequence(leases []Lease, clientID string) int {
	highest := 0

	for _, lease := range leases {
		if lease.ClientID == clientID && lease.Sequence > highest {
			highest = lease.Sequence
		}
	}

	return highest
}

// Renew extends the lease by writing the next heartbeat and dropping the previous one.
//
// The new heartbeat is recorded before the old one is deleted. If the delete fails, the old
// object is remembered and deleted on the next renewal or at Release, so a transient failure
// cannot leave a live lease behind that refuses another client for a whole TTL.
func (h *LeaseHolder) Renew(ctx context.Context) error {
	previous := h.current

	if err := h.write(ctx); err != nil {
		return err
	}

	if previous != "" {
		h.superseded = append(h.superseded, previous)
	}

	return h.dropSuperseded(ctx)
}

// dropSuperseded deletes the leases this process has replaced, keeping the ones that fail.
func (h *LeaseHolder) dropSuperseded(ctx context.Context) error {
	if len(h.superseded) == 0 {
		return nil
	}

	if _, err := h.store.Delete(ctx, h.superseded); err != nil {
		return fmt.Errorf("dropping the previous lease: %w", err)
	}

	h.superseded = nil

	return nil
}

// KeepAlive renews the lease every third of its TTL until the returned stop function runs.
//
// A renewal that fails is passed to onError and retried on the next tick rather than ending the
// loop, so one failure still leaves a second attempt inside the TTL. A renewal always runs to
// completion, bounded by renewTimeout, not by ctx or stop: abandoning one mid-flight
// can leave the new lease written but unrecorded, and Release would then miss it. The stop
// function waits for a renewal in flight, so call it before Release; the two share the
// holder's state. A nil holder (locking disabled), or one whose TTL is too short to split into
// renewals, returns a stop function that does nothing.
func (h *LeaseHolder) KeepAlive(ctx context.Context, onError func(error)) (stop func()) {
	if h == nil || h.ttl/leaseRenewalsPerTTL <= 0 {
		return func() {}
	}

	quit := make(chan struct{})
	done := make(chan struct{})

	go func() {
		defer close(done)

		ticker := time.NewTicker(h.ttl / leaseRenewalsPerTTL)
		defer ticker.Stop()

		for {
			select {
			case <-quit:
				return
			case <-ticker.C:
				renewCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), h.renewTimeout())
				err := h.Renew(renewCtx)

				cancel()

				if err != nil {
					onError(err)
				}
			}
		}
	}()

	var once sync.Once

	return func() {
		once.Do(func() { close(quit) })
		<-done
	}
}

// Release drops the lease and reports whether it managed to. After an error the holder keeps
// the names it could not delete, so calling Release again retries them.
//
// A caller should warn rather than fail on an error: a lease left behind expires on its own,
// and failing a completed sync because a cleanup call did not land would be the wrong trade.
func (h *LeaseHolder) Release(ctx context.Context) error {
	if h == nil {
		return nil
	}

	names := slices.Clone(h.superseded)
	if h.current != "" {
		names = append(names, h.current)
	}

	if len(names) == 0 {
		return nil
	}

	if _, err := h.store.Delete(ctx, names); err != nil {
		return fmt.Errorf("releasing the lease on %s: %w", strings.Join(names, ", "), err)
	}

	h.current = ""
	h.superseded = nil

	return nil
}

// Expiry returns when the current lease lapses.
func (h *LeaseHolder) Expiry() time.Time {
	return time.Now().Add(h.ttl)
}

func (h *LeaseHolder) write(ctx context.Context) error {
	h.sequence++

	now := time.Now().UTC()

	lease := Lease{
		ClientID:  h.clientID,
		PID:       os.Getpid(),
		StartedAt: now.Format(time.RFC3339Nano),
		ExpiresAt: now.Add(h.ttl).Format(time.RFC3339Nano),
		Sequence:  h.sequence,
	}

	if host, err := os.Hostname(); err == nil {
		lease.Host = host
	}

	if current, err := user.Current(); err == nil {
		lease.User = current.Username
	}

	content, err := json.Marshal(lease)
	if err != nil {
		return fmt.Errorf("encoding lease: %w", err)
	}

	name := leaseObjectName(h.store.MetaPrefix(), h.clientID, h.sequence)

	object := Object{Name: name, Content: content, Size: int64(len(content))}

	if err := h.store.Commit(ctx, nil, []Object{object}, CommitOptions{Overwrite: drapi.OverwriteError}); err != nil {
		// A failed commit may still have landed, for example when the response arrives after a
		// timeout. Deleting a name that does not exist is harmless, so remember it for the next
		// renewal and the release rather than leave a live lease nobody tracks. A conflict is the
		// exception: the commit was rejected whole because the name exists, so the object is
		// someone else's and must stay.
		if !drapi.IsConflict(err) {
			h.superseded = append(h.superseded, name)
		}

		return fmt.Errorf("claiming the container: %w", err)
	}

	h.current = name

	return nil
}

// StaleLeaseNames returns lease objects that have expired, which a garbage collection pass can
// clear away.
func StaleLeaseNames(leases []Lease, now time.Time) []string {
	var names []string

	for _, lease := range leases {
		if lease.Expired(now) && lease.ObjectName != "" {
			names = append(names, lease.ObjectName)
		}
	}

	sort.Strings(names)

	return names
}
