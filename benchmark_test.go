package main

import (
	"fmt"
	"path/filepath"
	"testing"
)

func BenchmarkScanArchiveIndex(b *testing.B) {
	const fileCount = 2_000
	directory := b.TempDir()
	volumePath := filepath.Join(directory, "benchmark1.tib")

	var section []byte
	var rootOffset uint64
	section, rootOffset = appendPathWithContent(b, section, 0, `C:\`, 1, 0, nil)
	records := make([]catalogTestRecord, 0, fileCount+1)
	for index := 0; index < fileCount; index++ {
		name := fmt.Sprintf("file-%05d.txt", index)
		var offset uint64
		section, offset = appendPathWithContent(
			b,
			section,
			0,
			name,
			0,
			1,
			[]byte("benchmark payload"),
		)
		records = append(records, catalogTestRecord{name, name, 1, offset})
	}
	records = append(records, catalogTestRecord{`C:\`, "C:", 1, rootOffset})
	section = append(section, encodeField(b, 0x67, encodeCatalog(records...))...)
	section = append(section, encodeField(b, 0x68, nil)...)
	writeTestVolume(b, volumePath, 1, section, nil)

	volumes, err := discoverVolumes(volumePath)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.SetBytes(volumes[0].size)
	b.ResetTimer()
	for range b.N {
		if _, err := scanArchiveIndex(volumes, 1<<20); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkLoadArchiveIndexCache(b *testing.B) {
	const fileCount = 10_000
	volumePath := filepath.Join(b.TempDir(), "benchmark1.tib")
	writeTestVolume(b, volumePath, 1, make([]byte, fileCount*128), nil)
	volumes, err := discoverVolumes(volumePath)
	if err != nil {
		b.Fatal(err)
	}

	entries := make([]snapshotEntry, fileCount)
	for index := range entries {
		entries[index] = snapshotEntry{
			components:   []string{`C:\`, fmt.Sprintf("file-%05d.txt", index)},
			kind:         0,
			timestamp:    1,
			recordOffset: uint64(index * 128),
		}
	}
	index := &archiveIndex{
		sections: []archiveSection{{
			startVolume: 0,
			endVolume:   0,
			length:      uint64(fileCount * 128),
		}},
		snapshots: []catalogSnapshot{{
			section: 0,
			entries: entries,
		}},
	}
	cachePath := filepath.Join(b.TempDir(), "benchmark.index.json")
	if err := saveArchiveIndexCache(cachePath, volumes, 1<<20, index); err != nil {
		b.Fatal(err)
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := loadArchiveIndexCache(cachePath, volumes, 1<<20); err != nil {
			b.Fatal(err)
		}
	}
}
