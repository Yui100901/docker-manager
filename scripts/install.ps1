param(
    [string]$InstallDir,
    [string]$BinDir,
    [string]$ConfigDir,
    [string]$DataDir,
    [string]$Binary,
    [string[]]$Completion,
    [string]$CompletionDir,
    [switch]$Build,
    [switch]$OverwriteConfig,
    [switch]$NoPathUpdate,
    [switch]$NoEnvironment,
    [switch]$NoCompletion,
    [switch]$NoCompletionProfile,
    [switch]$MachineScope,
    [switch]$DryRun
)

$ErrorActionPreference = "Stop"
$RootDir = (Resolve-Path -LiteralPath (Join-Path $PSScriptRoot "..")).Path
$Scope = if ($MachineScope) { "Machine" } else { "User" }

function ConvertTo-InstallPath {
    param(
        [string]$Path,
        [string]$Description
    )
    if ([string]::IsNullOrWhiteSpace($Path)) { throw "Missing $Description." }
    try {
        $full = [System.IO.Path]::GetFullPath($Path)
        $root = [System.IO.Path]::GetPathRoot($full)
        if ($full.Length -le $root.Length) { return $root }
        return $full.TrimEnd(
            [System.IO.Path]::DirectorySeparatorChar,
            [System.IO.Path]::AltDirectorySeparatorChar
        )
    } catch {
        throw "Invalid ${Description}: $Path"
    }
}

function Assert-NotFileSystemRoot {
    param(
        [string]$Path,
        [string]$Description
    )
    $root = [System.IO.Path]::GetPathRoot($Path)
    if ($Path.Equals($root, [System.StringComparison]::OrdinalIgnoreCase)) {
        throw "Refusing to use a filesystem root as ${Description}: $Path"
    }
}

function Get-InstallPathItem {
    param([string]$Path)
    try {
        return Get-Item -LiteralPath $Path -Force -ErrorAction Stop
    } catch [System.Management.Automation.ItemNotFoundException] {
        return $null
    } catch {
        throw "Cannot safely inspect install path '$Path': $($_.Exception.Message)"
    }
}

function Test-InstallReparsePoint {
    param([System.IO.FileSystemInfo]$Item)
    return [bool](($Item.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0)
}

function Assert-NoInstallReparsePoint {
    param(
        [string]$Path,
        [string]$Description = "install path"
    )
    $full = ConvertTo-InstallPath -Path $Path -Description $Description
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
        $item = Get-InstallPathItem -Path $paths[$i]
        if ($null -eq $item) { break }
        if (Test-InstallReparsePoint -Item $item) {
            throw "Refusing $Description through reparse point: $($paths[$i])"
        }
        if ($i -lt $paths.Count - 1 -and -not $item.PSIsContainer) {
            throw "Refusing $Description through non-directory path component: $($paths[$i])"
        }
    }
}

function Assert-NoInstallReparseTree {
    param(
        [string]$Path,
        [string]$Description = "install tree"
    )
    $full = ConvertTo-InstallPath -Path $Path -Description $Description
    Assert-NoInstallReparsePoint -Path $full -Description $Description
    $rootItem = Get-InstallPathItem -Path $full
    if ($null -eq $rootItem -or -not $rootItem.PSIsContainer) { return }
    $stack = New-Object 'System.Collections.Generic.Stack[string]'
    $stack.Push($full)
    while ($stack.Count -gt 0) {
        $current = $stack.Pop()
        Assert-NoInstallReparsePoint -Path $current -Description $Description
        $currentItem = Get-InstallPathItem -Path $current
        if ($null -eq $currentItem) { throw "$Description changed while it was being inspected: $current" }
        if (Test-InstallReparsePoint -Item $currentItem) { throw "Refusing reparse point in ${Description}: $current" }
        if (-not $currentItem.PSIsContainer) { throw "$Description changed from a directory: $current" }
        foreach ($child in @(Get-ChildItem -LiteralPath $current -Force -ErrorAction Stop)) {
            if (Test-InstallReparsePoint -Item $child) {
                throw "Refusing reparse point in ${Description}: $($child.FullName)"
            }
            if ($child.PSIsContainer) { $stack.Push($child.FullName) }
        }
    }
}

