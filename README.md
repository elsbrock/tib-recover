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
tib-recover snapshots daten1.tib
tib-recover snapshots --index-cache daten.index.json daten1.tib
tib-recover extract --destination recovered daten1.tib
tib-recover extract --snapshot latest --index-cache daten.index.json \
  --destination recovered-latest daten1.tib
tib-recover extract --destination recovered-one \
  --match IMGP1772.JPG --max-files 1 daten1.tib
```

`inspect` discovers and validates numbered volumes in the same directory.
`records` decodes the tagged raw-DEFLATE fields in one volume and recognizes
UTF-16 path records. Its `--dump-field` option can stream a complete decoded
field to disk for format analysis. `verify` streams the complete base snapshot,
including fields split across volume boundaries, and checks every stored
Adler-32 or CRC-32 checksum.

`snapshots` discovers every complete catalog, streams split sections, verifies
all field checksums, and reconstructs each full tree from the catalog's
post-order records. Catalog entries reference the exact historical path record
that owns their payload, so unchanged files do not need to be copied into every
increment.

`extract` defaults to the complete base snapshot. Pass `--snapshot latest` to
recover the newest complete catalog, replaying additions, modifications,
deletions, and renames across the incremental chain. It:

- reconstructs the Windows directory tree below a sanitized drive directory;
- rejects absolute paths and traversal components;
- strips the legacy Acronis file-content envelope;
- checks both the field checksum and embedded original content length;
- restores file and directory timestamps from the selected catalog;
- writes to a temporary file, syncs it, and atomically renames it;
- refuses to overwrite an existing destination.

If the archive ends during an unfinished backup, `snapshots` reports the
trailing incomplete section and `--snapshot latest` selects the preceding
complete catalog. It never constructs a tree from a snapshot that has no
closing catalog.

### Reusing a validated index

Scanning a large incremental chain requires inflating and checksumming every
field. Pass `--index-cache PATH` to `snapshots` or a latest-snapshot extraction
to save that result and reuse it on later runs. The initial run still performs
the complete scan. A cache hit verifies the volume names, sequence numbers,
sizes, modification times, and SHA-256 hashes of the first and last 64 KiB of
each volume before accepting the cached section and catalog metadata. Cached
paths and logical offsets are validated again when loaded, and every extracted
payload still has its archive checksum checked.

The cache is a trusted local derivative of the archive, contains archived
filenames and timestamps, and is created with mode `0600`. Keep it outside the
archive directory if that directory should remain read-only. Delete the cache
to force a complete rescan.

## Status

Volume discovery, legacy headers, field framing, cross-volume decompression,
checksums, catalog decoding, incremental tree reconstruction, incomplete-tail
detection, and safe base/latest payload extraction are implemented for the
legacy file-level format exercised by this archive.

## Unsupported and unverified

- Other TIB generations, newer container layouts, and `.tibx` archives are not
  supported. Compatibility is only established for the exact legacy,
  file-level variant exercised by the real recovery archive and synthetic
  tests.
- Disk- or partition-image backups, encrypted or password-protected archives,
  and compression or framing other than the observed raw-DEFLATE fields are not
  supported.
- Only regular files and directories are restored. NTFS alternate data streams,
  ACLs, Windows attributes, sparse layout, hard links, and reparse-point or
  symlink semantics are not recreated.
- Complete snapshots can be listed, but extraction currently selects only the
  base or newest complete snapshot—not an arbitrary intermediate snapshot.
- Extraction requires an empty destination, does not overwrite or resume files,
  and stops on corruption. The only tolerated damage is an incomplete trailing
  snapshot when an earlier complete catalog remains recoverable.

Do not use the tool as the only copy of valuable data. Work from a verified disk
image and keep the original archive unchanged.

## Provenance and related work

The legacy format support in this repository was developed by examining a
real archive, recording observed framing and metadata relationships, and
testing recovery against that archive. No source code is copied or vendored
from another TIB implementation.

[dennisss/acronis-tib](https://github.com/dennisss/acronis-tib) is useful
related prior art for other generations of the proprietary TIB family. It is a
separate TypeScript project under the MIT license and is not a dependency of
`tib-recover`.

Acronis and True Image are trademarks of their respective owner. This project
is independent and is not affiliated with or endorsed by Acronis.

## License

`tib-recover` is available under the [MIT License](LICENSE).
