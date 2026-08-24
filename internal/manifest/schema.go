// Copyright 2026 DataRobot, Inc. and its affiliates.
// All rights reserved.
// DataRobot, Inc.
// This is proprietary source code of DataRobot, Inc. and its affiliates.
// Released under the terms of DataRobot Tool and Utility Agreement.

// Package manifest defines the sync state dr-sync keeps in the remote container.
//
// The container holds no plain files: everything lives under a reserved prefix as immutable
// objects — generation-named manifests and content-addressed tar.xz packs. There is
// deliberately no mutable pointer object, because the API offers no compare-and-swap and a
// mutable head is exactly where a lost update becomes corruption. The head is derived by
// listing the manifests directory and taking the lexicographic maximum.
//
// See SPEC.md for the full rationale.
package manifest

import (
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"time"
)

// FormatVersion is the schema version dr-sync writes. A reader refuses anything newer,
// because silently ignoring fields it does not understand would drop entries.
const FormatVersion = 1

// DefaultMetaPrefix is the reserved directory holding all dr-sync state. It is configurable
// only so a genuine collision with a user's own directory is a one-flag fix.
const DefaultMetaPrefix = ".dr-sync/"

// Reserved subdirectories under the meta prefix.
const (
	manifestsDir = "manifests"
	packsDir     = "packs"
	locksDir     = "locks"
	writersDir   = "writers"
	readmeName   = "README.txt"
)

// EntryType distinguishes what an entry describes.
type EntryType string

const (
	// TypeFile is a regular file. Its content lives in a pack, keyed by sha256, unless it
	// is empty.
	TypeFile EntryType = "file"
	// TypeDir is a directory. Recording every directory keeps mode restoration complete
	// and makes empty directories free.
	TypeDir EntryType = "dir"
	// TypeSymlink is a symbolic link; its target is in the manifest, not in a pack.
	TypeSymlink EntryType = "symlink"
)

// Codec is how a pack's tar stream is compressed.
type Codec string

const (
	// CodecXZ is xz/LZMA2, the default.
	CodecXZ Codec = "xz"
	// CodecStore is a bare tar, chosen automatically for corpora that do not compress —
	// spending minutes to shrink a directory of JPEGs by one percent is not a good trade.
	CodecStore Codec = "store"
)

// Manifest is the complete state of one synced tree at one generation.
type Manifest struct {
	FormatVersion int `json:"formatVersion"`
	// Generation increases by one per commit. It orders manifests and, with
	// ParentManifestID, makes a concurrent fork detectable.
	Generation int64 `json:"generation"`
	// ID is "<generation zero-padded to 10>-<8 random hex>", which makes lexicographic
	// order match numeric order while still tolerating two writers at the same generation.
	ID               string `json:"manifestId"`
	ParentManifestID string `json:"parentManifestId,omitempty"`
	Generator        string `json:"generator"`
	CreatedAt        string `json:"createdAt"`
	CreatedBy        Author `json:"createdBy"`
	// ObservedCatalogVersionID is what extendedDetails reported when the parent was read.
	// Advisory only: it is a change detector, not a compare-and-swap token.
	ObservedCatalogVersionID string `json:"observedCatalogVersionId,omitempty"`
	MetaPrefix               string `json:"metaPrefix"`
	// PathEncoding is always "nfc". Recorded so a future reader can tell what a producer
	// normalized to rather than having to guess.
	PathEncoding string      `json:"pathEncoding"`
	Compression  Compression `json:"compression"`
	Stats        Stats       `json:"stats"`
	Packs        []Pack      `json:"packs"`
	Entries      []Entry     `json:"entries"`
	// EntriesShards is reserved for very large trees, where entries move to sharded side
	// objects. Always null in this version; declared so adding it later is not a format
	// break.
	EntriesShards []any `json:"entriesShards"`
}

