param(
    [string]$InstallDir,
    [string]$BinDir,
    [string]$ConfigDir,
    [switch]$MachineScope,
    [switch]$Purge,
    [switch]$DryRun
)

$ErrorActionPreference = "Stop"
$Scope = if ($MachineScope) { "Machine" } else { "User" }

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

$Manifest = Join-Path $ConfigDir "install.json"
$DataDir = Join-Path $InstallDir "data"
$LibexecDir = Join-Path $InstallDir "lib"
$CompletionFiles = @()
$CompletionProfile = $null
$CompletionProfileStart = "# >>> docker-manager completion >>>"
$CompletionProfileEnd = "# <<< docker-manager completion <<<"
if (Test-Path $Manifest) {
    $manifestData = Get-Content $Manifest -Raw | ConvertFrom-Json
    if (-not $PSBoundParameters.ContainsKey("InstallDir")) { $InstallDir = $manifestData.install_dir }
    if (-not $PSBoundParameters.ContainsKey("BinDir")) { $BinDir = $manifestData.bin_dir }
    if (-not $PSBoundParameters.ContainsKey("ConfigDir")) { $ConfigDir = $manifestData.config_dir }
    $DataDir = $manifestData.data_dir
    $LibexecDir = $manifestData.libexec_dir
    if ($manifestData.scope -eq "Machine") { $Scope = "Machine" }
    if ($manifestData.completion_files) { $CompletionFiles = @($manifestData.completion_files) }
    if ($manifestData.completion_profile) { $CompletionProfile = $manifestData.completion_profile }
    if ($manifestData.completion_profile_start) { $CompletionProfileStart = $manifestData.completion_profile_start }
    if ($manifestData.completion_profile_end) { $CompletionProfileEnd = $manifestData.completion_profile_end }
}

$Wrapper = Join-Path $BinDir "dm.cmd"
$InstalledBin = Join-Path $LibexecDir "dm-bin.exe"

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
    $current = if (Test-Path $Path -PathType Leaf) { Split-Path -Parent $Path } else { $Path }
    $stopFull = [System.IO.Path]::GetFullPath($StopDir).TrimEnd([System.IO.Path]::DirectorySeparatorChar, [System.IO.Path]::AltDirectorySeparatorChar)
    while ($current) {
        $currentFull = [System.IO.Path]::GetFullPath($current).TrimEnd([System.IO.Path]::DirectorySeparatorChar, [System.IO.Path]::AltDirectorySeparatorChar)
        if (-not $currentFull.StartsWith($stopFull, [System.StringComparison]::OrdinalIgnoreCase)) {
            break
        }
        if (-not (Test-Path $currentFull -PathType Container)) {
            $current = Split-Path -Parent $currentFull
            continue
        }
        if ((Get-ChildItem -LiteralPath $currentFull -Force | Select-Object -First 1)) {
            break
        }
        Remove-Item -Force -ErrorAction SilentlyContinue $currentFull
        if ($currentFull -eq $stopFull) {
            break
        }
        $current = Split-Path -Parent $currentFull
    }
}

Write-Host "Uninstalling docker-manager"

Invoke-Step {
    Remove-Item -Force -ErrorAction SilentlyContinue $Wrapper, $InstalledBin
    foreach ($file in $CompletionFiles) {
        if ($file) {
            Remove-Item -Force -ErrorAction SilentlyContinue $file
        }
    }
    if (Test-Path $LibexecDir) {
        Remove-Item -Force -ErrorAction SilentlyContinue $LibexecDir
    }
    Remove-EmptyParents -Path $BinDir -StopDir $InstallDir
    foreach ($file in $CompletionFiles) {
        if ($file) {
            Remove-EmptyParents -Path (Split-Path -Parent $file) -StopDir $InstallDir
        }
    }
    Remove-EmptyParents -Path $LibexecDir -StopDir $InstallDir
} "remove installed files"

if ($CompletionProfile -and (Test-Path $CompletionProfile)) {
    Invoke-Step {
        $existing = Get-Content $CompletionProfile -Raw
        $pattern = "(?s)" + [regex]::Escape($CompletionProfileStart) + ".*?" + [regex]::Escape($CompletionProfileEnd) + "\r?\n?"
        $clean = [regex]::Replace($existing, $pattern, "")
        Set-Content -Path $CompletionProfile -Value $clean -Encoding UTF8
    } "remove PowerShell completion block from $CompletionProfile"
}

Invoke-Step {
    [Environment]::SetEnvironmentVariable("DM_HOME", $null, $Scope)
    [Environment]::SetEnvironmentVariable("DM_CONFIG", $null, $Scope)
    [Environment]::SetEnvironmentVariable("DM_OUTPUT_DIR", $null, $Scope)
    $oldPath = [Environment]::GetEnvironmentVariable("PATH", $Scope)
    if ($oldPath) {
        $newPath = (($oldPath -split ';') | Where-Object { $_ -and ($_ -ne $BinDir) }) -join ';'
        [Environment]::SetEnvironmentVariable("PATH", $newPath, $Scope)
    }
} "remove environment variables"

if ($Purge) {
    Invoke-Step {
        Remove-Item -Recurse -Force -ErrorAction SilentlyContinue $ConfigDir, $DataDir
        Remove-EmptyParents -Path $InstallDir -StopDir $InstallDir
    } "remove config and data"
} else {
    Write-Host "Keeping config and data. Use -Purge to remove:"
    Write-Host "  $ConfigDir"
    Write-Host "  $DataDir"
}

Write-Host "Uninstall complete."
