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
	"errors"
	"fmt"
	"strings"
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
	// Checked first: a rejected tag should not leave an orphan container behind.
	if err := ValidateTags(opts.Tags); err != nil {
		return nil, err
	}

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

	// From here on the container exists, so any failure has to take it back down again. A
	// half-created container is worse than none: it clutters the catalog and nobody can tell
	// what it was for.
	seeded, err := setUp(ctx, client, store, container.ID, metaPrefix, opts)
	if err != nil {
		if cleanupErr := client.DeleteContainer(ctx, container.ID); cleanupErr != nil {
			// Both failures matter: the first says why the container is unusable, the second
			// says it is still sitting in the catalog and needs removing by hand.
			return nil, errors.Join(err, fmt.Errorf("the half-created container %s could not be removed: %w",
				container.ID, cleanupErr))
		}

		return nil, err
	}

	reporter.Phase("seed", seeded.ID+" written")

	return &InitResult{ContainerID: container.ID, Manifest: seeded}, nil
}

// ErrInvalidTag is a tag the catalog will not accept.
var ErrInvalidTag = errors.New("invalid catalog tag")

// ValidateTags rejects tags the catalog refuses, so the failure names the rule rather than
// arriving as a raw 422 after the container has already been created.
//
// The dash restriction is not documented anywhere; it was found by having the API reject one.
func ValidateTags(tags []string) error {
	for _, tag := range tags {
		trimmed := strings.TrimSpace(tag)

		if trimmed == "" {
			return fmt.Errorf("%w: a tag cannot be empty", ErrInvalidTag)
		}

		if strings.ContainsAny(trimmed, "-") {
			return fmt.Errorf("%w: %q contains a dash, which the DataRobot catalog rejects", ErrInvalidTag, tag)
		}
	}

	return nil
}

// setUp applies the metadata and writes the seed objects. Split out so its failure has one
// obvious place to trigger the rollback.
func setUp(ctx context.Context, client *drapi.Client, store *Store, containerID, metaPrefix string, opts InitOptions) (*manifest.Manifest, error) {
	if err := applyCatalogMetadata(ctx, client, containerID, opts); err != nil {
		return nil, err
	}

	return seedManifest(ctx, store, containerID, metaPrefix)
}

// applyCatalogMetadata sets the container's name, description and tags when asked.
func applyCatalogMetadata(ctx context.Context, client *drapi.Client, containerID string, opts InitOptions) error {
	if opts.Name == "" && opts.Description == "" && len(opts.Tags) == 0 {
		return nil
	}

	if err := ValidateTags(opts.Tags); err != nil {
		return err
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
