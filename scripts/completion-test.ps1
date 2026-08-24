param(
    [string]$DmBin = $(Join-Path (Resolve-Path -LiteralPath (Join-Path $PSScriptRoot "..")).Path "dm.exe"),
    [string]$WorkDir = $(Join-Path ([System.IO.Path]::GetTempPath()) ("dm-completion-" + [guid]::NewGuid().ToString("N"))),
    [switch]$NoDocker,
    [switch]$KeepWorkDir,
    [switch]$RequireDocker
)

$ErrorActionPreference = "Stop"
if ($NoDocker -and $RequireDocker) {
    throw "-NoDocker and -RequireDocker cannot be used together."
}
$oldOutputEncoding = $OutputEncoding
$oldConsoleOutputEncoding = [Console]::OutputEncoding
$utf8NoBom = New-Object System.Text.UTF8Encoding($false)
$OutputEncoding = $utf8NoBom
[Console]::OutputEncoding = $utf8NoBom
$RootDir = (Resolve-Path -LiteralPath (Join-Path $PSScriptRoot "..")).Path
$Pass = 0
$Fail = 0
$Skip = 0
$Results = @()
$Cleanup = New-Object System.Collections.Generic.List[scriptblock]
$WorkDirOwned = $false
$WorkDirToken = "dm-completion:${PID}:$([guid]::NewGuid().ToString('N'))"
$WorkDirSentinel = $null

function Write-Utf8Text {
    param(
        [string]$Path,
        [AllowEmptyString()]
        [string]$Text
    )
    $normalized = $Text.Replace("`r`n", "`n").Replace("`r", "`n")
    if ($normalized.Length -gt 0 -and -not $normalized.EndsWith("`n")) {
        $normalized += "`n"
    }
    [System.IO.File]::WriteAllText($Path, $normalized, $utf8NoBom)
}

function Get-NormalizedPath {
    param([string]$Path)
    $full = [System.IO.Path]::GetFullPath($Path)
    $root = [System.IO.Path]::GetPathRoot($full)
    if ($full.Length -le $root.Length) { return $root }
    return $full.TrimEnd(
        [System.IO.Path]::DirectorySeparatorChar,
        [System.IO.Path]::AltDirectorySeparatorChar
    )
}

function Test-ProtectedWorkDir {
    param([string]$Path)
    $candidate = Get-NormalizedPath $Path
    $protected = @(
        (Get-NormalizedPath ([System.IO.Path]::GetPathRoot($candidate))),
        (Get-NormalizedPath $RootDir)
    )
    foreach ($userRoot in @($env:USERPROFILE, $HOME, [Environment]::GetFolderPath("UserProfile"))) {
        if ($userRoot) { $protected += Get-NormalizedPath $userRoot }
    }
    foreach ($item in $protected) {
        if ($candidate.Equals($item, [System.StringComparison]::OrdinalIgnoreCase)) { return $true }
    }
    return $false
}

