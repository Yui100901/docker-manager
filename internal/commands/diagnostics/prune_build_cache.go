package diagnostics

import (
	"fmt"
	"time"

	"github.com/moby/moby/api/types/build"
)

type pruneBuildCacheSnapshot struct {
	ID             string
	UntilCutoff    time.Time
	HasUntilCutoff bool
}

func newPruneBuildCacheSnapshot(cache *build.CacheRecord, cutoff time.Time, hasCutoff bool) *pruneBuildCacheSnapshot {
	if cache == nil {
		return nil
	}
	return &pruneBuildCacheSnapshot{
		ID:             cache.ID,
		UntilCutoff:    cutoff,
		HasUntilCutoff: hasCutoff,
	}
}

func (snapshot *pruneBuildCacheSnapshot) validateCurrent(cache *build.CacheRecord) error {
	if snapshot == nil {
		return fmt.Errorf("snapshot identity is missing")
	}
	if cache == nil || cache.ID != snapshot.ID {
		return fmt.Errorf("cache identity changed after the report snapshot")
	}
	if cache.InUse {
		return fmt.Errorf("cache record is now in use")
	}
	if snapshot.HasUntilCutoff && cache.LastUsedAt != nil && cache.LastUsedAt.After(snapshot.UntilCutoff) {
		return fmt.Errorf("cache record was used after the fixed until cutoff %s", snapshot.UntilCutoff.Format(time.RFC3339Nano))
	}
	return nil
}
