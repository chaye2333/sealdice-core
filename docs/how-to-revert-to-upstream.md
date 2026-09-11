# 从本 fork 回退到官方海豹主线：操作手册

面向：用本 fork 镜像**体验**官 bot 相关功能，将来想换回官方 `sealdice` 主线（可能已经不是当初那个版本了）。

先给结论，再给依据。

---

## 一、一句话结论

**部署可以随时回退，而且是「换个镜像 tag」级别的操作；
代码回退也不是重写，但需要处理 11 个上游文件的改动。**

关键是这两件事要分开看：

| 层次 | 回退难度 | 说明 |
|---|---|---|
| **你的部署 / 数据** | ⭐ 极低 | 换镜像即可，`data/` 和 `serve.yaml` 都不用动 |
| **代码（想丢掉这个 fork）** | ⭐⭐⭐ 中等 | 新增文件直接删；11 个上游文件要撤掉改动 |

---

## 二、你的数据不会有问题（这是重点）

这是最要紧的一条：**本 fork 没有改数据库结构，也没有做任何数据迁移加固**。

### 2.1 没有 schema 变更

```
git diff cd870eb HEAD --name-only -- migrate model
（输出为空）
```

生产代码里**没有**新增表、没有新增列、没有 `AutoMigrate` 调用
（diff 里出现的 `AutoMigrate` 全在 `_test.go` 里，只影响测试用的临时库）。

所以：

* 你用本 fork 跑出来的 `data/default/*.db`，**官方主线可以直接打开**
* 反过来也一样
* 不存在「用了 fork 的库就回不去了」这种情况

### 2.2 唯一新增的落盘文件是独立的

```
data/default/identity-bindings.json
```

纯 JSON，里面只有「新 ID → 旧 ID」的对照关系。官方主线不认识这个文件，会当它不存在，
放在那里不管即可（想干净就删掉）。

### 2.3 `serve.yaml` 里的多余键是安全的

本 fork 往配置里加了这些键：

```yaml
identityBindEnable
identityBindQuestionCount
identityBindCooldownSec
identityBindFailCooldownSec
officialQQRequestTimeoutSec
officialQQChunkedUploadEnable
logMultiBotDedupWindowSec
```

`dice/dice_config.go` 的加载路径是：

```go
func (c *Config) LoadYamlConfig(data []byte) error {
	err := yaml.Unmarshal(data, &c)   // ← 普通的 Unmarshal
	...
}
```

`Config` **没有**自定义 `UnmarshalYAML`，也没有开 `KnownFields(true)`。
gopkg.in/yaml.v3 在默认配置下会**静默忽略结构体里没有的键**。

所以官方主线读到这些多余的键不会报错、不会崩，就是忽略掉。
构建器（`api`）也不会把它们重新写回去。

> 唯一要注意：官方主线**不会**再写这些键，所以你改过它们的话，
> 回退后这些设置就失效了（比如你把 `officialQQRequestTimeoutSec` 调成了 120，
> 回退后官方主线用它自己的值）。这属于预期行为。

### 2.4 回退操作

```bash
# 1) 先备份（任何迁移前都该做的事）
cp -a ./data ./data.bak-$(date +%F)

# 2) 把 compose 里的镜像换成官方主线
#    image: ghcr.io/chaye2333/sealdice-core:latest   ← 换掉这一行
#    image: ghcr.io/sealdice/sealdice:latest

# 3) 起
docker compose pull && docker compose up -d
```

如果官方主线因为 `serve.yaml` 里的多余键真的报错了（理论上不会），
把上面那 7 个键删掉即可。

---

## 三、代码回退：改动清单

以 `cd870eb`（你的 fork 起点）为基线。

### 3.1 新增文件 —— 直接删掉就行

| 文件 | 作用 |
|---|---|
| `dice/ext_identity_bind.go` | 绑定核心 |
| `dice/ext_identity_bind_test.go` | 绑定测试 |
| `dice/official_qq_character_roll_markdown.go` | 虚拟角色状态栏 |
| `dice/official_qq_character_roll_markdown_test.go` | 状态栏测试 |
| `dice/help_title_test.go` | `.help` 标题锁定 |
| `dice/platform_adapter_official_qq_chunked.go` | 分片上传 |
| `dice/platform_adapter_official_qq_chunked_test.go` | 分片上传测试 |
| `dice/platform_adapter_official_qq_media_test.go` | 富媒体测试 |
| `Dockerfile` / `.dockerignore` / `docker-compose.example.yml` | 镜像构建 |
| `.github/workflows/docker-ghcr.yml` | CI |
| `ui/pnpm-workspace.yaml` | UI 依赖授权 |
| `docs/identity-bind-and-ghcr.md` / `docs/official-qq-improvements.md` | 文档 |

**但注意**：删掉新增文件后，下面 11 个上游文件会**编译失败**（引用了被删的符号），
所以两边必须一起处理。

### 3.2 改过的 11 个上游文件

