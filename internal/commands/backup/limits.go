package backup

import (
	"fmt"
	"math"
	"time"
)

// These limits are deliberately generous for normal image backups while still
// bounding every allocation and temporary-file growth driven by backup input.
const (
	maxBackupEncryptionPlaintextBytes int64 = 512 << 30

	maxBackupArchiveEntries       int64 = 100_000
	maxBackupArchivePathBytes           = 4 << 10
	maxBackupArchiveFileBytes     int64 = 256 << 30
	maxBackupArchiveExpandedBytes int64 = 1 << 40
	maxBackupArchiveBytes         int64 = 512 << 30
	maxBackupTemporaryBytes       int64 = 2 << 40
	maxBackupArchivePartBytes     int64 = 64 << 30
	maxBackupArchivePartCount           = 999
	maxBackupArchiveOperationTime       = 30 * time.Minute

	maxBackupManifestJSONBytes int64 = 16 << 20
	maxBackupInspectJSONBytes  int64 = 64 << 20
	maxBackupResourceJSONBytes int64 = 16 << 20
	maxBackupJSONTotalBytes    int64 = 256 << 20
	maxBackupJSONDepth               = 128
	maxBackupJSONTokens        int64 = 1_000_000
	maxBackupContainers              = 10_000
	maxBackupResourceRefs            = 100_000

	maxBackupPartsManifestBytes int64 = 4 << 20
)

type backupByteBudget struct {
	name  string
	limit int64
	used  int64
}

type backupLimits struct {
	archiveBytes  int64
	expandedBytes int64
	jsonBytes     int64
	parts         int
}

func defaultBackupLimits() backupLimits {
	return backupLimits{
		archiveBytes:  maxBackupArchiveBytes,
		expandedBytes: maxBackupArchiveExpandedBytes,
		jsonBytes:     maxBackupJSONTotalBytes,
		parts:         maxBackupArchivePartCount,
	}
}

func resolveRestoreLimits(opts RestoreOptions) (backupLimits, error) {
	limits := defaultBackupLimits()
	if opts.MaxArchiveBytes != 0 {
		limits.archiveBytes = opts.MaxArchiveBytes
	}
	if opts.MaxExpandedBytes != 0 {
		limits.expandedBytes = opts.MaxExpandedBytes
	}
	if opts.MaxJSONBytes != 0 {
		limits.jsonBytes = opts.MaxJSONBytes
	}
	if opts.MaxParts != 0 {
		limits.parts = opts.MaxParts
	}
	for _, limit := range []struct {
		name string
		got  int64
		hard int64
	}{
		{name: "archive", got: limits.archiveBytes, hard: maxBackupArchiveBytes},
		{name: "expanded archive", got: limits.expandedBytes, hard: maxBackupArchiveExpandedBytes},
		{name: "JSON", got: limits.jsonBytes, hard: maxBackupJSONTotalBytes},
	} {
		if limit.got <= 0 || limit.got > limit.hard {
			return backupLimits{}, fmt.Errorf("%s byte limit must be between 1 and %d", limit.name, limit.hard)
		}
	}
	if limits.parts <= 0 || limits.parts > maxBackupArchivePartCount {
		return backupLimits{}, fmt.Errorf("part limit must be between 1 and %d", maxBackupArchivePartCount)
	}
	return limits, nil
}

func (l backupLimits) temporaryBytes() int64 {
	// A split encrypted restore can hold the joined ciphertext, decrypted
	// archive, and extracted tree at the same time.
	if l.archiveBytes > (maxBackupTemporaryBytes-l.expandedBytes)/2 {
		return maxBackupTemporaryBytes
	}
	return l.archiveBytes*2 + l.expandedBytes
}

type backupTarBudget struct {
	entries     int64
	bytes       int64
	maxExpanded int64
}

func (b *backupTarBudget) Add(name string, size int64, regular bool) error {
	b.entries++
	if b.entries > maxBackupArchiveEntries {
		return fmt.Errorf("backup archive exceeds the %d-entry limit", maxBackupArchiveEntries)
	}
	if len([]byte(name)) > maxBackupArchivePathBytes {
		return fmt.Errorf("backup archive path exceeds the %d-byte limit: %s", maxBackupArchivePathBytes, name)
	}
	if !regular {
		return nil
	}
	if size < 0 || size > maxBackupArchiveFileBytes {
		return fmt.Errorf("backup archive file exceeds the %d-byte limit: %s", maxBackupArchiveFileBytes, name)
	}
	maxExpanded := b.maxExpanded
	if maxExpanded <= 0 {
		maxExpanded = maxBackupArchiveExpandedBytes
	}
	if size > maxExpanded-b.bytes {
		return fmt.Errorf("backup archive exceeds the %d-byte expanded-size limit", maxExpanded)
	}
	b.bytes += size
	return nil
}

func newBackupByteBudget(name string, limit int64) *backupByteBudget {
	return &backupByteBudget{name: name, limit: limit}
}

func (b *backupByteBudget) Add(size int64) error {
	if size < 0 {
		return fmt.Errorf("%s contains a negative byte count", b.name)
	}
	if size > b.limit || b.used > b.limit-size || b.used > math.MaxInt64-size {
		return fmt.Errorf("%s exceeds the %d-byte budget", b.name, b.limit)
	}
	b.used += size
	return nil
}
