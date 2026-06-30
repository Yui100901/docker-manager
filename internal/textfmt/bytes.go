package textfmt

import "fmt"

// Bytes formats byte counts with binary units for CLI reports and progress.
func Bytes(size uint64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	value := float64(size)
	for _, suffix := range []string{"KiB", "MiB", "GiB", "TiB"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%.1f PiB", value/unit)
}

// SignedBytes clamps negative values to zero before formatting.
func SignedBytes(size int64) string {
	if size <= 0 {
		return "0 B"
	}
	return Bytes(uint64(size))
}

// Rate formats bytes per second with binary units.
func Rate(bytesPerSecond float64) string {
	if bytesPerSecond < 0 {
		bytesPerSecond = 0
	}
	const unit = 1024
	if bytesPerSecond < unit {
		return fmt.Sprintf("%.0f B/s", bytesPerSecond)
	}
	value := bytesPerSecond
	for _, suffix := range []string{"KiB/s", "MiB/s", "GiB/s"} {
		value /= unit
		if value < unit {
			return fmt.Sprintf("%.1f %s", value, suffix)
		}
	}
	return fmt.Sprintf("%.1f TiB/s", value/unit)
}
