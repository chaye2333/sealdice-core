# 本地构建 Windows 版（含管理界面）的脚本
#
# 为什么需要它：`static/frontend/index.html` 是**被 git 跟踪的占位页**。
# 一旦做过 git checkout / stash / rebase / pull，它就会被恢复成占位页，
# 于是 go build 出来的 exe 里只有 "UI assets are not bundled" 那张提示页
# （而且编译不会报错，很难发现）。所以每次构建前都重新装一遍 UI。
#
# 用法（在仓库根目录）：
#     pwsh -File scripts\build-windows.ps1
#
# 产物：
#     dist-build\sealdice-core.exe
#     dist-build\sealdice-core-<版本>-windows-amd64-<hash>.zip
#
# UI 来源优先级：
#   1. -UiDist <路径> 指定的目录
#   2. 环境变量 SEALDICE_UI_DIST 指向的目录
#   3. 默认的 UI fork 克隆：%TEMP%\sealdice-ui-fork\dist
# 都没有时会明确报错，并提示怎么准备（clone + pnpm build-only）。

[CmdletBinding()]
param(
    [string]$UiDist = "",
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

# ---------- 准备 UI ----------
if (-not $UiDist) {
    if ($env:SEALDICE_UI_DIST) { $UiDist = $env:SEALDICE_UI_DIST }
    elseif (Test-Path (Join-Path $env:TEMP "sealdice-ui-fork\dist")) {
        $UiDist = Join-Path $env:TEMP "sealdice-ui-fork\dist"
    }
}
if (-not $UiDist -or -not (Test-Path $UiDist)) {
    $hint = "找不到前端构建产物。请先准备（sealdice-ui 的 fork）：`n" +
            "    git clone https://github.com/chaye2333/sealdice-ui.git `$env:TEMP\sealdice-ui-fork`n" +
            "    cd `$env:TEMP\sealdice-ui-fork; pnpm install; pnpm run build-only`n" +
            "然后用 -UiDist 指定它的 dist 目录，或设 `$env:SEALDICE_UI_DIST。"
    throw $hint
}
if (-not (Test-Path (Join-Path $UiDist "index.html"))) { throw "$UiDist 里没有 index.html，不像是构建产物" }

Write-Host ">> 安装管理界面: $UiDist"
Copy-Item (Join-Path $UiDist "*") -Destination (Join-Path $repo "static\frontend") -Recurse -Force

$uiIndex = Get-Content (Join-Path $repo "static\frontend\index.html") -Raw
if ($uiIndex -match "not bundled in this checkout") {
    throw "static/frontend/index.html 仍是占位页，UI 没装进去"
}
if ($uiIndex -notmatch "assets/index-") {
    throw "static/frontend/index.html 没引用构建后的 assets，UI 可能不完整"
}
Write-Host ">> UI 就绪（引用了构建产物）"

# ---------- 构建 ----------
$hash = (git rev-parse --short=7 HEAD).Trim()
$date = (Get-Date).ToUniversalTime().ToString("yyyyMMdd")
$meta = "+$date.$hash"
$outDir = Join-Path $repo "dist-build"
New-Item -ItemType Directory -Force -Path $outDir | Out-Null
$exe = Join-Path $outDir "sealdice-core.exe"

Write-Host ">> 编译: 版本元数据 $meta"
$env:GOOS = "windows"; $env:GOARCH = "amd64"
& go build -trimpath -ldflags "-s -w -X 'sealdice-core/dice.VERSION_BUILD_METADATA=$meta'" -o $exe .
if ($LASTEXITCODE -ne 0) { throw "go build 失败" }
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
Write-Host "   然后打开 http://127.0.0.1:3211 确认是管理界面而不是占位页。"
