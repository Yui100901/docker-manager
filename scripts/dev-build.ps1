param(
    [string]$Output,
    [string]$Version = $(if ($env:VERSION) { $env:VERSION } else { "dev" }),
    [string]$Commit = $(if ($env:COMMIT) { $env:COMMIT } else { "" }),
    [string]$BuildDate = $(if ($env:BUILD_DATE) { $env:BUILD_DATE } else { "" }),
    [switch]$NoTest,
    [switch]$Vet,
    [switch]$Race,
    [string]$GoFlags = $env:GOFLAGS
)

$ErrorActionPreference = "Stop"
$RootDir = Resolve-Path (Join-Path $PSScriptRoot "..")

if (-not $Commit) {
    try {
        $Commit = (& git -C $RootDir rev-parse --short HEAD 2>$null).Trim()
    } catch {
        $Commit = "unknown"
    }
    if (-not $Commit) { $Commit = "unknown" }
}

if (-not $BuildDate) {
    $BuildDate = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")
}

$GoOS = (& go env GOOS).Trim()
$GoArch = (& go env GOARCH).Trim()
if (-not $Output) {
    $suffix = if ($GoOS -eq "windows") { ".exe" } else { "" }
    $Output = Join-Path $RootDir "bin/dev/dm$suffix"
}

$OutputDir = Split-Path -Parent $Output
if ($OutputDir) {
    New-Item -ItemType Directory -Force -Path $OutputDir | Out-Null
}

$LdFlags = "-X docker-manager/internal/version.version=$Version -X docker-manager/internal/version.commit=$Commit -X docker-manager/internal/version.buildDate=$BuildDate"
$BuildArgs = @("-trimpath", "-ldflags", $LdFlags)
if ($Race) {
    $BuildArgs = @("-race") + $BuildArgs
}

if (-not $NoTest) {
    Write-Host "==> go test ./..."
    Push-Location $RootDir
    try {
        if ($GoFlags) { $env:GOFLAGS = $GoFlags }
        go test ./...
    } finally {
        Pop-Location
    }
}

if ($Vet) {
    Write-Host "==> go vet ./..."
    Push-Location $RootDir
    try {
        if ($GoFlags) { $env:GOFLAGS = $GoFlags }
        go vet ./...
    } finally {
        Pop-Location
    }
}

Write-Host "==> build $GoOS/$GoArch $Version $Commit"
Push-Location $RootDir
try {
    if ($GoFlags) { $env:GOFLAGS = $GoFlags }
    go build @BuildArgs -o $Output .
} finally {
    Pop-Location
}

Write-Host "Built: $Output"
& $Output version
