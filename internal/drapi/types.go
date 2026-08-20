// Copyright 2026 DataRobot, Inc. and its affiliates.
// All rights reserved.
// DataRobot, Inc.
// This is proprietary source code of DataRobot, Inc. and its affiliates.
// Released under the terms of DataRobot Tool and Utility Agreement.

// Package drapi is a client for the subset of the DataRobot Files API that dr-sync uses.
//
// Wire conventions that are easy to get wrong, all verified against the live API and
// recorded in SPEC.md:
//
//   - the configured endpoint already contains /api/v2, and every path carries a trailing
//     slash;
//   - JSON is camelCase in both directions, and unset fields are omitted rather than null;
//   - `allFiles/` pages at 100 by default and caps `limit` at 1000, so `next` must be
//     followed; a `prefix` must end in "/";
//   - `DELETE files/{id}/allFiles/` takes a JSON request body;
//   - downloads are presigned S3 URLs and must be fetched with no Authorization header,
//     which returns 400 if present;
//   - `fromStage` is synchronous and atomic: 200 with the new version, or 409 with nothing
//     applied.
package drapi

import "time"

// FileEntry is one object in a container, as returned by `allFiles/`.
type FileEntry struct {
	// Name is the full path relative to the container root.
	Name string `json:"fileName"`
	// Type is "folder" for directories, otherwise an extension or MIME-ish token.
	Type string `json:"fileType"`
	Size int64  `json:"fileSize"`
	// Checksum is the SHA-256 hex digest of the raw bytes. Populated for files uploaded
	// through a stage as soon as the commit returns.
	Checksum     string     `json:"fileChecksum"`
	IngestErrors string     `json:"ingestErrors"`
	CreatedAt    *time.Time `json:"fileCreatedAt"`
}

// IsFolder reports whether the entry is a synthetic directory rather than a file.
func (e FileEntry) IsFolder() bool {
	return e.Type == "folder"
}

// listResponse is the standard DataRobot pagination envelope.
type listResponse struct {
	Count      int         `json:"count"`
	TotalCount int         `json:"totalCount"`
	Next       string      `json:"next"`
	Previous   string      `json:"previous"`
	Data       []FileEntry `json:"data"`
}

// Container identifies a Files catalog item and the version a write produced.
type Container struct {
	ID        string `json:"catalogId"`
	VersionID string `json:"catalogVersionId"`
}

// ContainerDetails is the subset of `catalogItems/{id}/` dr-sync reads.
type ContainerDetails struct {
	ID          string     `json:"id"`
	Name        string     `json:"catalogName"`
	Description string     `json:"description"`
	Type        string     `json:"catalogType"`
	Tags        []string   `json:"tags"`
	NumFiles    int        `json:"numFiles"`
	FromArchive bool       `json:"fromArchive"`
	CreatedAt   *time.Time `json:"infoCreationDate"`
	CreatedBy   string     `json:"infoCreatorFullName"`
	ModifiedAt  *time.Time `json:"lastModificationDate"`
}

// ExtendedDetails carries the current version id, which dr-sync uses as a cheap
// "did anything change at all" detector. It is not a compare-and-swap token: the API has no
// conditional write to attach it to.
type ExtendedDetails struct {
	ID              string     `json:"id"`
	VersionID       string     `json:"catalogVersionId"`
	VersionCreated  *time.Time `json:"versionCreationDate"`
	VersionModified *time.Time `json:"versionModificationDate"`
	IsFirstVersion  bool       `json:"isFirstVersion"`
}

// Stage accumulates uploads that are applied together by one FromStage call.
type Stage struct {
	ContainerID string `json:"catalogId"`
	StageID     string `json:"stageId"`
}

// CommitResult is what `fromStage/` returns on success.
type CommitResult struct {
	ContainerID string `json:"catalogId"`
	VersionID   string `json:"catalogVersionId"`
	NumFiles    int    `json:"numFiles"`
}

// Link is a presigned URL for one file.
type Link struct {
	Name string `json:"fileName"`
	URL  string `json:"url"`
	// IssuedAt is filled in by the client, not the server, so callers can refresh a URL
	// before it expires mid-transfer.
	IssuedAt time.Time `json:"-"`
}

type linksResponse struct {
	Links []Link `json:"links"`
}

// DeleteResult reports what a delete actually removed.
type DeleteResult struct {
	ContainerID string `json:"catalogId"`
	VersionID   string `json:"catalogVersionId"`
	NumFiles    int    `json:"numFiles"`
	Results     []struct {
		Path            string `json:"path"`
		NumFilesDeleted int    `json:"numFilesDeleted"`
	} `json:"results"`
}

// OverwriteStrategy controls how `fromStage/` resolves a name that already exists in the
// container.
type OverwriteStrategy string

const (
	// OverwriteError rejects the whole commit if any staged name already exists, applying
	// nothing. This is dr-sync's default: every name it writes is content- or
	// generation-addressed, so a collision means a bug or a concurrent writer.
	OverwriteError OverwriteStrategy = "error"
	// OverwriteReplace prefers the uploaded file. A repair escape hatch, not a default.
	OverwriteReplace OverwriteStrategy = "replace"
	// OverwriteSkip keeps the existing file.
	OverwriteSkip OverwriteStrategy = "skip"
	// OverwriteRename is the API's own default and produces "name (1).ext". dr-sync never
	// wants it — a silently renamed pack is a pack no manifest can find.
	OverwriteRename OverwriteStrategy = "rename"
)

// MaxListLimit is the largest page `allFiles/` accepts; asking for more is a 422.
const MaxListLimit = 1000

// MaxLinkDuration is the longest presigned URL validity the API grants, in seconds.
const MaxLinkDuration = 3000

// MaxLinkBatch is how many names dr-sync requests per `links/` call. The API fails the whole
// batch if any single name is missing, so smaller batches localize the blame.
const MaxLinkBatch = 100
