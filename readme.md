# 鲸娘与豹 · sealdice-core 分支

> 一个以「QQ 官方机器人」为中心的 sealdice-core 分支：
> 让**官 bot 与民间 bot 的数据实时共通**，并把官 bot 缺的那部分能力补齐
> （WebUI 聊天控制台、虚拟角色状态栏、群组管理一键拉黑、发送文件保留文件名……）。
> 代码主要由 AI 写成，**仅供体验**；用之前请读完这份说明，并做好数据备份。

---

## ⚠️ 先看这里

- **没有改动数据库结构**：主库 `data.db` 没有新增表、没有新增字段，换回官方主线可以直接沿用同一份 `data/`。
  唯一的额外目录是聊天控制台自己的 `data/<骰子名>/chat-console/`（独立 SQLite，官版不读也不动它）。
- **本项目跟随海豹主线**：定期 `git fetch upstream && git merge upstream/master`，冲突一律「主线优先」。
  已合并到上游 **#1884**（`98463817`，帮助索引内存优化）；上游之后的提交尚未同步，
  用 `git log --oneline upstream/master` 可以看到差多少。
- 但「跟随主线」不等于零风险：绑定功能侵入了 `dice/ext_log.go`、
  `dice/platform_adapter_official_qq.go`、`dice/builtin_commands.go` 等文件；
  聊天控制台与一键拉黑还改了 `api/*.go`、`dice/chat_console*.go` 和仓库内的管理前端 `ui/`。
  上游大改这些地方时仍然需要手工合并。
- **管理界面有两套**：默认进旧版（`/old-ui/`），点右下角按钮切到新版（`/v2ui/`）。
  旧版源码不在本仓库（构建时从 `chaye2333/sealdice-ui` 拉取编译），新版就是仓库里的 `ui/`。
  详见下面「管理界面：两套 UI 的现状」。
- 完整改动清单（按功能分组，含兼容性与回退说明）见
  [`docs/release-notes-fork.md`](docs/release-notes-fork.md)。
- 用这版就默认你已经看过上面的声明。**叠甲、叠甲、叠甲。**

---

## 它比原版多了什么

### 1. 官 bot ↔ 民间 bot 双向数据共通（核心功能）

迁移到官方机器人之后，旧 QQ 号 / 旧群里的数据不会丢——绑定之后两边**读写同一份数据**：

| 指令 | 管哪个维度 | 涉及的数据 |
|---|---|---|
| `.bind <旧QQ号>` | **用户维度** | 角色卡、属性（`.pc` / `.st`）、`.sn` 名片模板 |
| `.group bind <旧群号>` | **群维度** | 日志状态与日志内容（`.log`）、群玩家记录 |

两个维度都做，官方侧与旧侧才完全互通；只做一个会有半边读不到。

设计要点：**规范 ID 固定是「旧身份」**，数据一条都不搬移——绑定只是一张对照表
（`data/<骰子名>/identity-bindings.json`），`.unbind` 后立刻恢复原状。

### 2. 官方 QQ 聊天控制台（WebUI）

WebUI → 扩展功能 → **聊天控制台**：像桌面 QQ 一样看/发官方机器人的消息。

| 能力 | 说明 |
|---|---|
| 会话与消息流 | 群聊 / 私聊、未读角标、在线状态、`仅 @ 消息` 权限提示；SSE 实时推送，断线降级轮询 |
| 发送文字 | 以机器人身份发言；群聊基本不受限，私聊受官方主动消息额度限制（失败会给出原因） |
| 发图片/语音/视频/文件 | 输入框「图片/文件」，≤30MB，复用适配器富媒体上传（含分片上传保留文件名） |
| **@ 与引用** | 悬停「回复」时，**最近 4 分钟内收到的消息**走官方被动回复做成**真引用**（QQ 里显示引用块），超窗口退回「回复 某人：原文」；正文也支持 CQ 码：`[CQ:at,qq=<openid>]`（含 `qq=all`）→ 官方 @，`[CQ:reply,id=<msg_id>]` → 被动回复真引用 |
| 撤回 / 禁言 | 只能撤回机器人自己的消息；禁言/解禁需机器人是群管理员（1 分钟 ~ 1 天） |
| 群资料 / 头像 / 置顶 / 改名 | 群名、人数、分类、标签、简介（白名单机器人才有），点「刷新」立刻向官方查一次 |
| 存储管理 | 总占用、每会话占用与条数、保留天数（0=永久）、清空单个/全部 |

