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
	"github.com/datarobot/golang-datarobot-filesync/internal/version"
)

// DefaultWriterTTL is how long a resident writer's registry entry is honoured before another
// writer treats it as gone.
//
// Shorter than DefaultLeaseTTL on purpose: a lease guards one push and errs toward patience, but
// a writer registration exists so a peer notices quickly that this process is still around.
const DefaultWriterTTL = 5 * time.Minute

// Writer is one resident process's presence on a container, written as an object under the
// writers prefix.
//
// It is advisory, exactly like a Lease, and for the same reason: the API has no
// compare-and-swap, so there is nothing to enforce with it. Its purpose is visibility — a
// process can tell that a peer is active and hold back on deletes rather than racing it. This
// is the foundation the eventual multi-writer FUSE mode builds on.
type Writer struct {
	ClientID string `json:"clientId"`
	Host     string `json:"host,omitempty"`
	User     string `json:"user,omitempty"`
	PID      int    `json:"pid,omitempty"`
	Version  string `json:"version,omitempty"`
	// Mode names what this process is doing with the container, e.g. "watch" or "upload".
	Mode string `json:"mode,omitempty"`
	// Root is the local sync root this process is writing from, for a human reading the list.
	Root           string `json:"root,omitempty"`
	StartedAt      string `json:"startedAt"`
	RefreshedAt    string `json:"refreshedAt"`
	ExpiresAt      string `json:"expiresAt"`
	Sequence       int    `json:"sequence"`
	LastGeneration int64  `json:"lastGeneration,omitempty"`
	LastManifestID string `json:"lastManifestId,omitempty"`
	// ObjectName is where this entry was read from; not part of the serialized form.
	ObjectName string `json:"-"`
}

// Expired reports whether the writer's registration has lapsed.
func (w Writer) Expired(now time.Time) bool {
	deadline, err := time.Parse(time.RFC3339Nano, w.ExpiresAt)
	if err != nil {
		// An unparseable deadline is treated as expired: a corrupt entry must not keep another
		// writer waiting on it forever.
		return true
	}

	return now.After(deadline)
}

// Describe renders a writer for a warning message, so the reader knows who else is active.
func (w Writer) Describe() string {
	who := w.ClientID

	if w.User != "" && w.Host != "" {
		who = fmt.Sprintf("%s@%s (%s)", w.User, w.Host, w.ClientID)
	}

	if w.Mode != "" {
		who += " running " + w.Mode
	}

	return who
}

// writerObjectName builds the object name for one heartbeat of a writer's registration.
//
// Each heartbeat is a new object rather than an overwrite of the old one, for the same reason a
// lease heartbeat is: the container stays append-only, and nothing in it is ever replaced in
// place.
func writerObjectName(metaPrefix, clientID string, sequence int) string {
	return path.Join(strings.TrimSuffix(manifest.WritersPrefix(metaPrefix), "/"),
		clientID+"-"+strconv.Itoa(sequence)+".json")
}

