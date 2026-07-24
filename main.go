package main

import (
	"bufio"
	"compress/flate"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"hash"
	"hash/adler32"
	"hash/crc32"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"
)

var legacyMagic = [8]byte{0xb4, 0x6e, 0x68, 0x44, 0x50, 0x14, 0xf8, 0xa9}

const (
	legacyHeaderSize = 12
	maxFieldPrefix   = 64 * 1024
	fileEnvelopeMax  = 16 * 1024 * 1024
	streamHeaderSize = 20
)

type volume struct {
	path     string
	sequence uint32
	size     int64
}

type volumeChainReader struct {
	volumes []volume
	index   int
	file    *os.File
	buffer  *bufio.Reader
}

func newVolumeChainReader(volumes []volume, start int) (*volumeChainReader, error) {
	if start < 0 || start >= len(volumes) {
		return nil, fmt.Errorf("volume start index %d out of range", start)
	}
	reader := &volumeChainReader{
		volumes: volumes,
		index:   start,
	}
	if err := reader.openCurrent(); err != nil {
		return nil, err
	}
	return reader, nil
}

func (reader *volumeChainReader) openCurrent() error {
	file, err := os.Open(reader.volumes[reader.index].path)
	if err != nil {
		return err
	}
	if _, err := file.Seek(legacyHeaderSize, io.SeekStart); err != nil {
		file.Close()
		return err
	}
	reader.file = file
	reader.buffer = bufio.NewReaderSize(file, 256*1024)
	return nil
}

func (reader *volumeChainReader) Read(buffer []byte) (int, error) {
	for {
		n, err := reader.buffer.Read(buffer)
		if n > 0 {
			return n, nil
		}
		if err != nil && !errors.Is(err, io.EOF) {
			return 0, err
		}
		if closeErr := reader.file.Close(); closeErr != nil {
			return 0, closeErr
		}
		reader.file = nil
		reader.buffer = nil
		reader.index++
		if reader.index >= len(reader.volumes) {
			return 0, io.EOF
		}
		if err := reader.openCurrent(); err != nil {
			return 0, err
		}
	}
}

func (reader *volumeChainReader) ReadByte() (byte, error) {
	for {
		value, err := reader.buffer.ReadByte()
		if err == nil {
			return value, nil
		}
		if !errors.Is(err, io.EOF) {
			return 0, err
		}
		if closeErr := reader.file.Close(); closeErr != nil {
			return 0, closeErr
		}
		reader.file = nil
		reader.buffer = nil
		reader.index++
		if reader.index >= len(reader.volumes) {
			return 0, io.EOF
		}
		if err := reader.openCurrent(); err != nil {
			return 0, err
		}
	}
}

func (reader *volumeChainReader) Close() error {
	if reader.file == nil {
		return nil
	}
	err := reader.file.Close()
	reader.file = nil
	reader.buffer = nil
	return err
}

type countingReader struct {
	reader io.Reader
	offset int64
}

func newCountingReader(reader io.Reader, offset int64) *countingReader {
	return &countingReader{
		reader: reader,
		offset: offset,
	}
}

func (reader *countingReader) Read(buffer []byte) (int, error) {
	n, err := reader.reader.Read(buffer)
	reader.offset += int64(n)
	return n, err
}

func (reader *countingReader) ReadByte() (byte, error) {
	if byteReader, ok := reader.reader.(io.ByteReader); ok {
		value, err := byteReader.ReadByte()
		if err == nil {
			reader.offset++
		}
		return value, err
	}
	var buffer [1]byte
	n, err := reader.reader.Read(buffer[:])
	reader.offset += int64(n)
	if n == 1 {
		return buffer[0], nil
	}
	if err == nil {
		err = io.ErrNoProgress
	}
	return 0, err
}

type prefixWriter struct {
	buffer []byte
	limit  int
}

func (writer *prefixWriter) Write(data []byte) (int, error) {
	remaining := writer.limit - len(writer.buffer)
	if remaining > 0 {
		if len(data) < remaining {
			remaining = len(data)
		}
		writer.buffer = append(writer.buffer, data[:remaining]...)
	}
	return len(data), nil
}