> **@ 的注意点**：`<qqbot-at-user>` 这类提及标签只有 **markdown 消息（msg_type=2）** 才会被 QQ 解析，
> 纯文本会原样显示成标签。所以带 @ 的消息会自动按 markdown 发出；发送失败会退化回纯文本
> （@ 变成 `@昵称`），消息不会丢。

详见 [`docs/official-qq-chat-console.md`](docs/official-qq-chat-console.md)，
实现取舍见 [`docs/official-qq-chat-console-design.md`](docs/official-qq-chat-console-design.md)。

### 3. 验证码（防抢号）

答题拦不住抢号（知道卡名就能接管），所以整套出题/判题逻辑已删除，只走验证码：

| 通道 | 可用条件 | 验证码送到 | 谁回复 |
|---|---|---|---|
| 邮箱 | 「邮箱通知」配齐（发件邮箱/密钥/SMTP） | `<旧QQ号>@qq.com`（群绑定则寄**旧群邀请人**） | 在**官方 bot 这边**回复，群里私聊都行 |
| 私聊 | 民间 bot（OneBot）**在线** | 民间 bot 私聊 | 用那个号在**民间 bot** 那边回复 |

两条通道自动二选一，**不需要额外开关**（骰主把 SMTP 配好就等于启用邮箱通道）；
想优先用邮箱就打开 `identityBindPreferEmailCode`。

- 猜码有上限：默认 5 次错误即作废；落库前还会复查唯一性
- 防滥用限频：同一个旧号/旧群 10 分钟最多发起一次、全实例每小时 20 次
  （只在真的发出去时计数；`.bind cancel` / `.group cancel` 会释放额度）
- **骰主兜底**：两条通道都不通时自动转人工并私聊通知骰主，
  骰主用 `.bind pending` 看列表、`.bind approve <旧QQ号>` / `.group approve <旧群号>`
  核实身份后确认（会跳过验证码）

### 4. `.sn` 名片（虚拟角色状态栏）

官方机器人**改不了群名片**，所以用 `.sn` 模板渲染成一行小号红字挂在回复顶部：

```
$\scriptsize\textcolor{#E5484D}{\text{马丁·弗卢吉尔 SAN69 HP12/12 DEX65}}$
```

- 名字取自**当前绑定的角色卡**，`.pc tag` 换卡后立刻跟着变
- 属性实时求值，不硬编码任何规则系统（COC / DND / FU / 自定义 `.sn expr` 都行）
- `.sn off` 可彻底关掉；**需要开启「官方QQ使用Markdown」**，否则状态栏自动隐藏
- 只对官方 QQ 端点生效，OneBot / TG / Discord 等平台回复格式完全不变
- **状态栏不进跑团日志**：红字只是展示用，写日志前会按本次发送的前缀剥掉，
  `.log` 导出的是干净正文（分页 / 批量发送的上下文也一并处理）

### 5. 群组管理（一键拉黑 + 官方群资料）

- 群卡片右上角「**拉黑 / 解除拉黑**」直接写黑白名单（禁止，-30），已拉黑的群标题旁显示标签；
  不用再去「综合设置 › 黑白名单」手输群号，**拉黑后立刻落库**，刷新列表就能核对
- 官方机器人群的 ID 很长，窄屏下会**折行**，不会再出现「拉黑 / 退出按钮被挤出卡片」的情况
- 官方群名、简介、分类、标签自动补全（`/v2/groups/{id}/info`，简介这些字段只有白名单机器人拿得到）；
  群列表的玩家数改成一次批量查询，几百个群也不会卡

### 6. 官方 QQ 适配增强

| 改动 | 说明 |
|---|---|
| 分片上传 | `officialQQChunkedUploadEnable` 打开后，**发文件（文件卡片）一定走分片上传，能保留文件名**（base64 路径腾讯没有文件名字段，会显示"未命名"）；图片/语音等 ≥1MB 才走；默认关 |
| 请求超时可配 | `officialQQRequestTimeoutSec`（默认 60 秒，与上游一致），5~600 |
| 掉线防崩 | 连接失败后端点 `Enable` 仍为 true，旧实现发消息会 nil 接口 panic 并**冲掉整条 websocket**；现在发送入口有守卫 |
| 邮件 | SMTP 支持 `host` 或 `host:port`，默认 465（隐式 TLS）、587 强制 STARTTLS，失败会如实报错 |

