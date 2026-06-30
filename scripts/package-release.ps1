param(
    [string]$DistDir,
    [string]$Version,
    [string]$Commit,
    [string]$BuildDate,
    [string[]]$Platform,
    [switch]$NoTest
)

$ErrorActionPreference = "Stop"
$RootDir = Resolve-Path (Join-Path $PSScriptRoot "..")
if (-not $DistDir) { $DistDir = Join-Path $RootDir "dist" }
if (-not $Version) { $Version = if ($env:VERSION) { $env:VERSION } else { "dev" } }
if (-not $Commit) {
    $Commit = if ($env:COMMIT) { $env:COMMIT } else { (& git -C $RootDir rev-parse --short HEAD 2>$null).Trim() }
    if (-not $Commit) { $Commit = "unknown" }
}
if (-not $BuildDate) {
    $BuildDate = if ($env:BUILD_DATE) { $env:BUILD_DATE } else { (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ") }
}
if (-not $Platform -or $Platform.Count -eq 0) {
    $Platform = @("linux/amd64", "linux/arm64", "windows/amd64", "darwin/amd64", "darwin/arm64")
}

function Assert-Command {
    param([string]$Name)
    if (-not (Get-Command $Name -ErrorAction SilentlyContinue)) {
        throw "Missing command: $Name"
    }
}

function Write-InstallGuide {
    param(
        [string]$Path,
        [string]$Binary,
        [string]$TargetPlatform,
        [string]$TargetOS
    )
    if ($TargetOS -eq "windows") {
        $content = @"
# docker-manager $Version $TargetPlatform

## Files

- ``$Binary``: dm executable for $TargetPlatform
- ``dm.yaml.example``: sample configuration
- ``scripts/install.ps1``: PowerShell install script
- ``scripts/uninstall.ps1``: PowerShell uninstall script

## Install

````powershell
.\scripts\install.ps1 -Binary .\$Binary
.\scripts\install.ps1 -Binary .\$Binary -NoCompletion
````

Verify after installation:

````powershell
dm version
dm doctor --check-e2e=false
````
"@
        Set-Content -Path $Path -Value $content -Encoding UTF8
        return
    }

    $content = @"
# docker-manager $Version $TargetPlatform

## Files

- ``$Binary``: dm executable for $TargetPlatform
- ``dm.yaml.example``: sample configuration
- ``scripts/install.sh``: shell install script
- ``scripts/uninstall.sh``: shell uninstall script

## Install

````bash
bash scripts/install.sh --binary ./$Binary
bash scripts/install.sh --binary ./$Binary --completion bash --completion zsh --completion fish
bash scripts/install.sh --binary ./$Binary --no-completion
````

Verify after installation:

````bash
dm version
dm doctor --check-e2e=false
````
"@
    Set-Content -Path $Path -Value $content -Encoding UTF8
}

function Copy-ReleaseScripts {
    param(
        [string]$TargetOS,
        [string]$ScriptDir
    )
    New-Item -ItemType Directory -Force -Path $ScriptDir | Out-Null
    if ($TargetOS -eq "windows") {
        Copy-Item -Force (Join-Path $RootDir "scripts/install.ps1") $ScriptDir
        Copy-Item -Force (Join-Path $RootDir "scripts/uninstall.ps1") $ScriptDir
        return
    }
    Copy-Item -Force (Join-Path $RootDir "scripts/install.sh") $ScriptDir
    Copy-Item -Force (Join-Path $RootDir "scripts/uninstall.sh") $ScriptDir
}

Assert-Command go
New-Item -ItemType Directory -Force -Path $DistDir | Out-Null
$DistDir = (Resolve-Path $DistDir).Path
$WorkDir = Join-Path ([System.IO.Path]::GetTempPath()) ("dm-release-" + [guid]::NewGuid())
New-Item -ItemType Directory -Force -Path $WorkDir | Out-Null

$ChecksumsFile = Join-Path $DistDir "checksums.txt"
$ManifestFile = Join-Path $DistDir "release-manifest.json"
$SummaryFile = Join-Path $DistDir "release-summary.md"
$Artifacts = @()
Set-Content -Path $ChecksumsFile -Value "" -Encoding ASCII

try {
    if (-not $NoTest) {
        Write-Host "==> go test ./..."
        Push-Location $RootDir
        try {
            go test ./...
        } finally {
            Pop-Location
        }
    }

    $summary = New-Object System.Collections.Generic.List[string]
    $summary.Add("# docker-manager $Version release artifacts")
    $summary.Add("")
    $summary.Add("- Commit: ``$Commit``")
    $summary.Add("- Build date: ``$BuildDate``")
    $summary.Add("- Checksums: ``checksums.txt``")
    $summary.Add("- Manifest: ``release-manifest.json``")
    $summary.Add("")
    $summary.Add("| Platform | Format | Archive | SHA256 |")
    $summary.Add("| --- | --- | --- | --- |")

    foreach ($item in $Platform) {
        if ($item -notmatch "^[A-Za-z0-9_]+/[A-Za-z0-9_]+$") {
            throw "Invalid platform: $item"
        }
        $parts = $item -split "/", 2
        $goos = $parts[0]
        $goarch = $parts[1]
        $name = "dm_${Version}_${goos}_${goarch}"
        $packageDir = Join-Path $WorkDir $name
        $binary = if ($goos -eq "windows") { "dm.exe" } else { "dm" }
        $format = if ($goos -eq "windows") { "zip" } else { "tar.gz" }

        New-Item -ItemType Directory -Force -Path $packageDir | Out-Null
        Write-Host "==> build $item"
        Push-Location $RootDir
        $oldGOOS = $env:GOOS
        $oldGOARCH = $env:GOARCH
        $oldCGO = $env:CGO_ENABLED
        try {
            $env:GOOS = $goos
            $env:GOARCH = $goarch
            $env:CGO_ENABLED = "0"
            $ldflags = "-s -w -X docker-manager/internal/version.version=$Version -X docker-manager/internal/version.commit=$Commit -X docker-manager/internal/version.buildDate=$BuildDate"
            go build -trimpath -ldflags $ldflags -o (Join-Path $packageDir $binary) .
        } finally {
            $env:GOOS = $oldGOOS
            $env:GOARCH = $oldGOARCH
            $env:CGO_ENABLED = $oldCGO
            Pop-Location
        }

        Copy-Item -Force (Join-Path $RootDir "README.md") $packageDir
        Copy-Item -Force (Join-Path $RootDir "LICENSE") $packageDir
        Copy-Item -Force (Join-Path $RootDir ".dm.yaml.example") (Join-Path $packageDir "dm.yaml.example")
        $scriptDir = Join-Path $packageDir "scripts"
        Copy-ReleaseScripts -TargetOS $goos -ScriptDir $scriptDir
        Write-InstallGuide -Path (Join-Path $packageDir "INSTALL.md") -Binary $binary -TargetPlatform $item -TargetOS $goos

        if ($goos -eq "windows") {
            $archive = Join-Path $DistDir "$name.zip"
            if (Test-Path $archive) { Remove-Item -Force $archive }
            Compress-Archive -Path $packageDir -DestinationPath $archive
        } else {
            Assert-Command tar
            $archive = Join-Path $DistDir "$name.tar.gz"
            if (Test-Path $archive) { Remove-Item -Force $archive }
            tar -C $WorkDir -czf $archive $name
        }

        $sha = (Get-FileHash -Algorithm SHA256 $archive).Hash.ToLowerInvariant()
        Add-Content -Path $ChecksumsFile -Encoding ASCII -Value "$sha  $(Split-Path -Leaf $archive)"
        $summary.Add("| ``$item`` | ``$format`` | ``$(Split-Path -Leaf $archive)`` | ``$sha`` |")
        $Artifacts += [ordered]@{
            platform = $item
            os       = $goos
            arch     = $goarch
            format   = $format
            binary   = $binary
            archive  = Split-Path -Leaf $archive
            sha256   = $sha
        }
    }

    [ordered]@{
        version    = $Version
        commit     = $Commit
        build_date = $BuildDate
        artifacts  = $Artifacts
    } | ConvertTo-Json -Depth 5 | Set-Content -Path $ManifestFile -Encoding UTF8
    $summary | Set-Content -Path $SummaryFile -Encoding UTF8

    Write-Host "Release artifacts written to: $DistDir"
    Write-Host "Checksums: $ChecksumsFile"
    Write-Host "Manifest: $ManifestFile"
    Write-Host "Summary: $SummaryFile"
} finally {
    Remove-Item -Recurse -Force -ErrorAction SilentlyContinue $WorkDir
}
