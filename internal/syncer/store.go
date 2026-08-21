// Copyright 2026 DataRobot, Inc. and its affiliates.
// All rights reserved.
// DataRobot, Inc.
// This is proprietary source code of DataRobot, Inc. and its affiliates.
// Released under the terms of DataRobot Tool and Utility Agreement.

// Package syncer orchestrates the phases of an upload, a download, and the garbage
// collection that follows them.
package syncer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"sync/atomic"

	"golang.org/x/sync/errgroup"

	"github.com/datarobot/golang-datarobot-filesync/internal/drapi"
	"github.com/datarobot/golang-datarobot-filesync/internal/manifest"
)

// Store is a container viewed through dr-sync's layout: manifests, packs and leases under a
// reserved prefix, and nothing else.
type Store struct {
	client      *drapi.Client
	containerID string
	metaPrefix  string
}

// NewStore wraps a client for one container.
func NewStore(client *drapi.Client, containerID, metaPrefix string) *Store {
	if metaPrefix == "" {
		metaPrefix = manifest.DefaultMetaPrefix
	}

	return &Store{client: client, containerID: containerID, metaPrefix: metaPrefix}
}

// ContainerID returns the container this store addresses.
func (s *Store) ContainerID() string {
	return s.containerID
}

// MetaPrefix returns the reserved prefix.
func (s *Store) MetaPrefix() string {
	return s.metaPrefix
}

// State is one listing of everything under the reserved prefix.
//
// It is read once at the start of a run and reused: it answers "which manifests exist", "is
// this pack already uploaded, with the digest I expect" and "does someone else hold a lease"
// without further requests.
type State struct {
	Manifests []manifest.Ref
	// Objects maps every object name under the prefix to its size and server-computed
	// SHA-256. That checksum is what makes a resumed upload free: an already-present pack is
	// recognized from the listing, with no download and no re-upload.
	Objects map[string]drapi.FileEntry
	Leases  []Lease
	// Writers is who else is resident on this container, from the writers registry. Advisory
	// only, like Leases: there is nothing here that enforces anything.
	Writers []Writer
}

// ReadState lists everything under the reserved prefix.
func (s *Store) ReadState(ctx context.Context) (*State, error) {
	state := &State{Objects: map[string]drapi.FileEntry{}}

	entries, err := s.client.ListFiles(ctx, s.containerID, drapi.ListOptions{Prefix: s.metaPrefix})
	if err != nil {
		return nil, err
	}

	for _, entry := range entries {
		if entry.IsFolder() {
			continue
		}

		state.Objects[entry.Name] = entry

		if ref, ok := manifest.ParseManifestObjectName(s.metaPrefix, entry.Name); ok {
			ref.Size = entry.Size
			ref.Checksum = entry.Checksum
			state.Manifests = append(state.Manifests, ref)

			continue
		}

		if _, _, ok := parseLeaseObjectName(s.metaPrefix, entry.Name); ok {
			lease, err := s.loadLease(ctx, entry.Name)
			if err != nil {
				// An unreadable lease must not block a run, but it must still be visible so
				// garbage collection can clear it away. With no deadline it reads as expired.
				lease = Lease{ObjectName: entry.Name}
			}

			state.Leases = append(state.Leases, lease)

			continue
		}

		if _, _, ok := parseWriterObjectName(s.metaPrefix, entry.Name); ok {
			writer, err := s.loadWriter(ctx, entry.Name)
			if err != nil {
				// Same reasoning as an unreadable lease: it must stay visible so the stale
				// sweep can clear it, and with no deadline it reads as expired.
				writer = Writer{ObjectName: entry.Name}
			}

			state.Writers = append(state.Writers, writer)
		}
	}

	sort.Slice(state.Manifests, func(i, j int) bool {
		return state.Manifests[i].ID < state.Manifests[j].ID
	})

	return state, nil
}

// ErrNoManifest means the container holds no dr-sync state.
var ErrNoManifest = errors.New("container holds no dr-sync manifest")

// Head returns the current manifest, its ref, and the listing it was chosen from.
func (s *Store) Head(ctx context.Context, state *State) (*manifest.Manifest, manifest.Ref, error) {
	ref, ok := manifest.SelectHead(state.Manifests)
	if !ok {
		return nil, manifest.Ref{}, ErrNoManifest
	}

	loaded, err := s.LoadManifest(ctx, ref)
	if err != nil {
		return nil, ref, err
	}

	return loaded, ref, nil
}

