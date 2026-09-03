//go:build windows

package pull

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
	"unsafe"

	"golang.org/x/sys/windows"
)

type pullBatchWindowsPathClaim struct {
	pullBatchPathClaim
	canonicalPath string
}

const (
	pullBatchWindowsVolumeNameDOS          = 0
	pullBatchWindowsInitialPathBufferUnits = 256
	pullBatchWindowsMaxPathBufferUnits     = 32768
)

const pullBatchWindowsCSTRCompareEqual = 2

var pullBatchWindowsCompareStringOrdinal = windows.NewLazySystemDLL("kernel32.dll").NewProc("CompareStringOrdinal")

type pullBatchWindowsOrdinalCall func(left, right []uint16) (uintptr, error)

func validatePullBatchPlatformPathClaims(claimed map[string]pullBatchPathClaim) error {
	claims := make([]pullBatchWindowsPathClaim, 0, len(claimed))
	for _, claim := range claimed {
		if err := validatePullBatchPlatformPath(claim.Path); err != nil {
			return fmt.Errorf("批量输出路径不安全: %s %s: %w", claim.Owner, claim.Path, err)
		}
		canonicalPath, err := canonicalPullBatchWindowsPath(claim.Path)
		if err != nil {
			return fmt.Errorf("解析批量输出路径的 Windows 实际祖先失败: %s %s: %w", claim.Owner, claim.Path, err)
		}
		claims = append(claims, pullBatchWindowsPathClaim{
			pullBatchPathClaim: claim,
			canonicalPath:      canonicalPath,
		})
	}
	sort.Slice(claims, func(left, right int) bool {
		return pullBatchPathLess(claims[left].canonicalPath, claims[right].canonicalPath)
	})
	for left := 0; left < len(claims); left++ {
		for right := left + 1; right < len(claims); right++ {
			switch {
			case pullBatchPathsEqual(claims[left].canonicalPath, claims[right].canonicalPath):
				return fmt.Errorf("批量输出路径 Windows 别名冲突: %s 与 %s 指向同一路径 (%s, %s)",
					claims[left].Owner, claims[right].Owner, claims[left].Path, claims[right].Path)
			case pullBatchPathContains(claims[left].canonicalPath, claims[right].canonicalPath):
				return fmt.Errorf("批量输出路径 Windows 别名拓扑冲突: %s %s 是 %s %s 的实际祖先路径",
					claims[left].Owner, claims[left].Path, claims[right].Owner, claims[right].Path)
			case pullBatchPathContains(claims[right].canonicalPath, claims[left].canonicalPath):
				return fmt.Errorf("批量输出路径 Windows 别名拓扑冲突: %s %s 是 %s %s 的实际祖先路径",
					claims[right].Owner, claims[right].Path, claims[left].Owner, claims[left].Path)
			}
		}
	}
	return nil
}

