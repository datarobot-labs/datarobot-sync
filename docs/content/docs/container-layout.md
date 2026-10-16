+++
title = 'Container layout'
weight = 7
+++

datarobot-sync does not store your files at their own paths. Everything lives under a
reserved prefix as immutable objects:

```
.dr-sync/manifests/0000000042-7f3a9c2b.json.xz   the file tree, one per push
.dr-sync/packs/p_9f2a1c8d3e4f5061.tar.xz         contents, named after their own digest
.dr-sync/locks/<client>-<n>.lock                 advisory claims held during a push
.dr-sync/writers/<client>-<n>.json               resident `watch` processes announcing themselves
```

That is a deliberate trade. DataRobot's intake cannot unpack xz, so genuine LZMA2
compression means datarobot-sync owns the layout on both sides. The container is not
browsable as files; in exchange the transfer is compressed, deduplicated, and
resumable. `datarobot sync ls` shows the tree without downloading anything.

Nothing in the container is ever overwritten, and the current manifest is the
highest-numbered one — which is what makes an interrupted push invisible rather than
corrupting.

## Content-addressed packs, immutable manifests

Tar members are named `blobs/<sha256hex>`, not by path. The manifest maps
path → sha256 → pack. This buys: identical content stored once, renames and reverts
that transfer zero bytes, "did this change?" as a map lookup, and no path-traversal
surface inside the tar.

There is no mutable pointer object, because the API has no compare-and-swap and a
mutable head is exactly where lost updates become corruption. The head is **derived**:
list `.dr-sync/manifests/` and take the lexicographic max — manifests are zero-padded
to 10 digits so lexicographic order equals numeric order.

Each pack is named `p_<sha256(blob)[0:16]>`, so a rebuild with the same tar order and
compression settings is byte-identical and produces the same name — crash-resume is a
listing lookup, not a re-upload. A pack is sliced (`.tar.xz.000`, `.tar.xz.001`, …) when
its compressed size exceeds `--slice-size`, which is what removes the file-size
ceiling on a single blob.

For the full manifest schema, field-by-field, see
[SPEC.md](https://github.com/datarobot-labs/datarobot-sync/blob/main/SPEC.md#content-addressed-packs-immutable-manifests).
