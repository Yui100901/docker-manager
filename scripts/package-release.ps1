param(
    [string]$DistDir,
    [string]$Version,
    [string]$Commit,
    [string]$BuildDate,
    [string[]]$Platform,
    [switch]$NoTest
)

$ErrorActionPreference = "Stop"
$RootDir = (Resolve-Path -LiteralPath (Join-Path $PSScriptRoot "..")).Path
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
if ($Version -notmatch "^[A-Za-z0-9][A-Za-z0-9._+-]*$") { throw "Invalid version: $Version" }
if ($Commit -notmatch "^[A-Za-z0-9][A-Za-z0-9._-]*$") { throw "Invalid commit: $Commit" }
if ($BuildDate -notmatch "^[0-9TZ:.+-]+$") { throw "Invalid build date: $BuildDate" }
if ($Version.Length -gt 128 -or $Commit.Length -gt 128 -or $BuildDate.Length -gt 128) {
    throw "Version, commit, and build date must each be at most 128 characters."
}
$seenRequestedPlatforms = @{}
foreach ($item in $Platform) {
    if ($item -notmatch "^[A-Za-z0-9_]+/[A-Za-z0-9_]+$") { throw "Invalid platform: $item" }
    if ($seenRequestedPlatforms.ContainsKey($item)) { throw "Duplicate platform: $item" }
    $seenRequestedPlatforms[$item] = $true
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

~~~powershell
.\scripts\install.ps1
.\scripts\install.ps1 -NoCompletion
.\scripts\install.ps1 -Binary .\$Binary
~~~

Verify after installation:

~~~powershell
dm version
dm doctor --check-e2e=false
~~~
"@
        Set-Content -LiteralPath $Path -Value $content -Encoding UTF8
        return
    }

    if ($TargetOS -eq "darwin") {
        $content = @"
# docker-manager $Version $TargetPlatform

## Files

- ``$Binary``: dm executable for $TargetPlatform
- ``dm.yaml.example``: sample configuration

## Install

The shell installer is Linux-only. Install the Darwin binary directly:

~~~bash
sudo mkdir -p /usr/local/bin
sudo install -m 0755 ./$Binary /usr/local/bin/dm
~~~

Verify after installation:

~~~bash
dm version
dm doctor --check-e2e=false
~~~

Uninstall the binary:

~~~bash
sudo rm -f /usr/local/bin/dm
~~~
"@
        Set-Content -LiteralPath $Path -Value $content -Encoding UTF8
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

~~~bash
bash scripts/install.sh
bash scripts/install.sh --completion bash --completion zsh --completion fish
bash scripts/install.sh --no-completion
bash scripts/install.sh --binary ./$Binary
~~~

Verify after installation:

~~~bash
dm version
dm doctor --check-e2e=false
~~~
"@
    Set-Content -LiteralPath $Path -Value $content -Encoding UTF8
}

function Copy-ReleaseScripts {
    param(
        [string]$TargetOS,
        [string]$ScriptDir
    )
    if ($TargetOS -eq "darwin") {
        return
    }
    New-Item -ItemType Directory -Force -Path $ScriptDir | Out-Null
    if ($TargetOS -eq "windows") {
        Copy-Item -LiteralPath (Join-Path $RootDir "scripts/install.ps1") -Destination $ScriptDir -Force
        Copy-Item -LiteralPath (Join-Path $RootDir "scripts/uninstall.ps1") -Destination $ScriptDir -Force
        return
    }
    Copy-Item -LiteralPath (Join-Path $RootDir "scripts/install.sh") -Destination $ScriptDir -Force
    Copy-Item -LiteralPath (Join-Path $RootDir "scripts/uninstall.sh") -Destination $ScriptDir -Force
}

