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

### 2.2.1 【最重要】回退后哪些数据还能看到，取决于你绑了几个维度

**结论：绑定了「个人 + 群」两个维度，回退后数据完全正常；
只绑了一个维度，会有一类数据变成"孤儿"。**

原因是数据库里有两类 key：

| 数据 | 存的 key | 归哪个绑定管 |
|---|---|---|
| 角色卡本体、卡名（`.pc list` 能列出） | `owner_id` = **用户ID** | 个人绑定 |
| 角色卡内容 / 群属性 / "最后用的那张卡" | **`群ID-用户ID`** 组合 key | 两个都管 |
| 日志（`logs` + `log_items`） | `group_id` = **群ID** | 群绑定 |

于是回退后的实际结果（"官"= 官方群/官方 bot，"旧"= 民间 bot 那个旧群）：

| 个人绑定 | 群绑定 | 属性/角色卡实际落在 | 从**旧群/民间 bot** 看 | 从**官方群/官方 bot** 看 |
|---|---|---|---|---|
| ✅ | ✅ | `旧群-旧号` | ✅ 完全正常（与上游 OneBot 侧算出来的 key 一致） | ❌ 官方侧不再归一，按 `官群-官号` 查 → 迁移期间的数据读不到 |
| ✅ | ❌ | `官群-旧号` | ❌ 读不到（上游旧侧按 `旧群-旧号` 查）**但 `.pc list` 能看到卡**（`owner_id` 是旧号） | ❌ 读不到 |
| ❌ | ✅ | `旧群-官号` | ❌ 读不到 | ❌ 读不到 |
| ❌ | ❌ | `官群-官号` | — | ✅ 正常（本来就没参与绑定，key 与上游一致） |

> **一句话**：绑定期间产生的数据全部记在「旧身份」名下，一条都没丢、也没被搬走。
> 回退到官方主线后，官方 bot 不再做归一，于是它按官方身份去读自己那份
> （通常就是空的或迁移前的旧内容）——看起来像"数据没了"，其实一直都在旧身份名下。

日志只在「群绑定」那一行受影响：

* 有群绑定 → 日志写在**旧群**名下 → 回退后旧群照常能看 ✅
* 没有群绑定 → 官 bot 在官方群记的日志写在**官方群**名下 → 回退后官方群自己也看不到
  （但只要回退后继续在同一个官方群里用，它仍然是那份，只是不再和旧群合并）

**所以最稳的做法是：两个维度都绑。**
设计目标就是让「个人 + 群」同时成立时，两边算出的 (群ID, 用户ID) 完全一致，
那份数据在回退前后都是同一个 key —— 代价是它固定落在旧身份名下。

**如果你已经只绑了个人绑定、还想保住官方群里的属性改动**，回退前补一步即可：

```
.group bind <旧群号>          # 补上群维度
.pc tag <你的卡名>            # 在官方群里重新"标记"当前卡（可选）
```

标记之后，卡绑定会写到归一后的（旧群）key 上，回退后仍然生效。

**回退前建议做的三件事**（都只是"读"，不改数据）：

1. 备份整个 `data/` 目录（最省事，出什么事都能回来）；
2. `.log export` 把绑定期间的日志导一份出来（`.log get` / `.log export` 在绑定生效时读的就是合并后的那份）；
3. `.bind status` / `.group status` / `.bind list` 截个图，记住自己绑过哪些号 —— 以后想再绑回来时照着填。

### 2.3 `serve.yaml` 里的多余键是安全的

本 fork 往配置里加了这些键：

```yaml
identityBindEnable
identityBindUseVerificationCode
identityBindCodeLength
identityBindCodeExpireSec
identityBindPreferEmailCode
identityBindCooldownSec
officialQQRequestTimeoutSec
officialQQChunkedUploadEnable
logMultiBotDedupWindowSec
```

（`identityBindKeepQuiz` / `identityBindQuestionCount` / `identityBindFailCooldownSec`
/ `identityBindUseEmailCode` 这几个键已经从 fork 里删掉了；如果你手上还有残留，
官方主线同样是"静默忽略"，无害。）

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
把上面那 9 个键删掉即可。