type field struct {
	tag              byte
	offset           int64
	compressedSize   int64
	decompressedSize int64
	checksum         uint32
	adler32          uint32
	crc32            uint32
	prefix           []byte
}

type reusableInflater struct {
	reader     io.ReadCloser
	resetter   flate.Resetter
	copyBuffer []byte
}

func (inflater *reusableInflater) reset(input io.Reader) (io.ReadCloser, error) {
	if inflater.reader == nil {
		inflater.reader = flate.NewReader(input)
		resetter, ok := inflater.reader.(flate.Resetter)
		if !ok {
			return nil, errors.New("DEFLATE reader does not support reset")
		}
		inflater.resetter = resetter
		return inflater.reader, nil
	}
	if err := inflater.resetter.Reset(input, nil); err != nil {
		return nil, err
	}
	return inflater.reader, nil
}

func readField(reader *countingReader, outputLimit int64) (field, error) {
	start := reader.offset
	tag, err := reader.ReadByte()
	if err != nil {
		return field{}, err
	}
	return readFieldBody(reader, start, tag, outputLimit, nil)
}

func readFieldBody(
	reader *countingReader,
	start int64,
	tag byte,
	outputLimit int64,
	output io.Writer,
) (field, error) {
	return readFieldBodyReusable(
		reader,
		start,
		tag,
		outputLimit,
		output,
		nil,
		maxFieldPrefix,
	)
}

func readFieldBodyReusable(
	reader *countingReader,
	start int64,
	tag byte,
	outputLimit int64,
	output io.Writer,
	reusable *reusableInflater,
	prefixLimit int,
) (field, error) {
	compressedStart := reader.offset
	var inflater io.ReadCloser
	var err error
	if reusable == nil {
		inflater = flate.NewReader(reader)
	} else {
		inflater, err = reusable.reset(reader)
		if err != nil {
			return field{}, fmt.Errorf("reset inflater at %#x: %w", start, err)
		}
	}
	prefix := &prefixWriter{limit: prefixLimit}
	adler := adler32.New()
	crc := crc32.NewIEEE()
	writers := []io.Writer{adler, crc}
	if prefixLimit > 0 {
		writers = append(writers, prefix)
	}
	if output != nil {
		writers = append(writers, output)
	}

	destination := io.MultiWriter(writers...)
	source := io.LimitReader(inflater, outputLimit+1)
	var written int64
	var copyErr error
	if reusable == nil {
		written, copyErr = io.Copy(destination, source)
	} else {
		if reusable.copyBuffer == nil {
			reusable.copyBuffer = make([]byte, 32*1024)
		}
		written, copyErr = io.CopyBuffer(destination, source, reusable.copyBuffer)
	}
	closeErr := inflater.Close()
	if copyErr != nil {
		return field{}, fmt.Errorf("inflate field at %#x: %w", start, copyErr)
	}
	if closeErr != nil {
		return field{}, fmt.Errorf("close inflater at %#x: %w", start, closeErr)
	}
	if written > outputLimit {
		return field{}, fmt.Errorf("field at %#x exceeds output limit %d", start, outputLimit)
	}

	var checksumBytes [4]byte
	if _, err := io.ReadFull(reader, checksumBytes[:]); err != nil {
		return field{}, fmt.Errorf("read checksum at %#x: %w", reader.offset, err)
	}

	return field{
		tag:              tag,
		offset:           start,
		compressedSize:   reader.offset - 4 - compressedStart,
		decompressedSize: written,
		checksum:         binary.LittleEndian.Uint32(checksumBytes[:]),
		adler32:          checksumValue(adler),
		crc32:            checksumValue(crc),
		prefix:           prefix.buffer,
	}, nil
}

func (value field) checksumValid() bool {
	return value.checksum == value.adler32 || value.checksum == value.crc32
}

func checksumValue(sum hash.Hash32) uint32 {
	return sum.Sum32()
}

type pathField struct {
	name  string
	kind  byte
	depth uint32
}

type snapshotEntry struct {
	components   []string
	kind         byte
	timestamp    uint64
	recordOffset uint64
}