### 7. 内容安全（公益服务向）

| 改动 | 说明 |
|---|---|
| 出站敏感词打码 | `censorMaskEnable`：骰子**要发出的文本**命中敏感词时替换成**等长**口口（4 字词 → 口口口口），而不是整条拦截。**帮助文档只打码不计数**（否则骰主会被自己的文档拉黑），**其它出站打码同时照常计违规**。CQ 码/海豹码内部不动 |
| 帮助文档图片化 | 帮助正文**命中敏感词**或**超过设定字数**时，改发一张现场渲染的图片（`imageRender*` 七个键）。一次性不缓存、失败自动回退文本。需要自部署渲染后端：`helpdoc-render`（纯 Go、无浏览器、内存十几 MB） |

详见 [`docs/helpdoc-image-and-censor-mask.md`](docs/helpdoc-image-and-censor-mask.md)。

### 8. 其它

- `.help` 第一行固定为 `鲸娘与豹 <版本号>`
- 私聊（`PG-` 伪群号）也参与归一：官方私聊与旧号私聊共用同一份属性/默认卡
- `.group doctor` / `.bind doctor` 一键自检所有绑定
- **日志上传**：缓存链接失效时自动重新上传（上游 #1811）；本版还修了「染色器返回 SPA 的
  index.html 却当成链接有效」导致永不上传的问题，日志页每张卡片提供「**强制重传**」按钮

---

## 怎么部署

### Docker（推荐）

镜像由 GitHub Actions 自动构建并推送到 GHCR：

```yaml
services:
  sealdice:
    image: ghcr.io/chaye2333/sealdice-core:latest
    container_name: sealdice-core
    restart: unless-stopped
    ports:
      - "3211:3211"
    volumes:
      - ./data:/app/data
      - ./extra:/app/extra
    environment:
      - TZ=Asia/Shanghai
    # 宿主机目录是 root 所有时（容器内以 uid 1000 运行，写不进去）可以打开这行
    # user: "0:0"
```

```bash
docker compose pull && docker compose up -d
```

> **国内拉取慢**：把镜像地址换成加速前缀（如 `ghcr.nju.edu.cn/chaye2333/sealdice-core:latest`），
> 或者把镜像同步到阿里云 ACR / 腾讯云 TCR 后用国内地址拉；`docker pull` 完再 `docker tag` 回原名字，
> compose 就不用改。

### Windows

仓库根目录执行：

```powershell
powershell -File scripts\build-windows.ps1
```

产物在 `dist-build\sealdice-core.exe`。脚本会把**两套界面都编进去**
（新版 `ui/` → `static/v2ui/dist`；老 UI 从 `chaye2333/sealdice-ui` 拉源码 → `static/frontend`），
所以要能访问 github.com 和 npm/pnpm 源；已有产物时可以用 `-SkipUi` 跳过。
把 exe 放到**固定目录**（不要在系统临时目录）运行，浏览器打开 `http://127.0.0.1:3211` 即可。

---

## 配置速查（`data/<名字>/serve.yaml` 或管理界面）

| 键 | 默认 | 说明 |
|---|---|---|
| `identityBindEnable` | `false` | 绑定功能总开关 |
| `identityBindUseVerificationCode` | `true` | 验证码（关掉等于放弃防抢号） |
| `identityBindPreferEmailCode` | `false` | 两条通道都可用时优先邮箱 |
| `identityBindCodeLength` | `6` | 4~8 |
| `identityBindCodeExpireSec` | `600` | 60~3600 秒 |
| `identityBindCooldownSec` | `60` | 同一用户两次发起的最小间隔 |
| `officialQQUseMarkdown` | `false` | **虚拟角色状态栏与聊天控制台的 @ 需要它**（状态栏靠 markdown 渲染，@ 靠 msg_type=2 才会被解析） |
| `officialQQRequestTimeoutSec` | `60` | 官方 QQ 请求超时（5~600） |
| `officialQQChunkedUploadEnable` | `false` | 本地文件分片上传（**发文件必走，保留文件名**；图片/语音 ≥1MB 才走） |
| `logMultiBotDedupWindowSec` | `5` | **建议保持 5**：调大并不能跨 bot 去重，只会把同一人窗口内的重复正文并成一条 |
| `censorMaskEnable` | `false` | 出站掩码：骰子发出的文本命中敏感词时替换成等长口口（帮助文档不计数，其它出站照常计数） |
| `censorMaskChar` | `口` | 掩码字符，最多 4 个字符 |

