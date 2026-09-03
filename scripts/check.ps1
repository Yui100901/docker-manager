param(
    [switch]$Race,
    [switch]$NoShellCheck,
    [switch]$NoGoChecks
)

$ErrorActionPreference = "Stop"
if ($NoGoChecks -and $Race) {
    throw "-Race and -NoGoChecks cannot be used together."
}
$oldOutputEncoding = $OutputEncoding
$oldConsoleOutputEncoding = [Console]::OutputEncoding
$hadOutFileEncoding = $PSDefaultParameterValues.ContainsKey("Out-File:Encoding")
$oldOutFileEncoding = $PSDefaultParameterValues["Out-File:Encoding"]
$utf8NoBom = New-Object System.Text.UTF8Encoding($false)
$OutputEncoding = $utf8NoBom
[Console]::OutputEncoding = $utf8NoBom
$PSDefaultParameterValues["Out-File:Encoding"] = "utf8"
$RootDir = (Resolve-Path -LiteralPath (Join-Path $PSScriptRoot "..")).Path

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
    # Use Git's exclusion rules so local caches and generated artifacts do not
    # expand the file set or exceed Windows' native command-line limit.
    $goFiles = @(git -c core.quotepath=false ls-files --cached --others --exclude-standard -- "*.go")
    if ($LASTEXITCODE -ne 0) {
        throw "Unable to enumerate Go files (git exit code $LASTEXITCODE)."
    }
    $gofmtFiles = @()
    foreach ($goFile in $goFiles) {
        $formatted = @(& gofmt -l $goFile)
        if ($LASTEXITCODE -ne 0) {
            throw "gofmt failed for $goFile (exit code $LASTEXITCODE)."
        }
        if ($formatted.Count -gt 0) {
            $gofmtFiles += $formatted
        }
    }
    if ($gofmtFiles.Count -gt 0) {
        $gofmtFiles | ForEach-Object { Write-Error "Run gofmt on $_" }
        exit 1
    }

    Write-Host "==> repository text encoding"
    Invoke-Native { go run ./scripts/text-check.go }

    if (-not $NoGoChecks) {
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
    } else {
        Write-Host "==> go test, go vet and race explicitly skipped (-NoGoChecks)"
    }

    Write-Host "==> git diff HEAD --check"
    Invoke-Native { git diff HEAD --check }

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
            $shellFiles = @(Get-ChildItem -Path scripts -Filter *.sh | ForEach-Object { $_.FullName })
            Invoke-Native { & $shellcheck.Source @shellFiles }
        } else {
            throw "shellcheck not found. Install ShellCheck or explicitly pass -NoShellCheck."
        }
    } else {
        Write-Host "==> shellcheck explicitly skipped (-NoShellCheck)"
    }

    Write-Host "All requested checks passed."
} finally {
    Pop-Location
    $OutputEncoding = $oldOutputEncoding
    [Console]::OutputEncoding = $oldConsoleOutputEncoding
    if ($hadOutFileEncoding) {
        $PSDefaultParameterValues["Out-File:Encoding"] = $oldOutFileEncoding
    } else {
        $PSDefaultParameterValues.Remove("Out-File:Encoding")
    }
}
