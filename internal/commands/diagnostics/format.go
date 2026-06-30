package diagnostics

import "docker-manager/internal/textfmt"

func humanBytes(size uint64) string {
	return textfmt.Bytes(size)
}