func validatePullBatchPlatformPath(path string) error {
	normalizedPath := strings.ReplaceAll(path, "/", `\`)
	lowerPath := strings.ToLower(normalizedPath)
	if strings.HasPrefix(lowerPath, `\\?\`) || strings.HasPrefix(lowerPath, `\\.\`) ||
		strings.HasPrefix(lowerPath, `\??\`) || strings.HasPrefix(lowerPath, `\\??\`) {
		return fmt.Errorf("拒绝 Windows 设备或扩展路径命名空间")
	}
	volume := filepath.VolumeName(path)
	normalizedVolume := strings.ReplaceAll(volume, "/", `\`)
	if strings.HasPrefix(normalizedVolume, `\\`) {
		for _, component := range strings.FieldsFunc(normalizedVolume, func(value rune) bool {
			return value == '\\'
		}) {
			if err := validatePullBatchWindowsPathComponent(component); err != nil {
				return err
			}
		}
	}
	remainder := strings.TrimPrefix(path, volume)
	for _, component := range strings.FieldsFunc(remainder, func(value rune) bool {
		return value == '\\' || value == '/'
	}) {
		if component == "." || component == ".." {
			continue
		}
		if err := validatePullBatchWindowsPathComponent(component); err != nil {
			return err
		}
	}
	return nil
}

func pullBatchPathsEqual(left, right string) bool {
	return pullBatchWindowsOrdinalCompare(filepath.Clean(left), filepath.Clean(right)) == pullBatchWindowsCSTRCompareEqual
}

func pullBatchPathLess(left, right string) bool {
	left = filepath.Clean(left)
	right = filepath.Clean(right)
	comparison := pullBatchWindowsOrdinalCompare(left, right)
	if comparison == pullBatchWindowsCSTRCompareEqual {
		return left < right
	}
	return comparison == 1
}

func pullBatchWindowsOrdinalCompare(left, right string) uintptr {
	return pullBatchWindowsOrdinalCompareWith(left, right, callPullBatchWindowsCompareStringOrdinal)
}

func pullBatchWindowsOrdinalCompareWith(left, right string, compare pullBatchWindowsOrdinalCall) uintptr {
	left = filepath.Clean(left)
	right = filepath.Clean(right)
	if left == right {
		return pullBatchWindowsCSTRCompareEqual
	}
	leftUTF16, leftErr := windows.UTF16FromString(left)
	rightUTF16, rightErr := windows.UTF16FromString(right)
	if leftErr != nil || rightErr != nil {
		panic(fmt.Sprintf("Windows ordinal 路径比较收到无效 UTF-16 输入: %v", errors.Join(leftErr, rightErr)))
	}
	result, callErr := compare(leftUTF16, rightUTF16)
	if result < 1 || result > 3 {
		panic(fmt.Sprintf("Windows ordinal 路径比较失败: result=%d error=%v", result, callErr))
	}
	return result
}

func callPullBatchWindowsCompareStringOrdinal(left, right []uint16) (uintptr, error) {
	result, _, callErr := pullBatchWindowsCompareStringOrdinal.Call(
		uintptr(unsafe.Pointer(&left[0])),
		uintptr(len(left)-1),
		uintptr(unsafe.Pointer(&right[0])),
		uintptr(len(right)-1),
		1,
	)
	return result, callErr
}

func pullBatchPathContains(parent, child string) bool {
	parentVolume, parentComponents := pullBatchWindowsPathParts(parent)
	childVolume, childComponents := pullBatchWindowsPathParts(child)
	if !pullBatchPathsEqual(parentVolume, childVolume) || len(parentComponents) >= len(childComponents) {
		return false
	}
	for index := range parentComponents {
		if !pullBatchPathsEqual(parentComponents[index], childComponents[index]) {
			return false
		}
	}
	return true
}

func pullBatchWindowsPathParts(path string) (string, []string) {
	path = filepath.Clean(path)
	volume := filepath.VolumeName(path)
	remainder := strings.TrimLeft(strings.TrimPrefix(path, volume), `\/`)
	if remainder == "" {
		return volume, nil
	}
	return volume, strings.FieldsFunc(remainder, func(value rune) bool {
		return value == '\\' || value == '/'
	})
}

func validatePullBatchWindowsPathComponent(component string) error {
	if strings.HasSuffix(component, ".") || strings.HasSuffix(component, " ") {
		return fmt.Errorf("windows 路径组件不能以点或空格结尾: %q", component)
	}
	if pullBatchWindowsReservedName(component) {
		return fmt.Errorf("windows 路径组件不能使用设备名: %q", component)
	}
	for _, value := range component {
		if value < 32 || strings.ContainsRune(`<>:"|?*`, value) {
			return fmt.Errorf("windows 路径组件包含保留字符: %q", component)
		}
	}
	return nil
}

