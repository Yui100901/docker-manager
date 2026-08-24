param(
    [string]$OutputDir,
    [switch]$SkipRace,
    [switch]$SkipInstall,
    [switch]$SkipDevBuild,
    [switch]$KeepWorkDir,
    [switch]$NoEnvironment
)

$ErrorActionPreference = "Stop"
$EnvironmentNames = @("DM_CONFIG", "DM_HOME", "DM_OUTPUT_DIR")
$EnvironmentSnapshot = foreach ($scopeName in @("User", "Process")) {
    foreach ($name in $EnvironmentNames) {
        $value = [Environment]::GetEnvironmentVariable($name, $scopeName)
        [pscustomobject]@{
            Name = $name
            Scope = $scopeName
            Present = $null -ne $value
            Value = $value
        }
    }
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
if (-not $OutputDir) {
    $OutputDir = Join-Path $RootDir "dist/local-test"
}
[System.IO.Directory]::CreateDirectory([System.IO.Path]::GetFullPath($OutputDir)) | Out-Null
$OutputDir = (Resolve-Path -LiteralPath $OutputDir).Path

$WorkDir = Join-Path ([System.IO.Path]::GetTempPath()) ("dm-local-test-" + [guid]::NewGuid())
$LogDir = Join-Path $OutputDir "logs"
$ResultsFile = Join-Path $OutputDir "results.tsv"
$ReportFile = Join-Path $OutputDir "local-test-report.md"
New-Item -ItemType Directory -Force -Path $WorkDir, $LogDir | Out-Null
"case`tstatus`texit_code`tseconds`tlog" | Set-Content -LiteralPath $ResultsFile -Encoding UTF8

$script:Failures = 0
$script:Skipped = 0
$script:Passed = 0
$script:ExpectedFailures = 0

function Add-Result {
    param(
        [string]$Name,
        [string]$Status,
        [int]$ExitCode,
        [int]$Seconds,
        [string]$Log
    )
    "$Name`t$Status`t$ExitCode`t$Seconds`t$Log" | Add-Content -LiteralPath $ResultsFile -Encoding UTF8
    switch ($Status) {
        "PASS" { $script:Passed++ }
        "XFAIL" { $script:ExpectedFailures++ }
        "SKIP" { $script:Skipped++ }
        default { $script:Failures++ }
    }
}

function Invoke-Case {
    param(
        [string]$Name,
        [scriptblock]$Action,
        [switch]$ExpectFailure,
        [switch]$Skip,
        [string]$SkipReason = "skipped"
    )
    $safeName = ($Name -replace "[^A-Za-z0-9_.-]", "_")
    $log = Join-Path $LogDir "$safeName.log"
    if ($Skip) {
        $SkipReason | Set-Content -LiteralPath $log -Encoding UTF8
        Add-Result -Name $Name -Status "SKIP" -ExitCode 0 -Seconds 0 -Log $log
        Write-Host "SKIP $Name ($SkipReason)"
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
        $_ | Out-String | Add-Content -LiteralPath $log -Encoding UTF8
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

function Test-LocalReparsePoint {
    param([System.IO.FileSystemInfo]$Item)
    return [bool](($Item.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0)
}

function Remove-LocalTestJunction {
    param([string]$Path)
    if (-not $Path) { return }
    $item = Get-Item -LiteralPath $Path -Force -ErrorAction SilentlyContinue
    if ($null -eq $item) { return }
    if (-not (Test-LocalReparsePoint -Item $item)) {
        throw "Refusing to remove non-junction test path as a junction: $Path"
    }
    if ($item.PSIsContainer) {
        [System.IO.Directory]::Delete($item.FullName, $false)
    } else {
        [System.IO.File]::Delete($item.FullName)
    }
}

function New-LocalTestJunction {
    param(
        [string]$Path,
        [string]$Target
    )
    if (Test-Path -LiteralPath $Path) { throw "Junction test path already exists: $Path" }
    New-Item -ItemType Junction -Path $Path -Target $Target -ErrorAction Stop | Out-Null
    $item = Get-Item -LiteralPath $Path -Force -ErrorAction Stop
    if (-not $item.PSIsContainer -or -not (Test-LocalReparsePoint -Item $item)) {
        throw "Junction test path was not created as a reparse point: $Path"
    }
}

function Remove-LocalTestTreeSafely {
    param([string]$Path)
    $full = [System.IO.Path]::GetFullPath($Path)
    $rootItem = Get-Item -LiteralPath $full -Force -ErrorAction SilentlyContinue
    if ($null -eq $rootItem) { return }
    if (-not $rootItem.PSIsContainer -or (Test-LocalReparsePoint -Item $rootItem)) {
        throw "Refusing unsafe local-test cleanup root: $full"
    }
    $directories = New-Object 'System.Collections.Generic.List[string]'
    $files = New-Object 'System.Collections.Generic.List[string]'
    $stack = New-Object 'System.Collections.Generic.Stack[string]'
    $directories.Add($full)
    $stack.Push($full)
    while ($stack.Count -gt 0) {
        $current = $stack.Pop()
        $currentItem = Get-Item -LiteralPath $current -Force -ErrorAction Stop
        if (-not $currentItem.PSIsContainer -or (Test-LocalReparsePoint -Item $currentItem)) {
            throw "Refusing reparse point during local-test cleanup: $current"
        }
        foreach ($child in @(Get-ChildItem -LiteralPath $current -Force -ErrorAction Stop)) {
            if (Test-LocalReparsePoint -Item $child) {
                throw "Refusing reparse point during local-test cleanup: $($child.FullName)"
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
        $item = Get-Item -LiteralPath $file -Force -ErrorAction SilentlyContinue
        if ($null -eq $item) { continue }
        if ($item.PSIsContainer -or (Test-LocalReparsePoint -Item $item)) {
            throw "Local-test file changed before cleanup: $file"
        }
        Remove-Item -LiteralPath $file -Force -ErrorAction Stop
    }
    foreach ($directory in @($directories | Sort-Object { $_.Length } -Descending)) {
        $item = Get-Item -LiteralPath $directory -Force -ErrorAction SilentlyContinue
        if ($null -eq $item) { continue }
        if (-not $item.PSIsContainer -or (Test-LocalReparsePoint -Item $item)) {
            throw "Local-test directory changed before cleanup: $directory"
        }
        [System.IO.Directory]::Delete($directory, $false)
    }
}

function Test-LocalJunctionSupport {
    $probeRoot = Join-Path $WorkDir "junction-support-probe"
    $probeTarget = Join-Path $probeRoot "target"
    $probeLink = Join-Path $probeRoot "link"
    [System.IO.Directory]::CreateDirectory($probeTarget) | Out-Null
    try {
        New-LocalTestJunction -Path $probeLink -Target $probeTarget
        return $true
    } catch {
        $script:JunctionSkipReason = "junction creation unavailable: $($_.Exception.Message)"
        return $false
    } finally {
        Remove-LocalTestJunction -Path $probeLink
        Remove-LocalTestTreeSafely -Path $probeRoot
    }
}

try {
    $DmBin = Join-Path $WorkDir "dm.exe"
    if ($SkipInstall) {
        $script:JunctionSkipReason = "install tests were skipped"
        $JunctionTestsAvailable = $false
    } else {
        $script:JunctionSkipReason = "junction tests are unavailable"
        $JunctionTestsAvailable = Test-LocalJunctionSupport
    }

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
    Invoke-Case "shortcut pull help" { & $DmBin pull --help }
    Invoke-Case "shortcut health help" { & $DmBin health --help }
    Invoke-Case "shortcut registry help" { & $DmBin registry --help }
    foreach ($shell in @("bash", "zsh", "fish", "powershell")) {
        Invoke-Case "completion $shell" { & $DmBin completion $shell | Set-Content -LiteralPath (Join-Path $WorkDir "$shell.completion") -Encoding UTF8 }
    }

    Invoke-Case "DM_CONFIG doctor" {
        $configDir = Join-Path $WorkDir "config"
        New-Item -ItemType Directory -Force -Path $configDir | Out-Null
        $configFile = Join-Path $configDir "dm.yaml"
        $outDir = (Join-Path $WorkDir "configured-output").Replace("\", "/")
        Set-Content -LiteralPath $configFile -Encoding UTF8 -Value "output_dir: '$outDir'`nlog_json: false`n"
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

    Invoke-Case "old logs-scan rejected" { & $DmBin logs-scan --help } -ExpectFailure
    Invoke-Case "old inspect-diff rejected" { & $DmBin inspect-diff --help } -ExpectFailure
    Invoke-Case "old prune-report rejected" { & $DmBin prune-report --help } -ExpectFailure
    Invoke-Case "old registry-login-check rejected" { & $DmBin registry-login-check --help } -ExpectFailure
    Invoke-Case "old global json rejected" { & $DmBin --json version } -ExpectFailure

    Invoke-Case "PowerShell script parse" {
        foreach ($file in Get-ChildItem -LiteralPath (Join-Path $RootDir "scripts") -Filter *.ps1) {
            $tokens = $null
            $errors = $null
            [System.Management.Automation.Language.Parser]::ParseFile($file.FullName, [ref]$tokens, [ref]$errors) | Out-Null
            if ($errors.Count -gt 0) {
                throw "$($file.Name) parse errors: $($errors | ConvertTo-Json -Compress)"
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
            $installOptions = @{
                Binary = $DmBin
                InstallDir = $installRoot
                ConfigDir = $configRoot
                NoPathUpdate = $true
                NoCompletionProfile = $true
            }
            if ($NoEnvironment) { $installOptions.NoEnvironment = $true }
            $beforeInstallEnvironment = @{}
            foreach ($name in $EnvironmentNames) {
                $beforeInstallEnvironment[$name] = [Environment]::GetEnvironmentVariable($name, "User")
            }
            & (Join-Path $RootDir "scripts/install.ps1") @installOptions
            & (Join-Path $installRoot "bin/dm.exe") version
            if (Test-Path -LiteralPath (Join-Path $installRoot "bin/dm.cmd")) {
                throw "legacy dm.cmd wrapper should not be installed"
            }
            $completion = Join-Path $installRoot "completions/dm-completion.ps1"
            if (-not (Test-Path -LiteralPath $completion)) {
                throw "completion file was not created"
            }
            if (-not $NoEnvironment) {
                $foreignHome = Join-Path $WorkDir "foreign-dm-home"
                [Environment]::SetEnvironmentVariable("DM_HOME", $foreignHome, "User")
            }
            & (Join-Path $RootDir "scripts/uninstall.ps1") -InstallDir $installRoot -ConfigDir $configRoot -Purge
            if ((Test-Path -LiteralPath $completion) -or (Test-Path -LiteralPath $configRoot) -or (Test-Path -LiteralPath $installRoot)) {
                throw "install artifacts were not cleaned"
            }
            foreach ($name in $EnvironmentNames) {
                $actual = [Environment]::GetEnvironmentVariable($name, "User")
                $want = if ($name -eq "DM_HOME" -and -not $NoEnvironment) { $foreignHome } else { $beforeInstallEnvironment[$name] }
                if ($actual -ne $want) {
                    throw "$name was not preserved/restored: got '$actual', want '$want'"
                }
            }
        } finally {
            Pop-Location
        }
    } -Skip:$SkipInstall

    Invoke-Case "install.ps1 multiple-install ownership" {
        $installA = Join-Path $WorkDir "install-a"
        $configA = Join-Path $WorkDir "install-a-config"
        $installB = Join-Path $WorkDir "install-b"
        $configB = Join-Path $WorkDir "install-b-config"
		$installC = Join-Path $WorkDir "install-c"
		$configC = Join-Path $WorkDir "install-c-config"
        $caseSnapshot = @{}
        foreach ($name in $EnvironmentNames) {
            $caseSnapshot[$name] = [Environment]::GetEnvironmentVariable($name, "User")
        }
        try {
            & (Join-Path $RootDir "scripts/install.ps1") -Binary $DmBin -InstallDir $installA -ConfigDir $configA -NoPathUpdate -NoCompletion
            & (Join-Path $RootDir "scripts/install.ps1") -Binary $DmBin -InstallDir $installB -ConfigDir $configB -NoPathUpdate -NoCompletion
			& (Join-Path $RootDir "scripts/install.ps1") -Binary $DmBin -InstallDir $installC -ConfigDir $configC -NoPathUpdate -NoCompletion
			$expectedC = @{
				DM_HOME = Join-Path $installC "data"
				DM_CONFIG = Join-Path $configC "dm.yaml"
				DM_OUTPUT_DIR = Join-Path $installC "data/images"
            }
            & (Join-Path $RootDir "scripts/uninstall.ps1") -InstallDir $installA -ConfigDir $configA -Purge
            foreach ($name in $EnvironmentNames) {
                $actual = [Environment]::GetEnvironmentVariable($name, "User")
				if ($actual -ne $expectedC[$name]) {
					throw "uninstalling A changed C-owned ${name}: got '$actual', want '$($expectedC[$name])'"
                }
            }
            & (Join-Path $RootDir "scripts/uninstall.ps1") -InstallDir $installB -ConfigDir $configB -Purge
			foreach ($name in $EnvironmentNames) {
				$actual = [Environment]::GetEnvironmentVariable($name, "User")
				if ($actual -ne $expectedC[$name]) {
					throw "uninstalling B changed C-owned ${name}: got '$actual', want '$($expectedC[$name])'"
				}
			}
			& (Join-Path $RootDir "scripts/uninstall.ps1") -InstallDir $installC -ConfigDir $configC -Purge
            foreach ($name in $EnvironmentNames) {
                $actual = [Environment]::GetEnvironmentVariable($name, "User")
                if ($actual -ne $caseSnapshot[$name]) {
					throw "uninstalling C did not restore original ${name}: got '$actual', want '$($caseSnapshot[$name])'"
                }
            }
        } finally {
            foreach ($name in $EnvironmentNames) {
                if ($null -ne $caseSnapshot[$name]) {
                    [Environment]::SetEnvironmentVariable($name, [string]$caseSnapshot[$name], "User")
                } else {
                    [Environment]::SetEnvironmentVariable($name, [System.Management.Automation.Language.NullString]::Value, "User")
                }
            }
			Remove-Item -LiteralPath $installA, $configA, $installB, $configB, $installC, $configC -Recurse -Force -ErrorAction SilentlyContinue
        }
    } -Skip:($SkipInstall -or $NoEnvironment)

	Invoke-Case "install.ps1 file transaction rollback" {
		$installRoot = Join-Path $WorkDir "install-[literal]-rollback"
		$configRoot = Join-Path $WorkDir "install-[literal]-rollback-config"
		$installedBin = Join-Path $installRoot "bin/dm.exe"
		$configFile = Join-Path $configRoot "dm.yaml"
		$manifestDir = Join-Path $configRoot "install.json"
		New-Item -ItemType Directory -Force -Path (Split-Path -Parent $installedBin), $manifestDir | Out-Null
		Set-Content -LiteralPath $installedBin -Value "old-binary" -Encoding ASCII
		Set-Content -LiteralPath $configFile -Value "old-config" -Encoding ASCII
		Set-Content -LiteralPath (Join-Path $manifestDir "sentinel") -Value "keep" -Encoding ASCII
		$rollbackEnvironment = @{}
		foreach ($name in $EnvironmentNames) {
			$rollbackEnvironment[$name] = [Environment]::GetEnvironmentVariable($name, "User")
		}
		$failed = $false
		try {
			$rollbackOptions = @{
				Binary = $DmBin
				InstallDir = $installRoot
				ConfigDir = $configRoot
				NoPathUpdate = $true
				NoCompletion = $true
				OverwriteConfig = $true
			}
			if ($NoEnvironment) { $rollbackOptions.NoEnvironment = $true }
			& (Join-Path $RootDir "scripts/install.ps1") @rollbackOptions
		} catch {
			$failed = $true
		}
		if (-not $failed) { throw "install unexpectedly succeeded with a directory at install.json" }
		if ((Get-Content -LiteralPath $installedBin -Raw).Trim() -ne "old-binary") { throw "binary was not rolled back" }
		if ((Get-Content -LiteralPath $configFile -Raw).Trim() -ne "old-config") { throw "config was not rolled back" }
		if (-not (Test-Path -LiteralPath (Join-Path $manifestDir "sentinel") -PathType Leaf)) { throw "manifest directory was not restored" }
		if (-not $NoEnvironment) {
			foreach ($name in $EnvironmentNames) {
				$actual = [Environment]::GetEnvironmentVariable($name, "User")
				if ($actual -ne $rollbackEnvironment[$name]) {
					throw "$name was not rolled back after manifest commit failure"
				}
			}
		}
	} -Skip:$SkipInstall

	Invoke-Case "uninstall.ps1 rejects forged environment ownership" {
		$installRoot = Join-Path $WorkDir "forged-install"
		$configRoot = Join-Path $WorkDir "forged-config"
		$binDir = Join-Path $installRoot "bin"
		$dataDir = Join-Path $installRoot "data"
		[System.IO.Directory]::CreateDirectory($binDir) | Out-Null
		[System.IO.Directory]::CreateDirectory($configRoot) | Out-Null
		$pathBefore = [Environment]::GetEnvironmentVariable("PATH", "User")
		$forgedManifest = [ordered]@{
			install_id = "00000000000000000000000000000000"
			install_dir = $installRoot
			bin_dir = $binDir
			installed_bin = Join-Path $binDir "dm.exe"
			config_dir = $configRoot
			config_file = Join-Path $configRoot "dm.yaml"
			data_dir = $dataDir
			output_dir = Join-Path $dataDir "images"
			scope = "User"
			environment_variables = @([ordered]@{
				name = "PATH"
				value = $pathBefore
				previous_present = $false
				previous_value = $null
			})
			path_entry_added = $false
			completion_records = @()
		}
		$manifestPath = Join-Path $configRoot "install.json"
		$forgedManifest | ConvertTo-Json -Depth 5 | Set-Content -LiteralPath $manifestPath -Encoding UTF8
		$rejected = $false
		try {
			& (Join-Path $RootDir "scripts/uninstall.ps1") -InstallDir $installRoot -ConfigDir $configRoot -Purge
		} catch {
			$rejected = $true
		}
		if (-not $rejected) { throw "forged environment ownership was accepted" }
		if ([Environment]::GetEnvironmentVariable("PATH", "User") -ne $pathBefore) { throw "PATH changed after forged manifest rejection" }
		if (-not (Test-Path -LiteralPath $manifestPath -PathType Leaf)) { throw "forged manifest validation was not fail-closed" }
	} -Skip:$SkipInstall

    Invoke-Case "install.ps1 rejects junction binary directory" {
        $caseRoot = Join-Path $WorkDir "junction-install-boundary"
        $installRoot = Join-Path $caseRoot "install"
        $configRoot = Join-Path $caseRoot "config"
        $externalRoot = Join-Path $caseRoot "external-bin"
        $binLink = Join-Path $installRoot "bin"
        [System.IO.Directory]::CreateDirectory($installRoot) | Out-Null
        [System.IO.Directory]::CreateDirectory($configRoot) | Out-Null
        [System.IO.Directory]::CreateDirectory($externalRoot) | Out-Null
        $sentinel = Join-Path $externalRoot "sentinel.txt"
        Set-Content -LiteralPath $sentinel -Value "keep" -Encoding ASCII
        New-LocalTestJunction -Path $binLink -Target $externalRoot
        try {
            $rejected = $false
            try {
                & (Join-Path $RootDir "scripts/install.ps1") -Binary $DmBin -InstallDir $installRoot -ConfigDir $configRoot -NoEnvironment -NoPathUpdate -NoCompletion
            } catch {
                $rejected = $true
            }
            if (-not $rejected) { throw "install accepted a junction binary directory" }
            if (Test-Path -LiteralPath (Join-Path $externalRoot "dm.exe") -PathType Leaf) {
                throw "install wrote dm.exe through the junction boundary"
            }
            if ((Get-Content -LiteralPath $sentinel -Raw).Trim() -ne "keep") {
                throw "install changed the external junction sentinel"
            }
        } finally {
            Remove-LocalTestJunction -Path $binLink
        }
    } -Skip:($SkipInstall -or -not $JunctionTestsAvailable) -SkipReason $JunctionSkipReason

    Invoke-Case "uninstall.ps1 rejects nested config junction before purge" {
        $caseRoot = Join-Path $WorkDir "junction-config-purge"
        $installRoot = Join-Path $caseRoot "install"
        $configRoot = Join-Path $caseRoot "config"
        $externalRoot = Join-Path $caseRoot "external-config"
        $configLink = Join-Path $configRoot "nested-link"
        [System.IO.Directory]::CreateDirectory($externalRoot) | Out-Null
        $sentinel = Join-Path $externalRoot "sentinel.txt"
        Set-Content -LiteralPath $sentinel -Value "keep" -Encoding ASCII
        try {
            & (Join-Path $RootDir "scripts/install.ps1") -Binary $DmBin -InstallDir $installRoot -ConfigDir $configRoot -NoEnvironment -NoPathUpdate -NoCompletion
            New-LocalTestJunction -Path $configLink -Target $externalRoot
            $rejected = $false
            try {
                & (Join-Path $RootDir "scripts/uninstall.ps1") -InstallDir $installRoot -ConfigDir $configRoot -Purge
            } catch {
                $rejected = $true
            }
            if (-not $rejected) { throw "purge accepted a nested config junction" }
            if (-not (Test-Path -LiteralPath (Join-Path $installRoot "bin/dm.exe") -PathType Leaf)) {
                throw "purge mutated the installation before rejecting the config junction"
            }
            if (-not (Test-Path -LiteralPath (Join-Path $configRoot "install.json") -PathType Leaf)) {
                throw "purge removed the manifest before rejecting the config junction"
            }
            if ((Get-Content -LiteralPath $sentinel -Raw).Trim() -ne "keep") {
                throw "purge changed the external config sentinel"
            }
        } finally {
            Remove-LocalTestJunction -Path $configLink
            try {
                & (Join-Path $RootDir "scripts/uninstall.ps1") -InstallDir $installRoot -ConfigDir $configRoot -Purge
            } catch {
                Write-Warning "Junction config test cleanup was incomplete: $($_.Exception.Message)"
            }
        }
    } -Skip:($SkipInstall -or -not $JunctionTestsAvailable) -SkipReason $JunctionSkipReason

    Invoke-Case "uninstall.ps1 rejects data directory junction before purge" {
        $caseRoot = Join-Path $WorkDir "junction-data-purge"
        $installRoot = Join-Path $caseRoot "install"
        $configRoot = Join-Path $caseRoot "config"
        $dataRoot = Join-Path $caseRoot "data"
        $externalRoot = Join-Path $caseRoot "external-data"
        [System.IO.Directory]::CreateDirectory($externalRoot) | Out-Null
        $sentinel = Join-Path $externalRoot "sentinel.txt"
        Set-Content -LiteralPath $sentinel -Value "keep" -Encoding ASCII
        try {
            & (Join-Path $RootDir "scripts/install.ps1") -Binary $DmBin -InstallDir $installRoot -ConfigDir $configRoot -DataDir $dataRoot -NoEnvironment -NoPathUpdate -NoCompletion
            Remove-LocalTestTreeSafely -Path $dataRoot
            New-LocalTestJunction -Path $dataRoot -Target $externalRoot
            $rejected = $false
            try {
                & (Join-Path $RootDir "scripts/uninstall.ps1") -InstallDir $installRoot -ConfigDir $configRoot -DataDir $dataRoot -Purge
            } catch {
                $rejected = $true
            }
            if (-not $rejected) { throw "purge accepted a junction data directory" }
            if (-not (Test-Path -LiteralPath (Join-Path $installRoot "bin/dm.exe") -PathType Leaf)) {
                throw "purge mutated the installation before rejecting the data junction"
            }
            if ((Get-Content -LiteralPath $sentinel -Raw).Trim() -ne "keep") {
                throw "purge changed the external data sentinel"
            }
        } finally {
            Remove-LocalTestJunction -Path $dataRoot
            try {
                & (Join-Path $RootDir "scripts/uninstall.ps1") -InstallDir $installRoot -ConfigDir $configRoot -DataDir $dataRoot -Purge
            } catch {
                Write-Warning "Junction data test cleanup was incomplete: $($_.Exception.Message)"
            }
        }
    } -Skip:($SkipInstall -or -not $JunctionTestsAvailable) -SkipReason $JunctionSkipReason

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

    $rows = Import-Csv -LiteralPath $ResultsFile -Delimiter "`t"
    $report = New-Object System.Collections.Generic.List[string]
    $report.Add("# docker-manager local test report")
    $report.Add("")
    $report.Add("- Generated at: ``$((Get-Date).ToString("s"))``")
    $report.Add("- Platform: ``$([System.Runtime.InteropServices.RuntimeInformation]::OSDescription.Trim())``")
    $report.Add("- Go: ``$((go version) -join " ")``")
    $report.Add("- Docker command: ``$(if (Test-CommandExists docker) { "available" } else { "missing" })``")
    $report.Add("- Bash command: ``$(if (Test-CommandExists bash) { "available" } else { "missing" })``")
    $report.Add("- Passed: ``$script:Passed``")
    $report.Add("- Expected failures: ``$script:ExpectedFailures``")
    $report.Add("- Skipped: ``$script:Skipped``")
    $report.Add("- Failed: ``$script:Failures``")
    $report.Add("")
    $report.Add("| Case | Status | Exit | Seconds | Log |")
    $report.Add("| --- | --- | --- | --- | --- |")
    foreach ($row in $rows) {
        $report.Add("| $($row.case) | $($row.status) | $($row.exit_code) | $($row.seconds) | $($row.log) |")
    }
    $report | Set-Content -LiteralPath $ReportFile -Encoding UTF8

    Write-Host "Report: $ReportFile"
    Write-Host "Results: $ResultsFile"
    if ($script:Failures -gt 0) {
        exit 1
    }
} finally {
    try {
        if (-not $KeepWorkDir) {
            try {
                Remove-LocalTestTreeSafely -Path $WorkDir
            } catch {
                Write-Warning "Work dir was kept because safe cleanup failed: $WorkDir ($($_.Exception.Message))"
            }
        } else {
            Write-Host "Work dir kept: $WorkDir"
        }
    } finally {
        foreach ($item in $EnvironmentSnapshot) {
            if ($item.Present) {
                [Environment]::SetEnvironmentVariable($item.Name, [string]$item.Value, $item.Scope)
            } else {
                [Environment]::SetEnvironmentVariable($item.Name, [System.Management.Automation.Language.NullString]::Value, $item.Scope)
            }
        }
        $OutputEncoding = $oldOutputEncoding
        [Console]::OutputEncoding = $oldConsoleOutputEncoding
        if ($hadOutFileEncoding) {
            $PSDefaultParameterValues["Out-File:Encoding"] = $oldOutFileEncoding
        } else {
            $PSDefaultParameterValues.Remove("Out-File:Encoding")
        }
    }
}
