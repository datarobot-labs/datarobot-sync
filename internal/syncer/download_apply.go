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
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"github.com/datarobot-labs/datarobot-sync/internal/drapi"
	"github.com/datarobot-labs/datarobot-sync/internal/manifest"
	"github.com/datarobot-labs/datarobot-sync/internal/packer"
	"github.com/datarobot-labs/datarobot-sync/internal/progress"
	"github.com/datarobot-labs/datarobot-sync/internal/scan"
)

// fetchPack reads one pack and stages the members it wants.
//
// The pack is streamed once, forwards, because xz cannot seek: unwanted members are discarded
// rather than skipped over, and reading stops as soon as the last wanted member has been
// passed. For a pack whose wanted members sit near the front that is the difference between
// transferring a few megabytes and transferring all of it.
func (r *downloadRun) fetchPack(ctx context.Context, fetch PackFetch, links map[string]drapi.Link) (int64, error) {
	var transferred int64

	staged := map[string]string{}

	cleanup := func() {
		for _, path := range staged {
			_ = os.Remove(path)
		}
	}

	reader := packer.NewSliceReader(len(fetch.Pack.Slices), func(index int) (io.ReadCloser, error) {
		slice := fetch.Pack.Slices[index]

		body, err := r.openSlice(ctx, slice.FileName, links)
		if err != nil {
			return nil, err
		}

		transferred += slice.Size

		return body, nil
	})

	defer reader.Close()

	seen := 0

	err := packer.Read(ctx, reader, fetch.Pack.Codec, func(member packer.Member) error {
		position := seen
		seen++

		if _, want := fetch.Wanted[member.SHA256]; !want {
			if position >= fetch.LastNeeded {
				// Everything wanted has already been read; the rest of the pack is of no
				// interest however much of it remains.
				return packer.ErrStopReading
			}

			return nil
		}

		path, err := r.stageMember(member)
		if err != nil {
			return err
		}

		staged[member.SHA256] = path

		if position >= fetch.LastNeeded {
			return packer.ErrStopReading
		}

		return nil
	})
	if err != nil {
		cleanup()

		return transferred, err
	}

	for digest := range fetch.Wanted {
		if _, ok := staged[digest]; !ok {
			cleanup()

			return transferred, &packer.IntegrityError{
				Digest: digest,
				Reason: "pack " + fetch.Pack.ID + " does not contain it",
			}
		}
	}

	r.reporter.Note(describeFetch(fetch))

	return transferred, nil
}

// openSlice fetches one slice, refreshing its presigned URL if it is close to expiry.
//
// A pack can take minutes, so starting a slice on a nearly-expired URL invites a 403
// mid-stream. Refreshing before the fact is cheaper than recovering after it.
func (r *downloadRun) openSlice(ctx context.Context, name string, links map[string]drapi.Link) (io.ReadCloser, error) {
	link, ok := links[name]

	if !ok || link.Expiring() {
		refreshed, err := r.store.SignedLinks(ctx, []string{name})
		if err != nil {
			return nil, err
		}

		link, ok = refreshed[name]
		if !ok {
			return nil, &StaleManifestError{ObjectName: name}
		}

		links[name] = link
	}

	body, _, err := r.store.client.OpenBlob(ctx, link.URL, 0)
	if err == nil {
		return body, nil
	}

	if !drapi.IsBlobExpired(err) {
		return nil, err
	}

	// The URL lapsed anyway; one refresh and retry, then give up rather than loop.
	refreshed, refreshErr := r.store.SignedLinks(ctx, []string{name})
	if refreshErr != nil {
		return nil, refreshErr
	}

	link, ok = refreshed[name]
	if !ok {
		return nil, &StaleManifestError{ObjectName: name}
	}

	links[name] = link

	body, _, err = r.store.client.OpenBlob(ctx, link.URL, 0)

	return body, err
}

// stageMember writes one member to the scratch directory, verifying its digest.
func (r *downloadRun) stageMember(member packer.Member) (string, error) {
	path := filepath.Join(r.tempDir, member.SHA256+".part")

	handle, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return "", fmt.Errorf("staging %s: %w", member.SHA256[:12], err)
	}

	if err := packer.ExtractMember(handle, member); err != nil {
		handle.Close()

		_ = os.Remove(path)

		return "", err
	}

	if err := handle.Sync(); err != nil {
		handle.Close()

		_ = os.Remove(path)

		return "", fmt.Errorf("flushing %s: %w", path, err)
	}

	if err := handle.Close(); err != nil {
		_ = os.Remove(path)

		return "", fmt.Errorf("closing %s: %w", path, err)
	}

	return path, nil
}

