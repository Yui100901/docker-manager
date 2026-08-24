param(
    [string]$InstallDir,
    [string]$BinDir,
    [string]$ConfigDir,
    [string]$DataDir,
    [string]$CompletionDir,
    [string]$CompletionProfilePath,
    [switch]$MachineScope,
    [switch]$Purge,
    [switch]$DryRun
)

$ErrorActionPreference = "Stop"
$Scope = if ($MachineScope) { "Machine" } else { "User" }
$InstallDirExplicit = $PSBoundParameters.ContainsKey("InstallDir")
$BinDirExplicit = $PSBoundParameters.ContainsKey("BinDir")
$ConfigDirExplicit = $PSBoundParameters.ContainsKey("ConfigDir")
$DataDirExplicit = $PSBoundParameters.ContainsKey("DataDir")
$CompletionDirExplicit = $PSBoundParameters.ContainsKey("CompletionDir")
$CompletionProfileExplicit = $PSBoundParameters.ContainsKey("CompletionProfilePath")

function ConvertTo-NormalizedPath {
    param(
        [string]$Path,
        [string]$Description = "path"
    )
    if ([string]::IsNullOrWhiteSpace($Path)) {
        throw "Missing $Description."
    }
    try {
        $full = [System.IO.Path]::GetFullPath($Path)
    } catch {
        throw "Invalid ${Description}: $Path"
    }
    $root = [System.IO.Path]::GetPathRoot($full)
    if ($full.Length -le $root.Length) {
        return $root
    }
    return $full.TrimEnd([System.IO.Path]::DirectorySeparatorChar, [System.IO.Path]::AltDirectorySeparatorChar)
}

function ConvertFrom-ManifestPath {
    param(
        [string]$Path,
        [string]$Description
    )
    if ([string]::IsNullOrWhiteSpace($Path) -or -not [System.IO.Path]::IsPathRooted($Path)) {
        throw "Install manifest contains a missing or non-absolute ${Description}: $Path"
    }
    return ConvertTo-NormalizedPath -Path $Path -Description "manifest $Description"
}

function Test-PathWithin {
    param(
        [string]$Path,
        [string]$Root
    )
    $pathFull = ConvertTo-NormalizedPath -Path $Path
    $rootFull = ConvertTo-NormalizedPath -Path $Root
    if ($pathFull.Equals($rootFull, [System.StringComparison]::OrdinalIgnoreCase)) {
        return $true
    }
    $boundary = if ($rootFull.EndsWith([string][System.IO.Path]::DirectorySeparatorChar) -or
        $rootFull.EndsWith([string][System.IO.Path]::AltDirectorySeparatorChar)) {
        $rootFull
    } else {
        $rootFull + [System.IO.Path]::DirectorySeparatorChar
    }
    return $pathFull.StartsWith($boundary, [System.StringComparison]::OrdinalIgnoreCase)
}

function Assert-PathWithin {
    param(
        [string]$Path,
        [string]$Root,
        [string]$Description,
        [switch]$RequireDescendant
    )
    $pathFull = ConvertTo-NormalizedPath -Path $Path
    $rootFull = ConvertTo-NormalizedPath -Path $Root
    Assert-NoUninstallReparsePoint -Path $pathFull -Description $Description
    Assert-NoUninstallReparsePoint -Path $rootFull -Description "$Description root"
    if (-not (Test-PathWithin -Path $pathFull -Root $rootFull) -or
        ($RequireDescendant -and $pathFull.Equals($rootFull, [System.StringComparison]::OrdinalIgnoreCase))) {
        throw "Refusing manifest $Description outside its authorized root: $pathFull (root: $rootFull)"
    }
}

function Assert-PathsEqual {
    param(
        [string]$Actual,
        [string]$Expected,
        [string]$Description
    )
    $actualFull = ConvertTo-NormalizedPath -Path $Actual
    $expectedFull = ConvertTo-NormalizedPath -Path $Expected
    Assert-NoUninstallReparsePoint -Path $actualFull -Description $Description
    Assert-NoUninstallReparsePoint -Path $expectedFull -Description "$Description authorized path"
    if (-not $actualFull.Equals($expectedFull, [System.StringComparison]::OrdinalIgnoreCase)) {
        throw "Install manifest $Description does not match the authorized path: $actualFull (expected: $expectedFull)"
    }
}

function Assert-NotFileSystemRoot {
    param(
        [string]$Path,
        [string]$Description
    )
    $full = ConvertTo-NormalizedPath -Path $Path
    $root = [System.IO.Path]::GetPathRoot($full)
    if ($full.Equals($root, [System.StringComparison]::OrdinalIgnoreCase)) {
        throw "Refusing to use a filesystem root as ${Description}: $full"
    }
}

