package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode/utf16"
)

const (
	catalogHeaderSize = 32
	maxCatalogSize    = 256 * 1024 * 1024
)

type rawPathRecord struct {
	path          pathField
	logicalOffset uint64
}

type archiveSection struct {
	startVolume  int
	endVolume    int
	logicalStart uint64
	length       uint64
	catalog      []byte
}

type catalogSnapshot struct {
	section int
	entries []snapshotEntry
}

type archiveIndex struct {
	sections              []archiveSection
	paths                 map[uint64]*rawPathRecord
	snapshots             []catalogSnapshot
	incompleteStartVolume int
	incompleteError       error
}

func scanArchiveIndex(volumes []volume, outputLimit int64) (*archiveIndex, error) {
	index := &archiveIndex{paths: make(map[uint64]*rawPathRecord)}
	var logicalStart uint64

	for startVolume := 0; startVolume < len(volumes); {
		section, paths, err := scanArchiveSection(
			volumes,
			startVolume,
			logicalStart,
			outputLimit,
		)
		if err != nil {
			if section.endVolume >= len(volumes)-1 &&
				(errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF)) {
				index.incompleteStartVolume = startVolume
				index.incompleteError = err
				break
			}
			return nil, fmt.Errorf("scan section starting in volume %d: %w", startVolume+1, err)
		}
		for _, record := range paths {
			if _, exists := index.paths[record.logicalOffset]; exists {
				return nil, fmt.Errorf(
					"duplicate logical path-record offset %#x",
					record.logicalOffset,
				)
			}
			index.paths[record.logicalOffset] = record
		}
		index.sections = append(index.sections, section)
		// Logical catalog offsets count the 12-byte header at the start of each
		// new record section, but not headers of volumes that merely continue a
		// split field within that section.
		logicalStart += section.length + legacyHeaderSize
		startVolume = section.endVolume + 1
	}

	for sectionIndex, section := range index.sections {
		if len(section.catalog) == 0 {
			continue
		}
		if len(index.snapshots) > 0 {
			previous := index.sections[index.snapshots[len(index.snapshots)-1].section].catalog
			if bytes.Equal(previous, section.catalog) {
				continue
			}
		}
		entries, err := parseCatalog(section.catalog, index.paths)
		if err != nil {
			return nil, fmt.Errorf(
				"parse catalog in volume %d: %w",
				section.endVolume+1,
				err,
			)
		}
		index.snapshots = append(index.snapshots, catalogSnapshot{
			section: sectionIndex,
			entries: entries,
		})
	}
	if len(index.snapshots) == 0 {
		return nil, errors.New("archive contains no complete snapshot catalog")
	}
	return index, nil
}

func scanArchiveSection(
	volumes []volume,
	startVolume int,
	logicalStart uint64,
	outputLimit int64,
) (archiveSection, []*rawPathRecord, error) {
	chain, err := newVolumeChainReader(volumes, startVolume)
	if err != nil {
		return archiveSection{}, nil, err
	}
	defer chain.Close()

	reader := newCountingReader(chain, 0)
	inflater := &reusableInflater{}
	var paths []*rawPathRecord
	var current *rawPathRecord
	var catalog []byte

	for fieldIndex := 1; ; fieldIndex++ {
		start := reader.offset
		tag, err := reader.ReadByte()
		if err != nil {
			return archiveSection{}, nil, fmt.Errorf("field %d tag: %w", fieldIndex, err)
		}

		var capture *prefixWriter
		var output io.Writer
		if tag == 0x67 {
			if catalog != nil {
				return archiveSection{}, nil, errors.New("section contains multiple catalogs")
			}
			capture = &prefixWriter{limit: maxCatalogSize}
			output = capture
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
			context := ""
			if current != nil {
				context = fmt.Sprintf(" for %q", current.path.name)
			}
			return archiveSection{
					startVolume:  startVolume,
					endVolume:    min(chain.index, len(volumes)-1),
					logicalStart: logicalStart,
					length:       uint64(reader.offset),
				}, nil, fmt.Errorf(
					"field %d tag=%#02x at %#x%s: %w",
					fieldIndex,
					tag,
					start,
					context,
					err,
				)
		}
		if !value.checksumValid() {
			return archiveSection{}, nil, fmt.Errorf(
				"field %d at %#x has invalid checksum: stored=%08x crc32=%08x adler32=%08x",
				fieldIndex,
				start,
				value.checksum,
				value.crc32,
				value.adler32,
			)
		}

		switch tag {
		case 0x66:
			path, err := decodePathField(value.prefix)
			if err != nil {
				return archiveSection{}, nil, fmt.Errorf("decode path field at %#x: %w", start, err)
			}
			current = &rawPathRecord{
				path:          path,
				logicalOffset: logicalStart + uint64(start),
			}
			paths = append(paths, current)
		case 0x67:
			if value.decompressedSize > maxCatalogSize {
				return archiveSection{}, nil, fmt.Errorf(
					"catalog is %d bytes; limit is %d",
					value.decompressedSize,
					maxCatalogSize,
				)
			}
			catalog = slices.Clone(capture.buffer)
		case 0x68:
			return archiveSection{
				startVolume:  startVolume,
				endVolume:    chain.index,
				logicalStart: logicalStart,
				length:       uint64(reader.offset),
				catalog:      catalog,
			}, paths, nil
		}
	}
}

