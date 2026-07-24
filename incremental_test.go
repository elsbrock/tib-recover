package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"unicode/utf16"
)

func appendCatalogString(output []byte, value string) []byte {
	units := utf16.Encode([]rune(value))
	var length [4]byte
	binary.LittleEndian.PutUint32(length[:], uint32(len(units)))
	output = append(output, length[:]...)
	for _, unit := range units {
		var encoded [2]byte
		binary.LittleEndian.PutUint16(encoded[:], unit)
		output = append(output, encoded[:]...)
	}
	return output
}

func appendCatalogRecord(
	output []byte,
	name string,
	shortName string,
	timestamp uint64,
	recordOffset uint64,
) []byte {
	output = appendCatalogString(output, name)
	output = appendCatalogString(output, shortName)
	metadata := make([]byte, 36)
	binary.LittleEndian.PutUint64(metadata[0:], timestamp)
	binary.LittleEndian.PutUint64(metadata[12:], recordOffset)
	return append(output, metadata...)
}

func TestParseCatalogReconstructsPostorderTree(t *testing.T) {
	data := make([]byte, catalogHeaderSize)
	for index, value := range []uint32{1, 2, 1, 1} {
		binary.LittleEndian.PutUint32(data[index*4:], value)
	}
	data = appendCatalogRecord(data, "notes.txt", "notes.txt", 30, 200)
	data = appendCatalogRecord(data, "Documents", "DOCUME~1", 20, 100)
	data = appendCatalogRecord(data, `C:\`, "C:", 0, 0)

	paths := map[uint64]*rawPathRecord{
		0: {
			path:          pathField{name: `C:\`, kind: 1, depth: 0},
			logicalOffset: 0,
		},
		100: {
			path:          pathField{name: "Documents", kind: 1, depth: 1},
			logicalOffset: 100,
		},
		200: {
			path:          pathField{name: "notes.txt", kind: 0, depth: 2},
			logicalOffset: 200,
		},
	}

	entries, err := parseCatalog(data, paths)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries = %d", len(entries))
	}
	if !slices.Equal(entries[2].components, []string{`C:\`, "Documents", "notes.txt"}) {
		t.Fatalf("file path = %#v", entries[2].components)
	}
	if entries[2].recordOffset != 200 || entries[2].timestamp != 30 {
		t.Fatalf("file metadata = %#v", entries[2])
	}
}

func TestParseCatalogRejectsUnknownRecordOffset(t *testing.T) {
	data := make([]byte, catalogHeaderSize)
	for index, value := range []uint32{1, 2, 1, 1} {
		binary.LittleEndian.PutUint32(data[index*4:], value)
	}
	data = appendCatalogRecord(data, "missing", "missing", 1, 999)

	if _, err := parseCatalog(data, nil); err == nil {
		t.Fatal("expected unknown-offset error")
	}
}

func appendPathWithContent(
	t *testing.T,
	section []byte,
	logicalStart uint64,
	name string,
	kind byte,
	depth uint32,
	content []byte,
) ([]byte, uint64) {
	t.Helper()
	offset := logicalStart + uint64(len(section))
	section = append(section, encodeField(t, 0x66, encodePath(name, kind, depth))...)
	if kind == 0 {
		payload := encodeFilePayload(encodedStream{streamType: 1, data: content})
		section = append(section, encodeField(t, 0x02, payload)...)
	}
	return section, offset
}

type catalogTestRecord struct {
	name         string
	shortName    string
	timestamp    uint64
	recordOffset uint64
}

func encodeCatalog(records ...catalogTestRecord) []byte {
	data := make([]byte, catalogHeaderSize)
	for index, value := range []uint32{1, 2, 1, 1} {
		binary.LittleEndian.PutUint32(data[index*4:], value)
	}
	for _, record := range records {
		data = appendCatalogRecord(
			data,
			record.name,
			record.shortName,
			record.timestamp,
			record.recordOffset,
		)
	}
	return data
}

func writeTestVolume(t *testing.T, path string, sequence uint32, section, trailer []byte) {
	t.Helper()
	header := make([]byte, legacyHeaderSize)
	copy(header, legacyMagic[:])
	binary.LittleEndian.PutUint32(header[8:], sequence)
	data := append(header, section...)
	data = append(data, trailer...)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLatestCatalogExtractionEndToEnd(t *testing.T) {
	directory := t.TempDir()
	volume1 := filepath.Join(directory, "archive1.tib")
	volume2 := filepath.Join(directory, "archive2.tib")

	var base []byte
	var rootOffset, directoryOffset, oldKeepOffset, deletedOffset uint64
	base, rootOffset = appendPathWithContent(t, base, 0, `C:\`, 1, 0, nil)
	base, directoryOffset = appendPathWithContent(t, base, 0, "Documents", 1, 1, nil)
	base, oldKeepOffset = appendPathWithContent(
		t, base, 0, "keep.txt", 0, 2, []byte("old contents"),
	)
	base, deletedOffset = appendPathWithContent(
		t, base, 0, "deleted.txt", 0, 2, []byte("deleted contents"),
	)
	baseCatalog := encodeCatalog(
		catalogTestRecord{"keep.txt", "keep.txt", 1, oldKeepOffset},
		catalogTestRecord{"deleted.txt", "deleted.txt", 1, deletedOffset},
		catalogTestRecord{"Documents", "DOCUME~1", 1, directoryOffset},
		catalogTestRecord{`C:\`, "C:", 0, rootOffset},
	)
	base = append(base, encodeField(t, 0x67, baseCatalog)...)
	base = append(base, encodeField(t, 0x68, nil)...)
	writeTestVolume(t, volume1, 1, base, []byte("opaque trailer"))

	incrementLogicalStart := uint64(len(base) + legacyHeaderSize)
	var increment []byte
	var newKeepOffset, addedOffset uint64
	increment, newKeepOffset = appendPathWithContent(
		t,
		increment,
		incrementLogicalStart,
		"keep.txt",
		0,
		2,
		[]byte("new contents"),
	)
	increment, addedOffset = appendPathWithContent(
		t,
		increment,
		incrementLogicalStart,
		"added.txt",
		0,
		2,
		[]byte("added contents"),
	)
	latestCatalogData := encodeCatalog(
		catalogTestRecord{"renamed.txt", "RENAMED.TXT", 2, newKeepOffset},
		catalogTestRecord{"added.txt", "added.txt", 2, addedOffset},
		catalogTestRecord{"Documents", "DOCUME~1", 1, directoryOffset},
		catalogTestRecord{`C:\`, "C:", 0, rootOffset},
	)
	increment = append(increment, encodeField(t, 0x67, latestCatalogData)...)
	increment = append(increment, encodeField(t, 0x68, nil)...)
	writeTestVolume(t, volume2, 2, increment, nil)

	volumes, err := discoverVolumes(volume1)
	if err != nil {
		t.Fatal(err)
	}
	index, err := scanArchiveIndex(volumes, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(index.snapshots) != 2 {
		t.Fatalf("snapshots = %d", len(index.snapshots))
	}

	destination := t.TempDir()
	extracted, err := extractCatalogSnapshot(
		volumes,
		index,
		latestCatalog(index),
		destination,
		"",
		0,
		1<<20,
	)
	if err != nil {
		t.Fatal(err)
	}
	if extracted != 2 {
		t.Fatalf("extracted = %d", extracted)
	}
	keep, err := os.ReadFile(filepath.Join(destination, "C", "Documents", "renamed.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(keep, []byte("new contents")) {
		t.Fatalf("renamed.txt = %q", keep)
	}
	added, err := os.ReadFile(filepath.Join(destination, "C", "Documents", "added.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(added, []byte("added contents")) {
		t.Fatalf("added.txt = %q", added)
	}
	if _, err := os.Stat(filepath.Join(destination, "C", "Documents", "deleted.txt")); !os.IsNotExist(err) {
		t.Fatalf("deleted.txt still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(destination, "C", "Documents", "keep.txt")); !os.IsNotExist(err) {
		t.Fatalf("pre-rename keep.txt still exists: %v", err)
	}
}