// Author records who wrote a manifest, which is what makes a fork report actionable.
type Author struct {
	ClientID string `json:"clientId"`
	Host     string `json:"host,omitempty"`
	User     string `json:"user,omitempty"`
}

// Compression records the settings a manifest's packs were written with.
type Compression struct {
	Codec   Codec `json:"codec"`
	Level   int   `json:"level"`
	DictCap int   `json:"dictCap"`
}

// Stats is derived data, stored so `inspect` and the post-push advisory need no computation
// and no network.
type Stats struct {
	FileCount           int   `json:"fileCount"`
	DirCount            int   `json:"dirCount"`
	SymlinkCount        int   `json:"symlinkCount"`
	UniqueBlobCount     int   `json:"uniqueBlobCount"`
	TotalSize           int64 `json:"totalSize"`
	PackCount           int   `json:"packCount"`
	PackCompressedBytes int64 `json:"packCompressedBytes"`
	LiveBytes           int64 `json:"liveBytes"`
	DeadBytes           int64 `json:"deadBytes"`
}

// Slice is one uploaded object making up a pack.
//
// A pack is split across slices so no single request body exceeds what the gateway accepts:
// the API has no chunked upload, so this is what removes the file-size ceiling entirely.
type Slice struct {
	FileName string `json:"fileName"`
	Size     int64  `json:"size"`
	SHA256   string `json:"sha256"`
}

