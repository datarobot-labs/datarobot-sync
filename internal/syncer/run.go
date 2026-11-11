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
	"os"
	"slices"
	"strings"
	"time"

	"github.com/datarobot-labs/datarobot-sync/internal/manifest"
	"github.com/datarobot-labs/datarobot-sync/internal/progress"
)

// maxNamedFailures caps how many unpulled paths an IncompleteDownloadError spells out.
const maxNamedFailures = 5

// IncompleteDownloadError means the pull before a command left paths out, so the command was not
// started.
type IncompleteDownloadError struct {
	// Paths names each path that was not pulled, with the reason when the pull skipped it.
	Paths []string
}

func (e *IncompleteDownloadError) Error() string {
	shown := e.Paths
	if len(shown) > maxNamedFailures {
		shown = shown[:maxNamedFailures]
	}

	return fmt.Sprintf("%d paths were not pulled (%s), so the command was not started: "+
		"pushing the tree afterwards would drop them from the container",
		len(e.Paths), strings.Join(shown, ", "))
}

// IsIncompleteDownload reports whether err is an IncompleteDownloadError.
func IsIncompleteDownload(err error) bool {
	var incomplete *IncompleteDownloadError

	return errors.As(err, &incomplete)
}

// RunOptions configures Run.
type RunOptions struct {
	// Download and Upload carry the root, matcher and tuning for the two transfers. Run forces
	// Download.Backup on and Download.DryRun off, and sets Upload.ExpectHead to the pulled head.
	// It owns the lease, so Upload.Lock, Upload.LeaseTTL, Upload.ClientID and Upload.Force are
	// ignored.
	Download DownloadOptions
	Upload   UploadOptions

	// Lock holds an advisory lease for the whole run, renewed while the command works. LeaseTTL
	// and Force are as for Upload.
	Lock     bool
	LeaseTTL time.Duration
	Force    bool

	// Timeout bounds each transfer, not the command. Zero means no bound.
	Timeout time.Duration

	// Command is the work between the pull and the push. Its error is reported in the result
	// and does not stop the push: a half-finished run's state is what the next run needs.
	Command func(ctx context.Context) error

	// PushContext, when set, supplies the context for the push after Command returns. The
	// default outlives the cancellation of ctx, so an interrupt aimed at the command does not
	// strand its changes locally.
	PushContext func(ctx context.Context) (context.Context, context.CancelFunc)
}

// RunResult is what a run did.
type RunResult struct {
	// Downloaded is nil when the container held no manifest and nothing was pulled.
	Downloaded *DownloadResult
	Uploaded   *UploadResult
	// CommandErr is what Command returned.
	CommandErr error
}

// Run makes a directory a working copy of the container for one command: it takes a lease, pulls
// the head into the directory, runs the command, and pushes the result as a successor of that
// head. The pull keeps every local file it replaces or removes under .dr-sync-backup/. The push
// is refused with a conflict when the head moved in between, and the command's changes stay in
// the directory.
//
// It returns before starting the command when ctx is already done.
func Run(ctx context.Context, store *Store, reporter *progress.Reporter, opts RunOptions) (*RunResult, error) {
	lease, err := acquireRunLease(ctx, store, opts)
	if err != nil {
		return nil, err
	}

	defer releaseLease(ctx, reporter, lease)

	// Deferred after releaseLease, so it runs first: renewal has to stop before the release.
	stopRenewing := lease.KeepAlive(ctx, func(err error) {
		reporter.Warn(fmt.Sprintf("renewing the lease: %v", err))
	})
	defer stopRenewing()

	result := &RunResult{}

	base, err := pullBeforeRun(ctx, store, reporter, opts, result)
	if err != nil {
		return nil, err
	}

	// An interrupt that landed during the pull must not start the command.
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("interrupted before the command started: %w", err)
	}

	result.CommandErr = opts.Command(ctx)

	if err := pushAfterRun(ctx, store, reporter, opts, base, result); err != nil {
		return result, fmt.Errorf("the command's changes were not pushed and remain in %s: %w",
			opts.Upload.Root, err)
	}

	return result, nil
}