function New-OwnedWorkDir {
    param([string]$Path)
    $candidate = Get-NormalizedPath $Path
    if (Test-ProtectedWorkDir $candidate) {
        throw "Refusing protected completion work directory: $candidate"
    }
    $existing = Get-Item -LiteralPath $candidate -Force -ErrorAction SilentlyContinue
    if ($existing -or (Test-Path -LiteralPath $candidate -ErrorAction SilentlyContinue)) {
        throw "Completion work directory must not already exist: $candidate"
    }
    $parent = Split-Path -Parent $candidate
    if (-not $parent -or -not (Test-Path -LiteralPath $parent -PathType Container)) {
        throw "Completion work directory parent does not exist: $parent"
    }
    New-Item -ItemType Directory -Path $candidate -ErrorAction Stop | Out-Null
    $created = Get-Item -LiteralPath $candidate -Force
    if (($created.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) {
        Remove-Item -LiteralPath $candidate -Force -ErrorAction SilentlyContinue
        throw "Completion work directory must not be a symbolic link: $candidate"
    }
    $script:WorkDir = $candidate
    $script:WorkDirSentinel = Join-Path $candidate ".dm-completion-owned"
    try {
        Set-Content -LiteralPath $script:WorkDirSentinel -Value $WorkDirToken -Encoding ASCII -NoNewline
        $script:WorkDirOwned = $true
    } catch {
        if (Test-Path -LiteralPath $script:WorkDirSentinel) {
            Remove-Item -LiteralPath $script:WorkDirSentinel -Force -ErrorAction SilentlyContinue
        }
        Remove-Item -LiteralPath $candidate -Force -ErrorAction SilentlyContinue
        throw
    }
}

function Remove-OwnedWorkDir {
    if (-not $WorkDirOwned -or -not $WorkDir -or -not (Test-Path -LiteralPath $WorkDir -PathType Container)) {
        Write-Warning "Refusing to remove unowned completion work directory: $WorkDir"
        return
    }
    $directory = Get-Item -LiteralPath $WorkDir -Force
    $sentinel = Get-Item -LiteralPath $WorkDirSentinel -Force -ErrorAction SilentlyContinue
    if ((Test-ProtectedWorkDir $WorkDir) -or
        (($directory.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) -or
        -not $sentinel -or
        (($sentinel.Attributes -band [System.IO.FileAttributes]::ReparsePoint) -ne 0) -or
        ((Get-Content -LiteralPath $WorkDirSentinel -Raw) -ne $WorkDirToken)) {
        Write-Warning "Refusing to remove unsafe completion work directory: $WorkDir"
        return
    }
    Remove-Item -LiteralPath $WorkDir -Recurse -Force
    $script:WorkDirOwned = $false
}

function Add-Result {
    param(
        [string]$Name,
        [string]$Status,
        [string]$Note,
        [string]$Log = ""
    )
    $script:Results += [pscustomobject]@{ Case = $Name; Status = $Status; Note = $Note; Log = $Log }
    switch ($Status) {
        "PASS" { $script:Pass++ }
        "FAIL" { $script:Fail++ }
        "SKIP" { $script:Skip++ }
    }
    Write-Host "$Name $Status $Note"
}

function Add-UnavailableResult {
    param(
        [string]$Name,
        [string]$Note,
        [bool]$Required
    )
    if ($Required) {
        Add-Result $Name "FAIL" $Note
    } else {
        Add-Result $Name "SKIP" $Note
    }
}

function Invoke-Case {
    param(
        [string]$Name,
        [string]$Want,
        [scriptblock]$Body,
        [string]$OutputPath = ""
    )
    $log = Join-Path $WorkDir "$Name.log"
    try {
        $previousErrorActionPreference = $ErrorActionPreference
        $ErrorActionPreference = "Continue"
        try {
            $global:LASTEXITCODE = 0
            $output = & $Body 2>&1 | Out-String
            $exitCode = if ($LASTEXITCODE -is [int]) { $LASTEXITCODE } else { 0 }
        } finally {
            $ErrorActionPreference = $previousErrorActionPreference
        }
        Write-Utf8Text -Path $log -Text $output
        if ($OutputPath) {
            Write-Utf8Text -Path $OutputPath -Text $output
        }
        if ($exitCode -eq 0 -and $output.Contains($Want)) {
            Add-Result $Name "PASS" "found $Want" $log
        } else {
            Add-Result $Name "FAIL" "rc=$exitCode; want $Want" $log
        }
    } catch {
        Write-Utf8Text -Path $log -Text ($_ | Out-String)
        Add-Result $Name "FAIL" $_.Exception.Message $log
    }
}

try {
New-OwnedWorkDir $WorkDir
$DmBin = (Resolve-Path -LiteralPath $DmBin -ErrorAction Stop).Path
if (-not (Test-Path -LiteralPath $DmBin -PathType Leaf)) { throw "dm binary is not a file: $DmBin" }
$powerShellCompletion = Join-Path $WorkDir "dm-completion.ps1"
$dmCommand = Join-Path $WorkDir "dm.exe"
Copy-Item -LiteralPath $DmBin -Destination $dmCommand

try {
    Invoke-Case "generate-powershell" "Register-ArgumentCompleter" { & $DmBin completion powershell } -OutputPath $powerShellCompletion
    Invoke-Case "generate-bash" "__start_dm" { & $DmBin completion bash }
    Invoke-Case "generate-zsh" "_dm" { & $DmBin completion zsh }
    Invoke-Case "generate-fish" "complete -c dm" { & $DmBin completion fish }

    $oldPath = $env:PATH
    $env:PATH = $WorkDir + [IO.Path]::PathSeparator + $oldPath
    try {
        Invoke-Case "powershell-command-complete" "report" { & $DmBin __completeNoDesc re }
        Invoke-Case "powershell-script-load" "report" {
            . $powerShellCompletion
            (TabExpansion2 -inputScript "dm re" -cursorColumn 5).CompletionMatches |
                ForEach-Object { $_.CompletionText }
        }

        $docker = Get-Command docker -ErrorAction SilentlyContinue
        if ($NoDocker) {
            Add-Result "docker-resource-complete" "SKIP" "Docker checks explicitly skipped"
        } elseif (-not $docker) {
            Add-UnavailableResult "docker-resource-complete" "docker command not found" $RequireDocker
        } else {
            $dockerInfo = & docker info 2>$null
            if ($LASTEXITCODE -ne 0) {
                Add-UnavailableResult "docker-resource-complete" "Docker daemon unavailable" $RequireDocker
            } else {
                $suffix = "$(Get-Date -Format 'yyyyMMddHHmmss')_${PID}_$([guid]::NewGuid().ToString('N').Substring(0, 8))"
                $containerName = "dm_completion_api_$suffix"
                $volumeName = "dm_completion_vol_$suffix"
                $imageRef = (& docker images --format "{{.Repository}}:{{.Tag}}" | Where-Object { $_ -and $_ -notmatch "<none>" } | Select-Object -First 1)
                & docker volume create --label "dm.completion=$suffix" $volumeName | Out-Null
                if ($LASTEXITCODE -eq 0) {
                    $volumeToRemove = $volumeName
                    $Cleanup.Add(({ & docker volume rm $volumeToRemove | Out-Null }.GetNewClosure()))
                    Invoke-Case "powershell-volume-filter-complete" $volumeName { & $DmBin __completeNoDesc volumes --filter "" }
                } else {
                    Add-UnavailableResult "powershell-volume-filter-complete" "Could not create test volume" $RequireDocker
                }

                if (-not $imageRef) {
                    Add-UnavailableResult "docker-image-resource-complete" "No local tagged images; no external pull attempted" $RequireDocker
                } else {
                    $prefix = $imageRef.Substring(0, [Math]::Min(4, $imageRef.Length))
                    Invoke-Case "powershell-image-filter-complete" $imageRef { & $DmBin __completeNoDesc save --filter $prefix }
                    $containerToRemove = $containerName
                    $Cleanup.Add(({ & docker rm -f $containerToRemove | Out-Null }.GetNewClosure()))
                    & docker run -d --name $containerName --label "dm.completion=$suffix" $imageRef sh -c "sleep 3600" | Out-Null
                    if ($LASTEXITCODE -eq 0) {
                        Invoke-Case "powershell-container-complete" $containerName { & $DmBin __completeNoDesc backup dm_completion }
                    } else {
                        Add-UnavailableResult "powershell-container-complete" "Could not start test container from $imageRef" $RequireDocker
                    }
                }
            }
        }
    } finally {
        $env:PATH = $oldPath
    }
} finally {
    for ($i = $Cleanup.Count - 1; $i -ge 0; $i--) {
        try { & $Cleanup[$i] } catch { }
    }
}

$resultsPath = Join-Path $WorkDir "results.tsv"
$resultLines = @($Results | ForEach-Object { "$($_.Case)`t$($_.Status)`t$($_.Note)`t$($_.Log)" })
Write-Utf8Text -Path $resultsPath -Text ($resultLines -join "`n")

$report = Join-Path $WorkDir "completion-test-report.md"
$reportLines = @(
    "# dm completion test",
    "",
    "- Time: $(Get-Date -Format o)",
    "- Binary: $DmBin",
    "- Work dir: $WorkDir",
    "",
    "## Summary",
    "",
    "- PASS: $Pass",
    "- FAIL: $Fail",
    "- SKIP: $Skip",
    "",
    "## Results",
    "",
    "| Case | Status | Note | Log |",
    "| --- | --- | --- | --- |"
) + @($Results | ForEach-Object {
    "| $($_.Case) | $($_.Status) | $($_.Note) | $([IO.Path]::GetFileName($_.Log)) |"
})
Write-Utf8Text -Path $report -Text ($reportLines -join "`n")

Write-Host ([System.IO.File]::ReadAllText($report, $utf8NoBom))

if ($Fail -gt 0) {
    exit 1
}
} finally {
    try {
        if (-not $KeepWorkDir -and $WorkDirOwned) {
            Remove-OwnedWorkDir
        }
    } finally {
        $OutputEncoding = $oldOutputEncoding
        [Console]::OutputEncoding = $oldConsoleOutputEncoding
    }
}
