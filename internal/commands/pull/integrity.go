package pull

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	digest "github.com/opencontainers/go-digest"
	ocispec "github.com/opencontainers/image-spec/specs-go/v1"
)

const (
	maxConfigBlobSize   int64 = 16 * 1024 * 1024
	maxManifestBlobSize int64 = 16 * 1024 * 1024
)

var errRegistryResponseTooLarge = errors.New("registry response exceeds size limit")

func validateDescriptor(descriptor ocispec.Descriptor, subject string) error {
	if err := descriptor.Digest.Validate(); err != nil {
		return fmt.Errorf("%s digest 无效: %w", subject, err)
	}
	if descriptor.Size < 0 {
		return fmt.Errorf("%s 大小无效: %d", subject, descriptor.Size)
	}
	return nil
}

func validateConfigDescriptor(descriptor ocispec.Descriptor) error {
	if err := validateDescriptor(descriptor, "镜像 config"); err != nil {
		return err
	}
	if descriptor.Digest.Algorithm() != digest.Canonical {
		return fmt.Errorf("镜像 config digest 算法不受支持: %s", descriptor.Digest.Algorithm())
	}
	if descriptor.Size > maxConfigBlobSize {
		return fmt.Errorf("镜像 config 大小 %d 超过上限 %d", descriptor.Size, maxConfigBlobSize)
	}
	return nil
}

func validateManifestDescriptor(descriptor ocispec.Descriptor) error {
	if err := validateDescriptor(descriptor, "平台 manifest"); err != nil {
		return err
	}
	if descriptor.Size > maxManifestBlobSize {
		return fmt.Errorf("平台 manifest 大小 %d 超过上限 %d", descriptor.Size, maxManifestBlobSize)
	}
	return nil
}

func boundedReadLimit(declaredSize, maxSize int64) int64 {
	if declaredSize >= 0 && declaredSize < maxSize {
		if declaredSize == 0 {
			return 1
		}
		return declaredSize
	}
	return maxSize
}

func configBlobFileName(descriptor ocispec.Descriptor) (string, error) {
	if err := validateConfigDescriptor(descriptor); err != nil {
		return "", err
	}

	name := descriptor.Digest.Encoded() + ".json"
	if !fs.ValidPath(name) || !filepath.IsLocal(name) || filepath.Base(name) != name {
		return "", fmt.Errorf("镜像 config 文件名不安全: %q", name)
	}
	return name, nil
}

func verifyBytesDigest(data []byte, expected digest.Digest, subject string) error {
	if err := expected.Validate(); err != nil {
		return fmt.Errorf("%s digest 无效: %w", subject, err)
	}
	actual := expected.Algorithm().FromBytes(data)
	if actual != expected {
		return fmt.Errorf("%s digest 校验失败: 期望 %s，实际 %s", subject, expected, actual)
	}
	return nil
}

func verifyDescriptorBytes(data []byte, descriptor ocispec.Descriptor, subject string) error {
	if err := validateDescriptor(descriptor, subject); err != nil {
		return err
	}
	if int64(len(data)) != descriptor.Size {
		return fmt.Errorf("%s 大小校验失败: 期望 %d，实际 %d", subject, descriptor.Size, len(data))
	}
	return verifyBytesDigest(data, descriptor.Digest, subject)
}

func writeFileWithinRoot(rootPath, name string, data []byte, perm fs.FileMode) (retErr error) {
	if !fs.ValidPath(name) || !filepath.IsLocal(name) {
		return fmt.Errorf("拒绝写入根目录外路径: %q", name)
	}

	rootInfo, err := os.Lstat(rootPath)
	if err != nil {
		return err
	}
	if rootInfo.Mode()&os.ModeSymlink != 0 || !rootInfo.IsDir() {
		return fmt.Errorf("临时根目录不是实际目录: %q", rootPath)
	}

	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return err
	}
	defer func() {
		if closeErr := root.Close(); retErr == nil && closeErr != nil {
			retErr = closeErr
		}
	}()

	file, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
	if err != nil {
		return err
	}
	closed := false
	keep := false
	defer func() {
		if !closed {
			_ = file.Close()
		}
		if !keep {
			_ = root.Remove(name)
		}
	}()

	if _, err := file.Write(data); err != nil {
		return err
	}
	if err := file.Close(); err != nil {
		closed = true
		return err
	}
	closed = true
	keep = true
	return nil
}