// acquireRunLease takes the lease for the whole run, or none when locking is off.
func acquireRunLease(ctx context.Context, store *Store, opts RunOptions) (*LeaseHolder, error) {
	if !opts.Lock {
		return nil, nil
	}

	clientID, err := manifest.NewClientID()
	if err != nil {
		return nil, err
	}

	state, err := store.ReadState(ctx)
	if err != nil {
		return nil, err
	}

	return AcquireLease(ctx, store, state, clientID, opts.LeaseTTL, opts.Force)
}

// pullBeforeRun pulls the head into the directory, records it in result, and returns its manifest
// id, empty for a container with no manifest. The push then expects an empty container.
func pullBeforeRun(ctx context.Context, store *Store, reporter *progress.Reporter, opts RunOptions, result *RunResult) (string, error) {
	pullCtx, cancel := withOptionalTimeout(ctx, opts.Timeout)
	defer cancel()

	pull := opts.Download
	pull.Backup = true
	pull.DryRun = false

	downloaded, err := Download(pullCtx, store, reporter, pull)

	switch {
	case errors.Is(err, ErrNoManifest):
		reporter.Phase("remote", "container is empty, this run makes the first push")

		// A pull that finds a head creates the root. Without one, the command still needs it.
		if err := os.MkdirAll(pull.Root, 0o755); err != nil {
			return "", fmt.Errorf("creating %s: %w", pull.Root, err)
		}

		return "", nil

	case err != nil:
		return "", err

	case downloaded.Partial() || len(downloaded.Plan.Skipped) > 0:
		return "", incompleteDownload(downloaded)
	}

	if downloaded.BackedUp > 0 {
		reporter.Warn(fmt.Sprintf("%d local paths differed from the head and were moved to %s. "+
			"The command runs on the head, not on them. If they are state an earlier run failed to "+
			"push, merge them back before trusting this run.", downloaded.BackedUp, downloaded.BackupDir))
	}

	result.Downloaded = downloaded

	return downloaded.Manifest.ID, nil
}

// incompleteDownload names what the pull left out: files that failed, and entries it skipped.
// With the mirror push that follows, either would be dropped from the container.
func incompleteDownload(downloaded *DownloadResult) *IncompleteDownloadError {
	paths := slices.Clone(downloaded.Failed)

	for _, skipped := range downloaded.Plan.Skipped {
		paths = append(paths, fmt.Sprintf("%s (%s)", skipped.Path, skipped.Reason))
	}

	return &IncompleteDownloadError{Paths: paths}
}

// pushAfterRun uploads the directory as a successor of base. An empty base expects a container
// with no manifest.
func pushAfterRun(ctx context.Context, store *Store, reporter *progress.Reporter, opts RunOptions, base string, result *RunResult) error {
	detach := opts.PushContext
	if detach == nil {
		detach = func(parent context.Context) (context.Context, context.CancelFunc) {
			return context.WithCancel(context.WithoutCancel(parent))
		}
	}

	pushCtx, cancelPush := detach(ctx)
	defer cancelPush()

	pushCtx, cancel := withOptionalTimeout(pushCtx, opts.Timeout)
	defer cancel()

	push := opts.Upload
	push.Lock = false
	push.ClientID = ""
	push.ExpectHead = base
	push.ExpectEmpty = base == ""

	uploaded, err := Upload(pushCtx, store, reporter, push)
	if err != nil {
		return err
	}

	result.Uploaded = uploaded

	return nil
}

// withOptionalTimeout bounds ctx by limit, or only makes it cancelable when limit is not positive.
func withOptionalTimeout(ctx context.Context, limit time.Duration) (context.Context, context.CancelFunc) {
	if limit <= 0 {
		return context.WithCancel(ctx)
	}

	return context.WithTimeout(ctx, limit)
}

// releaseLease releases lease with a context that survives cancellation of ctx. A nil lease is a
// no-op. A release failure is reported as a warning, not returned.
func releaseLease(ctx context.Context, reporter *progress.Reporter, lease *LeaseHolder) {
	if lease == nil {
		return
	}

	// Cancel-proof, or an interrupted run's delete is canceled and the lease lingers until it
	// expires.
	releaseCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), leaseReleaseTimeout)
	defer cancel()

	if err := lease.Release(releaseCtx); err != nil {
		// The lease expires on its own, so a cleanup failure must not fail a completed run.
		reporter.Warn(err.Error())
	}
}
