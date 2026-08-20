// Copyright 2026 DataRobot, Inc. and its affiliates.
// All rights reserved.
// DataRobot, Inc.
// This is proprietary source code of DataRobot, Inc. and its affiliates.
// Released under the terms of DataRobot Tool and Utility Agreement.

package syncer

import (
	"context"
	"fmt"
	"time"

	"github.com/datarobot/golang-datarobot-filesync/internal/drapi"
	"github.com/datarobot/golang-datarobot-filesync/internal/manifest"
	"github.com/datarobot/golang-datarobot-filesync/internal/version"
)

// readme is left in the container once, for whoever finds it in the DataRobot UI and wonders
// why none of their files are in it.
const readme = `This container holds a dr-sync snapshot.

dr-sync is rsync for the DataRobot Files API. It does not store your files at their own
paths: everything lives under %s as immutable objects.

  manifests/<generation>-<random>.json.xz   the file tree, one per push. The current one is
                                            the highest-numbered.
  packs/p_<digest>.tar.xz[.NNN]             file contents, tarred, xz-compressed, and named
                                            after their own digest. Split into .000, .001…
                                            when large.
  locks/<client>-<n>.lock                   advisory claims held while a push is running.

To get your files back:

    dr-sync download <path> --catalog-id %s

Deleting or editing anything under %s by hand will make this container unreadable to
dr-sync. Written by %s.
`

// InitOptions configures container creation.
type InitOptions struct {
	Name        string
	Description string
	Tags        []string
	MetaPrefix  string
}

// InitResult reports the new container.
type InitResult struct {
	ContainerID string
	Manifest    *manifest.Manifest
}

// Init creates a container and seeds it with an empty generation-zero manifest.
//
// Seeding matters: a container that already holds a valid manifest removes the "no manifest
// yet" special case from every reader, and it means an interrupted first push leaves something
// coherent behind rather than an empty catalog item nobody can explain.
func Init(ctx context.Context, client *drapi.Client, reporter interface{ Phase(string, string) }, opts InitOptions) (*InitResult, error) {
	container, err := client.CreateContainer(ctx)
	if err != nil {
		return nil, err
	}

	reporter.Phase("create", "container "+container.ID)

	metaPrefix := opts.MetaPrefix
	if metaPrefix == "" {
		metaPrefix = manifest.DefaultMetaPrefix
	}

	store := NewStore(client, container.ID, metaPrefix)

	if err := applyCatalogMetadata(ctx, client, container.ID, opts); err != nil {
		return nil, err
	}

	seeded, err := seedManifest(ctx, store, container.ID, metaPrefix)
	if err != nil {
		return nil, err
	}

	reporter.Phase("seed", seeded.ID+" written")

	return &InitResult{ContainerID: container.ID, Manifest: seeded}, nil
}

// applyCatalogMetadata sets the container's name, description and tags when asked.
func applyCatalogMetadata(ctx context.Context, client *drapi.Client, containerID string, opts InitOptions) error {
	if opts.Name == "" && opts.Description == "" && len(opts.Tags) == 0 {
		return nil
	}

	update := drapi.ContainerUpdate{Name: opts.Name, Description: opts.Description}

	// Tags are sent only when there are some: an empty list on the wire clears whatever is
	// there, which is not what "no --tag was passed" means.
	if len(opts.Tags) > 0 {
		tags := opts.Tags
		update.Tags = &tags
	}

	return client.UpdateContainer(ctx, containerID, update)
}

// seedManifest writes the readme and the empty generation-zero manifest.
func seedManifest(ctx context.Context, store *Store, containerID, metaPrefix string) (*manifest.Manifest, error) {
	seeded := manifest.New(metaPrefix)
	seeded.Compression = manifest.Compression{
		Codec:   manifest.CodecXZ,
		Level:   manifest.DefaultLevel,
		DictCap: manifest.DictCapForLevel(manifest.DefaultLevel),
	}

	clientID, err := manifest.NewClientID()
	if err != nil {
		return nil, err
	}

	if err := seeded.Stamp(clientID, time.Now()); err != nil {
		return nil, err
	}

	seeded.RecomputeStats()

	encoded, err := manifest.Encode(seeded)
	if err != nil {
		return nil, err
	}

	note := fmt.Sprintf(readme, metaPrefix, containerID, metaPrefix, version.Generator())

	objects := []Object{
		{Name: manifest.ReadmePath(metaPrefix), Content: []byte(note), Size: int64(len(note))},
		{Name: seeded.ObjectName(), Content: encoded, Size: int64(len(encoded))},
	}

	if err := store.Commit(ctx, nil, objects, CommitOptions{Overwrite: drapi.OverwriteError}); err != nil {
		return nil, err
	}

	return seeded, nil
}