// LoadManifest fetches and decodes one manifest.
func (s *Store) LoadManifest(ctx context.Context, ref manifest.Ref) (*manifest.Manifest, error) {
	content, err := s.ReadObject(ctx, ref.ObjectName)
	if err != nil {
		return nil, fmt.Errorf("reading manifest %s: %w", ref.ID, err)
	}

	loaded, err := manifest.DecodeBytes(content)
	if err != nil {
		return nil, fmt.Errorf("reading manifest %s: %w", ref.ID, err)
	}

	return loaded, nil
}

// ReadObject fetches one object's bytes in full. Intended for manifests and leases, which are
// small; pack content is streamed instead.
func (s *Store) ReadObject(ctx context.Context, name string) ([]byte, error) {
	links, err := s.client.SignedLinks(ctx, s.containerID, []string{name}, drapi.LinkLifetime)
	if err != nil {
		return nil, err
	}

	if len(links) == 0 {
		return nil, fmt.Errorf("no link returned for %s", name)
	}

	body, _, err := s.client.OpenBlob(ctx, links[0].URL, 0)
	if err != nil {
		return nil, err
	}

	defer body.Close()

	return io.ReadAll(body)
}

// SignedLinks resolves presigned URLs for the named objects, keyed by name.
//
// The API fails the whole request if any one name is missing, so batches are small and a
// failure is re-issued name by name to say which one is the problem. That failure is also the
// normal way a stale manifest surfaces — a pack it references has been compacted away.
func (s *Store) SignedLinks(ctx context.Context, names []string) (map[string]drapi.Link, error) {
	links := make(map[string]drapi.Link, len(names))

	for start := 0; start < len(names); start += drapi.MaxLinkBatch {
		end := min(start+drapi.MaxLinkBatch, len(names))
		batch := names[start:end]

		batched, err := s.client.SignedLinks(ctx, s.containerID, batch, drapi.LinkLifetime)
		if err == nil {
			for _, link := range batched {
				links[link.Name] = link
			}

			continue
		}

		if !drapi.IsMissingFileName(err) {
			return nil, err
		}

		if named := s.nameMissingObject(ctx, batch); named != nil {
			return nil, named
		}

		return nil, err
	}

	return links, nil
}

// nameMissingObject re-requests one name at a time to identify which one the API rejected.
func (s *Store) nameMissingObject(ctx context.Context, names []string) error {
	for _, name := range names {
		_, err := s.client.SignedLinks(ctx, s.containerID, []string{name}, drapi.LinkLifetime)
		if err == nil {
			continue
		}

		if drapi.IsMissingFileName(err) {
			return &StaleManifestError{ObjectName: name}
		}
	}

	return nil
}

// StaleManifestError means the manifest references an object the container no longer holds,
// which is what a concurrent compaction looks like from a reader's side.
type StaleManifestError struct {
	ObjectName string
}

func (e *StaleManifestError) Error() string {
	return fmt.Sprintf("%s is referenced by the manifest but no longer exists; "+
		"the container was compacted while this run was reading it, so re-run", e.ObjectName)
}

// IsStaleManifest reports whether err is a reference to a vanished object.
func IsStaleManifest(err error) bool {
	var stale *StaleManifestError

	return errors.As(err, &stale)
}

// CatalogVersion returns the container's current version id, a cheap detector for "did
// anything change at all". It is not a compare-and-swap token: the API has no conditional
// write to attach one to.
func (s *Store) CatalogVersion(ctx context.Context) (string, error) {
	details, err := s.client.GetExtendedDetails(ctx, s.containerID)
	if err != nil {
		return "", err
	}

	return details.VersionID, nil
}

// Object is one thing to upload.
type Object struct {
	Name string
	// LocalPath streams from disk. Set this for pack slices — never Content, which would
	// mean holding a slice in memory.
	LocalPath string
	// Content uploads from memory. For manifests and leases only.
	Content []byte
	Size    int64
}

// CommitOptions bounds one commit.
type CommitOptions struct {
	// Parallel is how many uploads run at once.
	Parallel int
	// MaxStageFiles and MaxStageBytes cap one stage; beyond them the upload is split.
	MaxStageFiles int
	MaxStageBytes int64
	Overwrite     drapi.OverwriteStrategy
	// Progress is called after each object lands.
	Progress func(uploaded int, bytes int64)
}