// apply materializes the plan on disk.
//
// Single-threaded and in sorted order, so a parent directory always exists before anything is
// written into it. Every file arrives by rename from the scratch directory, which is under the
// sync root precisely so that rename is same-filesystem and therefore atomic: a reader never
// sees a half-written file.
func (r *downloadRun) apply(plan *DownloadPlan, failed []string) (int, error) {
	failedPaths := make(map[string]bool, len(failed))
	for _, path := range failed {
		failedPaths[path] = true
	}

	if err := r.applyDirs(plan); err != nil {
		return 0, err
	}

	written := 0

	for _, entry := range plan.Empties {
		if err := r.writeEmpty(entry); err != nil {
			return written, err
		}

		written++
	}

	for _, copy := range plan.LocalCopies {
		if err := r.copyLocal(copy); err != nil {
			return written, err
		}

		written++
	}

	staged, err := r.placeStaged(plan, failedPaths)
	if err != nil {
		return written, err
	}

	written += staged

	if err := r.applyLinks(plan); err != nil {
		return written, err
	}

	if err := r.applyFixups(plan); err != nil {
		return written, err
	}

	r.reporter.Phase("apply", fmt.Sprintf("%s files  %s dirs  %s symlinks",
		progress.Count(written), progress.Count(len(plan.Dirs)), progress.Count(len(plan.Links))))

	return written, nil
}

// applyDirs creates directories before anything is written into them.
func (r *downloadRun) applyDirs(plan *DownloadPlan) error {
	ordered := make([]manifest.Entry, len(plan.Dirs))
	copy(ordered, plan.Dirs)
	manifest.SortEntries(ordered)

	for _, entry := range ordered {
		full, err := scan.LocalPath(r.opts.Root, entry.Path)
		if err != nil {
			return err
		}

		// Created writable regardless of the recorded mode, because content still has to go
		// inside; the real mode is set afterwards.
		if err := os.MkdirAll(full, 0o700|r.mode(entry)); err != nil {
			return fmt.Errorf("creating %s: %w", full, err)
		}
	}

	return nil
}

// placeStaged renames the staged members into place.
func (r *downloadRun) placeStaged(plan *DownloadPlan, failedPaths map[string]bool) (int, error) {
	digests := make([]string, 0, len(plan.Needed))
	for digest := range plan.Needed {
		digests = append(digests, digest)
	}

	sort.Strings(digests)

	written := 0

	for _, digest := range digests {
		source := filepath.Join(r.tempDir, digest+".part")

		if _, err := os.Stat(source); err != nil {
			// Its pack failed; the paths were already reported.
			continue
		}

		entries := plan.Needed[digest]

		for i, entry := range entries {
			if failedPaths[entry.Path] {
				continue
			}

			// The last path to want this content takes the staged file; the others get a
			// copy, so one blob can satisfy several paths.
			last := i == len(entries)-1

			if err := r.placeOne(entry, source, last); err != nil {
				return written, err
			}

			written++
		}
	}

	return written, nil
}

// placeOne moves or copies a staged blob to its final path.
func (r *downloadRun) placeOne(entry manifest.Entry, source string, move bool) error {
	full, err := scan.LocalPath(r.opts.Root, entry.Path)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(full), err)
	}

	if move {
		if err := replaceFile(source, full); err != nil {
			return err
		}
	} else if err := copyFile(source, full); err != nil {
		return err
	}

	return r.restoreMetadata(entry, full)
}

// replaceFile renames source over target, working around Windows refusing to rename onto an
// existing file.
func replaceFile(source, target string) error {
	err := os.Rename(source, target)
	if err == nil {
		return nil
	}

	if runtime.GOOS != "windows" {
		return fmt.Errorf("moving %s into place: %w", target, err)
	}

	if removeErr := os.Remove(target); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return fmt.Errorf("replacing %s: %w", target, removeErr)
	}

	if err := os.Rename(source, target); err != nil {
		return fmt.Errorf("moving %s into place: %w", target, err)
	}

	return nil
}

