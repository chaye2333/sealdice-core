# AGENTS.md — 本 fork 的提交前自检与踩坑提醒

给 AI 助手（也是给未来的自己）。**每次 push 之前照着跑一遍**。
CI 的 lint 步骤只在 push 时执行、PR 里看不到失败，所以「本地全绿」是唯一可靠的判断。

## 1. 一键自检

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File scripts/pre-push-check.ps1
powershell -NoProfile -ExecutionPolicy Bypass -File scripts/pre-push-check.ps1 -SkipUi   # 只动 Go
```

（本机没装 PowerShell 7，别写 `pwsh`。脚本本身存成 **UTF-8 with BOM**，否则 5.1 会按 GBK 解析中文而报语法错。）

脚本内容是 CI 的等价物（`.github/workflows/test-and-lint.yml` + `ui.yml`）：

```powershell
go build ./...
go vet ./...
go test ./...
golangci-lint run --timeout 20m          # 必须是 v2.15.0，见下
cd ui; npx -y pnpm@10.14.0 run check:prepared
```

跑 UI 检查前先清增量缓存，否则 `vue-tsc --build` 会拿旧 `.tsbuildinfo` 直接跳过：

```powershell
Remove-Item ui\node_modules\.tmp\*.tsbuildinfo -ErrorAction SilentlyContinue
```

## 2. golangci-lint 版本必须 v2.15.0

```powershell
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.15.0
```

- 若 PATH 上是旧的 v2.14.0，`run` 会直接打
  `failed to set analyzer setting "check" with value ["switch" "mapliteral"]` 并 **exit 3**。
  这是工具版本不匹配，**不是** `.golangci.toml` 写错了：`exhaustive.check` 的新写法
  `mapliteral`（旧值 `map`）只有 ≥ v2.15.0 认。
- 看到这个报错不要去改配置，去升级工具。
- `golangci-lint config verify` 通过不代表能跑，务必真的 `run` 一次。

## 3. 已经踩过的坑（都真实让 CI 红过）

| 坑 | 现象 | 正确做法 |
| --- | --- | --- |
| `goprintffuncname` | 收 `format string, args ...any` 的函数必须带 `f` 后缀，否则报错（`identityBindInfof`/`Warnf`/`Errorf` 那次就是这么红的） | 命名 `xxxf` |
| `intrange` | `for i := 0; i < n; i++` 被判可改 | `for i := range n`（Go 1.22+） |
| 测试产物入库 | 跑完测试 `git add -A` 会把 `api/v2/deck/cache/`、`*.blocks` 提交上去，含本机绝对路径 `c:/users/...` | 提交前看 `git status`；误提交用 `git rm -r --cached <path>` 并补 `.gitignore` |
| 缺 `static/v2ui/dist` | `go:embed all:v2ui` 编译失败 | 先 `npx -y pnpm@10.14.0 --dir ui run build:embed:prepared` |
| `.dockerignore` 排掉 embed 目录 | GHCR 构建在 `go run . --gen-openapi` 阶段 exit 1：`pattern frontend: no matching files found`（`static/static.go` 的 `go:embed frontend` 一个文件都匹配不到） | 忽略规则只留占位页：`/static/frontend/*` + `!/static/frontend/index.html`；验证办法是用 `git archive HEAD` 解到临时目录，在里面跑一遍 `go run . --gen-openapi=./ui/openapi.json` 和 `go build .` |
| 手改生成物 | 不要动 `ui/src/api/generated/`、`ui/openapi.json` | `npx -y pnpm@10.14.0 --dir ui run generate-api` |
| Windows 无 gcc | 跑不了 `go test -race ./...`（CI 会跑） | 动过并发代码（全局 `dm.Dice`、store 的 `sync.Mutex`）时人工 review；本地 `go test -count=1` |
| 只跑 `type-check` | `pnpm run type-check` 依赖 `vite build` 生成的自动导入声明，单独跑会漏报/误报 | 一律用 `check:prepared` |
| 增量缓存骗人 | `vue-tsc --build` 命中旧 `ui/node_modules/.tmp/*.tsbuildinfo` 会直接跳过，把真实 TS 错误藏起来（聊天控制台的 TS7006 就是这么漏过的） | 提交前删掉 `.tsbuildinfo` 再跑一遍 |
| 模板内联箭头函数 | `@select="key => f(key)"` 报 TS7006 隐式 any | 显式标注：`@select="(key: string \\| number) => f(String(key))"` |
| PowerShell 5.1 | `>` 重定向写出 UTF-16；`git checkout --theirs` 是整文件级 | 用 `git checkout <rev> -- <path>` 取单文件；冲突按 hunk 解决 |

## 4. 跑完必看的收尾项

- `git status` 里不应出现 `data/`、`*-out.txt`、`.env`、`dist/`、测试缓存。
- `ui/openapi.json` 若被 `generate-api` 改动，要么提交（契约真变了），要么还原。
- 提交信息用 Conventional Commits + 中文摘要，例如 `fix(chat-console): 卡片链接只允许 http/https`。

## 5. fork 与上游的关系

- 上游用 squash merge，PR 号出现在 commit 标题末尾 `(#NNNN)`。
- **有意跳过** 的 PR：`#1853`、`#1873`（`.sn` 名片）。本 fork 保留自己的 `.sn` 状态栏，
  整体 merge 上游后要显式丢掉 `dice/official_qq_name_card.go` / `officialQQUseNameCard`，
  保留 `dice/official_qq_character_roll_markdown.go`。
- 上游 UI 的 `ui/AGENTS.md` 同样有效（更具体的规则优先）。
