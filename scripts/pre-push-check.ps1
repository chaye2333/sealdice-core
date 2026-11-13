<#
.SYNOPSIS
    提交前自检：把 GitHub Actions（Test & Lint / UI checks）的步骤在本地跑一遍。

.DESCRIPTION
    CI 的 golangci-lint 只在 push 时运行，PR 里看不到失败；所以在本地复刻一遍。
    golangci-lint 必须 v2.15.0（.golangci.toml 用了 exhaustive.check 的 mapliteral 写法），
    脚本会自己找 $env:USERPROFILE\go\bin 下的那一个，并校验版本。

.PARAMETER SkipUi
    跳过 ui 的 check:prepared（只改 Go 的时候用）。

.PARAMETER SkipTests
    跳过 go test（只想快速看编译 + lint 时用）。

.EXAMPLE
    pwsh scripts/pre-push-check.ps1
    pwsh scripts/pre-push-check.ps1 -SkipUi
#>
[CmdletBinding()]
param(
    [switch]$SkipUi,
    [switch]$SkipTests
)

$ErrorActionPreference = 'Stop'
$requiredLintVersion = '2.15.0'
$repoRoot = Split-Path -Parent $PSScriptRoot
Push-Location $repoRoot
$failed = New-Object System.Collections.Generic.List[string]

function Invoke-Step {
    param(
        [Parameter(Mandatory)][string]$Name,
        [Parameter(Mandatory)][scriptblock]$Action
    )
    Write-Host ""
    Write-Host "=== $Name ===" -ForegroundColor Cyan
    & $Action
    if ($LASTEXITCODE -ne 0) {
        Write-Host "!!! $Name 失败 (exit $LASTEXITCODE)" -ForegroundColor Red
        $failed.Add($Name)
    } else {
        Write-Host "ok  $Name" -ForegroundColor Green
    }
}

function Resolve-GolangciLint {
    $candidates = @(
        (Join-Path $env:USERPROFILE 'go\bin\golangci-lint.exe'),
        'golangci-lint'
    )
    foreach ($candidate in $candidates) {
        $cmd = Get-Command $candidate -ErrorAction SilentlyContinue
        if (-not $cmd) { continue }
        $versionLine = & $cmd.Source --version 2>&1 | Out-String
        if ($versionLine -match 'version\s+v?([0-9]+\.[0-9]+\.[0-9]+)') {
            if ($Matches[1] -eq $requiredLintVersion) {
                return $cmd.Source
            }
            Write-Host "跳过 $($cmd.Source)（$($Matches[1])，需要 $requiredLintVersion）" -ForegroundColor Yellow
        }
    }
    return $null
}

try {
    Invoke-Step 'go build ./...' { go build ./... }
    Invoke-Step 'go vet ./...' { go vet ./... }

    if (-not $SkipTests) {
        Invoke-Step 'go test ./...' { go test ./... }
    }

    $lint = Resolve-GolangciLint
    if (-not $lint) {
        Write-Host ""
        Write-Host '!!! 找不到 golangci-lint v2.15.0' -ForegroundColor Red
        Write-Host '    go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0'
        $failed.Add("golangci-lint v$requiredLintVersion 未安装")
    } else {
        Invoke-Step "golangci-lint run ($requiredLintVersion)" {
            & $lint config verify
            if ($LASTEXITCODE -ne 0) { return }
            & $lint run --timeout 20m
        }
    }

    if (-not $SkipUi) {
        Invoke-Step 'ui check:prepared' {
            Push-Location (Join-Path $repoRoot 'ui')
            try {
                npx -y pnpm@10.14.0 run check:prepared
            } finally {
                Pop-Location
            }
        }
    }

    Write-Host ""
    if ($failed.Count -gt 0) {
        Write-Host "自检未通过：$($failed -join '、')" -ForegroundColor Red
        exit 1
    }
    Write-Host '全部通过，可以提交。' -ForegroundColor Green
} finally {
    Pop-Location
}