帮助文档图片化的 7 个键不在 `serve.yaml`，而在 `data/helpdoc/help_config.yaml`
（管理界面 → 帮助文档 里改）：`imageRenderEnable`、`imageRenderMinLength`、
`imageRenderMaskInImage`、`imageRenderUrl`、`imageRenderToken`、
`imageRenderTimeoutSec`、`imageRenderWidth`。

聊天控制台的保留天数等设置也不在 `serve.yaml`：它在
`data/<骰子名>/chat-console/settings.json`，在 WebUI 的聊天控制台「存储」面板里改。

已删除的历史配置项：`identityBindQuestionCount`、`identityBindKeepQuiz`、
`identityBindFailCooldownSec`、`identityBindUseEmailCode`（答题路径与邮箱开关都已取消；
残留的旧键会被静默忽略）。

## 管理界面：两套 UI 的现状

**用这个版本不需要自己构建前端**：Docker 镜像和 Windows 包里两套界面都已经编译进去了。

| | 旧版 UI | 新版 UI（V2 UI） |
|---|---|---|
| 入口 | `/`（默认） → `/old-ui/` | `/v2ui/` |
| 源码 | 不在本仓库，构建时从 `chaye2333/sealdice-ui` 拉指定 commit 编译 | 仓库里的 `ui/` |
| 功能对齐 | 全量（聊天控制台、身份绑定、官方QQ设置、群管理一键拉黑、出站掩码、帮助渲染图都在） | 已对齐主体功能，少数细节还在补 |

切换方式：

- 旧版右下角有「**切换到新版 UI**」胶囊按钮，点一下会记住偏好（cookie `sd_default_ui`，一年）
- 也可以直接访问 `http://你的地址:3211/?ui=new`（进新版）/ `?ui=old`（回旧版）
- 新版页面右上角有「切换到旧版 UI」入口

两套界面**共用同一个后端和同一份数据**，随时切换不会丢东西。

### 新版 UI 目前的状态

已经和旧版对齐的：

- 聊天控制台：会话列表/未读/在线/置顶、头像（群成员按 openid 取；群会话官方没有头像接口，
  显示按名字取色的首字）、消息流与实时推送（SSE，断线退避重连）、发送文字/图片/语音/视频/文件、
  `@ 成员`（从历史消息归纳成员、选人生成标签）、引用回复（窗口内走真引用 `REFIDX`）、
  撤回/禁言、`复制 ID` 与 `资料` 弹窗、`存储` 面板（保留天数/占用/清空）、搜索、卡片链接
- 群组管理：一键拉黑（写黑白名单 -30）、官方群分类/标签/简介
- 内容安全：拦截词页的**出站掩码**开关、帮助文档页的**图片渲染**设置
- 官方 QQ 设置：请求超时、分片上传、身份绑定 6 个开关、多机器人日志去重窗口

已知差异 / 限制：

- **群会话没有真实群头像**：官方接口只给 `group_openid`，拿不到群号，所以列表里是彩色首字；
  单个成员的头像正常（`thirdqq.qlogo.cn/qqapp/{AppId}/{openid}/0`）
- 早期（旧版本采集的）消息可能没有 `sender_open_id`，这些历史消息的成员头像补不回来
- 新版 UI 的页面数量还比旧版少一些（旧版有些页面还没搬过去），需要用旧版时直接切回去

> 只有改代码的人需要关心 OpenAPI / 构建流程，那份说明已经挪到
> [`ui/AGENTS.md`](ui/AGENTS.md) 与仓库根目录的 [`AGENTS.md`](AGENTS.md) 里，普通使用者不用看。

---

## 与上游（官方 sealdice-core）的差别