function Copy-ReleaseDocumentation {
    param([string]$PackageDir)
    Copy-Item -LiteralPath (Join-Path $RootDir "README.md") -Destination $PackageDir -Force
    Copy-Item -LiteralPath (Join-Path $RootDir "CHANGELOG.md") -Destination $PackageDir -Force
    Copy-Item -LiteralPath (Join-Path $RootDir "LICENSE") -Destination $PackageDir -Force
    $docsDir = Join-Path $PackageDir "docs"
    New-Item -ItemType Directory -Path $docsDir | Out-Null
    foreach ($name in @("TESTING.md", "RELEASE_CHECKLIST.md", "DOCKER_API_MIGRATION.md")) {
        Copy-Item -LiteralPath (Join-Path $RootDir "docs/$name") -Destination $docsDir -Force
    }
}

function Assert-ReleaseManifest {
    param(
        [string]$ReleaseDir,
        [string]$ExpectedVersion,
        [string]$ExpectedCommit,
        [int]$ExpectedArtifacts
    )
    $manifestPath = Join-Path $ReleaseDir "release-manifest.json"
    $manifest = Get-Content -LiteralPath $manifestPath -Raw | ConvertFrom-Json
    if ($manifest.version -ne $ExpectedVersion -or $manifest.commit -ne $ExpectedCommit) {
        throw "Release manifest identity mismatch"
    }
    if (@($manifest.artifacts).Count -ne $ExpectedArtifacts) {
        throw "Release manifest artifact count mismatch"
    }
    $checksumLines = @(Get-Content -LiteralPath (Join-Path $ReleaseDir "checksums.txt") -Encoding ASCII)
    $seenPlatforms = @{}
    $seenArchives = @{}
    foreach ($artifact in @($manifest.artifacts)) {
        $archiveName = [string]$artifact.archive
        if (-not $archiveName -or $archiveName -ne [System.IO.Path]::GetFileName($archiveName)) {
            throw "Invalid archive path in release manifest: $archiveName"
        }
        if ($seenPlatforms.ContainsKey([string]$artifact.platform) -or $seenArchives.ContainsKey($archiveName)) {
            throw "Duplicate platform or archive in release manifest: $($artifact.platform) / $archiveName"
        }
        $seenPlatforms[[string]$artifact.platform] = $true
        $seenArchives[$archiveName] = $true
        $archivePath = Join-Path $ReleaseDir $archiveName
        if (-not (Test-Path -LiteralPath $archivePath -PathType Leaf)) {
            throw "Release archive is missing: $archiveName"
        }
        $actual = (Get-FileHash -Algorithm SHA256 -LiteralPath $archivePath).Hash.ToLowerInvariant()
        if ($actual -ne [string]$artifact.sha256) {
            throw "Release archive digest mismatch: $archiveName"
        }
        if ($checksumLines -notcontains "$actual  $archiveName") {
            throw "checksums.txt is missing the manifest digest for $archiveName"
        }
    }
    if ($checksumLines.Count -ne $ExpectedArtifacts) {
        throw "checksums.txt contains unexpected entries"
    }
}

