package backup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
)

const backupWindowsReparsePointAttr = 0x400

type privateBackupDirectory struct {
	path  string
	root  *os.Root
	owner os.FileInfo
}

func ensurePrivateBackupDirectory(path string) error {
	if err := rejectBackupPathSymlinks(path); err != nil {
		return err
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	if err := rejectBackupPathSymlinks(path); err != nil {
		return err
	}
	if err := os.Chmod(path, 0700); err != nil {
		return fmt.Errorf("secure backup directory %s: %w", path, err)
	}
	return nil
}

func createPrivateBackupDirectory(path string) (*privateBackupDirectory, error) {
	if err := rejectBackupPathSymlinks(path); err != nil {
		return nil, err
	}
	if _, err := os.Lstat(path); err == nil {
		return nil, fmt.Errorf("encrypted backup plaintext directory already exists: %s", path)
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	parent := filepath.Dir(path)
	if err := rejectBackupPathSymlinks(parent); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(parent, 0700); err != nil {
		return nil, err
	}
	if err := rejectBackupPathSymlinks(parent); err != nil {
		return nil, err
	}
	if err := os.Mkdir(path, 0700); err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, fmt.Errorf("open encrypted backup plaintext directory: %w", err)
	}
	owner, err := root.Stat(".")
	if err != nil {
		_ = root.Close()
		return nil, fmt.Errorf("inspect encrypted backup plaintext directory: %w", err)
	}
	current, err := os.Lstat(path)
	if err != nil || backupInfoIsReparsePoint(current) || !current.IsDir() || !os.SameFile(owner, current) {
		_ = root.Close()
		if err != nil {
			return nil, fmt.Errorf("verify encrypted backup plaintext directory: %w", err)
		}
		return nil, fmt.Errorf("encrypted backup plaintext directory changed while opening: %s", path)
	}
	return &privateBackupDirectory{path: path, root: root, owner: owner}, nil
}

func (d *privateBackupDirectory) removeAll() error {
	if d == nil || d.root == nil {
		return nil
	}
	root := d.root
	d.root = nil
	var resultErr error

	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		resultErr = errors.Join(resultErr, fmt.Errorf("read encrypted backup plaintext directory: %w", err))
	} else {
		for _, entry := range entries {
			if err := root.RemoveAll(entry.Name()); err != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("remove encrypted backup plaintext entry %s: %w", entry.Name(), err))
			}
		}
	}

	current, err := os.Lstat(d.path)
	var identityErr error
	if err != nil {
		if os.IsNotExist(err) {
			identityErr = fmt.Errorf("encrypted backup plaintext directory moved or removed before cleanup: %s", d.path)
		} else {
			identityErr = fmt.Errorf("inspect encrypted backup plaintext directory before cleanup: %w", err)
		}
	} else if backupInfoIsReparsePoint(current) || !current.IsDir() || d.owner == nil || !os.SameFile(d.owner, current) {
		identityErr = fmt.Errorf("refusing to remove replaced encrypted backup plaintext directory: %s", d.path)
	}
	closeErr := root.Close()
	resultErr = errors.Join(resultErr, closeErr, identityErr)
	if identityErr != nil || closeErr != nil {
		return resultErr
	}
	if err := os.Remove(d.path); err != nil && !os.IsNotExist(err) {
		resultErr = errors.Join(resultErr, fmt.Errorf("remove encrypted backup plaintext directory %s: %w", d.path, err))
	}
	return resultErr
}

func backupInfoIsReparsePoint(info os.FileInfo) bool {
	if info == nil || info.Mode()&os.ModeSymlink != 0 {
		return info != nil
	}
	value := reflect.ValueOf(info.Sys())
	if value.IsValid() && value.Kind() == reflect.Pointer && !value.IsNil() {
		value = value.Elem()
	}
	if !value.IsValid() || value.Kind() != reflect.Struct {
		return false
	}
	attributes := value.FieldByName("FileAttributes")
	return attributes.IsValid() && attributes.CanUint() && attributes.Uint()&backupWindowsReparsePointAttr != 0
}

func writePrivateBackupFile(path string, data []byte, mode os.FileMode) error {
	if err := ensurePrivateBackupDirectory(filepath.Dir(path)); err != nil {
		return err
	}
	if err := rejectBackupPathSymlinks(path); err != nil {
		return err
	}
	if err := os.WriteFile(path, data, mode); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}
