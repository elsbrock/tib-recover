package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const (
	indexCacheVersion  = 1
	indexCacheMaxSize  = 256 * 1024 * 1024
	fingerprintEdgeLen = 64 * 1024
)

var errIndexCacheStale = errors.New("index cache is stale")

type cachedVolume struct {
	Name            string `json:"name"`
	Sequence        uint32 `json:"sequence"`
	Size            int64  `json:"size"`
	ModTimeUnixNano int64  `json:"mtime_unix_nano"`
	EdgeSHA256      string `json:"edge_sha256"`
}

type cachedSection struct {
	StartVolume  int    `json:"start_volume"`
	EndVolume    int    `json:"end_volume"`
	LogicalStart uint64 `json:"logical_start"`
	Length       uint64 `json:"length"`
}

type cachedEntry struct {
	Components   []string `json:"components"`
	Kind         byte     `json:"kind"`
	Timestamp    uint64   `json:"timestamp"`
	RecordOffset uint64   `json:"record_offset"`
}

type cachedSnapshot struct {
	Section int           `json:"section"`
	Entries []cachedEntry `json:"entries"`
}

type indexCache struct {
	Version               int              `json:"version"`
	OutputLimit           int64            `json:"output_limit"`
	Volumes               []cachedVolume   `json:"volumes"`
	Sections              []cachedSection  `json:"sections"`
	Snapshots             []cachedSnapshot `json:"snapshots"`
	IncompleteStartVolume int              `json:"incomplete_start_volume,omitempty"`
	IncompleteError       string           `json:"incomplete_error,omitempty"`
}

func loadOrScanArchiveIndex(
	volumes []volume,
	outputLimit int64,
	cachePath string,
) (*archiveIndex, bool, error) {
	if cachePath == "" {
		index, err := scanArchiveIndex(volumes, outputLimit)
		return index, false, err
	}

	index, err := loadArchiveIndexCache(cachePath, volumes, outputLimit)
	if err == nil {
		return index, true, nil
	}
	if !errors.Is(err, os.ErrNotExist) && !errors.Is(err, errIndexCacheStale) {
		return nil, false, err
	}

	index, err = scanArchiveIndex(volumes, outputLimit)
	if err != nil {
		return nil, false, err
	}
	if err := saveArchiveIndexCache(cachePath, volumes, outputLimit, index); err != nil {
		return nil, false, err
	}
	return index, false, nil
}