// Member is one blob inside a pack, in tar order.
//
// The ordering is load-bearing: it is what lets a download stop reading a pack once it has
// passed the last member it wants, and what makes dead-byte accounting possible.
type Member struct {
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// Pack is a tar stream of blobs, compressed and uploaded as one or more slices.
type Pack struct {
	// ID is "p_<first 16 hex of the compressed blob's sha256>". Deriving it from content
	// makes a rebuild produce the same name, which is what turns crash recovery into a
	// listing lookup instead of a re-upload.
	ID    string `json:"id"`
	Codec Codec  `json:"codec"`
	// SHA256 is over the logical concatenation of the slices.
	SHA256           string   `json:"sha256"`
	CompressedSize   int64    `json:"compressedSize"`
	UncompressedSize int64    `json:"uncompressedSize"`
	Slices           []Slice  `json:"slices"`
	Members          []Member `json:"members"`
	CreatedAt        string   `json:"createdAt,omitempty"`
}

// Entry is one path in the synced tree.
type Entry struct {
	Path string    `json:"path"`
	Type EntryType `json:"type"`
	Size int64     `json:"size,omitempty"`
	// SHA256 is the digest of the file's content. Empty for dirs and symlinks.
	SHA256 string `json:"sha256,omitempty"`
	// Mode holds the POSIX permission bits as a decimal number, since JSON has no octal
	// literal. Always rendered as octal in human output.
	Mode uint32 `json:"mode,omitempty"`
	// ModTime is RFC3339 with nanoseconds, in UTC. Load-bearing for the size+mtime fast
	// path, not for correctness.
	ModTime string `json:"modTime,omitempty"`
	// Target is the link destination, for symlinks only.
	Target string `json:"target,omitempty"`
	// Pack indexes into Manifest.Packs. Nil for dirs, symlinks, and empty files, none of
	// which need a pack member.
	Pack *int `json:"pack,omitempty"`
}

// FileMode returns the entry's permission bits.
func (e Entry) FileMode() fs.FileMode {
	return fs.FileMode(e.Mode).Perm()
}

// IsEmptyFile reports whether the entry is a zero-length file, which is materialized
// directly without touching any pack.
func (e Entry) IsEmptyFile() bool {
	return e.Type == TypeFile && e.Size == 0
}

// ModifiedAt parses ModTime, returning the zero time when it is absent or unparseable.
func (e Entry) ModifiedAt() time.Time {
	if e.ModTime == "" {
		return time.Time{}
	}

	parsed, err := time.Parse(time.RFC3339Nano, e.ModTime)
	if err != nil {
		return time.Time{}
	}

	return parsed
}

// New returns an empty manifest at generation zero, which is what a freshly initialized
// container holds. Starting valid rather than absent removes a special case from every
// reader.
func New(metaPrefix string) *Manifest {
	if metaPrefix == "" {
		metaPrefix = DefaultMetaPrefix
	}

	return &Manifest{
		FormatVersion: FormatVersion,
		Generation:    0,
		MetaPrefix:    metaPrefix,
		PathEncoding:  "nfc",
		Entries:       []Entry{},
		Packs:         []Pack{},
	}
}

// Prefix returns the meta prefix, defaulting when unset.
func (m *Manifest) Prefix() string {
	if m == nil || m.MetaPrefix == "" {
		return DefaultMetaPrefix
	}

	return m.MetaPrefix
}

// ManifestsPrefix is the directory holding manifests, with a trailing slash.
func ManifestsPrefix(metaPrefix string) string {
	return path.Join(strings.TrimSuffix(metaPrefix, "/"), manifestsDir) + "/"
}

// PacksPrefix is the directory holding packs, with a trailing slash.
func PacksPrefix(metaPrefix string) string {
	return path.Join(strings.TrimSuffix(metaPrefix, "/"), packsDir) + "/"
}

// LocksPrefix is the directory holding advisory leases, with a trailing slash.
func LocksPrefix(metaPrefix string) string {
	return path.Join(strings.TrimSuffix(metaPrefix, "/"), locksDir) + "/"
}

// WritersPrefix is the directory holding the resident-writer registry, with a trailing slash.
//
// Each writer owns a sequence-named object here, refreshed the same way a lease is: a new
// object followed by deleting the previous one, never an overwrite. There is no single mutable
// registry file because the API offers no compare-and-swap to make overwriting one safe.
func WritersPrefix(metaPrefix string) string {
	return path.Join(strings.TrimSuffix(metaPrefix, "/"), writersDir) + "/"
}

// ReadmePath is the human-readable note left in the container.
func ReadmePath(metaPrefix string) string {
	return path.Join(strings.TrimSuffix(metaPrefix, "/"), readmeName)
}

// ObjectName returns the container path of this manifest.
func (m *Manifest) ObjectName() string {
	return ObjectName(m.Prefix(), m.ID)
}

// ObjectName returns the container path for a manifest id.
func ObjectName(metaPrefix, id string) string {
	return ManifestsPrefix(metaPrefix) + id + ".json.xz"
}

// SliceName returns the container path for one slice of a pack. A single-slice pack has no
// numeric suffix, so the common case stays readable.
func SliceName(metaPrefix, packID string, index, total int) string {
	if total <= 1 {
		return PacksPrefix(metaPrefix) + packID + ".tar.xz"
	}

	return fmt.Sprintf("%s%s.tar.xz.%03d", PacksPrefix(metaPrefix), packID, index)
}

// ErrUnsupportedVersion is returned for a manifest written by a newer dr-sync.
var ErrUnsupportedVersion = errors.New("manifest format is newer than this dr-sync understands")

// Validate checks a decoded manifest for the invariants the rest of the code assumes:
// a known format version, sorted unique paths, well-formed entries, and pack references
// that actually resolve.
func (m *Manifest) Validate() error {
	if m.FormatVersion > FormatVersion {
		return fmt.Errorf("%w: manifest is version %d, this build understands %d",
			ErrUnsupportedVersion, m.FormatVersion, FormatVersion)
	}

	if m.FormatVersion <= 0 {
		return errors.New("manifest has no formatVersion")
	}

	if err := m.validatePacks(); err != nil {
		return err
	}

	return m.validateEntries()
}

func (m *Manifest) validatePacks() error {
	seen := make(map[string]struct{}, len(m.Packs))

	for i, pack := range m.Packs {
		if pack.ID == "" {
			return fmt.Errorf("pack %d has no id", i)
		}

		if _, duplicate := seen[pack.ID]; duplicate {
			return fmt.Errorf("pack %s appears twice", pack.ID)
		}

		seen[pack.ID] = struct{}{}

		if len(pack.Slices) == 0 {
			return fmt.Errorf("pack %s has no slices", pack.ID)
		}

		if pack.Codec != CodecXZ && pack.Codec != CodecStore {
			return fmt.Errorf("pack %s has unknown codec %q", pack.ID, pack.Codec)
		}
	}

	return nil
}

func (m *Manifest) validateEntries() error {
	var previous string

	for i, entry := range m.Entries {
		if err := validatePath(entry.Path); err != nil {
			return fmt.Errorf("entry %d: %w", i, err)
		}

		if i > 0 && entry.Path <= previous {
			return fmt.Errorf("entries must be sorted and unique: %q follows %q", entry.Path, previous)
		}

		previous = entry.Path

		if err := m.validateEntryShape(entry); err != nil {
			return err
		}
	}

	return nil
}

func (m *Manifest) validateEntryShape(entry Entry) error {
	switch entry.Type {
	case TypeFile:
		return m.validateFileEntry(entry)

	case TypeSymlink:
		if entry.Target == "" {
			return fmt.Errorf("symlink %q has no target", entry.Path)
		}

		return nil

	case TypeDir:
		if entry.Pack != nil {
			return fmt.Errorf("directory %q must not reference a pack", entry.Path)
		}

		return nil

	default:
		return fmt.Errorf("entry %q has unknown type %q", entry.Path, entry.Type)
	}
}

// validateFileEntry checks that a file's content is actually reachable. An empty file is the
// one case with no digest and no pack, because it needs no stored bytes.
func (m *Manifest) validateFileEntry(entry Entry) error {
	if entry.Size == 0 {
		return nil
	}

	if entry.SHA256 == "" {
		return fmt.Errorf("file %q has no sha256", entry.Path)
	}

	if entry.Pack == nil {
		return fmt.Errorf("file %q references no pack", entry.Path)
	}

	if *entry.Pack < 0 || *entry.Pack >= len(m.Packs) {
		return fmt.Errorf("file %q references pack index %d, out of %d packs",
			entry.Path, *entry.Pack, len(m.Packs))
	}

	return nil
}

// validatePath enforces the path rules that keep extraction inside the sync root. Every one
// of these is a way a crafted manifest could otherwise write somewhere it should not.
func validatePath(p string) error {
	if err := validatePathShape(p); err != nil {
		return err
	}

	for _, element := range strings.Split(p, "/") {
		if element == "" {
			return fmt.Errorf("path %q has an empty element", p)
		}

		if element == "." || element == ".." {
			return fmt.Errorf("path %q escapes the sync root", p)
		}
	}

	return nil
}

func validatePathShape(p string) error {
	switch {
	case p == "":
		return errors.New("path is empty")
	case strings.HasPrefix(p, "/"):
		return fmt.Errorf("path %q is absolute", p)
	case p == "." || p == "..":
		return fmt.Errorf("path %q is not normalized", p)
	case strings.HasPrefix(p, "./"):
		return fmt.Errorf("path %q is not normalized", p)
	case strings.HasSuffix(p, "/"):
		return fmt.Errorf("path %q has a trailing slash", p)
	case strings.Contains(p, "\x00"):
		return fmt.Errorf("path %q contains a NUL byte", p)
	case strings.Contains(p, `\`):
		return fmt.Errorf("path %q contains a backslash; paths are POSIX-separated", p)
	}

	return nil
}

// ValidatePath exposes the path rules so the scanner and the extractor apply exactly the
// same ones.
func ValidatePath(p string) error {
	return validatePath(p)
}

// SortEntries orders entries the way the format requires: byte-ascending, so a parent
// directory always precedes its children.
func SortEntries(entries []Entry) {
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Path < entries[j].Path
	})
}