> 还有一个副作用要知道：官方主线**不认识**这些键，它保存设置时会按自己的
> 结构体重新写 `serve.yaml`，于是这些键会**从文件里消失**。
> 以后想再切回 fork，把值重新填一遍就行（fork 的默认值本身就是可用的）。

---

## 三、代码回退：改动清单

以 `cd870eb`（你的 fork 起点）为基线。

### 3.1 新增文件 —— 直接删掉就行

| 文件 | 作用 |
|---|---|
| `dice/ext_identity_bind.go` | 绑定核心（规范 ID 归一、指令处理、自检） |
| `dice/ext_identity_bind_code.go` | 验证码：挑战状态机、两条投递通道、骰主人工兜底 |
| `dice/ext_identity_bind_test.go` | 绑定核心测试 |
| `dice/ext_identity_bind_code_test.go` | 验证码测试 |
| `dice/ext_identity_bind_email_test.go` | 邮箱通道测试 |
| `dice/ext_identity_bind_master_test.go` | 通道诚实性 + 骰主兜底测试 |
| `dice/ext_log_share_test.go` | 群绑定后日志共通的锁定测试 |
| `dice/official_qq_character_roll_markdown.go` | 虚拟角色状态栏 |
| `dice/official_qq_character_roll_markdown_test.go` | 状态栏测试 |
| `dice/help_title_test.go` | `.help` 标题锁定 |
| `dice/platform_adapter_official_qq_chunked.go` | 分片上传 |
| `dice/platform_adapter_official_qq_chunked_test.go` | 分片上传测试 |
| `dice/platform_adapter_official_qq_media_test.go` | 富媒体测试 |
| `dice/utils_email_test.go` | SMTP 端口/加密方式测试 |
| `Dockerfile` / `.dockerignore` / `docker-compose.example.yml` | 镜像构建 |
| `.github/workflows/docker-ghcr.yml` | CI |
| `ui/pnpm-workspace.yaml` | UI 依赖授权 |
| `docs/identity-bind-and-ghcr.md` / `docs/how-to-revert-to-upstream.md` | 文档 |
| `docs/official-qq-improvements.md` | **本地草稿，从未入库**（`.gitignore` 之外，注意别 commit） |

**但注意**：删掉新增文件后，下面 14 个上游文件会**编译失败**（引用了被删的符号），
所以两边必须一起处理。

### 3.2 改过的 14 个上游文件

（数字可用 `git diff --stat upstream/master...HEAD -- <文件>` 复核。）

| 文件 | 改动量 | 撤掉什么 |
|---|---|---|
| `dice/ext_log.go` | ~300 行 | `stateGroup` 归一、日志写入归一、去重窗口可配置、`EvalPlayerGroupCardTemplate` 抽取 |
| `dice/platform_adapter_official_qq.go` | ~136 行 | 超时可配置、`file_type=4`、`file_info` 解码、`apiDomainOverride`、`officialQQGroupIDPrefix` |
| `dice/dice_config.go` | ~122 行 | 新增字段 + `FixIdentityBindConfig()` / `FixOfficialQQConfig()` |
| `dice/builtin_commands.go` | ~84 行 | `.bind`/`.unbind`/`.group` 注册、`.pc` 系列改用数据层 ID、`.help` 标题 |
| `api/dice_config.go` | ~72 行 | 新键的解析 |
| `dice/dice_config_default.go` | ~29 行 | 新字段的默认值（**位置字面量，见第四节**） |
| `dice/im_helpers.go` | ~35 行 | `identityBindFillDataIDs` 调用 + 状态栏消费 |
| `dice/rollvm_migrate.go` | ~21 行 | 状态栏标记置位 |
| `dice/im_session.go` | ~28 行 | `DataUserID`/`DataGroupID`/`OfficialQQStatusBarPending` 字段 + 验证码拦截钩子 |
| `dice/utils_email.go` | ~68 行 | SMTP 端口可配、默认 465+SSL、`SendMailRow` 返回 error |
| `dice/im_vars.go` | ~13 行 | `VarGetValue` 的 nil 守卫（防止后台通知 panic 打崩官方 bot 连接） |
| `dice/dice_attrs_manager.go` | ~8 行 | `LoadByCtx` 改用数据层 ID |
| `dice/dice.go` | ~7 行 | `IdentityBindStore` 字段 + 验证码 worker 启动 |
| `dice/ext_dnd5e.go` | ~1 行 | 一个空行（无实际影响） |