func (entry snapshotEntry) String() string {
	if len(entry.components) > 1 && strings.HasSuffix(entry.components[0], `\`) {
		return entry.components[0] + strings.Join(entry.components[1:], `\`)
	}
	return strings.Join(entry.components, `\`)
}

var errStopWalk = errors.New("stop snapshot walk")

type payloadTarget struct {
	writer io.WriteCloser
	commit func() error
	abort  func()
}

type contentWriter struct {
	target       io.Writer
	pending      []byte
	streamOffset int
	foundContent bool
	expected     uint64
	written      uint64
}

func (writer *contentWriter) Write(data []byte) (int, error) {
	originalLength := len(data)
	if writer.foundContent {
		if err := writer.writeContent(data); err != nil {
			return 0, err
		}
		return originalLength, nil
	}

	writer.pending = append(writer.pending, data...)
	if len(writer.pending) > fileEnvelopeMax {
		return 0, fmt.Errorf("file envelope exceeds %d bytes", fileEnvelopeMax)
	}
	if len(writer.pending) < 20 {
		return originalLength, nil
	}
	if binary.LittleEndian.Uint32(writer.pending[0:4]) != 3 ||
		binary.LittleEndian.Uint32(writer.pending[4:8]) != 2 {
		return 0, errors.New("unsupported file content envelope")
	}
	if writer.streamOffset == 0 {
		metadataLength := binary.LittleEndian.Uint64(writer.pending[8:16])
		if metadataLength > fileEnvelopeMax-20 {
			return 0, fmt.Errorf("file metadata length %d exceeds limit", metadataLength)
		}
		writer.streamOffset = 20 + int(metadataLength)
	}

	for {
		if len(writer.pending) < writer.streamOffset+streamHeaderSize {
			return originalLength, nil
		}
		header := writer.pending[writer.streamOffset : writer.streamOffset+streamHeaderSize]
		streamType := binary.LittleEndian.Uint32(header[0:4])
		streamLength := binary.LittleEndian.Uint64(header[8:16])
		if streamLength > uint64(fileEnvelopeMax) && streamType != 1 {
			return 0, fmt.Errorf(
				"auxiliary stream type %d length %d exceeds limit",
				streamType,
				streamLength,
			)
		}
		dataOffset := writer.streamOffset + streamHeaderSize
		if streamType == 1 {
			writer.foundContent = true
			writer.expected = streamLength
			data := writer.pending[dataOffset:]
			writer.pending = nil
			if err := writer.writeContent(data); err != nil {
				return 0, err
			}
			return originalLength, nil
		}
		if len(writer.pending) < dataOffset+int(streamLength) {
			return originalLength, nil
		}
		writer.streamOffset = dataOffset + int(streamLength)
	}
}

func (writer *contentWriter) writeContent(data []byte) error {
	remaining := writer.expected - writer.written
	if uint64(len(data)) > remaining {
		data = data[:remaining]
	}
	if len(data) == 0 {
		return nil
	}
	n, err := writer.target.Write(data)
	writer.written += uint64(n)
	if err != nil {
		return err
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	return nil
}

func (writer *contentWriter) Validate() error {
	if !writer.foundContent {
		return errors.New("file payload has no default content stream")
	}
	if writer.written != writer.expected {
		return fmt.Errorf(
			"file content length mismatch: wrote %d, expected %d",
			writer.written,
			writer.expected,
		)
	}
	return nil
}

func walkSnapshot(
	volumes []volume,
	startVolume int,
	outputLimit int64,
	onEntry func(snapshotEntry) error,
	openPayload func(snapshotEntry) (*payloadTarget, error),
	knownEntries map[string]snapshotEntry,
) (int, error) {
	chain, err := newVolumeChainReader(volumes, startVolume)
	if err != nil {
		return startVolume, err
	}
	defer chain.Close()

	reader := newCountingReader(chain, 0)
	inflater := &reusableInflater{}
	var components []string
	var current snapshotEntry

	for fieldIndex := 1; ; fieldIndex++ {
		start := reader.offset
		tag, err := reader.ReadByte()
		if err != nil {
			return chain.index, fmt.Errorf("field %d tag: %w", fieldIndex, err)
		}

		var target *payloadTarget
		if tag == 0x02 && current.kind == 0 && openPayload != nil {
			target, err = openPayload(current)
			if err != nil {
				return chain.index, err
			}
		}

		var output io.Writer
		if target != nil {
			output = target.writer
		}
		prefixLimit := 0
		if tag == 0x66 {
			prefixLimit = maxFieldPrefix
		}
		value, err := readFieldBodyReusable(
			reader,
			start,
			tag,
			outputLimit,
			output,
			inflater,
			prefixLimit,
		)
		if err != nil {
			if target != nil {
				target.abort()
			}
			return chain.index, fmt.Errorf("field %d at %#x: %w", fieldIndex, start, err)
		}
		if !value.checksumValid() {
			if target != nil {
				target.abort()
			}
			return chain.index, fmt.Errorf(
				"field %d at %#x has invalid checksum: stored=%08x crc32=%08x adler32=%08x",
				fieldIndex,
				start,
				value.checksum,
				value.crc32,
				value.adler32,
			)
		}
		if target != nil {
			if err := target.commit(); err != nil {
				target.abort()
				return chain.index, err
			}
		}

		switch tag {
		case 0x66:
			path, err := decodePathField(value.prefix)
			if err != nil {
				return chain.index, fmt.Errorf("decode path field at %#x: %w", start, err)
			}
			depth := int(path.depth)
			components, err = resolveInheritedPath(components, path, knownEntries)
			if err != nil {
				return chain.index, fmt.Errorf("resolve path field at %#x: %w", start, err)
			}
			components = append(components[:depth], path.name)
			current = snapshotEntry{
				components: slices.Clone(components),
				kind:       path.kind,
			}
			if onEntry != nil {
				if err := onEntry(current); err != nil {
					return chain.index, err
				}
			}
		case 0x68:
			return chain.index, nil
		}
	}
}

func entryKey(components []string) string {
	return strings.Join(components, "\x00")
}

func hasComponentPrefix(components, prefix []string) bool {
	if len(components) < len(prefix) {
		return false
	}
	return slices.Equal(components[:len(prefix)], prefix)
}

func resolveInheritedPath(
	current []string,
	path pathField,
	knownEntries map[string]snapshotEntry,
) ([]string, error) {
	depth := int(path.depth)
	if len(knownEntries) == 0 {
		if depth <= len(current) {
			return current, nil
		}
		return nil, fmt.Errorf("path depth jumps from %d to %d", len(current), depth)
	}

	var exact []snapshotEntry
	for _, candidate := range knownEntries {
		if len(candidate.components) == depth+1 &&
			candidate.kind == path.kind &&
			candidate.components[depth] == path.name {
			exact = append(exact, candidate)
		}
	}
	if len(exact) == 1 {
		return slices.Clone(exact[0].components[:depth]), nil
	}
	if depth <= len(current) {
		proposed := append(slices.Clone(current[:depth]), path.name)
		if candidate, ok := knownEntries[entryKey(proposed)]; ok && candidate.kind == path.kind {
			return current, nil
		}
	}
	if len(exact) > 1 {
		if depth <= len(current) {
			return current, nil
		}
		return nil, fmt.Errorf(
			"path depth jumps from %d to %d and %d inherited paths match %q",
			len(current),
			depth,
			len(exact),
			path.name,
		)
	}
	if depth <= len(current) {
		return current, nil
	}

	var parents []snapshotEntry
	for _, candidate := range knownEntries {
		if len(candidate.components) == depth &&
			candidate.kind == 1 &&
			hasComponentPrefix(candidate.components, current) {
			parents = append(parents, candidate)
		}
	}
	if len(parents) == 1 {
		return slices.Clone(parents[0].components), nil
	}
	return nil, fmt.Errorf(
		"path depth jumps from %d to %d; found %d matching inherited parents for %q",
		len(current),
		depth,
		len(parents),
		path.name,
	)
}

func decodePathField(data []byte) (pathField, error) {
	if len(data) < 9 {
		return pathField{}, fmt.Errorf("path field is only %d bytes", len(data))
	}
	characters := binary.LittleEndian.Uint32(data[:4])
	stringBytes := int(characters) * 2
	if stringBytes < 0 || 4+stringBytes+5 != len(data) {
		return pathField{}, fmt.Errorf(
			"invalid UTF-16 length %d for %d-byte path field",
			characters,
			len(data),
		)
	}

	codeUnits := make([]uint16, characters)
	for index := range codeUnits {
		start := 4 + index*2
		codeUnits[index] = binary.LittleEndian.Uint16(data[start : start+2])
	}
	trailer := data[4+stringBytes:]

	return pathField{
		name:  string(utf16.Decode(codeUnits)),
		kind:  trailer[0],
		depth: binary.LittleEndian.Uint32(trailer[1:]),
	}, nil
}

func readVolume(path string) (volume, error) {
	file, err := os.Open(path)
	if err != nil {
		return volume{}, err
	}
	defer file.Close()

	var header [legacyHeaderSize]byte
	if _, err := io.ReadFull(file, header[:]); err != nil {
		return volume{}, fmt.Errorf("%s: read header: %w", path, err)
	}
	if [8]byte(header[:8]) != legacyMagic {
		return volume{}, fmt.Errorf("%s: unsupported TIB signature %s", path, hex.EncodeToString(header[:8]))
	}

	info, err := file.Stat()
	if err != nil {
		return volume{}, err
	}
	return volume{
		path:     path,
		sequence: binary.LittleEndian.Uint32(header[8:12]),
		size:     info.Size(),
	}, nil
}

var numberedVolume = regexp.MustCompile(`^(.*?)([0-9]+)\.tib$`)

func discoverVolumes(path string) ([]volume, error) {
	initial, err := readVolume(path)
	if err != nil {
		return nil, err
	}

	match := numberedVolume.FindStringSubmatch(filepath.Base(path))
	if match == nil {
		return []volume{initial}, nil
	}

	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		return nil, err
	}

	var volumes []volume
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		candidateMatch := numberedVolume.FindStringSubmatch(entry.Name())
		if candidateMatch == nil || candidateMatch[1] != match[1] {
			continue
		}
		candidate, err := readVolume(filepath.Join(filepath.Dir(path), entry.Name()))
		if err != nil {
			return nil, err
		}
		volumes = append(volumes, candidate)
	}

	slices.SortFunc(volumes, func(left, right volume) int {
		return int(left.sequence) - int(right.sequence)
	})
	for index, candidate := range volumes {
		expected := uint32(index + 1)
		if candidate.sequence != expected {
			return nil, fmt.Errorf(
				"volume sequence gap: expected %d, found %d in %s",
				expected,
				candidate.sequence,
				candidate.path,
			)
		}
	}
	return volumes, nil
}

func checksumName(value field) string {
	switch value.checksum {
	case value.adler32:
		return "adler32"
	case value.crc32:
		return "crc32"
	default:
		return "unknown"
	}
}

func inspectCommand(arguments []string) error {
	flags := flag.NewFlagSet("inspect", flag.ContinueOnError)
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: tib-recover inspect VOLUME.tib")
	}

	volumes, err := discoverVolumes(flags.Arg(0))
	if err != nil {
		return err
	}

	var total int64
	fmt.Printf("format: legacy-tib (b46e68445014f8a9)\n")
	fmt.Printf("volumes: %d\n", len(volumes))
	for _, candidate := range volumes {
		total += candidate.size
		fmt.Printf(
			"  %3d  %12d bytes  %s\n",
			candidate.sequence,
			candidate.size,
			candidate.path,
		)
	}
	fmt.Printf("total: %d bytes\n", total)
	return nil
}

func recordsCommand(arguments []string) error {
	flags := flag.NewFlagSet("records", flag.ContinueOnError)
	limit := flags.Int("limit", 100, "maximum fields to decode")
	maxOutput := flags.Int64("max-output", 1<<30, "maximum decompressed bytes per field")
	dumpField := flags.Int("dump-field", 0, "write this decoded field number to --dump-output")
	dumpOutput := flags.String("dump-output", "", "path for --dump-field")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: tib-recover records [OPTIONS] VOLUME.tib")
	}

	file, err := os.Open(flags.Arg(0))
	if err != nil {
		return err
	}
	defer file.Close()

	candidate, err := readVolume(flags.Arg(0))
	if err != nil {
		return err
	}
	if _, err := file.Seek(legacyHeaderSize, io.SeekStart); err != nil {
		return err
	}

	reader := newCountingReader(file, legacyHeaderSize)
	inflater := &reusableInflater{}
	fmt.Printf("volume=%d path=%s\n", candidate.sequence, candidate.path)
	for index := 0; index < *limit; index++ {
		start := reader.offset
		tag, err := reader.ReadByte()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("field %d: %w", index+1, err)
		}

		var dumpFile *os.File
		if *dumpField == index+1 {
			if *dumpOutput == "" {
				return errors.New("--dump-field requires --dump-output")
			}
			dumpFile, err = os.OpenFile(*dumpOutput, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
			if err != nil {
				return err
			}
		}
		var dumpWriter io.Writer
		if dumpFile != nil {
			dumpWriter = dumpFile
		}
		value, readErr := readFieldBodyReusable(
			reader,
			start,
			tag,
			*maxOutput,
			dumpWriter,
			inflater,
			maxFieldPrefix,
		)
		if dumpFile != nil {
			if closeErr := dumpFile.Close(); readErr == nil {
				readErr = closeErr
			}
		}
		if readErr != nil {
			return fmt.Errorf("field %d: %w", index+1, readErr)
		}

		description := ""
		if value.tag == 0x66 {
			path, err := decodePathField(value.prefix)
			if err == nil {
				description = fmt.Sprintf(
					" name=%s kind=%#02x depth=%d",
					strconv.Quote(path.name),
					path.kind,
					path.depth,
				)
			}
		} else if value.tag == 0x01 && len(value.prefix) == 4 {
			description = fmt.Sprintf(
				" value=%d",
				binary.LittleEndian.Uint32(value.prefix),
			)
		} else if value.tag == 0x05 && len(value.prefix) == 16 {
			description = fmt.Sprintf(
				" values=%d,%d",
				binary.LittleEndian.Uint64(value.prefix[:8]),
				binary.LittleEndian.Uint64(value.prefix[8:]),
			)
		}

		fmt.Printf(
			"%6d off=%#012x tag=%#02x compressed=%-9d decoded=%-12d checksum=%s%s\n",
			index+1,
			value.offset,
			value.tag,
			value.compressedSize,
			value.decompressedSize,
			checksumName(value),
			description,
		)
		if checksumName(value) == "unknown" {
			fmt.Printf(
				"       stored=%08x adler32=%08x crc32=%08x prefix=%s\n",
				value.checksum,
				value.adler32,
				value.crc32,
				strings.ToUpper(hex.EncodeToString(value.prefix[:min(len(value.prefix), 16)])),
			)
		}
	}
	return nil
}

func listCommand(arguments []string) error {
	flags := flag.NewFlagSet("list", flag.ContinueOnError)
	limit := flags.Int("limit", 0, "stop after listing this many entries (0 means all)")
	maxOutput := flags.Int64("max-output", 64<<30, "maximum decompressed bytes per field")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: tib-recover list [OPTIONS] VOLUME.tib")
	}

	volumes, err := discoverVolumes(flags.Arg(0))
	if err != nil {
		return err
	}

	count := 0
	_, err = walkSnapshot(
		volumes,
		0,
		*maxOutput,
		func(entry snapshotEntry) error {
			entryType := "unknown"
			switch entry.kind {
			case 0:
				entryType = "file"
			case 1:
				entryType = "dir"
			}
			fmt.Printf("%-4s %s\n", entryType, entry.String())
			count++
			if *limit > 0 && count >= *limit {
				return errStopWalk
			}
			return nil
		},
		nil,
		nil,
	)
	if errors.Is(err, errStopWalk) {
		return nil
	}
	return err
}

func verifyCommand(arguments []string) error {
	flags := flag.NewFlagSet("verify", flag.ContinueOnError)
	maxOutput := flags.Int64("max-output", 64<<30, "maximum decompressed bytes per field")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: tib-recover verify [OPTIONS] VOLUME.tib")
	}

	volumes, err := discoverVolumes(flags.Arg(0))
	if err != nil {
		return err
	}
	var entries int
	var files int
	var directories int
	endVolume, err := walkSnapshot(
		volumes,
		0,
		*maxOutput,
		func(entry snapshotEntry) error {
			entries++
			switch entry.kind {
			case 0:
				files++
			case 1:
				directories++
			}
			return nil
		},
		nil,
		nil,
	)
	if err != nil {
		return err
	}
	fmt.Printf("base snapshot: valid\n")
	fmt.Printf("volumes: 1-%d\n", endVolume+1)
	fmt.Printf("entries: %d (%d files, %d directories)\n", entries, files, directories)
	return nil
}

func snapshotsCommand(arguments []string) error {
	flags := flag.NewFlagSet("snapshots", flag.ContinueOnError)
	maxOutput := flags.Int64("max-output", 64<<30, "maximum decompressed bytes per field")
	indexCache := flags.String("index-cache", "", "validated index cache path")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 1 {
		return errors.New("usage: tib-recover snapshots [OPTIONS] VOLUME.tib")
	}

	volumes, err := discoverVolumes(flags.Arg(0))
	if err != nil {
		return err
	}
	index, cacheHit, err := loadOrScanArchiveIndex(volumes, *maxOutput, *indexCache)
	if err != nil {
		return err
	}
	if *indexCache != "" {
		action := "saved"
		if cacheHit {
			action = "loaded"
		}
		fmt.Fprintf(os.Stderr, "index cache: %s %s\n", action, *indexCache)
	}
	for snapshotNumber, snapshot := range index.snapshots {
		var files, directories int
		for _, entry := range snapshot.entries {
			switch entry.kind {
			case 0:
				files++
			case 1:
				directories++
			}
		}
		section := index.sections[snapshot.section]
		label := "incremental"
		if snapshotNumber == 0 {
			label = "base"
		}
		fmt.Printf(
			"snapshot %d (%s): catalog volume %d, entries %d (%d files, %d directories)\n",
			snapshotNumber,
			label,
			section.endVolume+1,
			len(snapshot.entries),
			files,
			directories,
		)
	}
	if index.incompleteError != nil {
		fmt.Fprintf(
			os.Stderr,
			"incomplete trailing snapshot starting in volume %d: %v\n",
			index.incompleteStartVolume+1,
			index.incompleteError,
		)
	}
	return nil
}

func safeExtractionPath(destination string, entry snapshotEntry) (string, error) {
	if len(entry.components) == 0 {
		return "", errors.New("archive entry has no path components")
	}

	clean := make([]string, 0, len(entry.components))
	for index, component := range entry.components {
		if strings.ContainsRune(component, 0) {
			return "", errors.New("archive entry contains a NUL byte")
		}
		if index == 0 && len(component) == 3 && component[1:] == `:\` {
			component = component[:1]
		}
		if component == "" || component == "." || component == ".." ||
			strings.ContainsAny(component, `/\`) {
			return "", fmt.Errorf("unsafe archive path component %q", component)
		}
		clean = append(clean, component)
	}

	target := filepath.Join(append([]string{destination}, clean...)...)
	relative, err := filepath.Rel(destination, target)
	if err != nil {
		return "", err
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("archive path escapes destination: %s", entry.String())
	}
	return target, nil
}

func newFileTarget(path string) (*payloadTarget, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}

	partial := path + ".tib-recover.partial"
	file, err := os.OpenFile(partial, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	closed := false
	content := &contentWriter{target: file}

	abort := func() {
		if !closed {
			_ = file.Close()
			closed = true
		}
		_ = os.Remove(partial)
	}
	commit := func() error {
		if err := content.Validate(); err != nil {
			return err
		}
		if err := file.Sync(); err != nil {
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
		closed = true
		if _, err := os.Lstat(path); err == nil {
			return fmt.Errorf("refusing to overwrite %s", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return os.Rename(partial, path)
	}

	return &payloadTarget{
		writer: struct {
			io.Writer
			io.Closer
		}{
			Writer: content,
			Closer: file,
		},
		commit: commit,
		abort:  abort,
	}, nil
}

func extractCommand(arguments []string) error {
	flags := flag.NewFlagSet("extract", flag.ContinueOnError)
	destination := flags.String("destination", "", "empty directory for recovered files")
	snapshot := flags.String("snapshot", "base", "snapshot to extract: base or latest")
	match := flags.String("match", "", "only extract paths containing this text")
	maxFiles := flags.Int("max-files", 0, "stop after extracting this many files (0 means all)")
	maxOutput := flags.Int64("max-output", 64<<30, "maximum decompressed bytes per field")
	indexCache := flags.String("index-cache", "", "validated index cache path for latest snapshot")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 1 || *destination == "" {
		return errors.New(
			"usage: tib-recover extract --destination DIR [OPTIONS] VOLUME.tib",
		)
	}

	volumes, err := discoverVolumes(flags.Arg(0))
	if err != nil {
		return err
	}

	destinationPath, err := ensureEmptyDestination(*destination)
	if err != nil {
		return err
	}
	if *snapshot == "latest" {
		index, cacheHit, err := loadOrScanArchiveIndex(volumes, *maxOutput, *indexCache)
		if err != nil {
			return err
		}
		if *indexCache != "" {
			action := "saved"
			if cacheHit {
				action = "loaded"
			}
			fmt.Fprintf(os.Stderr, "index cache: %s %s\n", action, *indexCache)
		}
		if index.incompleteError != nil {
			fmt.Fprintf(
				os.Stderr,
				"warning: ignoring incomplete trailing snapshot starting in volume %d: %v\n",
				index.incompleteStartVolume+1,
				index.incompleteError,
			)
		}
		selected := latestCatalog(index)
		extracted, err := extractCatalogSnapshot(
			volumes,
			index,
			selected,
			destinationPath,
			*match,
			*maxFiles,
			*maxOutput,
		)
		fmt.Printf("extracted files: %d\n", extracted)
		return err
	}
	if *snapshot != "base" {
		return fmt.Errorf("unsupported snapshot %q: expected base or latest", *snapshot)
	}

	extracted := 0
	_, err = walkSnapshot(
		volumes,
		0,
		*maxOutput,
		nil,
		func(entry snapshotEntry) (*payloadTarget, error) {
			if entry.kind != 0 || (*match != "" && !strings.Contains(entry.String(), *match)) {
				return nil, nil
			}
			targetPath, err := safeExtractionPath(destinationPath, entry)
			if err != nil {
				return nil, err
			}
			fmt.Printf("extract %s\n", entry.String())
			target, err := newFileTarget(targetPath)
			if err != nil {
				return nil, err
			}
			originalCommit := target.commit
			target.commit = func() error {
				if err := originalCommit(); err != nil {
					return err
				}
				extracted++
				if *maxFiles > 0 && extracted >= *maxFiles {
					return errStopWalk
				}
				return nil
			}
			return target, nil
		},
		nil,
	)
	if errors.Is(err, errStopWalk) {
		err = nil
	}
	fmt.Printf("extracted files: %d\n", extracted)
	return err
}

func usage() {
	fmt.Fprintln(os.Stderr, `Usage:
  tib-recover inspect VOLUME.tib
  tib-recover records [--limit N] [--max-output BYTES] VOLUME.tib
  tib-recover list [--limit N] VOLUME.tib
  tib-recover verify VOLUME.tib
  tib-recover snapshots [--index-cache PATH] VOLUME.tib
  tib-recover extract --destination DIR [--snapshot base|latest] [--index-cache PATH] [--match TEXT] [--max-files N] VOLUME.tib

Archive volumes are always opened read-only. Legacy TIB support is experimental.`)
}

func run(arguments []string) error {
	if len(arguments) == 0 {
		usage()
		return flag.ErrHelp
	}
	switch arguments[0] {
	case "inspect":
		return inspectCommand(arguments[1:])
	case "records":
		return recordsCommand(arguments[1:])
	case "list":
		return listCommand(arguments[1:])
	case "verify":
		return verifyCommand(arguments[1:])
	case "snapshots":
		return snapshotsCommand(arguments[1:])
	case "extract":
		return extractCommand(arguments[1:])
	case "-h", "--help", "help":
		usage()
		return nil
	default:
		return fmt.Errorf("unknown command %q", arguments[0])
	}
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "tib-recover:", err)
		os.Exit(1)
	}
}