func pullBatchWindowsReservedName(name string) bool {
	base := name
	if separator := strings.IndexAny(base, ".:"); separator >= 0 {
		base = base[:separator]
	}
	base = strings.TrimRight(base, " ")
	upper := strings.ToUpper(base)
	switch upper {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$":
		return true
	}
	if len(upper) < 4 || (upper[:3] != "COM" && upper[:3] != "LPT") {
		return false
	}
	switch upper[3:] {
	case "1", "2", "3", "4", "5", "6", "7", "8", "9", "\u00b9", "\u00b2", "\u00b3":
		return true
	default:
		return false
	}
}

func canonicalPullBatchWindowsPath(path string) (string, error) {
	inspection, err := inspectPullBatchWindowsPath(path)
	if err != nil {
		return "", err
	}
	return inspection.canonicalPath, nil
}

func pullBatchWindowsLooksLikeShortAlias(name string) bool {
	base := name
	extension := ""
	if dot := strings.LastIndexByte(name, '.'); dot >= 0 {
		base = name[:dot]
		extension = name[dot+1:]
	}
	if len(base) == 0 || utf8.RuneCountInString(base) > 8 || utf8.RuneCountInString(extension) > 3 || strings.ContainsRune(base, '.') {
		return false
	}
	tilde := strings.LastIndexByte(base, '~')
	if tilde <= 0 || tilde == len(base)-1 {
		return false
	}
	for _, value := range base[tilde+1:] {
		if value < '0' || value > '9' {
			return false
		}
	}
	return true
}