function Remove-SafeInstallPath {
    param(
        [string]$Path,
        [string]$Description = "install path"
    )
    $full = ConvertTo-InstallPath -Path $Path -Description $Description
    Assert-NoInstallReparseTree -Path $full -Description $Description
    $rootItem = Get-InstallPathItem -Path $full
    if ($null -eq $rootItem) { return }
    if (-not $rootItem.PSIsContainer) {
        Assert-NoInstallReparsePoint -Path $full -Description $Description
        $rootItem = Get-InstallPathItem -Path $full
        if ($null -ne $rootItem) {
            if ($rootItem.PSIsContainer -or (Test-InstallReparsePoint -Item $rootItem)) {
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
        Assert-NoInstallReparsePoint -Path $current -Description $Description
        $currentItem = Get-InstallPathItem -Path $current
        if ($null -eq $currentItem) { continue }
        if (-not $currentItem.PSIsContainer -or (Test-InstallReparsePoint -Item $currentItem)) {
            throw "$Description changed during deletion preflight: $current"
        }
        foreach ($child in @(Get-ChildItem -LiteralPath $current -Force -ErrorAction Stop)) {
            if (Test-InstallReparsePoint -Item $child) {
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
        Assert-NoInstallReparsePoint -Path $file -Description $Description
        $item = Get-InstallPathItem -Path $file
        if ($null -eq $item) { continue }
        if ($item.PSIsContainer -or (Test-InstallReparsePoint -Item $item)) {
            throw "$Description changed before file deletion: $file"
        }
        Remove-Item -LiteralPath $file -Force -ErrorAction Stop
    }
    foreach ($directory in @($directories | Sort-Object { $_.Length } -Descending)) {
        Assert-NoInstallReparsePoint -Path $directory -Description $Description
        $item = Get-InstallPathItem -Path $directory
        if ($null -eq $item) { continue }
        if (-not $item.PSIsContainer -or (Test-InstallReparsePoint -Item $item)) {
            throw "$Description changed before directory deletion: $directory"
        }
        [System.IO.Directory]::Delete($directory, $false)
    }
}

function Copy-SafeInstallPath {
    param(
        [string]$Source,
        [string]$Destination,
        [string]$Description = "install snapshot"
    )
    $sourceFull = ConvertTo-InstallPath -Path $Source -Description "$Description source"
    $destinationFull = ConvertTo-InstallPath -Path $Destination -Description "$Description destination"
    Assert-NoInstallReparseTree -Path $sourceFull -Description "$Description source"
    Assert-NoInstallReparsePoint -Path $destinationFull -Description "$Description destination"
    if ($null -ne (Get-InstallPathItem -Path $destinationFull)) {
        throw "$Description destination already exists: $destinationFull"
    }
    $sourceItem = Get-InstallPathItem -Path $sourceFull
    if ($null -eq $sourceItem) { throw "$Description source disappeared: $sourceFull" }
    if (Test-InstallReparsePoint -Item $sourceItem) {
        throw "Refusing reparse point in ${Description}: $sourceFull"
    }
    if (-not $sourceItem.PSIsContainer) {
        Assert-NoInstallReparsePoint -Path $sourceFull -Description "$Description source"
        Assert-NoInstallReparsePoint -Path $destinationFull -Description "$Description destination"
        Copy-Item -LiteralPath $sourceFull -Destination $destinationFull -Force -ErrorAction Stop
        Assert-NoInstallReparsePoint -Path $destinationFull -Description "$Description destination"
        return
    }

    [System.IO.Directory]::CreateDirectory($destinationFull) | Out-Null
    Assert-NoInstallReparsePoint -Path $destinationFull -Description "$Description destination"
    $stack = New-Object 'System.Collections.Generic.Stack[object]'
    $stack.Push([pscustomobject]@{ Source = $sourceFull; Destination = $destinationFull })
    while ($stack.Count -gt 0) {
        $entry = $stack.Pop()
        Assert-NoInstallReparsePoint -Path $entry.Source -Description "$Description source"
        Assert-NoInstallReparsePoint -Path $entry.Destination -Description "$Description destination"
        foreach ($child in @(Get-ChildItem -LiteralPath $entry.Source -Force -ErrorAction Stop)) {
            if (Test-InstallReparsePoint -Item $child) {
                throw "Refusing reparse point in ${Description}: $($child.FullName)"
            }
            $childDestination = Join-Path $entry.Destination $child.Name
            Assert-NoInstallReparsePoint -Path $child.FullName -Description "$Description source"
            Assert-NoInstallReparsePoint -Path $childDestination -Description "$Description destination"
            if ($child.PSIsContainer) {
                [System.IO.Directory]::CreateDirectory($childDestination) | Out-Null
                Assert-NoInstallReparsePoint -Path $childDestination -Description "$Description destination"
                $stack.Push([pscustomobject]@{ Source = $child.FullName; Destination = $childDestination })
            } else {
                Assert-NoInstallReparsePoint -Path $child.FullName -Description "$Description source"
                Copy-Item -LiteralPath $child.FullName -Destination $childDestination -Force -ErrorAction Stop
                Assert-NoInstallReparsePoint -Path $childDestination -Description "$Description destination"
            }
        }
    }
}

if (-not $InstallDir) {
    if ($MachineScope) {
        $InstallDir = Join-Path ${env:ProgramFiles} "docker-manager"
    } else {
        $InstallDir = Join-Path $env:LOCALAPPDATA "docker-manager"
    }
}
if (-not $BinDir) { $BinDir = Join-Path $InstallDir "bin" }
if (-not $ConfigDir) {
    if ($MachineScope) {
        $ConfigDir = Join-Path $env:ProgramData "docker-manager"
    } else {
        $ConfigDir = Join-Path $env:APPDATA "docker-manager"
    }
}
if (-not $DataDir) { $DataDir = Join-Path $InstallDir "data" }

$InstallDir = ConvertTo-InstallPath -Path $InstallDir -Description "install directory"
$BinDir = ConvertTo-InstallPath -Path $BinDir -Description "binary directory"
$ConfigDir = ConvertTo-InstallPath -Path $ConfigDir -Description "config directory"
$DataDir = ConvertTo-InstallPath -Path $DataDir -Description "data directory"
foreach ($entry in @(
    @($InstallDir, "install directory"),
    @($BinDir, "binary directory"),
    @($ConfigDir, "config directory"),
    @($DataDir, "data directory")
)) {
    Assert-NotFileSystemRoot -Path $entry[0] -Description $entry[1]
    Assert-NoInstallReparsePoint -Path $entry[0] -Description $entry[1]
}

$ConfigFile = Join-Path $ConfigDir "dm.yaml"
$OutputDir = Join-Path $DataDir "images"
$YamlOutputDir = $OutputDir.Replace("'", "''")
$InstalledBin = Join-Path $BinDir "dm.exe"
$OldWrapper = Join-Path $BinDir "dm.cmd"
$OldLibexecDir = Join-Path $InstallDir "lib"
$OldInstalledBin = Join-Path $OldLibexecDir "dm-bin.exe"
$Manifest = Join-Path $ConfigDir "install.json"
$CompletionBaseDir = ConvertTo-InstallPath -Path $(if ($CompletionDir) { $CompletionDir } else { Join-Path $InstallDir "completions" }) -Description "completion directory"
Assert-NotFileSystemRoot -Path $CompletionBaseDir -Description "completion directory"
Assert-NoInstallReparsePoint -Path $CompletionBaseDir -Description "completion directory"
$CompletionFiles = @()
$CompletionRecords = @()
$CompletionProfile = if ($MachineScope) { $PROFILE.AllUsersAllHosts } else { $PROFILE.CurrentUserAllHosts }
$PreviousManifest = $null
Assert-NoInstallReparsePoint -Path $Manifest -Description "install manifest"
if (Test-Path -LiteralPath $Manifest -PathType Leaf) {
    try {
        Assert-NoInstallReparsePoint -Path $Manifest -Description "install manifest"
        $previousManifestFile = Get-Item -LiteralPath $Manifest -Force
        if ($previousManifestFile.Length -gt 1MB) {
            throw "Previous install manifest exceeds the 1 MiB safety limit."
        }
        Assert-NoInstallReparsePoint -Path $Manifest -Description "install manifest"
        $PreviousManifest = Get-Content -LiteralPath $Manifest -Raw | ConvertFrom-Json
    } catch {
        throw "Cannot safely replace previous install manifest '$Manifest': $($_.Exception.Message)"
    }
}
$InstallID = if ($PreviousManifest -and $PreviousManifest.install_id) {
    $candidateInstallID = [string]$PreviousManifest.install_id
    if ($candidateInstallID -notmatch "^[a-fA-F0-9]{32}$") {
        throw "Previous install manifest contains an invalid install_id."
    }
    $candidateInstallID
} else {
    [guid]::NewGuid().ToString("N")
}
$CompletionProfileStart = "# >>> docker-manager completion $InstallID >>>"
$CompletionProfileEnd = "# <<< docker-manager completion $InstallID <<<"
$CurrentOwnerManifest = $null
$CurrentOwnerManifestPath = $null
$currentConfigValue = [Environment]::GetEnvironmentVariable("DM_CONFIG", $Scope)
if ($currentConfigValue) {
    try {
        $currentConfigPath = ConvertTo-InstallPath -Path $currentConfigValue -Description "current DM_CONFIG path"
        $ownerCandidate = Join-Path ([System.IO.Path]::GetDirectoryName($currentConfigPath)) "install.json"
        Assert-NoInstallReparsePoint -Path $ownerCandidate -Description "environment owner manifest"
        if ($ownerCandidate -ne $Manifest -and (Test-Path -LiteralPath $ownerCandidate -PathType Leaf)) {
            Assert-NoInstallReparsePoint -Path $ownerCandidate -Description "environment owner manifest"
            $ownerManifestFile = Get-Item -LiteralPath $ownerCandidate -Force
            if ($ownerManifestFile.Length -gt 1MB) {
                throw "Environment owner manifest exceeds the 1 MiB safety limit."
            }
            Assert-NoInstallReparsePoint -Path $ownerCandidate -Description "environment owner manifest"
            $CurrentOwnerManifest = Get-Content -LiteralPath $ownerCandidate -Raw | ConvertFrom-Json
            $CurrentOwnerManifestPath = $ownerCandidate
        }
    } catch {
        Write-Warning "Ignoring unreadable environment owner manifest for DM_CONFIG '$currentConfigValue': $($_.Exception.Message)"
    }
}
$EnvironmentRecords = @()
$PathEntryAdded = $false
$PathAddedThisRun = $false
$TransactionRoot = $null
$TransactionRecords = New-Object System.Collections.Generic.List[object]
$TransactionCreatedDirectories = New-Object System.Collections.Generic.List[string]

function Invoke-Step {
    param([scriptblock]$Action, [string]$Text)
    if ($DryRun) {
        Write-Host "DRY-RUN: $Text"
    } else {
        & $Action
    }
}

function Initialize-InstallTransaction {
    if ($DryRun -or $TransactionRoot) { return }
    $tempRoot = ConvertTo-InstallPath -Path ([System.IO.Path]::GetTempPath()) -Description "transaction temporary directory"
    Assert-NoInstallReparsePoint -Path $tempRoot -Description "transaction temporary directory"
    $script:TransactionRoot = Join-Path $tempRoot ("dm-install-transaction-" + [guid]::NewGuid().ToString("N"))
    Assert-NoInstallReparsePoint -Path $script:TransactionRoot -Description "install transaction directory"
    [System.IO.Directory]::CreateDirectory($script:TransactionRoot) | Out-Null
    Assert-NoInstallReparsePoint -Path $script:TransactionRoot -Description "install transaction directory"
    $transactionItem = Get-InstallPathItem -Path $script:TransactionRoot
    if ($null -eq $transactionItem -or -not $transactionItem.PSIsContainer) {
        throw "Install transaction directory was not created safely: $script:TransactionRoot"
    }
}

function Add-InstallSnapshot {
    param([string]$Path)
    if ($DryRun -or -not $Path) { return }
    Initialize-InstallTransaction
    $full = ConvertTo-InstallPath -Path $Path -Description "install snapshot path"
    Assert-NoInstallReparsePoint -Path $full -Description "install snapshot path"
    foreach ($record in $TransactionRecords) {
        if ($record.Path -eq $full) { return }
    }
    $item = Get-InstallPathItem -Path $full
    $exists = $null -ne $item
    $backup = Join-Path $TransactionRoot ("snapshot-" + $TransactionRecords.Count)
    $isDirectory = $false
    if ($exists) {
        $isDirectory = $item.PSIsContainer
        Copy-SafeInstallPath -Source $full -Destination $backup -Description "install snapshot"
    }
    $TransactionRecords.Add([pscustomobject]@{
        Path = $full
        Exists = $exists
        IsDirectory = $isDirectory
        Backup = $backup
    })
}

function Ensure-InstallDirectory {
    param([string]$Path)
    if ($DryRun) { return }
    $full = ConvertTo-InstallPath -Path $Path -Description "install directory"
    Assert-NoInstallReparsePoint -Path $full -Description "install directory"
    $root = [System.IO.Path]::GetPathRoot($full)
    $current = $root
    $relative = $full.Substring($root.Length)
    $separators = [char[]]@(
        [System.IO.Path]::DirectorySeparatorChar,
        [System.IO.Path]::AltDirectorySeparatorChar
    )
    foreach ($segment in @($relative.Split($separators, [System.StringSplitOptions]::RemoveEmptyEntries))) {
        $current = Join-Path $current $segment
        Assert-NoInstallReparsePoint -Path $current -Description "install directory"
        $item = Get-InstallPathItem -Path $current
        if ($null -eq $item) {
            $parent = [System.IO.Path]::GetDirectoryName($current)
            Assert-NoInstallReparsePoint -Path $parent -Description "install directory parent"
            [System.IO.Directory]::CreateDirectory($current) | Out-Null
            Assert-NoInstallReparsePoint -Path $current -Description "install directory"
            $item = Get-InstallPathItem -Path $current
            if ($null -eq $item -or -not $item.PSIsContainer) {
                throw "Install directory was not created safely: $current"
            }
            if (-not $TransactionCreatedDirectories.Contains($current)) {
                $TransactionCreatedDirectories.Add($current)
            }
        } elseif (-not $item.PSIsContainer) {
            throw "Install directory path is a file: $current"
        }
    }
}

function Restore-InstallTransaction {
    if ($DryRun -or -not $TransactionRoot) { return }
    for ($i = $TransactionRecords.Count - 1; $i -ge 0; $i--) {
        $record = $TransactionRecords[$i]
        Remove-SafeInstallPath -Path $record.Path -Description "install rollback target"
        if ($record.Exists) {
            $parent = [System.IO.Path]::GetDirectoryName($record.Path)
            if ($parent) { Ensure-InstallDirectory -Path $parent }
            Copy-SafeInstallPath -Source $record.Backup -Destination $record.Path -Description "install rollback snapshot"
        }
    }
    for ($i = $TransactionCreatedDirectories.Count - 1; $i -ge 0; $i--) {
        $path = $TransactionCreatedDirectories[$i]
        Assert-NoInstallReparsePoint -Path $path -Description "install rollback directory"
        $item = Get-InstallPathItem -Path $path
        if ($null -ne $item -and $item.PSIsContainer -and -not (Get-ChildItem -LiteralPath $path -Force -ErrorAction Stop | Select-Object -First 1)) {
            Assert-NoInstallReparsePoint -Path $path -Description "install rollback directory"
            [System.IO.Directory]::Delete($path, $false)
        }
    }
}

function Remove-InstallTransaction {
    if ($TransactionRoot) {
        Remove-SafeInstallPath -Path $TransactionRoot -Description "install transaction directory"
    }
    $script:TransactionRoot = $null
}

function Resolve-DmBinary {
    if ($Binary) {
        Assert-NoInstallReparsePoint -Path $Binary -Description "dm binary"
        $resolved = Resolve-Path -LiteralPath $Binary -ErrorAction Stop
        Assert-NoInstallReparsePoint -Path $resolved.Path -Description "dm binary"
        if (-not (Test-Path -LiteralPath $resolved.Path -PathType Leaf)) {
            throw "dm binary is not a file: $Binary"
        }
        return $resolved.Path
    }
    $candidate = Join-Path $RootDir "dm.exe"
    Assert-NoInstallReparsePoint -Path $candidate -Description "dm binary"
    if (Test-Path -LiteralPath $candidate -PathType Leaf) { return (Resolve-Path -LiteralPath $candidate).Path }
    $candidate = Join-Path $RootDir "bin/dev/dm.exe"
    Assert-NoInstallReparsePoint -Path $candidate -Description "dm binary"
    if (Test-Path -LiteralPath $candidate -PathType Leaf) { return (Resolve-Path -LiteralPath $candidate).Path }
    if ($Build) {
        $built = Join-Path $RootDir "bin/install/dm.exe"
        Invoke-Step {
            Ensure-InstallDirectory -Path ([System.IO.Path]::GetDirectoryName($built))
            Assert-NoInstallReparsePoint -Path $built -Description "built dm binary"
            $version = if ($env:VERSION) { $env:VERSION } else { "dev" }
            $commit = if ($env:COMMIT) { $env:COMMIT } else { (& git -C $RootDir rev-parse --short HEAD 2>$null).Trim() }
            if (-not $commit) { $commit = "unknown" }
            $buildDate = if ($env:BUILD_DATE) { $env:BUILD_DATE } else { (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ") }
            $ldflags = "-s -w -X docker-manager/internal/version.version=$version -X docker-manager/internal/version.commit=$commit -X docker-manager/internal/version.buildDate=$buildDate"
            Push-Location $RootDir
            $oldCGO = $env:CGO_ENABLED
            try {
                $env:CGO_ENABLED = "0"
                go build -trimpath -ldflags $ldflags -o $built .
            } finally {
                $env:CGO_ENABLED = $oldCGO
                Pop-Location
            }
            Assert-NoInstallReparsePoint -Path $built -Description "built dm binary"
        } "build $built"
        return $built
    }
    throw "No dm binary found. Pass -Binary PATH or -Build."
}

function Get-PreviousEnvironmentRecord {
    param(
        [string]$Name,
        [AllowNull()][string]$CurrentValue
    )
    if (-not $PreviousManifest -or -not $PreviousManifest.environment_variables) {
        return $null
    }
    foreach ($record in @($PreviousManifest.environment_variables)) {
        if ($record.name -eq $Name -and [string]$record.value -eq $CurrentValue) {
            return $record
        }
    }
    return $null
}

function Set-OwnedEnvironmentVariable {
    param(
        [string]$Name,
        [string]$Value
    )
    $current = [Environment]::GetEnvironmentVariable($Name, $Scope)
    $previous = Get-PreviousEnvironmentRecord -Name $Name -CurrentValue $current
	$previousOwners = @()
	$baselinePresent = $null -ne $current
	$baselineValue = $current
    $previousOwnerManifest = $null
    $previousOwnerPreviousPresent = $false
    $previousOwnerPreviousValue = $null
    if ($previous) {
        $previousPresent = [bool]$previous.previous_present
        $previousValue = if ($previousPresent) { [string]$previous.previous_value } else { $null }
		if ($previous.PSObject.Properties.Name -contains "previous_owners") {
			$previousOwners = @($previous.previous_owners)
		} elseif ($previous.previous_owner_manifest) {
			$previousOwners = @([ordered]@{
				manifest = [string]$previous.previous_owner_manifest
				present = [bool]$previous.previous_present
				value = if ([bool]$previous.previous_present) { [string]$previous.previous_value } else { $null }
			})
		}
		if ($previous.PSObject.Properties.Name -contains "baseline_present") {
			$baselinePresent = [bool]$previous.baseline_present
			$baselineValue = if ($baselinePresent) { [string]$previous.baseline_value } else { $null }
		} elseif ($previous.previous_owner_manifest) {
			$baselinePresent = [bool]$previous.previous_owner_previous_present
			$baselineValue = if ($baselinePresent) { [string]$previous.previous_owner_previous_value } else { $null }
		} else {
			$baselinePresent = [bool]$previous.previous_present
			$baselineValue = if ($baselinePresent) { [string]$previous.previous_value } else { $null }
		}
        if ($previous.previous_owner_manifest) {
            $previousOwnerManifest = [string]$previous.previous_owner_manifest
            $previousOwnerPreviousPresent = [bool]$previous.previous_owner_previous_present
            $previousOwnerPreviousValue = if ($previousOwnerPreviousPresent) { [string]$previous.previous_owner_previous_value } else { $null }
        }
    } else {
        $previousPresent = $null -ne $current
        $previousValue = $current
        if ($CurrentOwnerManifest -and $CurrentOwnerManifest.environment_variables) {
            foreach ($ownerRecord in @($CurrentOwnerManifest.environment_variables)) {
                if ($ownerRecord.name -eq $Name -and [string]$ownerRecord.value -eq $current) {
                    $previousOwnerManifest = $CurrentOwnerManifestPath
					$previousOwners += [ordered]@{
						manifest = $CurrentOwnerManifestPath
						present = $true
						value = $current
					}
					if ($ownerRecord.PSObject.Properties.Name -contains "previous_owners") {
						$previousOwners += @($ownerRecord.previous_owners)
					} elseif ($ownerRecord.previous_owner_manifest) {
						$previousOwners += [ordered]@{
							manifest = [string]$ownerRecord.previous_owner_manifest
							present = [bool]$ownerRecord.previous_present
							value = if ([bool]$ownerRecord.previous_present) { [string]$ownerRecord.previous_value } else { $null }
						}
					}
					if ($ownerRecord.PSObject.Properties.Name -contains "baseline_present") {
						$baselinePresent = [bool]$ownerRecord.baseline_present
						$baselineValue = if ($baselinePresent) { [string]$ownerRecord.baseline_value } else { $null }
					} elseif ($ownerRecord.previous_owner_manifest) {
						$baselinePresent = [bool]$ownerRecord.previous_owner_previous_present
						$baselineValue = if ($baselinePresent) { [string]$ownerRecord.previous_owner_previous_value } else { $null }
					} else {
						$baselinePresent = [bool]$ownerRecord.previous_present
						$baselineValue = if ($baselinePresent) { [string]$ownerRecord.previous_value } else { $null }
					}
					$previousOwnerPreviousPresent = $baselinePresent
					$previousOwnerPreviousValue = $baselineValue
                    break
                }
            }
        }
    }
    [Environment]::SetEnvironmentVariable($Name, $Value, $Scope)
    return [ordered]@{
        name = $Name
        value = $Value
        previous_present = $previousPresent
        previous_value = $previousValue
        previous_owner_manifest = $previousOwnerManifest
        previous_owner_previous_present = $previousOwnerPreviousPresent
        previous_owner_previous_value = $previousOwnerPreviousValue
		previous_owners = @($previousOwners)
		baseline_present = $baselinePresent
		baseline_value = $baselineValue
        rollback_present = $null -ne $current
        rollback_value = $current
    }
}

function Undo-EnvironmentChanges {
    foreach ($record in @($EnvironmentRecords)) {
        $current = [Environment]::GetEnvironmentVariable([string]$record.name, $Scope)
        if ($current -eq [string]$record.value) {
            if ([bool]$record.rollback_present) {
                [Environment]::SetEnvironmentVariable([string]$record.name, [string]$record.rollback_value, $Scope)
            } else {
                [Environment]::SetEnvironmentVariable(
                    [string]$record.name,
                    [System.Management.Automation.Language.NullString]::Value,
                    $Scope
                )
            }
        }
    }
    if ($PathAddedThisRun) {
        $currentPath = [Environment]::GetEnvironmentVariable("PATH", $Scope)
        if ($currentPath) {
            $restoredPath = (($currentPath -split ';') | Where-Object { $_ -and ($_ -ne $BinDir) }) -join ';'
            [Environment]::SetEnvironmentVariable("PATH", $restoredPath, $Scope)
        }
    }
}

function Get-CompletionShells {
    if ($NoCompletion) { return @() }
    if (-not $Completion -or $Completion.Count -eq 0) {
        return @("powershell")
    }
    $items = @()
    foreach ($entry in $Completion) {
        foreach ($part in ($entry -split ',')) {
            $value = $part.Trim().ToLowerInvariant()
            if (-not $value) { continue }
            if ($value -eq "all") {
                $items += "powershell"
            } elseif ($value -eq "powershell" -or $value -eq "pwsh") {
                $items += "powershell"
            } else {
                throw "Unsupported completion shell on Windows install.ps1: $part. Use PowerShell."
            }
        }
    }
    return @($items | Select-Object -Unique)
}

function Install-Completions {
    $shells = Get-CompletionShells
    foreach ($shell in $shells) {
        $target = Join-Path $CompletionBaseDir "dm-completion.ps1"
        Invoke-Step {
			Ensure-InstallDirectory (Split-Path -Parent $target)
			Assert-NoInstallReparsePoint -Path $InstalledBin -Description "installed dm binary"
			Assert-NoInstallReparsePoint -Path $target -Description "PowerShell completion file"
			$content = @(& $InstalledBin completion powershell)
			if ($LASTEXITCODE -ne 0) { throw "dm completion powershell failed with exit code $LASTEXITCODE" }
			Assert-NoInstallReparsePoint -Path $target -Description "PowerShell completion file"
			$content | Set-Content -LiteralPath $target -Encoding UTF8
			Assert-NoInstallReparsePoint -Path $target -Description "PowerShell completion file"
        } "write PowerShell completion $target"
        $script:CompletionFiles += $target
		if (-not $DryRun) {
			Assert-NoInstallReparsePoint -Path $target -Description "PowerShell completion file"
			$script:CompletionRecords += [ordered]@{
				path = $target
				sha256 = (Get-FileHash -LiteralPath $target -Algorithm SHA256).Hash.ToLowerInvariant()
			}
		}
    }

    if (($shells -contains "powershell") -and -not $NoCompletionProfile) {
        Invoke-Step {
            $profileDir = Split-Path -Parent $CompletionProfile
            if ($profileDir) {
				Ensure-InstallDirectory $profileDir
            }
			Assert-NoInstallReparsePoint -Path $CompletionProfile -Description "PowerShell completion profile"
			if (-not (Test-Path -LiteralPath $CompletionProfile)) {
				Assert-NoInstallReparsePoint -Path $CompletionProfile -Description "PowerShell completion profile"
				[System.IO.File]::WriteAllBytes($CompletionProfile, [byte[]]@())
				Assert-NoInstallReparsePoint -Path $CompletionProfile -Description "PowerShell completion profile"
            }
			Assert-NoInstallReparsePoint -Path $CompletionProfile -Description "PowerShell completion profile"
			$existing = [string](Get-Content -LiteralPath $CompletionProfile -Raw)
			$clean = $existing
			$markerPairs = @([pscustomobject]@{ Start = $CompletionProfileStart; End = $CompletionProfileEnd })
			if ($PreviousManifest -and $PreviousManifest.completion_profile_start -and $PreviousManifest.completion_profile_end) {
				$markerPairs += [pscustomobject]@{ Start = [string]$PreviousManifest.completion_profile_start; End = [string]$PreviousManifest.completion_profile_end }
			}
			foreach ($pair in $markerPairs) {
				$pattern = "(?s)" + [regex]::Escape($pair.Start) + ".*?" + [regex]::Escape($pair.End) + "\r?\n?"
				$clean = [regex]::Replace($clean, $pattern, "")
			}
            $completionFile = $CompletionFiles[0]
            $completionLiteral = $completionFile.Replace("'", "''")
            $block = @"
$CompletionProfileStart
. '$completionLiteral'
$CompletionProfileEnd
"@
			Assert-NoInstallReparsePoint -Path $CompletionProfile -Description "PowerShell completion profile"
			Set-Content -LiteralPath $CompletionProfile -Value ($clean.TrimEnd() + [Environment]::NewLine + $block + [Environment]::NewLine) -Encoding UTF8
			Assert-NoInstallReparsePoint -Path $CompletionProfile -Description "PowerShell completion profile"
        } "update PowerShell profile $CompletionProfile"
    }
}

$SourceBin = Resolve-DmBinary
Assert-NoInstallReparsePoint -Path $SourceBin -Description "dm binary"

Write-Host "Installing docker-manager"
Write-Host "  binary:  $InstalledBin"
Write-Host "  config:  $ConfigFile"
Write-Host "  data:    $DataDir"

$configText = @"
# docker-manager config generated by install.ps1
proxy:
os: linux
arch: amd64
output_dir: '$YamlOutputDir'
verbose: false
quiet: false
log_json: false
"@
try {
	Initialize-InstallTransaction
	$plannedShells = @(Get-CompletionShells)
	foreach ($path in @($InstalledBin, $OldWrapper, $OldLibexecDir, $ConfigFile, $Manifest)) {
		Add-InstallSnapshot $path
	}
	if ($plannedShells -contains "powershell") {
		Add-InstallSnapshot (Join-Path $CompletionBaseDir "dm-completion.ps1")
		if (-not $NoCompletionProfile) { Add-InstallSnapshot $CompletionProfile }
	}

	Invoke-Step {
		Ensure-InstallDirectory $BinDir
		Ensure-InstallDirectory $ConfigDir
		Ensure-InstallDirectory $OutputDir
		Assert-NoInstallReparsePoint -Path $SourceBin -Description "dm binary"
		Assert-NoInstallReparsePoint -Path $InstalledBin -Description "installed dm binary"
		Copy-Item -LiteralPath $SourceBin -Destination $InstalledBin -Force
		Assert-NoInstallReparsePoint -Path $InstalledBin -Description "installed dm binary"
		foreach ($legacyFile in @($OldWrapper, $OldInstalledBin)) {
			Remove-SafeInstallPath -Path $legacyFile -Description "legacy install file"
		}
		Remove-SafeInstallPath -Path $OldLibexecDir -Description "legacy libexec directory"
	} "create directories and copy binary"

	Install-Completions

	Assert-NoInstallReparsePoint -Path $ConfigFile -Description "configuration file"
	if ($OverwriteConfig -or -not (Test-Path -LiteralPath $ConfigFile)) {
		Invoke-Step {
			Assert-NoInstallReparsePoint -Path $ConfigFile -Description "configuration file"
			Set-Content -LiteralPath $ConfigFile -Value $configText -Encoding UTF8
			Assert-NoInstallReparsePoint -Path $ConfigFile -Description "configuration file"
		} "write $ConfigFile"
	} else {
		Write-Host "Keeping existing config: $ConfigFile"
	}

    if (-not $NoEnvironment) {
        Invoke-Step {
            foreach ($entry in @(
                @("DM_HOME", $DataDir),
                @("DM_CONFIG", $ConfigFile),
                @("DM_OUTPUT_DIR", $OutputDir)
            )) {
                $script:EnvironmentRecords += Set-OwnedEnvironmentVariable -Name $entry[0] -Value $entry[1]
            }
            if (-not $NoPathUpdate) {
                $oldPath = [Environment]::GetEnvironmentVariable("PATH", $Scope)
                $parts = @()
                if ($oldPath) { $parts = @($oldPath -split ';' | Where-Object { $_ }) }
                $previousOwnedPath = $PreviousManifest -and
                    [bool]$PreviousManifest.path_entry_added -and
                    ([string]$PreviousManifest.bin_dir -eq $BinDir) -and
                    ($parts -contains $BinDir)
                if ($parts -notcontains $BinDir) {
                    $newPath = if ($oldPath) { "$BinDir;$oldPath" } else { $BinDir }
                    [Environment]::SetEnvironmentVariable("PATH", $newPath, $Scope)
                    $script:PathEntryAdded = $true
                    $script:PathAddedThisRun = $true
                } elseif ($previousOwnedPath) {
                    $script:PathEntryAdded = $true
                }
            }
        } "set owned environment variables"
    } else {
        Write-Host "Skipping persistent environment variables (-NoEnvironment)."
    }

    $manifestData = [ordered]@{
		install_id = $InstallID
        install_dir = $InstallDir
        bin_dir = $BinDir
        installed_bin = $InstalledBin
        config_dir = $ConfigDir
        config_file = $ConfigFile
        data_dir = $DataDir
        output_dir = $OutputDir
        scope = $Scope
        environment_variables = $EnvironmentRecords
        path_entry_added = $PathEntryAdded
        completion_files = $CompletionFiles
		completion_records = $CompletionRecords
        completion_dir = if ($CompletionFiles.Count -gt 0) { $CompletionBaseDir } else { $null }
        completion_profile = if ($CompletionFiles.Count -gt 0 -and -not $NoCompletionProfile) { $CompletionProfile } else { $null }
        completion_profile_start = $CompletionProfileStart
        completion_profile_end = $CompletionProfileEnd
    }
    Invoke-Step {
        Assert-NoInstallReparsePoint -Path $ConfigDir -Description "configuration directory"
        Assert-NoInstallReparsePoint -Path $Manifest -Description "install manifest"
        $manifestTemp = Join-Path $ConfigDir (".install.json.tmp-" + [guid]::NewGuid().ToString("N"))
        Assert-NoInstallReparsePoint -Path $manifestTemp -Description "temporary install manifest"
        try {
            $manifestJSON = $manifestData | ConvertTo-Json -Depth 4
            $utf8NoBom = New-Object System.Text.UTF8Encoding($false)
            Assert-NoInstallReparsePoint -Path $manifestTemp -Description "temporary install manifest"
            [System.IO.File]::WriteAllText($manifestTemp, $manifestJSON + [Environment]::NewLine, $utf8NoBom)
            Assert-NoInstallReparsePoint -Path $manifestTemp -Description "temporary install manifest"
            Assert-NoInstallReparsePoint -Path $Manifest -Description "install manifest"
            $manifestItem = Get-InstallPathItem -Path $Manifest
            if ($null -ne $manifestItem -and -not $manifestItem.PSIsContainer) {
                Assert-NoInstallReparsePoint -Path $manifestTemp -Description "temporary install manifest"
                Assert-NoInstallReparsePoint -Path $Manifest -Description "install manifest"
                [System.IO.File]::Replace($manifestTemp, $Manifest, $null)
            } else {
                Assert-NoInstallReparsePoint -Path $manifestTemp -Description "temporary install manifest"
                Assert-NoInstallReparsePoint -Path $Manifest -Description "install manifest"
                [System.IO.File]::Move($manifestTemp, $Manifest)
            }
            Assert-NoInstallReparsePoint -Path $Manifest -Description "install manifest"
        } finally {
            Remove-SafeInstallPath -Path $manifestTemp -Description "temporary install manifest"
        }
    } "write $Manifest"
} catch {
	$installError = $_
	$rollbackErrors = New-Object System.Collections.Generic.List[string]
	if (-not $DryRun -and -not $NoEnvironment) {
		try { Undo-EnvironmentChanges } catch { $rollbackErrors.Add("environment rollback: $($_.Exception.Message)") }
	}
	try { Restore-InstallTransaction } catch { $rollbackErrors.Add("file rollback: $($_.Exception.Message)") }
	if ($rollbackErrors.Count -gt 0) {
		throw "Installation failed: $($installError.Exception.Message); rollback errors: $($rollbackErrors -join '; ')"
	}
	throw $installError
} finally {
	Remove-InstallTransaction
}

Write-Host "Installation complete."
Write-Host "Restart the terminal, then run: dm version"
