param(
    [switch]$Race,
    [switch]$NoShellCheck
)

$ErrorActionPreference = "Stop"
$RootDir = Resolve-Path (Join-Path $PSScriptRoot "..")

function Invoke-Native {
    param(
        [Parameter(Mandatory = $true)]
        [scriptblock]$Command
    )
    & $Command
    if ($LASTEXITCODE -ne 0) {
        throw "Command failed with exit code $LASTEXITCODE"
    }
}

Push-Location $RootDir
try {
    Write-Host "==> gofmt check"
    $goFiles = Get-ChildItem -Path . -Recurse -Filter *.go |
        Where-Object { $_.FullName -notmatch "\\vendor\\" } |
        ForEach-Object { $_.FullName }
    $gofmtFiles = @()
    if ($goFiles.Count -gt 0) {
        $gofmtFiles = @(gofmt -l $goFiles)
    }
    if ($gofmtFiles.Count -gt 0) {
        $gofmtFiles | ForEach-Object { Write-Error "Run gofmt on $_" }
        exit 1
    }

    Write-Host "==> go test ./..."
    Invoke-Native { go test ./... }

    Write-Host "==> go vet ./..."
    Invoke-Native { go vet ./... }

    if ($Race) {
        Write-Host "==> go test -race ./..."
        $oldCGO = $env:CGO_ENABLED
        try {
            $env:CGO_ENABLED = "1"
            Invoke-Native { go test -race ./... }
        } finally {
            $env:CGO_ENABLED = $oldCGO
        }
    }

    Write-Host "==> git diff --check"
    Invoke-Native { git diff --check }

    Write-Host "==> PowerShell parse"
    Get-ChildItem -Path scripts -Filter *.ps1 | ForEach-Object {
        $tokens = $null
        $errors = $null
        [System.Management.Automation.Language.Parser]::ParseFile($_.FullName, [ref]$tokens, [ref]$errors) | Out-Null
        if ($errors.Count -gt 0) {
            $errors | ForEach-Object { Write-Error "$($_.Extent.File):$($_.Extent.StartLineNumber): $($_.Message)" }
            exit 1
        }
    }

    if (-not $NoShellCheck) {
        $shellcheck = Get-Command shellcheck -ErrorAction SilentlyContinue
        if ($shellcheck) {
            Write-Host "==> shellcheck"
            Invoke-Native { shellcheck scripts/*.sh }
        } else {
            Write-Host "shellcheck not found; skipped"
        }
    }

    Write-Host "All checks passed."
} finally {
    Pop-Location
}
