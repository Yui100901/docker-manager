//go:build ignore

package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const (
	formatTarGZ = "tar.gz"
	formatZip   = "zip"
)

var archiveEpoch = time.Unix(0, 0).UTC()

func main() {
	sourceDir := flag.String("source-dir", "", "package directory to archive")
	archivePath := flag.String("archive", "", "output archive path")
	format := flag.String("format", "", "archive format: tar.gz or zip")
	flag.Parse()

	if *sourceDir == "" || *archivePath == "" || (*format != formatTarGZ && *format != formatZip) {
		fmt.Fprintln(os.Stderr, "usage: create-release-archive --source-dir DIR --archive FILE --format tar.gz|zip")
		os.Exit(2)
	}
	if err := create(*sourceDir, *archivePath, *format); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func create(sourceDir, archivePath, format string) error {
	sourceDir, err := filepath.Abs(sourceDir)
	if err != nil {
		return fmt.Errorf("resolve source directory: %w", err)
	}
	archivePath, err = filepath.Abs(archivePath)
	if err != nil {
		return fmt.Errorf("resolve archive path: %w", err)
	}
	sourceInfo, err := os.Stat(sourceDir)
	if err != nil {
		return fmt.Errorf("stat source directory: %w", err)
	}
	if !sourceInfo.IsDir() {
		return fmt.Errorf("source path is not a directory: %s", sourceDir)
	}
	if _, err := os.Lstat(archivePath); err == nil {
		return fmt.Errorf("archive already exists: %s", archivePath)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect archive path: %w", err)
	}
	if rel, err := filepath.Rel(sourceDir, archivePath); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return fmt.Errorf("archive must not be inside source directory: %s", archivePath)
	}
	if err := os.MkdirAll(filepath.Dir(archivePath), 0o755); err != nil {
		return fmt.Errorf("create archive directory: %w", err)
	}
	rootName := filepath.Base(sourceDir)
	if rootName == "" || rootName == "." || rootName == ".." || strings.ContainsAny(rootName, `/\\`) {
		return fmt.Errorf("invalid package directory name: %q", rootName)
	}

	tmp, err := os.CreateTemp(filepath.Dir(archivePath), ".dm-release-archive-*")
	if err != nil {
		return fmt.Errorf("create archive staging file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("protect archive staging file: %w", err)
	}

	if format == formatTarGZ {
		err = writeTarGZ(tmp, sourceDir, rootName)
	} else {
		err = writeZip(tmp, sourceDir, rootName)
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("write %s archive: %w", format, err)
	}
	if err := os.Rename(tmpName, archivePath); err != nil {
		return fmt.Errorf("publish archive: %w", err)
	}
	if err := verifyArchive(archivePath, rootName, format); err != nil {
		_ = os.Remove(archivePath)
		return fmt.Errorf("verify %s archive: %w", format, err)
	}
	return nil
}

func writeTarGZ(dst io.Writer, sourceDir, rootName string) error {
	file, ok := dst.(*os.File)
	if !ok {
		return errors.New("tar destination is not a file")
	}
	gz := gzip.NewWriter(file)
	gz.Header.ModTime = archiveEpoch
	gz.Header.OS = 255
	tw := tar.NewWriter(gz)
	err := walkPackage(sourceDir, rootName, func(name string, entry fs.DirEntry, info fs.FileInfo) error {
		mode := releaseMode(name, info.IsDir())
		header := &tar.Header{
			Name:       name,
			Mode:       int64(mode),
			ModTime:    archiveEpoch,
			AccessTime: archiveEpoch,
			ChangeTime: archiveEpoch,
		}
		if info.IsDir() {
			header.Name = strings.TrimSuffix(name, "/") + "/"
			header.Typeflag = tar.TypeDir
			return tw.WriteHeader(header)
		}
		if entry.Type()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported package entry: %s", name)
		}
		header.Typeflag = tar.TypeReg
		header.Size = info.Size()
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		input, err := os.Open(filepath.Join(sourceDir, filepath.FromSlash(strings.TrimPrefix(name, rootName+"/"))))
		if err != nil {
			return err
		}
		written, copyErr := io.Copy(tw, input)
		closeErr := input.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if written != info.Size() {
			return fmt.Errorf("size changed while archiving %s", name)
		}
		return nil
	})
	if closeErr := tw.Close(); err == nil {
		err = closeErr
	}
	if closeErr := gz.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = file.Sync()
	}
	return err
}

