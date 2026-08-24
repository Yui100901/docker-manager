package pull

import (
	"fmt"
	"sync"
	"time"

	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	maxTokenResponseSize              int64 = 16 * 1024 * 1024
	defaultTokenResponseSize                = 1 * 1024 * 1024
	maxRegistryPingBodyBytes          int64 = 64 * 1024
	defaultMaxLayerBytes              int64 = 32 << 30
	defaultMaxExpandedLayerBytes      int64 = 64 << 30
	defaultMaxTotalLayerBytes         int64 = 64 << 30
	maxTotalLayerBytes                int64 = 512 << 30
	defaultMaxTotalExpandedLayerBytes int64 = 128 << 30
	maxTotalExpandedLayerBytes        int64 = 1 << 40
	defaultMaxTemporaryBytes          int64 = 272 << 30
	maxTemporaryBytes                 int64 = 3 << 40
	defaultMaxLayers                        = 1_000
	maxLayers                               = 10_000
	defaultPullTotalTimeout                 = time.Hour
	maxPullTotalTimeout                     = 24 * time.Hour
)

type PullResourceLimits struct {
	TokenBytes         int64
	ManifestBytes      int64
	ConfigBytes        int64
	LayerBytes         int64
	ExpandedLayerBytes int64
	TotalLayerBytes    int64
	TotalExpandedBytes int64
	TemporaryBytes     int64
	MaxLayers          int
	TotalTimeout       time.Duration
}

func defaultPullResourceLimits() PullResourceLimits {
	return PullResourceLimits{
		TokenBytes:         defaultTokenResponseSize,
		ManifestBytes:      maxManifestBlobSize,
		ConfigBytes:        maxConfigBlobSize,
		LayerBytes:         defaultMaxLayerBytes,
		ExpandedLayerBytes: defaultMaxExpandedLayerBytes,
		TotalLayerBytes:    defaultMaxTotalLayerBytes,
		TotalExpandedBytes: defaultMaxTotalExpandedLayerBytes,
		TemporaryBytes:     defaultMaxTemporaryBytes,
		MaxLayers:          defaultMaxLayers,
		TotalTimeout:       defaultPullTotalTimeout,
	}
}

func effectivePullResourceLimits(value PullResourceLimits) PullResourceLimits {
	defaults := defaultPullResourceLimits()
	if value.TokenBytes <= 0 {
		value.TokenBytes = defaults.TokenBytes
	}
	if value.ManifestBytes <= 0 {
		value.ManifestBytes = defaults.ManifestBytes
	}
	if value.ConfigBytes <= 0 {
		value.ConfigBytes = defaults.ConfigBytes
	}
	if value.LayerBytes <= 0 {
		value.LayerBytes = defaults.LayerBytes
	}
	if value.ExpandedLayerBytes <= 0 {
		value.ExpandedLayerBytes = defaults.ExpandedLayerBytes
	}
	if value.TotalLayerBytes <= 0 {
		value.TotalLayerBytes = defaults.TotalLayerBytes
	}
	if value.TotalExpandedBytes <= 0 {
		value.TotalExpandedBytes = defaults.TotalExpandedBytes
	}
	if value.TemporaryBytes <= 0 {
		value.TemporaryBytes = defaults.TemporaryBytes
	}
	if value.MaxLayers <= 0 {
		value.MaxLayers = defaults.MaxLayers
	}
	if value.TotalTimeout <= 0 {
		value.TotalTimeout = defaults.TotalTimeout
	}
	return value
}

func validatePullResourceLimits(value PullResourceLimits) error {
	value = effectivePullResourceLimits(value)
	return validateConfiguredPullResourceLimits(value)
}