func loadArchiveIndexCache(
	cachePath string,
	volumes []volume,
	outputLimit int64,
) (*archiveIndex, error) {
	file, err := os.Open(cachePath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > indexCacheMaxSize {
		return nil, fmt.Errorf("index cache %s is larger than %d bytes", cachePath, indexCacheMaxSize)
	}

	decoder := json.NewDecoder(io.LimitReader(file, indexCacheMaxSize+1))
	decoder.DisallowUnknownFields()
	var cache indexCache
	if err := decoder.Decode(&cache); err != nil {
		return nil, fmt.Errorf("decode index cache %s: %w", cachePath, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("unexpected trailing JSON value")
		}
		return nil, fmt.Errorf("decode index cache %s: %w", cachePath, err)
	}

	if cache.Version != indexCacheVersion {
		return nil, fmt.Errorf(
			"%w: cache format version %d, expected %d",
			errIndexCacheStale,
			cache.Version,
			indexCacheVersion,
		)
	}
	if cache.OutputLimit != outputLimit {
		return nil, fmt.Errorf(
			"%w: output limit %d, expected %d",
			errIndexCacheStale,
			cache.OutputLimit,
			outputLimit,
		)
	}
	currentVolumes, err := fingerprintVolumes(volumes)
	if err != nil {
		return nil, err
	}
	if !equalVolumeFingerprints(cache.Volumes, currentVolumes) {
		return nil, fmt.Errorf("%w: archive volumes changed", errIndexCacheStale)
	}

	index, err := cache.archiveIndex(volumes)
	if err != nil {
		return nil, fmt.Errorf("validate index cache %s: %w", cachePath, err)
	}
	return index, nil
}

func saveArchiveIndexCache(
	cachePath string,
	volumes []volume,
	outputLimit int64,
	index *archiveIndex,
) error {
	fingerprints, err := fingerprintVolumes(volumes)
	if err != nil {
		return err
	}
	cache := newIndexCache(fingerprints, outputLimit, index)

	directory := filepath.Dir(cachePath)
	file, err := os.CreateTemp(directory, "."+filepath.Base(cachePath)+".partial-*")
	if err != nil {
		return err
	}
	tempPath := file.Name()
	committed := false
	defer func() {
		if !committed {
			_ = file.Close()
			_ = os.Remove(tempPath)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	encoder := json.NewEncoder(file)
	if err := encoder.Encode(cache); err != nil {
		return fmt.Errorf("encode index cache: %w", err)
	}
	if err := file.Sync(); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, cachePath); err != nil {
		return err
	}
	committed = true
	return nil
}

func newIndexCache(
	volumes []cachedVolume,
	outputLimit int64,
	index *archiveIndex,
) indexCache {
	cache := indexCache{
		Version:               indexCacheVersion,
		OutputLimit:           outputLimit,
		Volumes:               volumes,
		Sections:              make([]cachedSection, len(index.sections)),
		Snapshots:             make([]cachedSnapshot, len(index.snapshots)),
		IncompleteStartVolume: index.incompleteStartVolume,
	}
	if index.incompleteError != nil {
		cache.IncompleteError = index.incompleteError.Error()
	}
	for sectionIndex, section := range index.sections {
		cache.Sections[sectionIndex] = cachedSection{
			StartVolume:  section.startVolume,
			EndVolume:    section.endVolume,
			LogicalStart: section.logicalStart,
			Length:       section.length,
		}
	}
	for snapshotIndex, snapshot := range index.snapshots {
		cached := cachedSnapshot{
			Section: snapshot.section,
			Entries: make([]cachedEntry, len(snapshot.entries)),
		}
		for entryIndex, entry := range snapshot.entries {
			cached.Entries[entryIndex] = cachedEntry{
				Components:   entry.components,
				Kind:         entry.kind,
				Timestamp:    entry.timestamp,
				RecordOffset: entry.recordOffset,
			}
		}
		cache.Snapshots[snapshotIndex] = cached
	}
	return cache
}

func (cache indexCache) archiveIndex(volumes []volume) (*archiveIndex, error) {
	if len(cache.Sections) == 0 {
		return nil, errors.New("cache contains no complete sections")
	}
	if len(cache.Snapshots) == 0 {
		return nil, errors.New("cache contains no snapshots")
	}

	index := &archiveIndex{
		sections:              make([]archiveSection, len(cache.Sections)),
		snapshots:             make([]catalogSnapshot, len(cache.Snapshots)),
		incompleteStartVolume: cache.IncompleteStartVolume,
	}
	if cache.IncompleteError != "" {
		index.incompleteError = errors.New(cache.IncompleteError)
		if cache.IncompleteStartVolume < 0 || cache.IncompleteStartVolume >= len(volumes) {
			return nil, errors.New("incomplete snapshot volume is out of range")
		}
	}

	for sectionIndex, section := range cache.Sections {
		if section.StartVolume < 0 ||
			section.EndVolume < section.StartVolume ||
			section.EndVolume >= len(volumes) ||
			section.Length == 0 ||
			section.LogicalStart > ^uint64(0)-section.Length {
			return nil, fmt.Errorf("section %d has invalid bounds", sectionIndex)
		}
		if sectionIndex == 0 {
			if section.StartVolume != 0 || section.LogicalStart != 0 {
				return nil, errors.New("first section does not start at the archive beginning")
			}
		} else {
			previous := cache.Sections[sectionIndex-1]
			if previous.LogicalStart+previous.Length > ^uint64(0)-legacyHeaderSize {
				return nil, fmt.Errorf("section %d logical offset overflows", sectionIndex)
			}
			if section.StartVolume != previous.EndVolume+1 ||
				section.LogicalStart != previous.LogicalStart+previous.Length+legacyHeaderSize {
				return nil, fmt.Errorf("section %d is not contiguous", sectionIndex)
			}
		}
		index.sections[sectionIndex] = archiveSection{
			startVolume:  section.StartVolume,
			endVolume:    section.EndVolume,
			logicalStart: section.LogicalStart,
			length:       section.Length,
		}
	}

	for snapshotIndex, snapshot := range cache.Snapshots {
		if snapshot.Section < 0 || snapshot.Section >= len(index.sections) {
			return nil, fmt.Errorf("snapshot %d refers to an invalid section", snapshotIndex)
		}
		if len(snapshot.Entries) == 0 {
			return nil, fmt.Errorf("snapshot %d contains no entries", snapshotIndex)
		}
		entries := make([]snapshotEntry, len(snapshot.Entries))
		seenPaths := make(map[string]struct{}, len(snapshot.Entries))
		seenOffsets := make(map[uint64]struct{}, len(snapshot.Entries))
		for entryIndex, entry := range snapshot.Entries {
			if entry.Kind != 0 && entry.Kind != 1 {
				return nil, fmt.Errorf(
					"snapshot %d entry %d has unsupported kind %d",
					snapshotIndex,
					entryIndex,
					entry.Kind,
				)
			}
			candidate := snapshotEntry{
				components:   entry.Components,
				kind:         entry.Kind,
				timestamp:    entry.Timestamp,
				recordOffset: entry.RecordOffset,
			}
			if _, err := safeExtractionPath(".", candidate); err != nil {
				return nil, fmt.Errorf(
					"snapshot %d entry %d has unsafe path: %w",
					snapshotIndex,
					entryIndex,
					err,
				)
			}
			key := entryKey(entry.Components)
			if _, exists := seenPaths[key]; exists {
				return nil, fmt.Errorf("snapshot %d contains duplicate path", snapshotIndex)
			}
			seenPaths[key] = struct{}{}
			if _, exists := seenOffsets[entry.RecordOffset]; exists {
				return nil, fmt.Errorf("snapshot %d contains duplicate record offset", snapshotIndex)
			}
			seenOffsets[entry.RecordOffset] = struct{}{}
			if !offsetInSections(entry.RecordOffset, index.sections) {
				return nil, fmt.Errorf(
					"snapshot %d entry %d has record offset outside all sections",
					snapshotIndex,
					entryIndex,
				)
			}
			entries[entryIndex] = candidate
		}
		index.snapshots[snapshotIndex] = catalogSnapshot{
			section: snapshot.Section,
			entries: entries,
		}
	}
	return index, nil
}

func offsetInSections(offset uint64, sections []archiveSection) bool {
	for _, section := range sections {
		if offset >= section.logicalStart && offset < section.logicalStart+section.length {
			return true
		}
	}
	return false
}

func fingerprintVolumes(volumes []volume) ([]cachedVolume, error) {
	fingerprints := make([]cachedVolume, len(volumes))
	for index, candidate := range volumes {
		info, err := os.Stat(candidate.path)
		if err != nil {
			return nil, err
		}
		if info.Size() != candidate.size {
			return nil, fmt.Errorf("%s changed size while being fingerprinted", candidate.path)
		}
		digest, err := fingerprintVolumeEdges(candidate.path, candidate.size)
		if err != nil {
			return nil, err
		}
		fingerprints[index] = cachedVolume{
			Name:            filepath.Base(candidate.path),
			Sequence:        candidate.sequence,
			Size:            candidate.size,
			ModTimeUnixNano: info.ModTime().UnixNano(),
			EdgeSHA256:      digest,
		}
	}
	return fingerprints, nil
}

func fingerprintVolumeEdges(path string, size int64) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	sum := sha256.New()
	firstLength := min(size, int64(fingerprintEdgeLen))
	if _, err := io.CopyN(sum, file, firstLength); err != nil {
		return "", err
	}
	if size > firstLength {
		lastLength := min(size-firstLength, int64(fingerprintEdgeLen))
		if _, err := file.Seek(size-lastLength, io.SeekStart); err != nil {
			return "", err
		}
		if _, err := io.CopyN(sum, file, lastLength); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(sum.Sum(nil)), nil
}

func equalVolumeFingerprints(left, right []cachedVolume) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
