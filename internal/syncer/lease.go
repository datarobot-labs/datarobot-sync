// Copyright 2026 DataRobot, Inc. and its affiliates.
// All rights reserved.
// DataRobot, Inc.
// This is proprietary source code of DataRobot, Inc. and its affiliates.
// Released under the terms of DataRobot Tool and Utility Agreement.

package syncer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/datarobot/golang-datarobot-filesync/internal/drapi"
	"github.com/datarobot/golang-datarobot-filesync/internal/manifest"
)

// DefaultLeaseTTL is how long a lease is honoured before it is treated as abandoned.
//
// Generous on purpose: expiry depends on wall clocks that different clients do not share, and
// a lease that expires early is worse than one that lingers — the point is to catch the case
// where two CI jobs push the same tree, not to fence anything.
const DefaultLeaseTTL = 30 * time.Minute

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

	base, digits, found := strings.Cut(stem, "-")
	if !found || base == "" {
		return "", 0, false
	}

	parsed, err := strconv.Atoi(digits)
	if err != nil {
		return "", 0, false
	}

	return base, parsed, true
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
}

// AcquireLease claims the container, refusing if someone else holds an unexpired lease.
func AcquireLease(ctx context.Context, store *Store, state *State, clientID string, ttl time.Duration, force bool) (*LeaseHolder, error) {
	if ttl <= 0 {
		ttl = DefaultLeaseTTL
	}

	if held := ForeignLeases(state.Leases, clientID, time.Now()); len(held) > 0 && !force {
		return nil, &LeaseHeldError{Leases: held}
	}

	holder := &LeaseHolder{store: store, clientID: clientID, ttl: ttl}

	if err := holder.write(ctx); err != nil {
		return nil, err
	}

	return holder, nil
}

// Renew extends the lease by writing the next heartbeat and dropping the previous one.
func (h *LeaseHolder) Renew(ctx context.Context) error {
	previous := h.current

	if err := h.write(ctx); err != nil {
		return err
	}

	if previous == "" {
		return nil
	}

	_, err := h.store.Delete(ctx, []string{previous})

	return err
}

// Release drops the lease and reports whether it managed to.
//
// A caller should warn rather than fail on an error: a lease left behind expires on its own,
// and failing a completed sync because a cleanup call did not land would be the wrong trade.
func (h *LeaseHolder) Release(ctx context.Context) error {
	if h == nil || h.current == "" {
		return nil
	}

	name := h.current
	h.current = ""

	if _, err := h.store.Delete(ctx, []string{name}); err != nil {
		return fmt.Errorf("releasing the lease on %s: %w", name, err)
	}

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
