# 本地构建 Windows 版（含新管理界面）的脚本
#
# 从上游 #1689 起，管理界面是**仓库里的 ui/**（新 UI），编译产物写入 static/v2ui/dist，
# 由 static/static.go 的 `go:embed all:v2ui` 打进 exe。老流程（把外部 dist 拷到
# static/frontend）已废弃——那条路现在只会塞一个占位页进去。
#
# 构建顺序必须是（对应 Taskfile.yml 的 build-with-v2ui）：
#   1. go run . --gen-openapi=./ui/openapi.json   （前端契约只能由 Go 导出）
#   2. pnpm --dir ui install --frozen-lockfile
#   3. pnpm --dir ui run generate-client
#   4. pnpm --dir ui run build:embed:prepared     （产物 → static/v2ui/dist）
#   5. go build
#
# 用法（在仓库根目录）：
#     pwsh -File scripts\build-windows.ps1
#     pwsh -File scripts\build-windows.ps1 -SkipUi      # UI 没改动时跳过前三步
#     pwsh -File scripts\build-windows.ps1 -SkipZip
#
# 产物：
#     dist-build\sealdice-core.exe
#     dist-build\sealdice-core-<版本>-windows-amd64-<hash>.zip

[CmdletBinding()]
param(
    [switch]$SkipUi,
    [switch]$SkipZip
)

$ErrorActionPreference = "Stop"

# ---------- 路径与工具链 ----------
$repo = Split-Path -Parent $PSScriptRoot
Set-Location $repo
if ($env:OS -ne "Windows_NT") { throw "此脚本仅用于 Windows 本地构建" }

# 便携 Go（按需改）
$goBin = Join-Path $env:USERPROFILE "tools\go\bin"
if (Test-Path $goBin) { $env:Path = "$goBin;$env:Path" }
if (-not (Get-Command go -ErrorAction SilentlyContinue)) { throw "找不到 go，请把 Go 放进 PATH" }
if (-not $env:CGO_ENABLED) { $env:CGO_ENABLED = "0" }   # 纯 Go sqlite，免 gcc

# ---------- 准备 UI（仓库内 ui/） ----------
$uiDir = Join-Path $repo "ui"
$v2uiDist = Join-Path $repo "static\v2ui\dist"
if (-not (Test-Path $uiDir)) { throw "找不到 ui/ 目录：新 UI 应该就在仓库里（上游 #1689 起）" }

if ($SkipUi) {
    if (-not (Test-Path (Join-Path $v2uiDist "index.html"))) {
        throw "-SkipUi 要求 static\v2ui\dist 里已经有编译好的 UI"
    }
    Write-Host ">> 跳过 UI 编译，沿用 static\v2ui\dist"
} else {
    if (-not (Get-Command node -ErrorAction SilentlyContinue)) { throw "找不到 node，请安装 Node ^20.19 或 >=22.12" }

    # 用 ui/package.json 里钉住的 pnpm 版本（preinstall 会校验 major）
    $pkg = Get-Content (Join-Path $uiDir "package.json") -Raw | ConvertFrom-Json
    $pm = $pkg.packageManager
    if (-not $pm) { $pm = "pnpm@10.14.0" }
    Write-Host ">> 使用包管理器: $pm"

    $pnpm = @("npx", "-y", $pm)
    function Invoke-Pnpm([string[]]$pnpmArgs) {
        & $pnpm[0] $pnpm[1] $pnpm[2] @pnpmArgs
        if ($LASTEXITCODE -ne 0) { throw ("pnpm " + ($pnpmArgs -join ' ') + " 失败") }
    }

    Write-Host ">> 1/4 导出 OpenAPI 契约 (ui/openapi.json)"
    & go run . --gen-openapi=./ui/openapi.json
    if ($LASTEXITCODE -ne 0) { throw "导出 openapi.json 失败" }
    if (-not (Test-Path (Join-Path $uiDir "openapi.json"))) { throw "ui/openapi.json 没生成" }

    if (-not (Test-Path (Join-Path $uiDir "node_modules"))) {
        Write-Host ">> 2/4 安装前端依赖（首次会慢）"
        Invoke-Pnpm @("--dir", "ui", "install", "--frozen-lockfile")
    } else {
        Write-Host ">> 2/4 已有 node_modules，跳过 install"
    }

    Write-Host ">> 3/4 生成 API 客户端"
    Invoke-Pnpm @("--dir", "ui", "run", "generate-client")

    Write-Host ">> 4/4 编译 UI（embed 模式 → static/v2ui/dist）"
    Invoke-Pnpm @("--dir", "ui", "run", "build:embed:prepared")

    $index = Join-Path $v2uiDist "index.html"
    if (-not (Test-Path $index)) { throw "static\v2ui\dist\index.html 不存在，UI 没编出来" }
    if (-not (Test-Path (Join-Path $v2uiDist "assets"))) { throw "static\v2ui\dist\assets 不存在，UI 产物不完整" }
    Write-Host ">> UI 就绪"
}

# ---------- 构建 ----------
$hash = (git rev-parse --short=7 HEAD).Trim()
$date = (Get-Date).ToUniversalTime().ToString("yyyyMMdd")
$meta = "+$date.$hash"
$outDir = Join-Path $repo "dist-build"
New-Item -ItemType Directory -Force -Path $outDir | Out-Null
$exe = Join-Path $outDir "sealdice-core.exe"

Write-Host ">> 编译: 版本 1.6.2-dev$meta"
$env:GOOS = "windows"; $env:GOARCH = "amd64"
& go build -trimpath -ldflags "-s -w -X 'sealdice-core/dice.VERSION_BUILD_METADATA=$meta'" -o $exe .
if ($LASTEXITCODE -ne 0) { throw "go build 失败" }

# 自检：把注入后的版本号打出来，免得又编出一个只有 1.6.2-dev 的无版本产物。
$builtVersion = (& $exe --version | Select-Object -First 1)
if (-not $builtVersion) { throw "无法读取产物版本号" }
Write-Host ">> 自检版本: $builtVersion"
Write-Host ">> 产物: $exe ($([math]::Round((Get-Item $exe).Length / 1MB, 1)) MB)"

# ---------- 打包 ----------
if (-not $SkipZip) {
    $name = "sealdice-core-1.6.2-dev-windows-amd64-$hash"
    $zip = Join-Path $outDir "$name.zip"
    $stage = Join-Path $outDir $name
    Remove-Item -Recurse -Force $stage -ErrorAction SilentlyContinue
    New-Item -ItemType Directory -Force -Path $stage | Out-Null
    Copy-Item $exe $stage
    Copy-Item (Join-Path $repo "dist-build\README-Windows.md") $stage -ErrorAction SilentlyContinue
    if (Test-Path $zip) { Remove-Item -Force $zip }
    Compress-Archive -Path (Join-Path $stage "*") -DestinationPath $zip
    Write-Host ">> 压缩包: $zip"
}

Write-Host ">> 完成。冒烟自测：把 exe 放到**非系统临时目录**再运行（临时目录会被程序拒绝启动），"
Write-Host "   然后打开 http://127.0.0.1:3211 确认是新管理界面而不是占位页。"
