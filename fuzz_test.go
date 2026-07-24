package main

import (
	"bytes"
	"testing"
)

func FuzzReadField(f *testing.F) {
	f.Add(encodeField(f, 0x66, encodePath("notes.txt", 0, 2)))
	f.Add([]byte{0x66})
	f.Fuzz(func(t *testing.T, data []byte) {
		reader := newCountingReader(bytes.NewReader(data), 0)
		value, err := readField(reader, 1<<20)
		if err == nil {
			_ = value.checksumValid()
		}
	})
}

func FuzzDecodePathField(f *testing.F) {
	f.Add(encodePath("Dokumente", 1, 2))
	f.Add([]byte{10, 0, 0, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = decodePathField(data)
	})
}

func FuzzDecodeCatalogString(f *testing.F) {
	f.Add(appendCatalogString(nil, "notes.txt"), 0)
	f.Add([]byte{0xff, 0xff, 0xff, 0xff}, 0)
	f.Fuzz(func(t *testing.T, data []byte, offset int) {
		_, _, _ = decodeCatalogString(data, offset)
	})
}

func FuzzParseCatalog(f *testing.F) {
	paths := map[uint64]*rawPathRecord{
		0: {
			path:          pathField{name: `C:\`, kind: 1, depth: 0},
			logicalOffset: 0,
		},
		128: {
			path:          pathField{name: "notes.txt", kind: 0, depth: 1},
			logicalOffset: 128,
		},
	}
	seed := encodeCatalog(
		catalogTestRecord{"notes.txt", "NOTES.TXT", 1, 128},
		catalogTestRecord{`C:\`, "C:", 1, 0},
	)
	f.Add(seed)
	f.Add([]byte("not a catalog"))
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = parseCatalog(data, paths)
	})
}