| 维度 | 情况 |
|---|---|
| 同步基线 | 已合并到上游 `#1884`（`98463817`），之后的提交待同步 |
| 有意**不跟**的 PR | `#1853` / `#1873`（`.sn` 用官方名片渲染）：本分支保留自己的 `.sn` 状态栏（markdown 渲染一行小字） |
| 数据库 | **结构零改动**：`data.db` 没有新增表/字段，换回官方版可直接用同一份 `data/`；新增的只是聊天控制台自己的 `data/<骰子名>/chat-console/`（独立 SQLite） |
| 配置 | 只**新增**键（`identityBind*`、`officialQQ*`、`logMultiBotDedupWindowSec`、`censorMask*`），没有删改上游的键；旧答题相关键已被移除，残留值会被静默忽略 |
| 侵入的文件 | `dice/ext_log.go`、`dice/ext_identity_bind*.go`、`dice/platform_adapter_official_qq.go`、`dice/chat_console*.go`、`api/*.go`、仓库内 `ui/`、`ui_router.go`、`Dockerfile` |
| 行为差异 | 默认进入旧版 UI（上游只提供旧版）；新增 `/v2ui/`、`/old-ui/` 两个路由前缀 |
| 回退到上游 | 换镜像即可，`data/` 不用动；但绑定期间的数据记在**旧身份**名下，官方版不会做归一，详见 [`docs/how-to-revert-to-upstream.md`](docs/how-to-revert-to-upstream.md) |

完整改动清单见 [`docs/release-notes-fork.md`](docs/release-notes-fork.md)。

---

## 更详细的文档

- [`docs/release-notes-fork.md`](docs/release-notes-fork.md)
  —— **本分支改动总清单**：按功能分组，含数据兼容与回退说明
- [`docs/official-qq-chat-console.md`](docs/official-qq-chat-console.md)
  —— 聊天控制台：能力、平台限制、数据存放、常见问题
- [`docs/identity-bind-and-ghcr.md`](docs/identity-bind-and-ghcr.md)
  —— 绑定与验证码的完整说明、镜像构建、常见问题与排查手册
- [`docs/how-to-revert-to-upstream.md`](docs/how-to-revert-to-upstream.md)
  —— **回退到官方主线**的操作手册：数据可见性、`serve.yaml` 兼容性、改动清单
- [`docs/helpdoc-image-and-censor-mask.md`](docs/helpdoc-image-and-censor-mask.md)
  —— 出站敏感词打码与帮助文档图片化：计数规则、自测清单、不足与合规风险
- [`helpdoc-render/README.md`](helpdoc-render/README.md)
  —— 帮助文档图片化的自部署渲染后端（接口契约、字体、systemd / docker 部署）

---

## 已知限制

- 群绑定的确认人是**旧群邀请人**：邀请人换号或没开 QQ 邮箱时，需要骰主手动确认兜底
- 群绑定要求旧群对象在内存里（骰子进过那个群），否则拿不到邀请人
- **同一个群里同时挂官 bot 与民间 bot 时无法去重**：同一条消息会记两遍，日志量 ×2
  （一般用法是官 bot 在官方群、民间 bot 在旧群，不受影响）
- 群配置（前缀、自定义回复、牌堆、扩展开关）**不共享**，只共享日志与玩家数据
- 群绑定后只看得到归一后（旧群）那一份日志；官方群自己那份**不会被删**，解绑或回退后又能看到
- **聊天控制台只能看到授权后的消息**：群消息是否完整取决于群主有没有在官方群设置里开启
  「允许机器人获取群消息」；官方也不提供历史回溯，只能记录功能启用之后的消息
- **聊天控制台的真引用受官方窗口限制**：只有最近几分钟内收到的消息能挂 `msg_id`，且同一个
  `msg_id` 的被动回复次数有限（官方约 5 次），超了会被拒并提示原因；私聊主动消息也有额度
- 聊天控制台只做群聊与私聊，不镜像频道（Guild）
- **群会话没有真实群头像**：官方只给 `group_openid`，列表里显示按名字取色的首字（这是正常的，不是加载失败）
- 旧版 UI 的源码不在本仓库：自己构建时要能访问 github.com 拉 `chaye2333/sealdice-ui`
  （Docker 镜像与 Windows 构建脚本都会自动处理）

---

## 授权

fork 自 [sealdice/sealdice-core](https://github.com/sealdice/sealdice-core)，
沿用上游的 **MIT License**（见 [LICENSE](LICENSE)）。上游版权归 sealdice 项目所有。

上游文档与手册：<https://sealdice.com> · 官方群 524364253