### 3.3 撤掉顺序（建议）

按依赖从外到内：

1. `dice/builtin_commands.go` —— 撤掉 `.bind`/`.unbind`/`.group` 的注册与调用
2. `dice/dice_attrs_manager.go` —— `LoadByCtx` 恢复 `am.Load(ctx.Group.GroupID, ctx.Player.UserID)`
3. `dice/ext_log.go` —— 删 `stateGroup`，恢复用 `group`；删去重窗口，恢复硬编码 5 秒 + `msg.RawID`
4. `dice/im_helpers.go` / `dice/im_session.go` / `dice/rollvm_migrate.go` / `dice/im_vars.go` —— 删字段与调用
5. `dice/platform_adapter_official_qq.go` —— 超时、file、前缀
6. `dice/utils_email.go` —— 恢复 `SendMailRow` 无返回值（或保留返回值，它向后兼容）
7. `dice/dice_config*.go` / `api/dice_config.go` —— 删新字段与 Fix 函数
8. 删所有新增文件
9. `go build ./...` + `go test ./...` 确认干净

> 其实**不用手工做这些**：`git checkout upstream/master -- .` 之类一条命令就到位了
> （见第五节的合并式做法，或者直接切到官方 tag 重新部署）。

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

如果你的真实目标是「保留部分功能 + 跟上上游」，用合并而不是回退。

`upstream` remote 已经加好了（指向 `sealdice/sealdice-core`），日常同步流程：

```bash
git fetch upstream --tags

# 先侦察，不要一上来就合
git rev-list --count HEAD..upstream/master        # 上游多几个提交
git log --oneline HEAD..upstream/master           # 都是什么
git diff --stat HEAD...upstream/master            # 上游侧改了哪些文件（三个点）
git merge-tree --write-tree HEAD upstream/master > $null   # 退出码 0 = 零冲突

# 留一条保险绳
git branch backup/before-upstream

# 在临时分支上合，验完再快进 master
git switch -c sync/upstream
git merge upstream/master
go build ./... && go test ./...
git switch master && git merge --ff-only sync/upstream && git push origin master
```

预期冲突集中在：

* `dice/dice_config.go` + `dice/dice_config_default.go` —— 字段顺序（位置字面量，见第四节）
* `dice/builtin_commands.go` —— `.pc` 系列与 `.help`
* `dice/ext_log.go` —— `.log` 状态与去重
* `dice/platform_adapter_official_qq.go` —— 官方 QQ 适配器
* `readme.md` —— 上游也改，冲突了保留自己那份即可（`git checkout --ours readme.md`）

**建议的取舍**：如果上游已经自己做了官 bot 的日志/身份适配，
优先用**上游的实现**，把本 fork 的东西删掉——上游的方案会和它自己的其他改动更协调。

> ⚠️ 不要去点 GitHub 网页上的 "Sync fork → Discard N commits"：
> 它会让你的分支直接变成上游状态，49 个功能提交一起消失。

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
- [ ] 从**旧群/民间 bot** 那边 `.pc list` / `.st show` 能读到旧角色卡
      （官方 bot 侧读不到，因为不再归一 —— 见 2.2.1 那张表）
- [ ] 旧的 `.log list` 能看到历史日志
- [ ] 官方 bot 基本功能正常（发一条 `.r 1d20`）
- [ ] `serve.yaml` 里那 7 个多余键如果碍事就删掉
