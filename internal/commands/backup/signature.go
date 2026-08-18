package backup

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const maxBackupKeyFileSize = 1024 * 1024

func signBackupChecksumsWithContext(ctx context.Context, root, privateKeyPath string) error {
	if err := checkBackupContext(ctx); err != nil {
		return err
	}
	if err := requireBackupKeyOutsideRoot(root, privateKeyPath, "signing key"); err != nil {
		return err
	}
	keyData, err := readLimitedBackupFile(ctx, privateKeyPath, maxBackupKeyFileSize)
	if err != nil {
		return fmt.Errorf("read signing key: %w", err)
	}
	privateKey, err := parseEd25519PrivateKey(keyData)
	if err != nil {
		return fmt.Errorf("parse signing key: %w", err)
	}
	checksums, err := readLimitedBackupFile(ctx, filepath.Join(root, backupChecksumName), maxBackupKeyFileSize)
	if err != nil {
		return fmt.Errorf("read checksums for signing: %w", err)
	}
	signature := ed25519.Sign(privateKey, checksums)
	encoded := base64.StdEncoding.EncodeToString(signature) + "\n"
	if err := os.WriteFile(filepath.Join(root, backupSignatureName), []byte(encoded), 0644); err != nil {
		return fmt.Errorf("write backup signature: %w", err)
	}
	return nil
}

func verifyBackupSignatureWithContext(ctx context.Context, root, publicKeyPath string) (string, error) {
	if strings.TrimSpace(publicKeyPath) == "" {
		return "未要求；checksum 仅校验完整性，不证明备份来源", nil
	}
	if err := checkBackupContext(ctx); err != nil {
		return "", err
	}
	if err := requireBackupKeyOutsideRoot(root, publicKeyPath, "trusted public key"); err != nil {
		return "", err
	}
	keyData, err := readLimitedBackupFile(ctx, publicKeyPath, maxBackupKeyFileSize)
	if err != nil {
		return "", fmt.Errorf("read trusted public key: %w", err)
	}
	publicKey, err := parseEd25519PublicKey(keyData)
	if err != nil {
		return "", fmt.Errorf("parse trusted public key: %w", err)
	}
	checksums, err := readLimitedBackupFile(ctx, filepath.Join(root, backupChecksumName), maxBackupKeyFileSize)
	if err != nil {
		return "", fmt.Errorf("read signed checksums: %w", err)
	}
	encodedSignature, err := readLimitedBackupFile(ctx, filepath.Join(root, backupSignatureName), maxBackupKeyFileSize)
	if err != nil {
		return "", fmt.Errorf("read backup signature: %w", err)
	}
	signature, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(encodedSignature)))
	if err != nil {
		return "", fmt.Errorf("decode backup signature: %w", err)
	}
	if len(signature) != ed25519.SignatureSize || !ed25519.Verify(publicKey, checksums, signature) {
		return "", fmt.Errorf("backup signature verification failed")
	}
	return "Ed25519 签名已验证", nil
}

func requireBackupKeyOutsideRoot(root, keyPath, description string) error {
	return requireBackupPathOutsideRoot(root, keyPath, description)
}

func requireBackupSensitiveFileOutsideRoot(root, candidate, description string) error {
	if err := requireBackupPathOutsideRoot(root, candidate, description); err != nil {
		return err
	}
	candidateInfo, err := os.Stat(candidate)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", description, err)
	}
	rootInfo, err := os.Stat(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("inspect backup root: %w", err)
	}
	if !rootInfo.IsDir() {
		return fmt.Errorf("backup root is not a directory: %s", root)
	}
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		if os.SameFile(candidateInfo, info) {
			return fmt.Errorf("%s must not be linked into the backup root", description)
		}
		return nil
	})
}

func requireBackupPathOutsideRoot(root, candidate, description string) error {
	rootPath, err := resolveBackupBoundaryPath(root)
	if err != nil {
		return fmt.Errorf("resolve backup root: %w", err)
	}
	candidatePath, err := resolveBackupBoundaryPath(candidate)
	if err != nil {
		return fmt.Errorf("resolve %s: %w", description, err)
	}
	rootVolume := filepath.VolumeName(rootPath)
	candidateVolume := filepath.VolumeName(candidatePath)
	if rootVolume != "" && candidateVolume != "" && !strings.EqualFold(rootVolume, candidateVolume) {
		return nil
	}
	relative, err := filepath.Rel(rootPath, candidatePath)
	if err != nil {
		return fmt.Errorf("compare backup root and %s: %w", description, err)
	}
	if relative == "." || (!filepath.IsAbs(relative) && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))) {
		return fmt.Errorf("%s must be outside the backup root", description)
	}
	return nil
}

func resolveBackupBoundaryPath(value string) (string, error) {
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	current := filepath.Clean(absolute)
	var missing []string
	for {
		if _, err := os.Lstat(current); err == nil {
			resolved, err := filepath.EvalSymlinks(current)
			if err != nil {
				return "", err
			}
			for i := len(missing) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, missing[i])
			}
			return filepath.Clean(resolved), nil
		} else if !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return filepath.Clean(absolute), nil
		}
		missing = append(missing, filepath.Base(current))
		current = parent
	}
}

func parseEd25519PrivateKey(data []byte) (ed25519.PrivateKey, error) {
	block, rest := pem.Decode(data)
	if block == nil || len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("expected one PEM private key")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("key is %T, want Ed25519 private key", parsed)
	}
	return key, nil
}

func parseEd25519PublicKey(data []byte) (ed25519.PublicKey, error) {
	block, rest := pem.Decode(data)
	if block == nil || len(bytes.TrimSpace(rest)) != 0 {
		return nil, fmt.Errorf("expected one PEM public key or certificate")
	}
	var parsed interface{}
	var err error
	if block.Type == "CERTIFICATE" {
		var certificate *x509.Certificate
		certificate, err = x509.ParseCertificate(block.Bytes)
		if err == nil {
			parsed = certificate.PublicKey
		}
	} else {
		parsed, err = x509.ParsePKIXPublicKey(block.Bytes)
	}
	if err != nil {
		return nil, err
	}
	key, ok := parsed.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("key is %T, want Ed25519 public key", parsed)
	}
	return key, nil
}

func readLimitedBackupFile(ctx context.Context, path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var output bytes.Buffer
	limited := &io.LimitedReader{R: file, N: limit + 1}
	if err := backupCopyWithContext(ctx, &output, limited); err != nil {
		return nil, err
	}
	if int64(output.Len()) > limit {
		return nil, fmt.Errorf("file exceeds %d bytes", limit)
	}
	return output.Bytes(), nil
}
