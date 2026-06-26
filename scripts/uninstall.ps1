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
if (Test-Path $Manifest) {
    $manifestData = Get-Content $Manifest -Raw | ConvertFrom-Json
    if (-not $PSBoundParameters.ContainsKey("InstallDir")) { $InstallDir = $manifestData.install_dir }
    if (-not $PSBoundParameters.ContainsKey("BinDir")) { $BinDir = $manifestData.bin_dir }
    if (-not $PSBoundParameters.ContainsKey("ConfigDir")) { $ConfigDir = $manifestData.config_dir }
    $DataDir = $manifestData.data_dir
    $LibexecDir = $manifestData.libexec_dir
    if ($manifestData.scope -eq "Machine") { $Scope = "Machine" }
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

Write-Host "Uninstalling docker-manager"

Invoke-Step {
    Remove-Item -Force -ErrorAction SilentlyContinue $Wrapper, $InstalledBin
    if (Test-Path $LibexecDir) {
        Remove-Item -Force -ErrorAction SilentlyContinue $LibexecDir
    }
} "remove installed files"

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
    } "remove config and data"
} else {
    Write-Host "Keeping config and data. Use -Purge to remove:"
    Write-Host "  $ConfigDir"
    Write-Host "  $DataDir"
}

Write-Host "Uninstall complete."
