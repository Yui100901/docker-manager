param(
    [string]$OutputDir,
    [switch]$SkipRace,
    [switch]$SkipInstall,
    [switch]$SkipDevBuild,
    [switch]$KeepWorkDir
)

$ErrorActionPreference = "Stop"
$RootDir = Resolve-Path (Join-Path $PSScriptRoot "..")
if (-not $OutputDir) {
    $OutputDir = Join-Path $RootDir "dist/local-test"
}
New-Item -ItemType Directory -Force -Path $OutputDir | Out-Null
$OutputDir = (Resolve-Path $OutputDir).Path

$WorkDir = Join-Path ([System.IO.Path]::GetTempPath()) ("dm-local-test-" + [guid]::NewGuid())
$LogDir = Join-Path $OutputDir "logs"
$ResultsFile = Join-Path $OutputDir "results.tsv"
$ReportFile = Join-Path $OutputDir "local-test-report.md"
New-Item -ItemType Directory -Force -Path $WorkDir, $LogDir | Out-Null
"case`tstatus`texit_code`tseconds`tlog" | Set-Content -Path $ResultsFile -Encoding UTF8

$script:Failures = 0
$script:Skipped = 0
$script:Passed = 0

function Add-Result {
    param(
        [string]$Name,
        [string]$Status,
        [int]$ExitCode,
        [int]$Seconds,
        [string]$Log
    )
    "$Name`t$Status`t$ExitCode`t$Seconds`t$Log" | Add-Content -Path $ResultsFile -Encoding UTF8
    switch ($Status) {
        "PASS" { $script:Passed++ }
        "XFAIL" { $script:Passed++ }
        "SKIP" { $script:Skipped++ }
        default { $script:Failures++ }
    }
}

function Invoke-Case {
    param(
        [string]$Name,
        [scriptblock]$Action,
        [switch]$ExpectFailure,
        [switch]$Skip
    )
    $safeName = ($Name -replace "[^A-Za-z0-9_.-]", "_")
    $log = Join-Path $LogDir "$safeName.log"
    if ($Skip) {
        "skipped" | Set-Content -Path $log -Encoding UTF8
        Add-Result -Name $Name -Status "SKIP" -ExitCode 0 -Seconds 0 -Log $log
        Write-Host "SKIP $Name"
        return
    }

    $start = Get-Date
    $code = 0
    try {
        $global:LASTEXITCODE = 0
        $oldErrorActionPreference = $ErrorActionPreference
        try {
            $ErrorActionPreference = "Continue"
            & $Action *> $log
        } finally {
            $ErrorActionPreference = $oldErrorActionPreference
        }
        $code = if ($LASTEXITCODE -is [int]) { $LASTEXITCODE } else { 0 }
    } catch {
        $_ | Out-String | Add-Content -Path $log -Encoding UTF8
        $code = 1
    }
    $seconds = [int]((Get-Date) - $start).TotalSeconds
    if ($ExpectFailure) {
        if ($code -ne 0) {
            Add-Result -Name $Name -Status "XFAIL" -ExitCode $code -Seconds $seconds -Log $log
            Write-Host "XFAIL $Name"
            return
        }
        Add-Result -Name $Name -Status "FAIL" -ExitCode $code -Seconds $seconds -Log $log
        Write-Host "FAIL $Name expected non-zero exit"
        return
    }
    if ($code -eq 0) {
        Add-Result -Name $Name -Status "PASS" -ExitCode $code -Seconds $seconds -Log $log
        Write-Host "PASS $Name"
    } else {
        Add-Result -Name $Name -Status "FAIL" -ExitCode $code -Seconds $seconds -Log $log
        Write-Host "FAIL $Name exit=$code"
    }
}

function Test-CommandExists {
    param([string]$Name)
    return [bool](Get-Command $Name -ErrorAction SilentlyContinue)
}