Assert-Command go
[System.IO.Directory]::CreateDirectory([System.IO.Path]::GetFullPath($DistDir)) | Out-Null
$DistRoot = (Resolve-Path -LiteralPath $DistDir).Path
$ReleaseKey = "$Version-$Commit"
$ReleaseDir = Join-Path $DistRoot $ReleaseKey
if (Test-Path -LiteralPath $ReleaseDir) {
    throw "Release directory already exists: $ReleaseDir"
}
$DistDir = Join-Path $DistRoot (".$ReleaseKey.staging-" + [guid]::NewGuid().ToString("N"))
$Published = $false
$WorkDir = $null
New-Item -ItemType Directory -Path $DistDir | Out-Null
try {
    $WorkDir = Join-Path ([System.IO.Path]::GetTempPath()) ("dm-release-" + [guid]::NewGuid())
    [System.IO.Directory]::CreateDirectory($WorkDir) | Out-Null

    $ChecksumsFile = Join-Path $DistDir "checksums.txt"
    $ManifestFile = Join-Path $DistDir "release-manifest.json"
    $SummaryFile = Join-Path $DistDir "release-summary.md"
    $Artifacts = @()
    $ChecksumLines = New-Object System.Collections.Generic.List[string]
    if (Test-Path -LiteralPath $ChecksumsFile) {
        foreach ($line in Get-Content -LiteralPath $ChecksumsFile -Encoding ASCII) {
            if (-not $line.Trim()) { continue }
            $parts = $line -split "\s+", 2
            if ($parts.Count -lt 2) { continue }
            $archiveName = $parts[1].TrimStart("*")
            if (Test-Path -LiteralPath (Join-Path $DistDir $archiveName)) {
                $ChecksumLines.Add($line)
            }
        }
    }

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

        Copy-ReleaseDocumentation -PackageDir $packageDir
        Copy-Item -LiteralPath (Join-Path $RootDir ".dm.yaml.example") -Destination (Join-Path $packageDir "dm.yaml.example") -Force
        $scriptDir = Join-Path $packageDir "scripts"
        Copy-ReleaseScripts -TargetOS $goos -ScriptDir $scriptDir
        Write-InstallGuide -Path (Join-Path $packageDir "INSTALL.md") -Binary $binary -TargetPlatform $item -TargetOS $goos

        if ($goos -eq "windows") {
            $archive = Join-Path $DistDir "$name.zip"
            if (Test-Path -LiteralPath $archive) { Remove-Item -LiteralPath $archive -Force }
            Compress-Archive -LiteralPath $packageDir -DestinationPath $archive
        } else {
            Assert-Command tar
            $archive = Join-Path $DistDir "$name.tar.gz"
            if (Test-Path -LiteralPath $archive) { Remove-Item -LiteralPath $archive -Force }
            tar -C $WorkDir -czf $archive $name
        }

        $sha = (Get-FileHash -Algorithm SHA256 -LiteralPath $archive).Hash.ToLowerInvariant()
        $archiveName = Split-Path -Leaf $archive
        for ($i = $ChecksumLines.Count - 1; $i -ge 0; $i--) {
            if ($ChecksumLines[$i] -match "\s+\*?$([regex]::Escape($archiveName))$") {
                $ChecksumLines.RemoveAt($i)
            }
        }
        $ChecksumLines.Add("$sha  $archiveName")
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

    $ChecksumLines | Set-Content -LiteralPath $ChecksumsFile -Encoding ASCII
    $manifestJSON = [ordered]@{
        version    = $Version
        commit     = $Commit
        build_date = $BuildDate
        artifacts  = @($Artifacts)
    } | ConvertTo-Json -Depth 5
    $utf8NoBom = New-Object System.Text.UTF8Encoding($false)
    [System.IO.File]::WriteAllText($ManifestFile, $manifestJSON + [Environment]::NewLine, $utf8NoBom)
    [System.IO.File]::WriteAllLines($SummaryFile, [string[]]$summary, $utf8NoBom)

    Assert-ReleaseManifest -ReleaseDir $DistDir -ExpectedVersion $Version -ExpectedCommit $Commit -ExpectedArtifacts $Platform.Count
    & go run (Join-Path $RootDir "scripts/verify-release-manifest.go") --dir $DistDir --version $Version --commit $Commit --count $Platform.Count
    if ($LASTEXITCODE -ne 0) { throw "Structured release manifest verification failed" }
    if (Test-Path -LiteralPath $ReleaseDir) {
        throw "Release directory was created while packaging: $ReleaseDir"
    }
    [System.IO.Directory]::Move($DistDir, $ReleaseDir)
    $Published = $true
    Write-Host "Release artifacts written to: $ReleaseDir"
    Write-Host "Checksums: $(Join-Path $ReleaseDir 'checksums.txt')"
    Write-Host "Manifest: $(Join-Path $ReleaseDir 'release-manifest.json')"
    Write-Host "Summary: $(Join-Path $ReleaseDir 'release-summary.md')"
} finally {
    if ($WorkDir) {
        Remove-Item -LiteralPath $WorkDir -Recurse -Force -ErrorAction SilentlyContinue
    }
    if (-not $Published) {
        Remove-Item -LiteralPath $DistDir -Recurse -Force -ErrorAction SilentlyContinue
    }
}
