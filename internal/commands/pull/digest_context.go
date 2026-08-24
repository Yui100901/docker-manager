package pull

import (
	"context"
	"fmt"
	"os"

	digest "github.com/opencontainers/go-digest"
)

func verifyFileDigestWithContext(ctx context.Context, path string, expected digest.Digest) error {
	if expected == "" {
		return nil
	}
	if err := expected.Validate(); err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	verifier := expected.Verifier()
	if err := copyWithContext(ctx, verifier, file); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !verifier.Verified() {
		return fmt.Errorf("digest 校验失败 %s: 期望 %s", path, expected)
	}
	return nil
}