// parseWriterObjectName recovers the client id and sequence from a writer object name.
func parseWriterObjectName(metaPrefix, objectName string) (clientID string, sequence int, ok bool) {
	prefix := manifest.WritersPrefix(metaPrefix)

	if !strings.HasPrefix(objectName, prefix) || !strings.HasSuffix(objectName, ".json") {
		return "", 0, false
	}

	stem := strings.TrimSuffix(strings.TrimPrefix(objectName, prefix), ".json")

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

// loadWriter reads one writer registration object.
func (s *Store) loadWriter(ctx context.Context, objectName string) (Writer, error) {
	content, err := s.ReadObject(ctx, objectName)
	if err != nil {
		return Writer{}, err
	}

	var writer Writer
	if err := json.Unmarshal(content, &writer); err != nil {
		return Writer{}, fmt.Errorf("parsing writer registration %s: %w", objectName, err)
	}

	writer.ObjectName = objectName

	return writer, nil
}

// ForeignWriters returns the unexpired registrations held by anyone other than clientID.
func ForeignWriters(writers []Writer, clientID string, now time.Time) []Writer {
	var foreign []Writer

	for _, writer := range writers {
		if writer.ClientID == clientID || writer.Expired(now) {
			continue
		}

		foreign = append(foreign, writer)
	}

	sort.Slice(foreign, func(i, j int) bool { return foreign[i].ObjectName < foreign[j].ObjectName })

	return foreign
}

// StaleWriterNames returns writer registrations that have expired, which a garbage collection
// pass can clear away.
//
// Writer objects are outside the pack/manifest reference graph, so nothing else will ever
// reclaim them; without this they would accumulate for as long as the container exists.
func StaleWriterNames(writers []Writer, now time.Time) []string {
	var names []string

	for _, writer := range writers {
		if writer.Expired(now) && writer.ObjectName != "" {
			names = append(names, writer.ObjectName)
		}
	}

	sort.Strings(names)

	return names
}

// WriterRegistration maintains this process's presence entry on a container.
type WriterRegistration struct {
	store    *Store
	clientID string
	ttl      time.Duration
	mode     string
	root     string
	sequence int
	// current is the object name of the entry this process most recently wrote.
	current string
}

// RegisterWriter announces this process's presence on a container.
//
// Unlike AcquireLease, this never refuses: a resident writer is expected to coexist with
// others, and the registry exists so they can see each other, not so the first one wins.
func RegisterWriter(ctx context.Context, store *Store, clientID, mode, root string, ttl time.Duration) (*WriterRegistration, error) {
	if ttl <= 0 {
		ttl = DefaultWriterTTL
	}

	registration := &WriterRegistration{store: store, clientID: clientID, ttl: ttl, mode: mode, root: root}

	if err := registration.write(ctx, 0, ""); err != nil {
		return nil, err
	}

	return registration, nil
}

// Refresh extends this process's registration, recording the generation and manifest it last
// wrote so a peer reading the registry can tell how current it is.
func (reg *WriterRegistration) Refresh(ctx context.Context, lastGeneration int64, lastManifestID string) error {
	previous := reg.current

	if err := reg.write(ctx, lastGeneration, lastManifestID); err != nil {
		return err
	}

	if previous == "" {
		return nil
	}

	_, err := reg.store.Delete(ctx, []string{previous})

	return err
}

// Deregister drops this process's registration.
//
// A caller should warn rather than fail on an error: an entry left behind expires on its own,
// and failing a clean shutdown because a cleanup call did not land would be the wrong trade.
func (reg *WriterRegistration) Deregister(ctx context.Context) error {
	if reg == nil || reg.current == "" {
		return nil
	}

	name := reg.current
	reg.current = ""

	if _, err := reg.store.Delete(ctx, []string{name}); err != nil {
		return fmt.Errorf("removing the writer registration %s: %w", name, err)
	}

	return nil
}

func (reg *WriterRegistration) write(ctx context.Context, lastGeneration int64, lastManifestID string) error {
	reg.sequence++

	now := time.Now().UTC()

	writer := Writer{
		ClientID:       reg.clientID,
		PID:            os.Getpid(),
		Version:        version.Short(),
		Mode:           reg.mode,
		Root:           reg.root,
		StartedAt:      now.Format(time.RFC3339Nano),
		RefreshedAt:    now.Format(time.RFC3339Nano),
		ExpiresAt:      now.Add(reg.ttl).Format(time.RFC3339Nano),
		Sequence:       reg.sequence,
		LastGeneration: lastGeneration,
		LastManifestID: lastManifestID,
	}

	if host, err := os.Hostname(); err == nil {
		writer.Host = host
	}

	if current, err := user.Current(); err == nil {
		writer.User = current.Username
	}

	content, err := json.Marshal(writer)
	if err != nil {
		return fmt.Errorf("encoding writer registration: %w", err)
	}

	name := writerObjectName(reg.store.MetaPrefix(), reg.clientID, reg.sequence)

	object := Object{Name: name, Content: content, Size: int64(len(content))}

	if err := reg.store.Commit(ctx, nil, []Object{object}, CommitOptions{Overwrite: drapi.OverwriteError}); err != nil {
		return fmt.Errorf("registering as a writer: %w", err)
	}

	reg.current = name

	return nil
}