function Get-UninstallPathItem {
    param([string]$Path)
    try {
        return Get-Item -LiteralPath $Path -Force -ErrorAction Stop
    } catch [System.Management.Automation.ItemNotFoundException] {
        return $null
    } catch {
        throw "Cannot safely inspect uninstall path '$Path': $($_.Exception.Message)"
    }
}

function Test-UninstallReparsePoint {
    param([System.IO.FileSystemInfo]$Item)
    return [bool](($Item.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0)
}

function Assert-NoUninstallReparsePoint {
    param(
        [string]$Path,
        [string]$Description = "uninstall path"
    )
    $full = ConvertTo-NormalizedPath -Path $Path -Description $Description
    $root = [System.IO.Path]::GetPathRoot($full)
    $current = $root
    $relative = $full.Substring($root.Length)
    $separators = [char[]]@(
        [System.IO.Path]::DirectorySeparatorChar,
        [System.IO.Path]::AltDirectorySeparatorChar
    )
    $segments = @($relative.Split($separators, [System.StringSplitOptions]::RemoveEmptyEntries))
    $paths = New-Object 'System.Collections.Generic.List[string]'
    $paths.Add($root)
    foreach ($segment in $segments) {
        $current = Join-Path $current $segment
        $paths.Add($current)
    }
    for ($i = 0; $i -lt $paths.Count; $i++) {
        $item = Get-UninstallPathItem -Path $paths[$i]
        if ($null -eq $item) { break }
        if (Test-UninstallReparsePoint -Item $item) {
            throw "Refusing $Description through reparse point: $($paths[$i])"
        }
        if ($i -lt $paths.Count - 1 -and -not $item.PSIsContainer) {
            throw "Refusing $Description through non-directory path component: $($paths[$i])"
        }
    }
}

function Assert-NoUninstallReparseTree {
    param(
        [string]$Path,
        [string]$Description = "uninstall tree"
    )
    $full = ConvertTo-NormalizedPath -Path $Path -Description $Description
    Assert-NoUninstallReparsePoint -Path $full -Description $Description
    $rootItem = Get-UninstallPathItem -Path $full
    if ($null -eq $rootItem -or -not $rootItem.PSIsContainer) { return }
    $stack = New-Object 'System.Collections.Generic.Stack[string]'
    $stack.Push($full)
    while ($stack.Count -gt 0) {
        $current = $stack.Pop()
        Assert-NoUninstallReparsePoint -Path $current -Description $Description
        $currentItem = Get-UninstallPathItem -Path $current
        if ($null -eq $currentItem) { throw "$Description changed while it was being inspected: $current" }
        if (Test-UninstallReparsePoint -Item $currentItem) { throw "Refusing reparse point in ${Description}: $current" }
        if (-not $currentItem.PSIsContainer) { throw "$Description changed from a directory: $current" }
        foreach ($child in @(Get-ChildItem -LiteralPath $current -Force -ErrorAction Stop)) {
            if (Test-UninstallReparsePoint -Item $child) {
                throw "Refusing reparse point in ${Description}: $($child.FullName)"
            }
            if ($child.PSIsContainer) { $stack.Push($child.FullName) }
        }
    }
}

function Remove-SafeUninstallPath {
    param(
        [string]$Path,
        [string]$Description = "uninstall path"
    )
    $full = ConvertTo-NormalizedPath -Path $Path -Description $Description
    Assert-NoUninstallReparseTree -Path $full -Description $Description
    $rootItem = Get-UninstallPathItem -Path $full
    if ($null -eq $rootItem) { return }
    if (-not $rootItem.PSIsContainer) {
        Assert-NoUninstallReparsePoint -Path $full -Description $Description
        $rootItem = Get-UninstallPathItem -Path $full
        if ($null -ne $rootItem) {
            if ($rootItem.PSIsContainer -or (Test-UninstallReparsePoint -Item $rootItem)) {
                throw "$Description changed before deletion: $full"
            }
            Remove-Item -LiteralPath $full -Force -ErrorAction Stop
        }
        return
    }

    $directories = New-Object 'System.Collections.Generic.List[string]'
    $files = New-Object 'System.Collections.Generic.List[string]'
    $stack = New-Object 'System.Collections.Generic.Stack[string]'
    $directories.Add($full)
    $stack.Push($full)
    while ($stack.Count -gt 0) {
        $current = $stack.Pop()
        Assert-NoUninstallReparsePoint -Path $current -Description $Description
        $currentItem = Get-UninstallPathItem -Path $current
        if ($null -eq $currentItem) { continue }
        if (-not $currentItem.PSIsContainer -or (Test-UninstallReparsePoint -Item $currentItem)) {
            throw "$Description changed during deletion preflight: $current"
        }
        foreach ($child in @(Get-ChildItem -LiteralPath $current -Force -ErrorAction Stop)) {
            if (Test-UninstallReparsePoint -Item $child) {
                throw "Refusing reparse point in ${Description}: $($child.FullName)"
            }
            if ($child.PSIsContainer) {
                $directories.Add($child.FullName)
                $stack.Push($child.FullName)
            } else {
                $files.Add($child.FullName)
            }
        }
    }

    foreach ($file in $files) {
        Assert-NoUninstallReparsePoint -Path $file -Description $Description
        $item = Get-UninstallPathItem -Path $file
        if ($null -eq $item) { continue }
        if ($item.PSIsContainer -or (Test-UninstallReparsePoint -Item $item)) {
            throw "$Description changed before file deletion: $file"
        }
        Remove-Item -LiteralPath $file -Force -ErrorAction Stop
    }
    foreach ($directory in @($directories | Sort-Object { $_.Length } -Descending)) {
        Assert-NoUninstallReparsePoint -Path $directory -Description $Description
        $item = Get-UninstallPathItem -Path $directory
        if ($null -eq $item) { continue }
        if (-not $item.PSIsContainer -or (Test-UninstallReparsePoint -Item $item)) {
            throw "$Description changed before directory deletion: $directory"
        }
        [System.IO.Directory]::Delete($directory, $false)
    }
}

function Remove-SafeUninstallFile {
    param(
        [string]$Path,
        [string]$Description = "uninstall file"
    )
    $full = ConvertTo-NormalizedPath -Path $Path -Description $Description
    Assert-NoUninstallReparsePoint -Path $full -Description $Description
    $item = Get-UninstallPathItem -Path $full
    if ($null -eq $item) { return }
    if ($item.PSIsContainer -or (Test-UninstallReparsePoint -Item $item)) {
        throw "Refusing to delete non-file ${Description}: $full"
    }
    Assert-NoUninstallReparsePoint -Path $full -Description $Description
    $item = Get-UninstallPathItem -Path $full
    if ($null -eq $item) { return }
    if ($item.PSIsContainer -or (Test-UninstallReparsePoint -Item $item)) {
        throw "$Description changed before deletion: $full"
    }
    Remove-Item -LiteralPath $full -Force -ErrorAction Stop
}

function Invoke-Step {
    param([scriptblock]$Action, [string]$Text)
    if ($DryRun) {
        Write-Host "DRY-RUN: $Text"
    } else {
        & $Action
    }
}

function Remove-EmptyParents {
    param(
        [string]$Path,
        [string]$StopDir
    )
    if (-not $Path -or -not $StopDir) { return }
    $current = ConvertTo-NormalizedPath -Path $Path
    $stopFull = ConvertTo-NormalizedPath -Path $StopDir
    Assert-NoUninstallReparsePoint -Path $stopFull -Description "empty-directory cleanup root"
    while ($current -and (Test-PathWithin -Path $current -Root $stopFull)) {
        Assert-NoUninstallReparsePoint -Path $current -Description "empty-directory cleanup path"
        $item = Get-UninstallPathItem -Path $current
        if ($null -eq $item) {
            if ($current.Equals($stopFull, [System.StringComparison]::OrdinalIgnoreCase)) { break }
            $parent = Split-Path -Parent $current
            if (-not $parent -or $parent -eq $current) { break }
            $current = ConvertTo-NormalizedPath -Path $parent
            continue
        }
        if (-not $item.PSIsContainer) {
            throw "Empty-directory cleanup path is not a directory: $current"
        }
        Assert-NoUninstallReparsePoint -Path $current -Description "empty-directory cleanup path"
        if ((Get-ChildItem -LiteralPath $current -Force -ErrorAction Stop | Select-Object -First 1)) {
            break
        }
        Assert-NoUninstallReparsePoint -Path $current -Description "empty-directory cleanup path"
        $item = Get-UninstallPathItem -Path $current
        if ($null -ne $item) {
            if (-not $item.PSIsContainer -or (Test-UninstallReparsePoint -Item $item)) {
                throw "Empty-directory cleanup path changed before deletion: $current"
            }
            [System.IO.Directory]::Delete($current, $false)
        }
        if ($current.Equals($stopFull, [System.StringComparison]::OrdinalIgnoreCase)) { break }
        $parent = Split-Path -Parent $current
        if (-not $parent -or $parent -eq $current) { break }
        $current = ConvertTo-NormalizedPath -Path $parent
    }
}

function Test-EnvironmentOwnerManifest {
    param(
        [string]$Path,
        [string]$CurrentManifest,
        [string]$ExpectedName,
        [string]$ExpectedValue
    )
    if (-not [System.IO.Path]::IsPathRooted($Path)) { return $false }
    try {
        $ownerPath = ConvertTo-NormalizedPath -Path $Path
        Assert-NoUninstallReparsePoint -Path $ownerPath -Description "environment owner manifest"
        if ($ownerPath.Equals($CurrentManifest, [System.StringComparison]::OrdinalIgnoreCase) -or
            -not (Test-Path -LiteralPath $ownerPath -PathType Leaf)) {
            return $false
        }
        Assert-NoUninstallReparsePoint -Path $ownerPath -Description "environment owner manifest"
        $ownerFile = Get-Item -LiteralPath $ownerPath -Force
        if ($ownerFile.Length -gt 1MB) { return $false }
        Assert-NoUninstallReparsePoint -Path $ownerPath -Description "environment owner manifest"
        $ownerManifest = Get-Content -LiteralPath $ownerPath -Raw | ConvertFrom-Json
        if ([string]$ownerManifest.scope -ne $Scope) { return $false }
        foreach ($ownerRecord in @($ownerManifest.environment_variables)) {
            if ([string]$ownerRecord.name -eq $ExpectedName -and [string]$ownerRecord.value -eq $ExpectedValue) {
                return $true
            }
        }
    } catch {
        return $false
    }
    return $false
}

function Get-EnvironmentRestoreRecord {
    param(
        [object]$Record,
        [string]$CurrentManifest
    )
    $properties = @($Record.PSObject.Properties.Name)
    if ($properties -contains "previous_owners") {
        foreach ($owner in @($Record.previous_owners)) {
            if (-not $owner -or -not $owner.manifest) { continue }
            $ownerPath = [string]$owner.manifest
            if (Test-EnvironmentOwnerManifest -Path $ownerPath -CurrentManifest $CurrentManifest -ExpectedName ([string]$Record.name) -ExpectedValue ([string]$owner.value)) {
                $present = [bool]$owner.present
                return [pscustomobject]@{
                    Present = $present
                    Value = if ($present) { [string]$owner.value } else { $null }
                }
            }
        }
        $baselinePresent = if ($properties -contains "baseline_present") {
            [bool]$Record.baseline_present
        } else {
            [bool]$Record.previous_present
        }
        return [pscustomobject]@{
            Present = $baselinePresent
            Value = if ($baselinePresent) {
                if ($properties -contains "baseline_value") { [string]$Record.baseline_value } else { [string]$Record.previous_value }
            } else {
                $null
            }
        }
    }

    $restorePresent = [bool]$Record.previous_present
    $restoreValue = if ($restorePresent) { [string]$Record.previous_value } else { $null }
    if ($Record.previous_owner_manifest) {
        $ownerPath = [string]$Record.previous_owner_manifest
        $ownerExists = Test-EnvironmentOwnerManifest -Path $ownerPath -CurrentManifest $CurrentManifest -ExpectedName ([string]$Record.name) -ExpectedValue ([string]$Record.previous_value)
        if (-not $ownerExists) {
            $restorePresent = [bool]$Record.previous_owner_previous_present
            $restoreValue = if ($restorePresent) { [string]$Record.previous_owner_previous_value } else { $null }
        }
    }
    return [pscustomobject]@{ Present = $restorePresent; Value = $restoreValue }
}

if (-not $InstallDir) {
    if ($MachineScope) {
        $InstallDir = Join-Path ${env:ProgramFiles} "docker-manager"
    } else {
        $InstallDir = Join-Path $env:LOCALAPPDATA "docker-manager"
    }
}
$AuthorizedInstallRoot = ConvertTo-NormalizedPath -Path $InstallDir -Description "install directory"
if (-not $BinDir) { $BinDir = Join-Path $AuthorizedInstallRoot "bin" }
if (-not $ConfigDir) {
    if ($MachineScope) {
        $ConfigDir = Join-Path $env:ProgramData "docker-manager"
    } else {
        $ConfigDir = Join-Path $env:APPDATA "docker-manager"
    }
}
if (-not $DataDir) { $DataDir = Join-Path $AuthorizedInstallRoot "data" }

$InstallDir = $AuthorizedInstallRoot
$BinDir = ConvertTo-NormalizedPath -Path $BinDir -Description "binary directory"
$ConfigDir = ConvertTo-NormalizedPath -Path $ConfigDir -Description "config directory"
$DataDir = ConvertTo-NormalizedPath -Path $DataDir -Description "data directory"
$CompletionRoot = if ($CompletionDirExplicit) {
    ConvertTo-NormalizedPath -Path $CompletionDir -Description "completion directory"
} else {
    $InstallDir
}
Assert-NotFileSystemRoot -Path $InstallDir -Description "install directory"
Assert-NotFileSystemRoot -Path $ConfigDir -Description "config directory"
Assert-NotFileSystemRoot -Path $DataDir -Description "data directory"
Assert-NotFileSystemRoot -Path $CompletionRoot -Description "completion directory"
foreach ($entry in @(
    @($InstallDir, "install directory"),
    @($BinDir, "binary directory"),
    @($ConfigDir, "config directory"),
    @($DataDir, "data directory"),
    @($CompletionRoot, "completion directory")
)) {
    Assert-NoUninstallReparsePoint -Path $entry[0] -Description $entry[1]
}

$Manifest = Join-Path $ConfigDir "install.json"
$InstalledBin = Join-Path $BinDir "dm.exe"
$OldWrapper = Join-Path $BinDir "dm.cmd"
$OldLibexecDir = Join-Path $InstallDir "lib"
$OldInstalledBin = Join-Path $OldLibexecDir "dm-bin.exe"
$ConfigFile = Join-Path $ConfigDir "dm.yaml"
$OutputDir = Join-Path $DataDir "images"
$CompletionRecords = @()
$CompletionProfile = $null
$CompletionProfileStart = "# >>> docker-manager completion >>>"
$CompletionProfileEnd = "# <<< docker-manager completion <<<"
$EnvironmentRecords = @()
$PathEntryAdded = $false

Assert-NoUninstallReparsePoint -Path $Manifest -Description "install manifest"
if (Test-Path -LiteralPath $Manifest -PathType Leaf) {
    Assert-NoUninstallReparsePoint -Path $Manifest -Description "install manifest"
    $manifestFile = Get-Item -LiteralPath $Manifest -Force
    if ($manifestFile.Length -gt 1MB) {
        throw "Install manifest exceeds the 1 MiB safety limit: $Manifest"
    }
    Assert-NoUninstallReparsePoint -Path $Manifest -Description "install manifest"
    $manifestData = Get-Content -LiteralPath $Manifest -Raw | ConvertFrom-Json
    $manifestProperties = @($manifestData.PSObject.Properties.Name)
    if (($manifestProperties -contains "install_id") -and
        [string]$manifestData.install_id -notmatch "^[a-fA-F0-9]{32}$") {
        throw "Install manifest contains an invalid install_id."
    }

    if ($manifestData.install_dir) {
        $manifestInstallDir = ConvertFrom-ManifestPath -Path ([string]$manifestData.install_dir) -Description "install_dir"
        if ($InstallDirExplicit) {
            Assert-PathsEqual -Actual $manifestInstallDir -Expected $InstallDir -Description "install_dir"
        } else {
            Assert-PathWithin -Path $manifestInstallDir -Root $AuthorizedInstallRoot -Description "install_dir"
            $InstallDir = $manifestInstallDir
        }
    }
    if ($manifestData.config_dir) {
        $manifestConfigDir = ConvertFrom-ManifestPath -Path ([string]$manifestData.config_dir) -Description "config_dir"
        Assert-PathsEqual -Actual $manifestConfigDir -Expected $ConfigDir -Description "config_dir"
    }
    if ($manifestData.bin_dir) {
        $manifestBinDir = ConvertFrom-ManifestPath -Path ([string]$manifestData.bin_dir) -Description "bin_dir"
        if ($BinDirExplicit) {
            Assert-PathsEqual -Actual $manifestBinDir -Expected $BinDir -Description "bin_dir"
        } else {
            Assert-PathWithin -Path $manifestBinDir -Root $InstallDir -Description "bin_dir"
            $BinDir = $manifestBinDir
        }
    } elseif (-not $BinDirExplicit) {
        $BinDir = Join-Path $InstallDir "bin"
    }
    if ($manifestData.data_dir) {
        $manifestDataDir = ConvertFrom-ManifestPath -Path ([string]$manifestData.data_dir) -Description "data_dir"
        if ($DataDirExplicit) {
            Assert-PathsEqual -Actual $manifestDataDir -Expected $DataDir -Description "data_dir"
        } else {
            Assert-PathWithin -Path $manifestDataDir -Root $InstallDir -Description "data_dir"
            $DataDir = $manifestDataDir
        }
    } elseif (-not $DataDirExplicit) {
        $DataDir = Join-Path $InstallDir "data"
    }
    if ($manifestData.scope -and [string]$manifestData.scope -ne $Scope) {
        throw "Install manifest scope '$($manifestData.scope)' does not match requested scope '$Scope'."
    }

    $InstalledBin = Join-Path $BinDir "dm.exe"
    $OldWrapper = Join-Path $BinDir "dm.cmd"
    $OldLibexecDir = Join-Path $InstallDir "lib"
    if ($manifestData.installed_bin) {
        $InstalledBin = ConvertFrom-ManifestPath -Path ([string]$manifestData.installed_bin) -Description "installed_bin"
    }
    Assert-PathWithin -Path $InstalledBin -Root $BinDir -Description "installed_bin" -RequireDescendant
    Assert-PathsEqual -Actual $InstalledBin -Expected (Join-Path $BinDir "dm.exe") -Description "installed_bin"

    if ($manifestData.libexec_dir) {
        $OldLibexecDir = ConvertFrom-ManifestPath -Path ([string]$manifestData.libexec_dir) -Description "libexec_dir"
    }
    Assert-PathWithin -Path $OldLibexecDir -Root $InstallDir -Description "libexec_dir" -RequireDescendant
    Assert-PathsEqual -Actual $OldLibexecDir -Expected (Join-Path $InstallDir "lib") -Description "libexec_dir"
    $OldInstalledBin = Join-Path $OldLibexecDir "dm-bin.exe"
    Assert-PathWithin -Path $OldInstalledBin -Root $OldLibexecDir -Description "legacy installed binary" -RequireDescendant

    if ($manifestData.completion_dir) {
        $manifestCompletionRoot = ConvertFrom-ManifestPath -Path ([string]$manifestData.completion_dir) -Description "completion_dir"
        if ($CompletionDirExplicit) {
            Assert-PathsEqual -Actual $manifestCompletionRoot -Expected $CompletionRoot -Description "completion_dir"
        } elseif (-not (Test-PathWithin -Path $manifestCompletionRoot -Root $InstallDir)) {
            throw "Install used a custom completion directory; pass -CompletionDir with the same path to uninstall it."
        }
    }
    if ($CompletionDirExplicit) {
        $CompletionRoot = ConvertTo-NormalizedPath -Path $CompletionDir -Description "completion directory"
    } else {
        $CompletionRoot = $InstallDir
    }
    $seenCompletionPaths = @{}
    if ($manifestProperties -contains "completion_records") {
        foreach ($record in @($manifestData.completion_records)) {
            if (-not $record -or -not $record.path) { throw "Install manifest contains an invalid completion record." }
            $completionPath = ConvertFrom-ManifestPath -Path ([string]$record.path) -Description "completion record path"
            if (-not (Test-PathWithin -Path $completionPath -Root $InstallDir) -and
                -not ($CompletionDirExplicit -and (Test-PathWithin -Path $completionPath -Root $CompletionRoot))) {
                throw "Refusing manifest completion record outside its authorized roots: $completionPath"
            }
            if ($completionPath.Equals($InstallDir, [System.StringComparison]::OrdinalIgnoreCase) -or
                $completionPath.Equals($CompletionRoot, [System.StringComparison]::OrdinalIgnoreCase)) {
                throw "Refusing manifest completion record that names an authorized directory itself: $completionPath"
            }
            $expectedHash = ([string]$record.sha256).ToLowerInvariant()
            if ($expectedHash -notmatch "^[a-f0-9]{64}$") {
                throw "Install manifest contains an invalid completion SHA-256 for: $completionPath"
            }
            if ($seenCompletionPaths.ContainsKey($completionPath)) {
                throw "Install manifest contains a duplicate completion path: $completionPath"
            }
            $seenCompletionPaths[$completionPath] = $true
            $CompletionRecords += [pscustomobject]@{ Path = $completionPath; SHA256 = $expectedHash; Legacy = $false }
        }
    } elseif ($manifestData.completion_files) {
        foreach ($file in @($manifestData.completion_files)) {
            if (-not $file) { continue }
            $completionPath = ConvertFrom-ManifestPath -Path ([string]$file) -Description "completion file"
            if (-not (Test-PathWithin -Path $completionPath -Root $InstallDir) -and
                -not ($CompletionDirExplicit -and (Test-PathWithin -Path $completionPath -Root $CompletionRoot))) {
                throw "Refusing legacy manifest completion file outside its authorized roots: $completionPath"
            }
            if ($completionPath.Equals($InstallDir, [System.StringComparison]::OrdinalIgnoreCase) -or
                $completionPath.Equals($CompletionRoot, [System.StringComparison]::OrdinalIgnoreCase)) {
                throw "Refusing legacy manifest completion file that names an authorized directory itself: $completionPath"
            }
            if ($seenCompletionPaths.ContainsKey($completionPath)) {
                throw "Install manifest contains a duplicate completion path: $completionPath"
            }
            $seenCompletionPaths[$completionPath] = $true
            $CompletionRecords += [pscustomobject]@{ Path = $completionPath; SHA256 = $null; Legacy = $true }
        }
    }

    if ($manifestData.completion_profile) {
        $CompletionProfile = ConvertFrom-ManifestPath -Path ([string]$manifestData.completion_profile) -Description "completion_profile"
        $authorizedProfile = if ($CompletionProfileExplicit) {
            ConvertTo-NormalizedPath -Path $CompletionProfilePath -Description "completion profile path"
        } elseif ($MachineScope) {
            ConvertTo-NormalizedPath -Path $PROFILE.AllUsersAllHosts -Description "all-users PowerShell profile"
        } else {
            ConvertTo-NormalizedPath -Path $PROFILE.CurrentUserAllHosts -Description "current-user PowerShell profile"
        }
        Assert-PathsEqual -Actual $CompletionProfile -Expected $authorizedProfile -Description "completion_profile"

        if ($manifestData.install_id) {
            $installID = [string]$manifestData.install_id
            if ($installID -notmatch "^[a-fA-F0-9]{32}$") {
                throw "Install manifest contains an invalid install_id."
            }
            $expectedStart = "# >>> docker-manager completion $installID >>>"
            $expectedEnd = "# <<< docker-manager completion $installID <<<"
            if ([string]$manifestData.completion_profile_start -ne $expectedStart -or
                [string]$manifestData.completion_profile_end -ne $expectedEnd) {
                throw "Install manifest completion profile markers do not match install_id."
            }
            $CompletionProfileStart = $expectedStart
            $CompletionProfileEnd = $expectedEnd
        } else {
            if (($manifestData.completion_profile_start -and [string]$manifestData.completion_profile_start -ne $CompletionProfileStart) -or
                ($manifestData.completion_profile_end -and [string]$manifestData.completion_profile_end -ne $CompletionProfileEnd)) {
                throw "Legacy install manifest contains unexpected completion profile markers."
            }
        }
    }

    if ($manifestProperties -contains "environment_variables") {
        $EnvironmentRecords = @($manifestData.environment_variables)
    } else {
        # Legacy manifests were written only after these three values were installed.
        $EnvironmentRecords = @(
            [pscustomobject]@{ name = "DM_HOME"; value = $manifestData.data_dir; previous_present = $false; previous_value = $null }
            [pscustomobject]@{ name = "DM_CONFIG"; value = $manifestData.config_file; previous_present = $false; previous_value = $null }
            [pscustomobject]@{ name = "DM_OUTPUT_DIR"; value = $manifestData.output_dir; previous_present = $false; previous_value = $null }
        )
    }
    $ConfigFile = Join-Path $ConfigDir "dm.yaml"
    $OutputDir = Join-Path $DataDir "images"
    if ($manifestData.config_file) {
        Assert-PathsEqual -Actual (ConvertFrom-ManifestPath -Path ([string]$manifestData.config_file) -Description "config_file") -Expected $ConfigFile -Description "config_file"
    }
    if ($manifestData.output_dir) {
        Assert-PathsEqual -Actual (ConvertFrom-ManifestPath -Path ([string]$manifestData.output_dir) -Description "output_dir") -Expected $OutputDir -Description "output_dir"
    }
    $expectedEnvironment = @{
        DM_HOME = $DataDir
        DM_CONFIG = $ConfigFile
        DM_OUTPUT_DIR = $OutputDir
    }
    $seenEnvironment = @{}
    foreach ($record in $EnvironmentRecords) {
        $name = [string]$record.name
        if (-not $expectedEnvironment.ContainsKey($name) -or $seenEnvironment.ContainsKey($name)) {
            throw "Install manifest contains an unsupported or duplicate environment variable: $name"
        }
        Assert-PathsEqual -Actual ([string]$record.value) -Expected ([string]$expectedEnvironment[$name]) -Description "environment variable $name"
        $seenEnvironment[$name] = $true
    }
    if ($null -ne $manifestData.path_entry_added) { $PathEntryAdded = [bool]$manifestData.path_entry_added }
}

Assert-NotFileSystemRoot -Path $InstallDir -Description "install directory"
Assert-NotFileSystemRoot -Path $BinDir -Description "binary directory"
Assert-NotFileSystemRoot -Path $ConfigDir -Description "config directory"
Assert-NotFileSystemRoot -Path $DataDir -Description "data directory"
Assert-NotFileSystemRoot -Path $CompletionRoot -Description "completion directory"
Assert-PathWithin -Path $InstalledBin -Root $BinDir -Description "installed binary" -RequireDescendant
Assert-PathWithin -Path $OldLibexecDir -Root $InstallDir -Description "legacy libexec directory" -RequireDescendant

foreach ($entry in @(
    @($InstallDir, "install directory"),
    @($BinDir, "binary directory"),
    @($ConfigDir, "config directory"),
    @($DataDir, "data directory"),
    @($CompletionRoot, "completion directory"),
    @($Manifest, "install manifest"),
    @($InstalledBin, "installed binary"),
    @($OldWrapper, "legacy wrapper"),
    @($OldInstalledBin, "legacy installed binary")
)) {
    Assert-NoUninstallReparsePoint -Path $entry[0] -Description $entry[1]
}
foreach ($record in $CompletionRecords) {
    Assert-NoUninstallReparsePoint -Path ([string]$record.Path) -Description "completion file"
}
if ($CompletionProfile) {
    Assert-NoUninstallReparsePoint -Path $CompletionProfile -Description "PowerShell completion profile"
}
Assert-NoUninstallReparseTree -Path $OldLibexecDir -Description "legacy libexec directory"
if ($Purge) {
    foreach ($directory in @($ConfigDir, $DataDir) | Select-Object -Unique) {
        Assert-NoUninstallReparseTree -Path $directory -Description "purge directory"
    }
}

Write-Host "Uninstalling docker-manager"

Invoke-Step {
    foreach ($file in @($InstalledBin, $OldWrapper, $OldInstalledBin)) {
        if ($file) { Remove-SafeUninstallFile -Path $file -Description "installed file" }
    }
    foreach ($record in $CompletionRecords) {
        $file = [string]$record.Path
        Assert-NoUninstallReparsePoint -Path $file -Description "completion file"
        if (-not (Test-Path -LiteralPath $file -PathType Leaf)) { continue }
        if (-not $record.Legacy) {
            Assert-NoUninstallReparsePoint -Path $file -Description "completion file"
            $actualHash = (Get-FileHash -LiteralPath $file -Algorithm SHA256).Hash.ToLowerInvariant()
            if ($actualHash -ne [string]$record.SHA256) {
                Write-Warning "Keeping modified completion file: $file"
                continue
            }
        }
        Remove-SafeUninstallFile -Path $file -Description "completion file"
    }
    Remove-SafeUninstallPath -Path $OldLibexecDir -Description "legacy libexec directory"
    Remove-EmptyParents -Path $BinDir -StopDir $InstallDir
    foreach ($record in $CompletionRecords) {
        $file = [string]$record.Path
        $stopDir = if ($CompletionDirExplicit -and (Test-PathWithin -Path $file -Root $CompletionRoot)) {
            $CompletionRoot
        } else {
            $InstallDir
        }
        Remove-EmptyParents -Path (Split-Path -Parent $file) -StopDir $stopDir
    }
    Remove-EmptyParents -Path $OldLibexecDir -StopDir $InstallDir
} "remove installed files"

if ($CompletionProfile) {
    Assert-NoUninstallReparsePoint -Path $CompletionProfile -Description "PowerShell completion profile"
    if (Test-Path -LiteralPath $CompletionProfile -PathType Leaf) {
        Invoke-Step {
            Assert-NoUninstallReparsePoint -Path $CompletionProfile -Description "PowerShell completion profile"
            $existing = Get-Content -LiteralPath $CompletionProfile -Raw
            $pattern = "(?s)" + [regex]::Escape($CompletionProfileStart) + ".*?" + [regex]::Escape($CompletionProfileEnd) + "\r?\n?"
            $clean = [regex]::Replace($existing, $pattern, "")
            Assert-NoUninstallReparsePoint -Path $CompletionProfile -Description "PowerShell completion profile"
            Set-Content -LiteralPath $CompletionProfile -Value $clean -Encoding UTF8
            Assert-NoUninstallReparsePoint -Path $CompletionProfile -Description "PowerShell completion profile"
        } "remove PowerShell completion block from $CompletionProfile"
    }
}

Invoke-Step {
    foreach ($record in $EnvironmentRecords) {
        if (-not $record.name) { continue }
        $current = [Environment]::GetEnvironmentVariable([string]$record.name, $Scope)
        if ($current -ne [string]$record.value) {
            Write-Host "Keeping $($record.name): current value is no longer owned by this installation."
            continue
        }
        $restore = Get-EnvironmentRestoreRecord -Record $record -CurrentManifest $Manifest
        if ($restore.Present) {
            [Environment]::SetEnvironmentVariable([string]$record.name, [string]$restore.Value, $Scope)
        } else {
            [Environment]::SetEnvironmentVariable(
                [string]$record.name,
                [System.Management.Automation.Language.NullString]::Value,
                $Scope
            )
        }
    }
    if ($PathEntryAdded) {
        $oldPath = [Environment]::GetEnvironmentVariable("PATH", $Scope)
        if ($oldPath) {
            $newPath = (($oldPath -split ';') | Where-Object { $_ -and ($_ -ne $BinDir) }) -join ';'
            [Environment]::SetEnvironmentVariable("PATH", $newPath, $Scope)
        }
    }
} "restore owned environment variables"

if (-not $Purge) {
    Invoke-Step {
        Remove-SafeUninstallFile -Path $Manifest -Description "install manifest"
    } "remove install ownership manifest"
}

if ($Purge) {
    Invoke-Step {
        foreach ($directory in @($ConfigDir, $DataDir) | Select-Object -Unique) {
            Remove-SafeUninstallPath -Path $directory -Description "purge directory"
        }
        Remove-EmptyParents -Path $InstallDir -StopDir $InstallDir
    } "remove config and data"
} else {
    Write-Host "Keeping config and data. Use -Purge to remove:"
    Write-Host "  $ConfigDir"
    Write-Host "  $DataDir"
}

Write-Host "Uninstall complete."
