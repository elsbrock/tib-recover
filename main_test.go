package main

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"hash/adler32"
	"io"
	"path/filepath"
	"slices"
	"testing"
	"unicode/utf16"
)

func encodeField(t *testing.T, tag byte, payload []byte) []byte {
	t.Helper()
	var output bytes.Buffer
	output.WriteByte(tag)
	writer, err := flate.NewWriter(&output, flate.DefaultCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	var checksum [4]byte
	binary.LittleEndian.PutUint32(checksum[:], adler32.Checksum(payload))
	output.Write(checksum[:])
	return output.Bytes()
}

func encodePath(name string, kind byte, depth uint32) []byte {
	units := utf16.Encode([]rune(name))
	output := make([]byte, 4+len(units)*2+5)
	binary.LittleEndian.PutUint32(output[:4], uint32(len(units)))
	for index, unit := range units {
		binary.LittleEndian.PutUint16(output[4+index*2:], unit)
	}
	trailer := output[4+len(units)*2:]
	trailer[0] = kind
	binary.LittleEndian.PutUint32(trailer[1:], depth)
	return output
}

func TestReadField(t *testing.T) {
	payload := encodePath("Dokumente", 1, 2)
	reader := newCountingReader(bytes.NewReader(encodeField(t, 0x66, payload)), 0)

	value, err := readField(reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if value.tag != 0x66 {
		t.Fatalf("tag = %#x", value.tag)
	}
	if value.checksum != value.adler32 {
		t.Fatalf("stored checksum %#x != Adler-32 %#x", value.checksum, value.adler32)
	}

	path, err := decodePathField(value.prefix)
	if err != nil {
		t.Fatal(err)
	}
	if path.name != "Dokumente" || path.kind != 1 || path.depth != 2 {
		t.Fatalf("unexpected path: %#v", path)
	}
}

func TestDecodePathRejectsBadLength(t *testing.T) {
	_, err := decodePathField([]byte{10, 0, 0, 0, 0, 0, 0, 0, 0})
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestSafeExtractionPath(t *testing.T) {
	destination := t.TempDir()
	path, err := safeExtractionPath(destination, snapshotEntry{
		components: []string{`C:\`, "Users", "photo.jpg"},
	})
	if err != nil {
		t.Fatal(err)
	}
	expected := filepath.Join(destination, "C", "Users", "photo.jpg")
	if path != expected {
		t.Fatalf("path = %q, expected %q", path, expected)
	}
}

func TestSafeExtractionPathRejectsTraversal(t *testing.T) {
	destination := t.TempDir()
	_, err := safeExtractionPath(destination, snapshotEntry{
		components: []string{`C:\`, "..", "secret"},
	})
	if err == nil {
		t.Fatal("expected traversal error")
	}
}

func TestResolveInheritedPath(t *testing.T) {
	entry := snapshotEntry{
		components: []string{`C:\`, "Users", "Simon", "Documents", "notes.txt"},
		kind:       0,
	}
	known := map[string]snapshotEntry{entryKey(entry.components): entry}

	resolved, err := resolveInheritedPath(
		[]string{`C:\`, "Users", "Simon"},
		pathField{name: "notes.txt", kind: 0, depth: 4},
		known,
	)
	if err != nil {
		t.Fatal(err)
	}
	expected := []string{`C:\`, "Users", "Simon", "Documents"}
	if !slices.Equal(resolved, expected) {
		t.Fatalf("resolved = %#v, expected %#v", resolved, expected)
	}
}

func TestResolveInheritedPathRejectsAmbiguousParent(t *testing.T) {
	known := make(map[string]snapshotEntry)
	for _, name := range []string{"one", "two"} {
		entry := snapshotEntry{
			components: []string{`C:\`, "Users", "Simon", name},
			kind:       1,
		}
		known[entryKey(entry.components)] = entry
	}

	_, err := resolveInheritedPath(
		[]string{`C:\`, "Users", "Simon"},
		pathField{name: "new.txt", kind: 0, depth: 4},
		known,
	)
	if err == nil {
		t.Fatal("expected ambiguous inherited parent error")
	}
}

func TestContentWriterStripsEnvelope(t *testing.T) {
	content := []byte("original file contents")
	payload := encodeFilePayload(
		encodedStream{streamType: 7, data: []byte("metadata")},
		encodedStream{streamType: 1, data: content},
		encodedStream{streamType: 4, data: []byte("alternate stream name")},
	)

	var output bytes.Buffer
	writer := &contentWriter{target: &output}
	if _, err := writer.Write(payload[:100]); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write(payload[100:]); err != nil {
		t.Fatal(err)
	}
	if err := writer.Validate(); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(output.Bytes(), content) {
		t.Fatalf("output = %q", output.Bytes())
	}
}

func TestContentWriterRejectsLengthMismatch(t *testing.T) {
	payload := encodeFilePayload(encodedStream{
		streamType:     1,
		data:           []byte("short"),
		declaredLength: 10,
	})

	writer := &contentWriter{target: io.Discard}
	if _, err := writer.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := writer.Validate(); err == nil {
		t.Fatal("expected length mismatch")
	}
}

type encodedStream struct {
	streamType     uint32
	data           []byte
	declaredLength uint64
}

func encodeFilePayload(streams ...encodedStream) []byte {
	const metadataLength = 12
	output := make([]byte, 20+metadataLength)
	binary.LittleEndian.PutUint32(output[0:4], 3)
	binary.LittleEndian.PutUint32(output[4:8], 2)
	binary.LittleEndian.PutUint64(output[8:16], metadataLength)

	for _, stream := range streams {
		header := make([]byte, streamHeaderSize)
		binary.LittleEndian.PutUint32(header[0:4], stream.streamType)
		length := stream.declaredLength
		if length == 0 {
			length = uint64(len(stream.data))
		}
		binary.LittleEndian.PutUint64(header[8:16], length)
		output = append(output, header...)
		output = append(output, stream.data...)
	}
	return output
}