func finalPullBatchWindowsHandlePath(handle windows.Handle) (string, error) {
	bufferSize := uint32(pullBatchWindowsInitialPathBufferUnits)
	for {
		buffer := make([]uint16, bufferSize)
		length, err := windows.GetFinalPathNameByHandle(
			handle,
			&buffer[0],
			bufferSize,
			pullBatchWindowsVolumeNameDOS,
		)
		if err != nil {
			return "", err
		}
		if length < bufferSize {
			resolved := windows.UTF16ToString(buffer[:length])
			switch {
			case strings.HasPrefix(strings.ToUpper(resolved), `\\?\UNC\`):
				resolved = `\\` + resolved[len(`\\?\UNC\`):]
			case strings.HasPrefix(resolved, `\\?\`):
				resolved = resolved[len(`\\?\`):]
			}
			return resolved, nil
		}
		if length == 0 || length > pullBatchWindowsMaxPathBufferUnits {
			return "", fmt.Errorf("windows 实际路径超过 %d 个 UTF-16 单元", pullBatchWindowsMaxPathBufferUnits)
		}
		bufferSize = length
	}
}

type pullBatchWindowsPathInspection struct {
	canonicalPath string
	leafExists    bool
	leafDirectory bool
	leafInfo      os.FileInfo
}

type pullBatchWindowsCaseSensitiveInfo struct {
	Flags uint32
}

type pullBatchWindowsCaseSensitiveQuery func(windows.Handle, *pullBatchWindowsCaseSensitiveInfo) error

func pullBatchWindowsDirectoryCaseSensitive(handle windows.Handle) (bool, error) {
	return pullBatchWindowsDirectoryCaseSensitiveWith(handle, func(handle windows.Handle, info *pullBatchWindowsCaseSensitiveInfo) error {
		return windows.GetFileInformationByHandleEx(
			handle,
			windows.FileCaseSensitiveInfo,
			(*byte)(unsafe.Pointer(info)),
			uint32(unsafe.Sizeof(*info)),
		)
	})
}

func pullBatchWindowsDirectoryCaseSensitiveWith(handle windows.Handle, query pullBatchWindowsCaseSensitiveQuery) (bool, error) {
	var info pullBatchWindowsCaseSensitiveInfo
	if err := query(handle, &info); err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) ||
			errors.Is(err, windows.ERROR_NOT_SUPPORTED) ||
			errors.Is(err, windows.ERROR_INVALID_FUNCTION) {
			return false, nil
		}
		return false, err
	}
	return info.Flags&windows.FILE_CS_FLAG_CASE_SENSITIVE_DIR != 0, nil
}

func inspectPullBatchWindowsPath(path string) (pullBatchWindowsPathInspection, error) {
	if err := validatePullBatchPlatformPath(path); err != nil {
		return pullBatchWindowsPathInspection{}, err
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return pullBatchWindowsPathInspection{}, err
	}
	absolute = filepath.Clean(absolute)
	volume, components := pullBatchWindowsPathParts(absolute)
	if volume == "" {
		return pullBatchWindowsPathInspection{}, fmt.Errorf("windows 绝对路径缺少卷或 UNC 共享: %s", path)
	}
	rootPath := volume + `\`
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return pullBatchWindowsPathInspection{}, fmt.Errorf("打开 Windows 路径根失败: %w", err)
	}
	defer root.Close()
	rootHandleFile, err := root.Open(".")
	if err != nil {
		return pullBatchWindowsPathInspection{}, fmt.Errorf("锚定 Windows 路径根失败: %w", err)
	}
	defer rootHandleFile.Close()

	parentHandle := windows.Handle(rootHandleFile.Fd())
	var ownedParent windows.Handle
	defer func() {
		if ownedParent != 0 {
			_ = windows.CloseHandle(ownedParent)
		}
	}()

	if len(components) == 0 {
		canonical, resolveErr := finalPullBatchWindowsHandlePath(parentHandle)
		leafInfo, statErr := rootHandleFile.Stat()
		return pullBatchWindowsPathInspection{
			canonicalPath: filepath.Clean(canonical),
			leafExists:    true,
			leafDirectory: true,
			leafInfo:      leafInfo,
		}, errors.Join(resolveErr, statErr)
	}
	for index, component := range components {
		caseSensitive, caseErr := pullBatchWindowsDirectoryCaseSensitive(parentHandle)
		if caseErr != nil {
			return pullBatchWindowsPathInspection{}, fmt.Errorf("检查 Windows 路径父目录大小写敏感属性失败: %w", caseErr)
		}
		if caseSensitive {
			parentPath := filepath.Join(append([]string{rootPath}, components[:index]...)...)
			return pullBatchWindowsPathInspection{}, fmt.Errorf("拒绝通过大小写敏感的 Windows 目录访问批量输出: %s", parentPath)
		}
		handle, info, openErr := openPullBatchWindowsPathComponent(parentHandle, component)
		if pullBatchWindowsPathComponentMissing(openErr) {
			for _, suffixComponent := range components[index:] {
				if pullBatchWindowsLooksLikeShortAlias(suffixComponent) {
					return pullBatchWindowsPathInspection{}, fmt.Errorf("拒绝尚不存在的 DOS 8.3 短名路径组件: %q", suffixComponent)
				}
			}
			canonicalParent, resolveErr := finalPullBatchWindowsHandlePath(parentHandle)
			if resolveErr != nil {
				return pullBatchWindowsPathInspection{}, resolveErr
			}
			canonical := canonicalParent
			for _, suffixComponent := range components[index:] {
				canonical = filepath.Join(canonical, suffixComponent)
			}
			return pullBatchWindowsPathInspection{canonicalPath: filepath.Clean(canonical)}, nil
		}
		if openErr != nil {
			return pullBatchWindowsPathInspection{}, fmt.Errorf("逐级检查 Windows 路径组件 %q 失败: %w", component, openErr)
		}
		if info.FileAttributes&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			_ = windows.CloseHandle(handle)
			return pullBatchWindowsPathInspection{}, fmt.Errorf("拒绝通过符号链接或重解析点访问批量输出: %s", filepath.Join(append([]string{rootPath}, components[:index+1]...)...))
		}
		last := index == len(components)-1
		isDirectory := info.FileAttributes&windows.FILE_ATTRIBUTE_DIRECTORY != 0
		if !last && !isDirectory {
			_ = windows.CloseHandle(handle)
			return pullBatchWindowsPathInspection{}, fmt.Errorf("批量输出路径祖先不是目录: %s", filepath.Join(append([]string{rootPath}, components[:index+1]...)...))
		}
		if last {
			canonical, resolveErr := finalPullBatchWindowsHandlePath(handle)
			file := os.NewFile(uintptr(handle), component)
			if file == nil {
				_ = windows.CloseHandle(handle)
				return pullBatchWindowsPathInspection{}, fmt.Errorf("创建 Windows 路径组件句柄失败: %s", component)
			}
			leafInfo, statErr := file.Stat()
			closeErr := file.Close()
			if err := errors.Join(resolveErr, statErr, closeErr); err != nil {
				return pullBatchWindowsPathInspection{}, err
			}
			return pullBatchWindowsPathInspection{
				canonicalPath: filepath.Clean(canonical),
				leafExists:    true,
				leafDirectory: isDirectory,
				leafInfo:      leafInfo,
			}, nil
		}
		if ownedParent != 0 {
			_ = windows.CloseHandle(ownedParent)
		}
		ownedParent = handle
		parentHandle = handle
	}
	return pullBatchWindowsPathInspection{}, fmt.Errorf("无法检查 Windows 路径: %s", path)
}

func openPullBatchWindowsPathComponent(parent windows.Handle, component string) (windows.Handle, windows.ByHandleFileInformation, error) {
	name, err := windows.NewNTUnicodeString(component)
	if err != nil {
		return 0, windows.ByHandleFileInformation{}, err
	}
	attributes := &windows.OBJECT_ATTRIBUTES{
		Length:        uint32(unsafe.Sizeof(windows.OBJECT_ATTRIBUTES{})),
		RootDirectory: parent,
		ObjectName:    name,
		Attributes:    windows.OBJ_CASE_INSENSITIVE,
	}
	var handle windows.Handle
	err = windows.NtCreateFile(
		&handle,
		windows.FILE_READ_ATTRIBUTES|windows.SYNCHRONIZE,
		attributes,
		&windows.IO_STATUS_BLOCK{},
		nil,
		windows.FILE_ATTRIBUTE_NORMAL,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		windows.FILE_OPEN,
		windows.FILE_SYNCHRONOUS_IO_NONALERT|windows.FILE_OPEN_REPARSE_POINT|windows.FILE_OPEN_FOR_BACKUP_INTENT,
		0,
		0,
	)
	if err != nil {
		return 0, windows.ByHandleFileInformation{}, err
	}
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(handle, &info); err != nil {
		_ = windows.CloseHandle(handle)
		return 0, windows.ByHandleFileInformation{}, err
	}
	return handle, info, nil
}

func pullBatchWindowsPathComponentMissing(err error) bool {
	return err == windows.STATUS_OBJECT_NAME_NOT_FOUND || err == windows.STATUS_OBJECT_PATH_NOT_FOUND
}

func rejectPullInternalOutputAlias(path string) error {
	inspection, err := inspectPullBatchWindowsPath(path)
	if err != nil {
		return err
	}
	canonicalName := filepath.Base(inspection.canonicalPath)
	if pullInternalOutputNameReserved(canonicalName) {
		return fmt.Errorf("输出文件的 Windows 实际名称使用了 pull 内部保留命名空间: %s", canonicalName)
	}
	return nil
}

func rejectPullArchiveOutputLinks(outputPath string) error {
	inspection, err := inspectPullBatchWindowsPath(outputPath)
	if err != nil {
		return err
	}
	if inspection.leafExists && inspection.leafDirectory {
		return fmt.Errorf("拒绝替换非普通归档输出: %s", outputPath)
	}
	return nil
}

func pullPathLstatNoFollow(path string) (os.FileInfo, error) {
	inspection, err := inspectPullBatchWindowsPath(path)
	if err != nil {
		return nil, err
	}
	if !inspection.leafExists {
		return nil, &os.PathError{Op: "lstat", Path: path, Err: os.ErrNotExist}
	}
	return inspection.leafInfo, nil
}
