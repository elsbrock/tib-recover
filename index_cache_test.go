package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func writeSingleSnapshotArchive(t testing.TB) string {
	t.Helper()
	directory := t.TempDir()
	volumePath := filepath.Join(directory, "archive1.tib")

	var section []byte
	var rootOffset, fileOffset uint64
	section, rootOffset = appendPathWithContent(t, section, 0, `C:\`, 1, 0, nil)
	section, fileOffset = appendPathWithContent(
		t,
		section,
		0,
		"notes.txt",
		0,
		1,
		[]byte("cached contents"),
	)
	catalog := encodeCatalog(
		catalogTestRecord{"notes.txt", "NOTES.TXT", 2, fileOffset},
		catalogTestRecord{`C:\`, "C:", 1, rootOffset},
	)
	section = append(section, encodeField(t, 0x67, catalog)...)
	section = append(section, encodeField(t, 0x68, nil)...)
	writeTestVolume(t, volumePath, 1, section, nil)
	return volumePath
}

func TestIndexCacheRoundTrip(t *testing.T) {
	volumePath := writeSingleSnapshotArchive(t)
	volumes, err := discoverVolumes(volumePath)
	if err != nil {
		t.Fatal(err)
	}
	index, err := scanArchiveIndex(volumes, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(t.TempDir(), "archive.index.json")
	if err := saveArchiveIndexCache(cachePath, volumes, 1<<20, index); err != nil {
		t.Fatal(err)
	}

	loaded, err := loadArchiveIndexCache(cachePath, volumes, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.sections) != 1 || len(loaded.snapshots) != 1 {
		t.Fatalf(
			"loaded %d sections and %d snapshots",
			len(loaded.sections),
			len(loaded.snapshots),
		)
	}
	if got := loaded.snapshots[0].entries[1].String(); got != `C:\notes.txt` {
		t.Fatalf("loaded path = %q", got)
	}

	destination := t.TempDir()
	extracted, err := extractCatalogSnapshot(
		volumes,
		loaded,
		latestCatalog(loaded),
		destination,
		"",
		0,
		1<<20,
	)
	if err != nil {
		t.Fatal(err)
	}
	if extracted != 1 {
		t.Fatalf("extracted files = %d", extracted)
	}
	content, err := os.ReadFile(filepath.Join(destination, "C", "notes.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "cached contents" {
		t.Fatalf("extracted content = %q", content)
	}
}

func TestIndexCacheRejectsChangedArchive(t *testing.T) {
	volumePath := writeSingleSnapshotArchive(t)
	volumes, err := discoverVolumes(volumePath)
	if err != nil {
		t.Fatal(err)
	}
	index, err := scanArchiveIndex(volumes, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(t.TempDir(), "archive.index.json")
	if err := saveArchiveIndexCache(cachePath, volumes, 1<<20, index); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(volumePath, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("changed")); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	changedVolumes, err := discoverVolumes(volumePath)
	if err != nil {
		t.Fatal(err)
	}

	_, err = loadArchiveIndexCache(cachePath, changedVolumes, 1<<20)
	if !errors.Is(err, errIndexCacheStale) {
		t.Fatalf("error = %v, expected stale cache", err)
	}
}

func TestIndexCacheRejectsUnsafeCachedPath(t *testing.T) {
	volumePath := writeSingleSnapshotArchive(t)
	volumes, err := discoverVolumes(volumePath)
	if err != nil {
		t.Fatal(err)
	}
	index, err := scanArchiveIndex(volumes, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	fingerprints, err := fingerprintVolumes(volumes)
	if err != nil {
		t.Fatal(err)
	}
	cache := newIndexCache(fingerprints, 1<<20, index)
	cache.Snapshots[0].Entries[1].Components = []string{`C:\`, "..", "escape"}
	data, err := json.Marshal(cache)
	if err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(t.TempDir(), "archive.index.json")
	if err := os.WriteFile(cachePath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := loadArchiveIndexCache(cachePath, volumes, 1<<20); err == nil {
		t.Fatal("expected unsafe cached path to be rejected")
	}
}

func TestLoadOrScanArchiveIndexCreatesAndReusesCache(t *testing.T) {
	volumePath := writeSingleSnapshotArchive(t)
	volumes, err := discoverVolumes(volumePath)
	if err != nil {
		t.Fatal(err)
	}
	cachePath := filepath.Join(t.TempDir(), "archive.index.json")

	if _, hit, err := loadOrScanArchiveIndex(volumes, 1<<20, cachePath); err != nil {
		t.Fatal(err)
	} else if hit {
		t.Fatal("first cache access was reported as a hit")
	}
	if _, hit, err := loadOrScanArchiveIndex(volumes, 1<<20, cachePath); err != nil {
		t.Fatal(err)
	} else if !hit {
		t.Fatal("second cache access was not reported as a hit")
	}
}
