# tib-recover

`tib-recover` is an experimental, read-only parser for the legacy Acronis True
Image container identified by the eight-byte signature
`b4 6e 68 44 50 14 f8 a9`.

The implementation is deliberately recovery-oriented:

- archive inputs are only ever opened read-only;
- decompression is streamed and bounded;
- every decoded field is checked against its stored checksum;
- volume sequence gaps are rejected;
- unsupported framing stops with an offset instead of guessing.

## Current commands

```console
tib-recover inspect daten1.tib
tib-recover records --limit 100 daten1.tib
tib-recover list --limit 100 daten1.tib
tib-recover verify daten1.tib
tib-recover extract --destination recovered daten1.tib
tib-recover extract --destination recovered-one \
  --match IMGP1772.JPG --max-files 1 daten1.tib
```

`inspect` discovers and validates numbered volumes in the same directory.
`records` decodes the tagged raw-DEFLATE fields in one volume and recognizes
UTF-16 path records. `verify` streams the complete base snapshot, including
fields split across volume boundaries, and checks every stored CRC.

`extract` currently recovers the complete base snapshot. It:

- reconstructs the Windows directory tree below a sanitized drive directory;
- rejects absolute paths and traversal components;
- strips the legacy Acronis file-content envelope;
- checks both the field CRC and embedded original content length;
- writes to a temporary file, syncs it, and atomically renames it;
- refuses to overwrite an existing destination.

## Status

Volume discovery, legacy headers, field framing, cross-volume decompression,
checksums, path reconstruction, base-snapshot verification, and safe payload
extraction are implemented.

Incremental snapshots are recognized as separate slices but are not replayed
yet. Later slices can begin in the middle of an inherited path and may encode
deletions and renames. Until those semantics are decoded, `extract` deliberately
recovers only the internally complete base snapshot rather than constructing a
plausible but potentially incorrect latest state.

Do not use the tool as the only copy of valuable data. Work from a verified disk
image and keep the original archive unchanged.