| 文件 | 改动量 | 撤掉什么 |
|---|---|---|
| `dice/ext_log.go` | ~254 行 | `stateGroup` 归一、日志写入归一、去重窗口可配置、`EvalPlayerGroupCardTemplate` 抽取 |
| `dice/platform_adapter_official_qq.go` | ~136 行 | 超时可配置、`file_type=4`、`file_info` 解码、`apiDomainOverride`、`officialQQGroupIDPrefix` |
| `dice/dice_config.go` | ~119 行 | 7 个新字段 + `FixIdentityBindConfig()` / `FixOfficialQQConfig()` |
| `dice/builtin_commands.go` | ~84 行 | `.bind`/`.unbind`/`.group` 注册、`.pc` 系列改用数据层 ID、`.help` 标题 |
| `api/dice_config.go` | ~63 行 | 7 个新键的解析 |
| `dice/dice_config_default.go` | ~63 行 | 7 个默认值 |
| `dice/im_helpers.go` | ~35 行 | `identityBindFillDataIDs` 调用 + 状态栏消费 |
| `dice/rollvm_migrate.go` | ~21 行 | 状态栏标记置位 |
| `dice/im_session.go` | ~17 行 | `DataUserID`/`DataGroupID`/`OfficialQQStatusBarPending` 字段 |
| `dice/dice_attrs_manager.go` | ~8 行 | `LoadByCtx` 改用数据层 ID |
| `dice/dice.go` | ~5 行 | `IdentityBindStore` 字段 |

### 3.3 撤掉顺序（建议）

按依赖从外到内：

1. `dice/builtin_commands.go` —— 撤掉 `.bind`/`.unbind`/`.group` 的注册与调用
2. `dice/dice_attrs_manager.go` —— `LoadByCtx` 恢复 `am.Load(ctx.Group.GroupID, ctx.Player.UserID)`
3. `dice/ext_log.go` —— 删 `stateGroup`，恢复用 `group`；删去重窗口，恢复硬编码 5 秒 + `msg.RawID`
4. `dice/im_helpers.go` / `dice/im_session.go` / `dice/rollvm_migrate.go` —— 删字段与调用
5. `dice/platform_adapter_official_qq.go` —— 超时、file、前缀
6. `dice/dice_config*.go` / `api/dice_config.go` —— 删 7 个字段与 Fix 函数
7. 删所有新增文件
8. `go build ./...` + `go test ./...` 确认干净

---

## 四、关于 `DefaultConfig` 的位置字面量（已专门处理）

`dice_config_default.go` 里是：

```go
var DefaultConfig = Config{
	nil, ConfigVersion,
	BaseConfig{ ... },      // ← 位置字面量，没写字段名
	RateLimitConfig{ ... },
	...
}
```

这是个**上游自带的隐患**：`BaseConfig{...}` 里全是位置值，
所以字段顺序和字面量顺序必须严格对应。往 `BaseConfig` 中间插字段，
会让后面的值整体错位；如果错位后类型恰好相同，**Go 不会报错，
只会把默认值静默赋给错误的字段**。

本 fork 已经把新增字段**统一追加到 `BaseConfig` 末尾**，
并加了 `TestDefaultConfigValuesLandOnTheRightFields` 把这个顺序钉住
（同时校验几个相邻的上游字段，一旦整体错位立刻变红）。

**所以你合并上游时**：如果上游也在 `BaseConfig` 末尾追加了字段，
只要把两段并排保留即可，不需要排序。但如果上游**往中间插了字段**，
就得小心——那个测试会帮你发现。

---

## 五、合并上游而不是整体回退（更推荐）

如果你的真实目标是「保留部分功能 + 跟上上游」，用合并而不是回退：

```bash
# 加一个上游 remote
git remote add upstream https://github.com/sealdice/sealdice-core.git
git fetch upstream

# 看看到底冲突在哪
git merge upstream/master --no-commit --no-ff
git status
```

预期冲突集中在：

* `dice/dice_config.go` + `dice/dice_config_default.go` —— 字段顺序
* `dice/builtin_commands.go` —— `.pc` 系列与 `.help`
* `dice/ext_log.go` —— `.log` 状态与去重
* `dice/platform_adapter_official_qq.go` —— 官方 QQ 适配器

**建议的取舍**：如果上游已经自己做了官 bot 的日志/身份适配，
优先用**上游的实现**，把本 fork 的东西删掉——上游的方案会和它自己的其他改动更协调。

---

## 六、想「只体验、不纠缠」的更省事做法

如果你的目的只是体验官 bot 功能，将来一定要回官方主线，那么还有个更省心的选择：

**把本 fork 当成一个独立的实验实例**，用不同的 `data` 目录 / 不同端口跑，
不要接管你现在正式骰子的数据目录。

这样回退 = **直接停掉实验实例**，
正式实例从头到尾都在官方主线上，一天都没离开过，零风险。

代价是官 bot 那边要重新绑定一次（`.bind` / `.group bind`），
但既然本来就要换代码，这一步迟早要做。

---

## 七、检查清单

回退前：

- [ ] `cp -a ./data ./data.bak-$(date +%F)` 备份
- [ ] 记下当前镜像 tag（`docker inspect --format '{{index .Config.Labels "sealdice.ui.ref"}}'`）
- [ ] 确认 `identity-bindings.json` 已备份（想保留绑定关系的话）

回退后：

- [ ] 启动无报错
- [ ] `.help` 正常（标题会变回官方主线自己的文案，这是预期的）
- [ ] `.pc list` / `.st show` 能读到旧角色卡（数据在数据库里，与代码无关）
- [ ] 旧的 `.log list` 能看到历史日志
- [ ] 官方 bot 基本功能正常（发一条 `.r 1d20`）
- [ ] `serve.yaml` 里那 7 个多余键如果碍事就删掉