// copyFile duplicates a file's contents through a temporary sibling, so the destination is
// never observed half-written.
func copyFile(source, target string) error {
	handle, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("reading %s: %w", source, err)
	}

	defer handle.Close()

	temp, err := os.CreateTemp(filepath.Dir(target), ".dr-sync-copy-*")
	if err != nil {
		return fmt.Errorf("staging a copy of %s: %w", target, err)
	}

	tempName := temp.Name()

	if _, err := io.Copy(temp, handle); err != nil {
		temp.Close()

		_ = os.Remove(tempName)

		return fmt.Errorf("copying to %s: %w", target, err)
	}

	if err := temp.Close(); err != nil {
		_ = os.Remove(tempName)

		return fmt.Errorf("closing the copy of %s: %w", target, err)
	}

	return replaceFile(tempName, target)
}

// copyLocal produces a file from an identical one already on disk.
func (r *downloadRun) copyLocal(local LocalCopy) error {
	full, err := scan.LocalPath(r.opts.Root, local.Entry.Path)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(full), err)
	}

	if err := copyFile(local.Source, full); err != nil {
		return err
	}

	return r.restoreMetadata(local.Entry, full)
}

// writeEmpty materializes a zero-length file, which needs no pack at all.
func (r *downloadRun) writeEmpty(entry manifest.Entry) error {
	full, err := scan.LocalPath(r.opts.Root, entry.Path)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(full), err)
	}

	handle, err := os.OpenFile(full, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("creating %s: %w", full, err)
	}

	if err := handle.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", full, err)
	}

	return r.restoreMetadata(entry, full)
}

// applyLinks replaces symlinks.
func (r *downloadRun) applyLinks(plan *DownloadPlan) error {
	for _, entry := range plan.Links {
		full, err := scan.LocalPath(r.opts.Root, entry.Path)
		if err != nil {
			return err
		}

		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			return fmt.Errorf("creating %s: %w", filepath.Dir(full), err)
		}

		if err := os.Remove(full); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("replacing %s: %w", full, err)
		}

		if err := os.Symlink(scan.Denormalize(entry.Target), full); err != nil {
			return fmt.Errorf("linking %s: %w", full, err)
		}
	}

	return nil
}

// applyFixups repairs mode and mtime on files whose content was already correct, and on the
// directories, which have to wait until their contents exist.
func (r *downloadRun) applyFixups(plan *DownloadPlan) error {
	for _, entry := range plan.ModeFixups {
		full, err := scan.LocalPath(r.opts.Root, entry.Path)
		if err != nil {
			return err
		}

		if err := r.restoreMetadata(entry, full); err != nil {
			return err
		}
	}

	// Deepest first, so setting a read-only mode on a parent cannot block its children.
	ordered := make([]manifest.Entry, len(plan.Dirs))
	copy(ordered, plan.Dirs)

	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Path > ordered[j].Path })

	for _, entry := range ordered {
		full, err := scan.LocalPath(r.opts.Root, entry.Path)
		if err != nil {
			return err
		}

		if err := r.restoreMetadata(entry, full); err != nil {
			return err
		}
	}

	return nil
}

// restoreMetadata applies the recorded mode and modification time.
//
// Order matters: the mode goes on before the timestamp, because writing content or changing
// permissions updates mtime.
func (r *downloadRun) restoreMetadata(entry manifest.Entry, full string) error {
	if err := os.Chmod(full, r.mode(entry)); err != nil {
		return fmt.Errorf("setting the mode of %s: %w", full, err)
	}

	modTime := entry.ModifiedAt()
	if modTime.IsZero() {
		return nil
	}

	if err := os.Chtimes(full, time.Now(), modTime); err != nil {
		return fmt.Errorf("setting the timestamp of %s: %w", full, err)
	}

	return nil
}

// mode returns the permission bits to apply.
//
// setuid, setgid and sticky are dropped unless explicitly asked for: materializing a setuid
// binary out of a remote blob is a privilege-escalation hole, and the caller who genuinely
// wants it can say so.
func (r *downloadRun) mode(entry manifest.Entry) fs.FileMode {
	mode := fs.FileMode(entry.Mode)

	if mode == 0 {
		if entry.Type == manifest.TypeDir {
			return 0o755
		}

		return 0o644
	}

	if r.opts.PreserveSpecialModes {
		return mode & (fs.ModePerm | fs.ModeSetuid | fs.ModeSetgid | fs.ModeSticky)
	}

	return mode.Perm()
}