// Commit uploads bulk objects and then final ones, applying them to the container.
//
// The split is the ordering invariant made explicit: a pack must be committed before any
// manifest that references it. When everything fits in one stage the whole run is a single
// atomic commit; when it does not, the packs land first and the manifest goes last and alone,
// so the worst case is orphan packs no reader can see rather than a manifest pointing at
// objects that are not there.
func (s *Store) Commit(ctx context.Context, bulk, final []Object, opts CommitOptions) error {
	batches := batchObjects(bulk, opts)

	if len(final) > 0 {
		if len(batches) == 0 {
			// Everything fits together: one stage, one atomic commit.
			batches = [][]Object{final}
		} else {
			batches = append(batches, final)
		}
	}

	for _, batch := range batches {
		if err := s.commitBatch(ctx, batch, opts); err != nil {
			return err
		}
	}

	return nil
}

// batchObjects splits objects into stages that respect the file and byte caps.
func batchObjects(objects []Object, opts CommitOptions) [][]Object {
	if len(objects) == 0 {
		return nil
	}

	maxFiles := opts.MaxStageFiles
	if maxFiles <= 0 {
		maxFiles = DefaultMaxStageFiles
	}

	maxBytes := opts.MaxStageBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxStageBytes
	}

	var (
		batches [][]Object
		current []Object
		bytes   int64
	)

	for _, object := range objects {
		tooMany := len(current) >= maxFiles
		tooBig := bytes+object.Size > maxBytes && len(current) > 0

		if tooMany || tooBig {
			batches = append(batches, current)
			current = nil
			bytes = 0
		}

		current = append(current, object)
		bytes += object.Size
	}

	if len(current) > 0 {
		batches = append(batches, current)
	}

	return batches
}

// Stage limits. The commit itself is fast regardless of file count — a 200-file stage commits
// in well under a second — so these exist to bound a single request's blast radius, not to
// amortize anything.
const (
	DefaultMaxStageFiles = 500
	DefaultMaxStageBytes = 4 << 30
)

// commitBatch uploads one stage's worth of objects and applies it.
func (s *Store) commitBatch(ctx context.Context, objects []Object, opts CommitOptions) error {
	if len(objects) == 0 {
		return nil
	}

	stage, err := s.client.CreateStage(ctx, s.containerID)
	if err != nil {
		return err
	}

	parallel := opts.Parallel
	if parallel <= 0 {
		parallel = DefaultUploadJobs
	}

	group, groupCtx := errgroup.WithContext(ctx)
	group.SetLimit(parallel)

	var uploaded, bytesDone atomic.Int64

	for _, object := range objects {
		group.Go(func() error {
			if err := s.upload(groupCtx, stage.StageID, object); err != nil {
				return err
			}

			done := uploaded.Add(1)
			total := bytesDone.Add(object.Size)

			if opts.Progress != nil {
				opts.Progress(int(done), total)
			}

			return nil
		})
	}

	if err := group.Wait(); err != nil {
		// The stage is abandoned rather than committed. Nothing becomes visible, so a re-run
		// simply plans again.
		return err
	}

	overwrite := opts.Overwrite
	if overwrite == "" {
		overwrite = drapi.OverwriteError
	}

	_, err = s.client.CommitStage(ctx, s.containerID, stage.StageID, overwrite)

	return err
}

// upload streams one object into a stage.
func (s *Store) upload(ctx context.Context, stageID string, object Object) error {
	if object.LocalPath != "" {
		return s.client.UploadFileToStage(ctx, s.containerID, stageID, object.Name, object.LocalPath)
	}

	return s.client.UploadBytesToStage(ctx, s.containerID, stageID, object.Name, object.Content)
}

// Delete removes objects from the container, in batches.
func (s *Store) Delete(ctx context.Context, names []string) (int, error) {
	const batchSize = 200

	deleted := 0

	for start := 0; start < len(names); start += batchSize {
		end := min(start+batchSize, len(names))

		result, err := s.client.DeletePaths(ctx, s.containerID, names[start:end])
		if err != nil {
			return deleted, err
		}

		for _, removed := range result.Results {
			deleted += removed.NumFilesDeleted
		}
	}

	return deleted, nil
}

// DefaultUploadJobs is how many objects upload concurrently. Per-request latency dominates,
// so concurrency helps well past the core count.
const DefaultUploadJobs = 4