try {
    $DmBin = Join-Path $WorkDir "dm.exe"

    Invoke-Case "go version" { go version }
    Invoke-Case "go test" { Push-Location $RootDir; try { go test ./... } finally { Pop-Location } }
    Invoke-Case "go vet" { Push-Location $RootDir; try { go vet ./... } finally { Pop-Location } }
    Invoke-Case "go test race" {
        Push-Location $RootDir
        $oldCGO = $env:CGO_ENABLED
        try {
            $env:CGO_ENABLED = "1"
            go test -race ./...
        } finally {
            $env:CGO_ENABLED = $oldCGO
            Pop-Location
        }
    } -Skip:$SkipRace
    Invoke-Case "git diff check" { Push-Location $RootDir; try { git diff --check } finally { Pop-Location } }
    Invoke-Case "go build dm" { Push-Location $RootDir; try { go build -o $DmBin . } finally { Pop-Location } }

    Invoke-Case "dm version" { & $DmBin version }
    Invoke-Case "dm root help" { & $DmBin --help }
    Invoke-Case "dm image help" { & $DmBin image --help }
    Invoke-Case "dm report help" { & $DmBin report --help }
    foreach ($shell in @("bash", "zsh", "fish", "powershell")) {
        Invoke-Case "completion $shell" { & $DmBin completion $shell | Set-Content -Path (Join-Path $WorkDir "$shell.completion") -Encoding UTF8 }
    }

    Invoke-Case "DM_CONFIG doctor" {
        $configDir = Join-Path $WorkDir "config"
        New-Item -ItemType Directory -Force -Path $configDir | Out-Null
        $configFile = Join-Path $configDir "dm.yaml"
        $outDir = (Join-Path $WorkDir "configured-output").Replace("\", "/")
        Set-Content -Path $configFile -Encoding UTF8 -Value "output_dir: '$outDir'`nlog_json: false`n"
        $oldConfig = $env:DM_CONFIG
        try {
            $env:DM_CONFIG = $configFile
            $output = & $DmBin doctor --format json --check-e2e=false
            if (($output -join "`n") -notmatch [regex]::Escape($outDir)) {
                throw "doctor output did not include configured output_dir"
            }
        } finally {
            $env:DM_CONFIG = $oldConfig
        }
    }

    Invoke-Case "old root pull rejected" { & $DmBin pull --help } -ExpectFailure
    Invoke-Case "old global json rejected" { & $DmBin --json version } -ExpectFailure

    Invoke-Case "PowerShell script parse" {
        foreach ($file in @("scripts/dev-build.ps1", "scripts/install.ps1", "scripts/uninstall.ps1", "scripts/package-release.ps1", "scripts/local-test.ps1")) {
            $errs = $null
            $null = [System.Management.Automation.PSParser]::Tokenize((Get-Content -Raw -Encoding UTF8 (Join-Path $RootDir $file)), [ref]$errs)
            if ($errs -and $errs.Count -gt 0) {
                throw "$file parse errors: $($errs | ConvertTo-Json -Compress)"
            }
        }
    }

    Invoke-Case "dev-build.ps1" {
        $devOut = Join-Path $WorkDir "dev-build.exe"
        Push-Location $RootDir
        try {
            & (Join-Path $RootDir "scripts/dev-build.ps1") -Output $devOut -Vet
            & $devOut version
        } finally {
            Pop-Location
        }
    } -Skip:$SkipDevBuild

    Invoke-Case "install.ps1 completion" {
        $installRoot = Join-Path $WorkDir "install"
        $configRoot = Join-Path $WorkDir "install-config"
        Push-Location $RootDir
        try {
            & (Join-Path $RootDir "scripts/install.ps1") -Binary $DmBin -InstallDir $installRoot -ConfigDir $configRoot -Completion PowerShell -NoPathUpdate -NoCompletionProfile
            & (Join-Path $installRoot "bin/dm.cmd") version
            $completion = Join-Path $installRoot "completions/dm-completion.ps1"
            if (-not (Test-Path $completion)) {
                throw "completion file was not created"
            }
            & (Join-Path $RootDir "scripts/uninstall.ps1") -InstallDir $installRoot -ConfigDir $configRoot -Purge
            if ((Test-Path $completion) -or (Test-Path $configRoot)) {
                throw "install artifacts were not cleaned"
            }
        } finally {
            Pop-Location
        }
    } -Skip:$SkipInstall

    Invoke-Case "docker unavailable behavior" {
        if (Test-CommandExists docker) {
            docker version
        } else {
            & $DmBin report health --format json
        }
    } -ExpectFailure:(!(Test-CommandExists docker))

    Invoke-Case "bash availability" {
        bash --version
    } -Skip:(!(Test-CommandExists bash))

    $rows = Import-Csv -Path $ResultsFile -Delimiter "`t"
    $report = New-Object System.Collections.Generic.List[string]
    $report.Add("# docker-manager 本地测试报告")
    $report.Add("")
    $report.Add("- Generated at: ``$((Get-Date).ToString("s"))``")
    $report.Add("- Platform: ``$([System.Runtime.InteropServices.RuntimeInformation]::OSDescription.Trim())``")
    $report.Add("- Go: ``$((go version) -join " ")``")
    $report.Add("- Docker command: ``$(if (Test-CommandExists docker) { "available" } else { "missing" })``")
    $report.Add("- Bash command: ``$(if (Test-CommandExists bash) { "available" } else { "missing" })``")
    $report.Add("- Passed: ``$script:Passed``")
    $report.Add("- Skipped: ``$script:Skipped``")
    $report.Add("- Failed: ``$script:Failures``")
    $report.Add("")
    $report.Add("| Case | Status | Exit | Seconds | Log |")
    $report.Add("| --- | --- | --- | --- | --- |")
    foreach ($row in $rows) {
        $report.Add("| $($row.case) | $($row.status) | $($row.exit_code) | $($row.seconds) | $($row.log) |")
    }
    $report | Set-Content -Path $ReportFile -Encoding UTF8

    Write-Host "Report: $ReportFile"
    Write-Host "Results: $ResultsFile"
    if ($script:Failures -gt 0) {
        exit 1
    }
} finally {
    if (-not $KeepWorkDir) {
        Remove-Item -Recurse -Force -ErrorAction SilentlyContinue $WorkDir
    } else {
        Write-Host "Work dir kept: $WorkDir"
    }
}