func writeZip(dst io.Writer, sourceDir, rootName string) error {
	file, ok := dst.(*os.File)
	if !ok {
		return errors.New("zip destination is not a file")
	}
	zw := zip.NewWriter(file)
	err := walkPackage(sourceDir, rootName, func(name string, entry fs.DirEntry, info fs.FileInfo) error {
		mode := releaseMode(name, info.IsDir())
		if info.IsDir() {
			name = strings.TrimSuffix(name, "/") + "/"
		}
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		header.SetModTime(archiveEpoch)
		if info.IsDir() {
			header.SetMode(fs.ModeDir | fs.FileMode(mode))
			header.Method = zip.Store
		} else {
			if entry.Type()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
				return fmt.Errorf("unsupported package entry: %s", name)
			}
			header.SetMode(fs.FileMode(mode))
		}
		writer, err := zw.CreateHeader(header)
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		input, err := os.Open(filepath.Join(sourceDir, filepath.FromSlash(strings.TrimPrefix(name, rootName+"/"))))
		if err != nil {
			return err
		}
		written, copyErr := io.Copy(writer, input)
		closeErr := input.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if written != info.Size() {
			return fmt.Errorf("size changed while archiving %s", name)
		}
		return nil
	})
	if closeErr := zw.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = file.Sync()
	}
	return err
}

func walkPackage(sourceDir, rootName string, visit func(string, fs.DirEntry, fs.FileInfo) error) error {
	return filepath.WalkDir(sourceDir, func(current string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("package contains symlink: %s", current)
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(sourceDir, current)
		if err != nil {
			return err
		}
		name := rootName
		if rel != "." {
			name += "/" + filepath.ToSlash(rel)
		}
		if info.IsDir() {
			name += "/"
		}
		return visit(name, entry, info)
	})
}

func releaseMode(name string, directory bool) int {
	if directory {
		return 0o755
	}
	base := path.Base(strings.TrimSuffix(name, "/"))
	if base == "dm" || strings.HasSuffix(base, ".sh") {
		return 0o755
	}
	return 0o644
}

func verifyArchive(archivePath, rootName, format string) error {
	if format == formatTarGZ {
		return verifyTarGZ(archivePath, rootName)
	}
	return verifyZip(archivePath, rootName)
}

func verifyTarGZ(archivePath, rootName string) error {
	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer file.Close()
	gz, err := gzip.NewReader(file)
	if err != nil {
		return err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	seen := map[string]bool{}
	entries := 0
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		name := strings.TrimSuffix(filepath.ToSlash(header.Name), "/")
		if !validArchiveName(name, rootName) {
			return fmt.Errorf("invalid archive entry path: %q", header.Name)
		}
		if seen[name] {
			return fmt.Errorf("duplicate archive entry: %s", name)
		}
		seen[name] = true
		entries++
		want := releaseMode(name, header.FileInfo().IsDir())
		if header.Mode&0o777 != int64(want) {
			return fmt.Errorf("archive mode for %s is %o, want %o", name, header.Mode&0o777, want)
		}
		if header.FileInfo().IsDir() {
			if header.Typeflag != tar.TypeDir {
				return fmt.Errorf("directory entry has wrong type: %s", name)
			}
		} else if header.Typeflag != tar.TypeReg && header.Typeflag != 0 {
			return fmt.Errorf("file entry has unsupported type: %s", name)
		}
	}
	if entries == 0 || !seen[rootName] {
		return errors.New("archive has no package root")
	}
	return nil
}

func verifyZip(archivePath, rootName string) error {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer reader.Close()
	seen := map[string]bool{}
	for _, entry := range reader.File {
		name := strings.TrimSuffix(filepath.ToSlash(entry.Name), "/")
		if !validArchiveName(name, rootName) {
			return fmt.Errorf("invalid archive entry path: %q", entry.Name)
		}
		if seen[name] {
			return fmt.Errorf("duplicate archive entry: %s", name)
		}
		seen[name] = true
		isDir := strings.HasSuffix(entry.Name, "/")
		want := releaseMode(name, isDir)
		if int(entry.Mode().Perm()) != want {
			return fmt.Errorf("archive mode for %s is %o, want %o", name, entry.Mode().Perm(), want)
		}
	}
	if len(seen) == 0 || !seen[rootName] {
		return errors.New("archive has no package root")
	}
	return nil
}

func validArchiveName(name, rootName string) bool {
	if name == "" || name == "." || name == ".." || path.IsAbs(name) || name != path.Clean(name) {
		return false
	}
	return name == rootName || strings.HasPrefix(name, rootName+"/")
}