func validateConfiguredPullResourceLimits(value PullResourceLimits) error {
	for _, limit := range []struct {
		name    string
		value   int64
		ceiling int64
	}{
		{name: "--max-token-bytes", value: value.TokenBytes, ceiling: maxTokenResponseSize},
		{name: "--max-manifest-bytes", value: value.ManifestBytes, ceiling: maxManifestBlobSize},
		{name: "--max-config-bytes", value: value.ConfigBytes, ceiling: maxConfigBlobSize},
		{name: "--max-layer-bytes", value: value.LayerBytes, ceiling: maxLayerTarBytes},
		{name: "--max-expanded-layer-bytes", value: value.ExpandedLayerBytes, ceiling: maxLayerTarBytes},
		{name: "--max-total-layer-bytes", value: value.TotalLayerBytes, ceiling: maxTotalLayerBytes},
		{name: "--max-total-expanded-bytes", value: value.TotalExpandedBytes, ceiling: maxTotalExpandedLayerBytes},
		{name: "--max-temporary-bytes", value: value.TemporaryBytes, ceiling: maxTemporaryBytes},
	} {
		if limit.value <= 0 || limit.value > limit.ceiling {
			return fmt.Errorf("%s 必须在 1 到 %d 之间", limit.name, limit.ceiling)
		}
	}
	if value.MaxLayers <= 0 || value.MaxLayers > maxLayers {
		return fmt.Errorf("--max-layers 必须在 1 到 %d 之间", maxLayers)
	}
	if value.TotalTimeout <= 0 || value.TotalTimeout > maxPullTotalTimeout {
		return fmt.Errorf("--total-timeout 必须大于 0 且不超过 %s", maxPullTotalTimeout)
	}
	return nil
}

func validateLayerDescriptor(descriptor ocispec.Descriptor, limits PullResourceLimits) error {
	if err := validateDescriptor(descriptor, "镜像层"); err != nil {
		return err
	}
	limits = effectivePullResourceLimits(limits)
	if descriptor.Size > limits.LayerBytes {
		return fmt.Errorf("镜像层大小 %d 超过上限 %d", descriptor.Size, limits.LayerBytes)
	}
	return nil
}

func validateManifestLayers(manifest *ocispec.Manifest, limits PullResourceLimits) error {
	if manifest == nil {
		return fmt.Errorf("镜像 manifest 为空")
	}
	limits = effectivePullResourceLimits(limits)
	if len(manifest.Layers) > limits.MaxLayers {
		return fmt.Errorf("镜像层数量 %d 超过上限 %d", len(manifest.Layers), limits.MaxLayers)
	}
	var total int64
	for _, layer := range manifest.Layers {
		if err := validateLayerDescriptor(layer, limits); err != nil {
			return err
		}
		if layer.Size > limits.TotalLayerBytes-total {
			return fmt.Errorf("镜像压缩层总大小超过上限 %d", limits.TotalLayerBytes)
		}
		total += layer.Size
	}
	return nil
}

// pullResourceBudget is shared by all layer workers for one image. Descriptor
// sizes bound compressed input up front; expanded and temporary bytes must be
// reserved as workers materialize data because those sizes are not declared.
type pullResourceBudget struct {
	mu             sync.Mutex
	limits         PullResourceLimits
	expandedBytes  int64
	temporaryBytes int64
}

func newPullResourceBudget(limits PullResourceLimits, initialTemporaryBytes int64) (*pullResourceBudget, error) {
	limits = effectivePullResourceLimits(limits)
	if initialTemporaryBytes < 0 || initialTemporaryBytes > limits.TemporaryBytes {
		return nil, fmt.Errorf("镜像临时文件大小 %d 超过上限 %d", initialTemporaryBytes, limits.TemporaryBytes)
	}
	return &pullResourceBudget{limits: limits, temporaryBytes: initialTemporaryBytes}, nil
}

func (b *pullResourceBudget) reserve(expandedDelta, temporaryDelta int64) error {
	if b == nil {
		return nil
	}
	if expandedDelta < 0 || temporaryDelta < 0 {
		return fmt.Errorf("资源预算增量不能为负数")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if expandedDelta > b.limits.TotalExpandedBytes-b.expandedBytes {
		return fmt.Errorf("镜像层展开总大小超过上限 %d", b.limits.TotalExpandedBytes)
	}
	if temporaryDelta > b.limits.TemporaryBytes-b.temporaryBytes {
		return fmt.Errorf("镜像临时文件峰值超过上限 %d", b.limits.TemporaryBytes)
	}
	b.expandedBytes += expandedDelta
	b.temporaryBytes += temporaryDelta
	return nil
}

func (b *pullResourceBudget) release(expandedDelta, temporaryDelta int64) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if expandedDelta >= b.expandedBytes {
		b.expandedBytes = 0
	} else if expandedDelta > 0 {
		b.expandedBytes -= expandedDelta
	}
	if temporaryDelta >= b.temporaryBytes {
		b.temporaryBytes = 0
	} else if temporaryDelta > 0 {
		b.temporaryBytes -= temporaryDelta
	}
}