type catalogRecord struct {
	offset       int
	afterNames   int
	name         string
	timestamp    uint64
	recordOffset uint64
	path         *rawPathRecord
}

func parseCatalog(data []byte, paths map[uint64]*rawPathRecord) ([]snapshotEntry, error) {
	if len(data) < catalogHeaderSize {
		return nil, fmt.Errorf("catalog is only %d bytes", len(data))
	}
	expectedHeader := [4]uint32{1, 2, 1, 1}
	for index, expected := range expectedHeader {
		if actual := binary.LittleEndian.Uint32(data[index*4:]); actual != expected {
			return nil, fmt.Errorf(
				"unsupported catalog header word %d: got %d, expected %d",
				index,
				actual,
				expected,
			)
		}
	}

	var records []catalogRecord
	seenOffsets := make(map[uint64]int)
	for offset := catalogHeaderSize; offset+36 <= len(data); offset += 2 {
		name, next, ok := decodeCatalogString(data, offset)
		if !ok {
			continue
		}
		_, afterNames, ok := decodeCatalogString(data, next)
		if !ok || afterNames+36 > len(data) {
			continue
		}

		timestamp := binary.LittleEndian.Uint64(data[afterNames:])
		recordOffset := binary.LittleEndian.Uint64(data[afterNames+12:])
		path, ok := paths[recordOffset]
		if !ok {
			continue
		}
		if previous, exists := seenOffsets[recordOffset]; exists {
			return nil, fmt.Errorf(
				"catalog records at %#x and %#x both reference path record %#x",
				previous,
				offset,
				recordOffset,
			)
		}
		seenOffsets[recordOffset] = offset
		records = append(records, catalogRecord{
			offset:       offset,
			afterNames:   afterNames,
			name:         name,
			timestamp:    timestamp,
			recordOffset: recordOffset,
			path:         path,
		})
	}
	if len(records) == 0 {
		return nil, errors.New("catalog contains no validated path records")
	}

	slices.SortFunc(records, func(left, right catalogRecord) int {
		return left.offset - right.offset
	})
	for index, record := range records {
		end := len(data)
		if index+1 < len(records) {
			end = records[index+1].offset
		}
		metadataLength := end - record.afterNames
		if metadataLength < 36 || metadataLength > 4096 {
			return nil, fmt.Errorf(
				"catalog record %q at %#x has implausible %d-byte metadata",
				record.name,
				record.offset,
				metadataLength,
			)
		}
	}

	var current []string
	entries := make([]snapshotEntry, 0, len(records))
	seenPaths := make(map[string]struct{}, len(records))
	for index := len(records) - 1; index >= 0; index-- {
		record := records[index]
		depth := int(record.path.path.depth)
		if depth > len(current) {
			var following []string
			for next := index + 1; next < len(records) && next <= index+4; next++ {
				followingRecord := records[next]
				following = append(
					following,
					fmt.Sprintf(
						"%q(depth=%d, catalog=%#x)",
						followingRecord.name,
						followingRecord.path.path.depth,
						followingRecord.offset,
					),
				)
			}
			return nil, fmt.Errorf(
				"catalog path %q at %#x jumps from reverse depth %d to %d; following records: %s",
				record.name,
				record.offset,
				len(current),
				depth,
				strings.Join(following, ", "),
			)
		}
		current = append(current[:depth], record.name)
		components := slices.Clone(current)
		key := entryKey(components)
		if _, exists := seenPaths[key]; exists {
			return nil, fmt.Errorf("catalog contains duplicate path %q", strings.Join(components, `\`))
		}
		seenPaths[key] = struct{}{}
		entries = append(entries, snapshotEntry{
			components:   components,
			kind:         record.path.path.kind,
			timestamp:    record.timestamp,
			recordOffset: record.recordOffset,
		})
	}
	return entries, nil
}

func decodeCatalogString(data []byte, offset int) (string, int, bool) {
	if offset < 0 || offset > len(data)-4 {
		return "", offset, false
	}
	length := binary.LittleEndian.Uint32(data[offset:])
	if length == 0 || length > 32*1024 {
		return "", offset, false
	}
	byteLength := int(length) * 2
	start := offset + 4
	end := start + byteLength
	if byteLength < 0 || end < start || end > len(data) {
		return "", offset, false
	}
	units := make([]uint16, length)
	for index := range units {
		units[index] = binary.LittleEndian.Uint16(data[start+index*2:])
	}
	value := string(utf16.Decode(units))
	if strings.ContainsRune(value, 0) {
		return "", offset, false
	}
	return value, end, true
}

func latestCatalog(index *archiveIndex) catalogSnapshot {
	return index.snapshots[len(index.snapshots)-1]
}

func extractCatalogSnapshot(
	volumes []volume,
	index *archiveIndex,
	snapshot catalogSnapshot,
	destination string,
	match string,
	maxFiles int,
	outputLimit int64,
) (int, error) {
	entries := slices.Clone(snapshot.entries)
	slices.SortFunc(entries, func(left, right snapshotEntry) int {
		return strings.Compare(strings.ToLower(left.String()), strings.ToLower(right.String()))
	})

	targets := make(map[uint64]snapshotEntry)
	for _, entry := range entries {
		if entry.kind == 1 {
			if match != "" {
				continue
			}
			targetPath, err := safeExtractionPath(destination, entry)
			if err != nil {
				return 0, err
			}
			if err := os.MkdirAll(targetPath, 0o700); err != nil {
				return 0, err
			}
			continue
		}
		if entry.kind != 0 || (match != "" && !strings.Contains(entry.String(), match)) {
			continue
		}
		if maxFiles > 0 && len(targets) >= maxFiles {
			break
		}
		if previous, exists := targets[entry.recordOffset]; exists {
			return 0, fmt.Errorf(
				"catalog paths %q and %q reference the same file record %#x",
				previous.String(),
				entry.String(),
				entry.recordOffset,
			)
		}
		targets[entry.recordOffset] = entry
	}

	extracted := make(map[uint64]struct{}, len(targets))
	for _, section := range index.sections {
		if err := extractSectionTargets(
			volumes,
			section,
			destination,
			targets,
			extracted,
			outputLimit,
		); err != nil {
			return len(extracted), err
		}
		if len(extracted) == len(targets) {
			break
		}
	}
	if len(extracted) != len(targets) {
		var missing []string
		for offset, entry := range targets {
			if _, ok := extracted[offset]; !ok {
				missing = append(missing, fmt.Sprintf("%s (%#x)", entry.String(), offset))
			}
		}
		slices.Sort(missing)
		return len(extracted), fmt.Errorf(
			"extracted %d of %d catalog files; missing: %s",
			len(extracted),
			len(targets),
			strings.Join(missing[:min(len(missing), 10)], ", "),
		)
	}
	if match == "" {
		directories := make([]snapshotEntry, 0)
		for _, entry := range entries {
			if entry.kind == 1 && entry.timestamp != 0 {
				directories = append(directories, entry)
			}
		}
		slices.SortFunc(directories, func(left, right snapshotEntry) int {
			return len(right.components) - len(left.components)
		})
		for _, entry := range directories {
			targetPath, err := safeExtractionPath(destination, entry)
			if err != nil {
				return len(extracted), err
			}
			timestamp := time.UnixMilli(int64(entry.timestamp))
			if err := os.Chtimes(targetPath, timestamp, timestamp); err != nil {
				return len(extracted), err
			}
		}
	}
	return len(extracted), nil
}

func extractSectionTargets(
	volumes []volume,
	section archiveSection,
	destination string,
	targets map[uint64]snapshotEntry,
	extracted map[uint64]struct{},
	outputLimit int64,
) error {
	chain, err := newVolumeChainReader(volumes, section.startVolume)
	if err != nil {
		return err
	}
	defer chain.Close()

	reader := newCountingReader(chain, 0)
	inflater := &reusableInflater{}
	var currentOffset uint64
	var currentTarget *snapshotEntry

	for fieldIndex := 1; ; fieldIndex++ {
		start := reader.offset
		tag, err := reader.ReadByte()
		if err != nil {
			return fmt.Errorf("field %d tag: %w", fieldIndex, err)
		}
		if tag == 0x66 {
			currentOffset = section.logicalStart + uint64(start)
			if target, ok := targets[currentOffset]; ok {
				targetCopy := target
				currentTarget = &targetCopy
			} else {
				currentTarget = nil
			}
		}

		var target *payloadTarget
		if tag == 0x02 && currentTarget != nil {
			if _, done := extracted[currentOffset]; done {
				return fmt.Errorf("file record %#x has multiple payload fields", currentOffset)
			}
			targetPath, err := safeExtractionPath(destination, *currentTarget)
			if err != nil {
				return err
			}
			fmt.Printf("extract %s\n", currentTarget.String())
			target, err = newFileTarget(targetPath)
			if err != nil {
				return err
			}
		}

		var output io.Writer
		if target != nil {
			output = target.writer
		}
		value, err := readFieldBodyReusable(
			reader,
			start,
			tag,
			outputLimit,
			output,
			inflater,
			0,
		)
		if err != nil {
			if target != nil {
				target.abort()
			}
			return fmt.Errorf("field %d at %#x: %w", fieldIndex, start, err)
		}
		if !value.checksumValid() {
			if target != nil {
				target.abort()
			}
			return fmt.Errorf(
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
				return err
			}
			extracted[currentOffset] = struct{}{}
			timestamp := time.UnixMilli(int64(currentTarget.timestamp))
			targetPath, err := safeExtractionPath(destination, *currentTarget)
			if err != nil {
				return err
			}
			if err := os.Chtimes(targetPath, timestamp, timestamp); err != nil {
				return err
			}
		}
		if tag == 0x68 {
			if chain.index != section.endVolume || uint64(reader.offset) != section.length {
				return fmt.Errorf(
					"section boundary changed: got volume %d length %d, expected volume %d length %d",
					chain.index+1,
					reader.offset,
					section.endVolume+1,
					section.length,
				)
			}
			return nil
		}
	}
}

func ensureEmptyDestination(destination string) (string, error) {
	destinationPath, err := filepath.Abs(destination)
	if err != nil {
		return "", err
	}
	if info, err := os.Stat(destinationPath); err == nil {
		if !info.IsDir() {
			return "", fmt.Errorf("destination is not a directory: %s", destinationPath)
		}
		entries, err := os.ReadDir(destinationPath)
		if err != nil {
			return "", err
		}
		if len(entries) != 0 {
			return "", fmt.Errorf("destination must be empty: %s", destinationPath)
		}
	} else if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(destinationPath, 0o700); err != nil {
			return "", err
		}
	} else {
		return "", err
	}
	return destinationPath, nil
}
