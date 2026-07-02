param(
    [string]$DmBin = $(Join-Path (Resolve-Path (Join-Path $PSScriptRoot "..")) "dm.exe"),
    [string]$WorkDir = $(Join-Path ([System.IO.Path]::GetTempPath()) ("dm-completion-" + [guid]::NewGuid().ToString("N"))),
    [switch]$NoDocker,
    [switch]$KeepWorkDir
)

$ErrorActionPreference = "Stop"
$Pass = 0
$Fail = 0
$Skip = 0
$Results = @()
$Cleanup = New-Object System.Collections.Generic.List[scriptblock]

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

function Invoke-Case {
    param(
        [string]$Name,
        [string]$Want,
        [scriptblock]$Body
    )
    $log = Join-Path $WorkDir "$Name.log"
    try {
        $previousErrorActionPreference = $ErrorActionPreference
        $ErrorActionPreference = "Continue"
        try {
            $output = & $Body 2>&1 | Out-String
        } finally {
            $ErrorActionPreference = $previousErrorActionPreference
        }
        $output | Set-Content -Path $log -Encoding UTF8
        if ($output.Contains($Want)) {
            Add-Result $Name "PASS" "found $Want" $log
        } else {
            Add-Result $Name "FAIL" "want $Want" $log
        }
    } catch {
        $_ | Out-String | Set-Content -Path $log -Encoding UTF8
        Add-Result $Name "FAIL" $_.Exception.Message $log
    }
}

function Test-CompletionOutput {
    param(
        [string]$Name,
        [string]$Want,
        [object[]]$Output
    )
    $log = Join-Path $WorkDir "$Name.log"
    ($Output | Out-String) | Set-Content -Path $log -Encoding UTF8
    if (($Output -join "`n").Contains($Want)) {
        Add-Result $Name "PASS" "found $Want" $log
    } else {
        Add-Result $Name "FAIL" "want $Want" $log
    }
}

New-Item -ItemType Directory -Force -Path $WorkDir | Out-Null
$DmBin = (Resolve-Path $DmBin).Path

try {
    Invoke-Case "generate-powershell" "Register-ArgumentCompleter" { & $DmBin completion powershell }
    Invoke-Case "generate-bash" "__start_dm" { & $DmBin completion bash }
    Invoke-Case "generate-zsh" "_dm" { & $DmBin completion zsh }
    Invoke-Case "generate-fish" "complete -c dm" { & $DmBin completion fish }

    $oldPath = $env:PATH
    $env:PATH = (Split-Path $DmBin) + [IO.Path]::PathSeparator + $oldPath
    try {
        Invoke-Case "powershell-command-complete" "report" { & $DmBin __completeNoDesc re }

        $docker = Get-Command docker -ErrorAction SilentlyContinue
        if ($NoDocker -or -not $docker) {
            Add-Result "docker-resource-complete" "SKIP" "Docker unavailable or skipped"
        } else {
            $dockerInfo = & docker info 2>$null
            if ($LASTEXITCODE -ne 0) {
                Add-Result "docker-resource-complete" "SKIP" "Docker daemon unavailable"
            } else {
                $suffix = Get-Date -Format "yyyyMMddHHmmss"
                $containerName = "dm_completion_api_$suffix"
                $volumeName = "dm_completion_vol_$suffix"
                $imageRef = (& docker images --format "{{.Repository}}:{{.Tag}}" | Where-Object { $_ -and $_ -notmatch "<none>" } | Select-Object -First 1)
                if (-not $imageRef) {
                    Add-Result "docker-resource-complete" "SKIP" "No local tagged images; no external pull attempted"
                } else {
                    & docker volume create --label "dm.completion=$suffix" $volumeName | Out-Null
                    $Cleanup.Add({ & docker volume rm $volumeName | Out-Null })
                    & docker run -d --name $containerName --label "dm.completion=$suffix" $imageRef sh -c "sleep 3600" | Out-Null
                    if ($LASTEXITCODE -eq 0) {
                        $Cleanup.Add({ & docker rm -f $containerName | Out-Null })
                        Invoke-Case "powershell-container-complete" $containerName { & $DmBin __completeNoDesc backup dm_completion }
                    } else {
                        Add-Result "powershell-container-complete" "SKIP" "Could not start test container from $imageRef"
                    }
                    $prefix = $imageRef.Substring(0, [Math]::Min(4, $imageRef.Length))
                    Invoke-Case "powershell-image-filter-complete" $imageRef { & $DmBin __completeNoDesc save --filter $prefix }
                    Invoke-Case "powershell-volume-filter-complete" $volumeName { & $DmBin __completeNoDesc volumes --filter "" }
                }
            }
        }
    } finally {
        $env:PATH = $oldPath
    }
} finally {
    foreach ($item in $Cleanup) {
        try { & $item } catch { }
    }
}

$resultsPath = Join-Path $WorkDir "results.tsv"
$Results | ForEach-Object { "$($_.Case)`t$($_.Status)`t$($_.Note)`t$($_.Log)" } |
    Set-Content -Path $resultsPath -Encoding UTF8

$report = Join-Path $WorkDir "completion-test-report.md"
@(
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
) + ($Results | ForEach-Object {
    "| $($_.Case) | $($_.Status) | $($_.Note) | $([IO.Path]::GetFileName($_.Log)) |"
}) | Set-Content -Path $report -Encoding UTF8

Get-Content $report

if (-not $KeepWorkDir) {
    Remove-Item -Recurse -Force $WorkDir -ErrorAction SilentlyContinue
}
if ($Fail -gt 0) {
    exit 1
}
