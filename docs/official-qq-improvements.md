# SeaDice 官方 QQ 适配改进说明

> 本文档是本地草稿（**未提交到仓库、也未推送**），供你自行整理后提交给上游。
> 内容基于 SeaDice 1.6.1-dev / 1.6.2-dev 主分支实测整理，涉及三块：
> ① 虚拟角色状态栏（红字属性显示）② 程序标题类文案硬编码 ③ 官方 QQ 富媒体上传。

---

## 一、背景与共性

QQ 官方机器人有两处**能力缺口**，导致「OneBot 时代能用、转到官方 bot 后失效」：

| 缺口 | 官方 bot | OneBot / NapCat |
|---|---|---|
| 修改群成员名片 | ❌ 无接口（`SetGroupCardName` 是空实现） | ✅ 可改，`.sn` 属性名片正常 |
| 发送文件（`file_type=4`） | ✅ 平台支持，但适配器未实现 | ✅ `upload_group_file` |

第 ① 块解决"名片改不了"，第 ③ 块解决"文件发不出 / 文件没名字"。
第 ② 块是顺手发现的一处文案可配置性缺陷。

下文所有改动都遵循两条原则：

1. **不破坏既有行为**——OneBot、Telegram、Discord 等平台的输出必须一字不变；
2. **不硬编码规则字段**——COC / DND / FU / SH / 第三方 JS 扩展都要能自动适配。

---

## 二、虚拟角色状态栏（红字属性显示）

### 2.1 现象

官方 QQ 上玩家用 `.sn` 设了属性模板，掷骰时**什么都不显示**：

```
.sn dnd
.r 1d20          # 回复里看不到任何属性
```

同一个玩家在 Telegram / Discord 上正常，因为那些平台真的会改名片，而官方 QQ 不会。

### 2.2 期望

在官方 QQ 的掷骰 / 检定回复**顶部**渲染一条"虚拟状态栏"，代替改名片：

```
$\scriptsize\textcolor{#E5484D}{\text{侦探 HP12/12 AC16}}$
侦探的"侦查"检定结果为: (1)=1/50 成功
```

* 属性行用 QQ Markdown 的数学片段实现**小号红字**；
* 角色名与鉴定正文保持正常字号；
* HP / SAN / AC 变化后，**下一条回复自动更新**（不做缓存）。

### 2.3 实现要点

#### (1) 把「只计算 .sn 模板」从"改名"里抽出来

现在 `SetPlayerGroupCardByTemplate`（`dice/ext_log.go`）同时做两件事：
求值模板 **+** 调用平台接口改名。抽出一个纯求值函数：

```go
// 只求值，不调用 SetGroupCardName
func EvalPlayerGroupCardTemplate(ctx *MsgContext, tmpl string) (string, error)

// 原函数改为：先求值，再改名
func SetPlayerGroupCardByTemplate(ctx *MsgContext, tmpl string) (string, error)
```

这样官方 QQ 可以只读返回值，把它当"虚拟名片"，完全不碰平台接口。

#### (2) 从 .sn 模板里抽出「属性部分」

`.sn` 模板的通用形态是「玩家名占位符 + 属性表达式」：

```
{$t玩家_RAW} HP{hp}/{hpmax} AC{ac}
{$t玩家_RAW} SAN{理智} HP{生命值}/{生命值上限} DEX{敏捷}
{$t玩家_RAW}                       ← 只设了名字，没有属性
```

**删掉玩家名占位符、只对剩下的部分求值**，就得到属性行：

```go
var playerPlaceholderRe = regexp.MustCompile(`\{\s*\$t玩家(?:_RAW)?\s*\}`)
attrPart := playerPlaceholderRe.ReplaceAllString(ctx.Player.AutoSetNameTemplate, "")
```

> **这是不硬编码规则字段的关键。** 只认"玩家名占位符"这一种结构，
> 剩下的交给现有表达式引擎求值，于是 `.sn coc` / `.sn dnd` / `.sn expr ...` /
> 未来新增的写法全部自动兼容。

#### (3) 组装小号红字

把角色名和属性**放进同一个数学片段**，客户端会整体渲染成小号红字：

```go
fmt.Sprintf(`$\scriptsize\textcolor{%s}{\text{%s}}$`, "#E5484D", escaped)
```

#### (4) 表达式里的特殊字符必须转义

属性值里可能出现 `\ { } $ # % _ ^ & ~`，会提前结束数学片段或被当成 LaTeX 命令；
换行会把状态栏撑成多行。建议整体换成全角、并把换行压成空格：

```go
var mathEscapeReplacer = strings.NewReplacer(
    `\`, `＼`, `{`, `｛`, `}`, `｝`, `$`, `＄`, `#`, `＃`,
    `%`, `％`, `_`, `＿`, `^`, `＾`, `&`, `＆`, `~`, `～`,
    "\r\n", " ", "\n", " ", "\r", " ", "\t", " ",
)
```

#### (5) ⚠️ 挂载位置：放在发送层，**不要**逐个规则改

**这是实践中最容易做错的一点，也是我们返工过一次的地方。**

最初的错误做法：在 `ext_coc7.go` / `ext_dnd5e.go` / `builtin_commands.go`
里逐个调"加状态栏"。结果 **FU / SH / 第三方 JS 扩展的检定全都没有状态栏**——
因为这些规则走的是各自的检定实现，不在手工挂载点上。

正确做法是**两段式**：

**① `DiceFormatTmpl`（`dice/rollvm_migrate.go`）打标记**

```go
var finalReplyTmplKeys = map[string]bool{
    "核心:骰点": true, "核心:骰点_多轮": true,
    "COC:检定": true, "COC:检定_多轮": true, "COC:理智检定": true,
}
if finalReplyTmplKeys[s] {
    ctx.OfficialQQStatusBarPending = true   // MsgContext 新增字段
}
```

**② 发送层消费标记**（`dice/im_helpers.go`，群聊与私聊**两个都要**，暗骰走私聊）

```go
func replyGroupRawNoCheck(...) {
    ...
    text = withOfficialQQCharacterStatusBar(ctx, text)   // 读标记 → 顶部插入 → 清除标记
    ...
}
```

好处：

* 任何规则系统（含未来新增的扩展）只要用这些核心模板渲染检定结果，**自动带状态栏**；
* 帮助文本、错误提示、`.pc` / `.st` 不受影响（它们不是这些模板）；
* 调用方在最终文本后面追加内容（如 `--ci` 的指令信息）也不会把状态栏挤到中间，
  因为插入发生在发送前的最后一刻。

标记必须**消费式**（用过就清），否则同一条回复会被加两次。

#### (6) 只在官方 QQ 生效

```go
func isOfficialQQEndpoint(ep *EndPointInfo) bool {
    return ep != nil && ep.Platform == "QQ" && ep.ProtocolType == "official"
}
```

OneBot / Telegram / Discord 行为**完全不变**。

#### (7) 渲染要有 panic 兜底

状态栏挂在每条掷骰回复的**热路径**上，而求值链内部有若干 `lo.Must` 断言，例如
`dice/rollvm_migrate.go` 的 `loadAttrValueByName`：

```go
attrs := lo.Must(ctx.Dice.AttrsManager.LoadByCtx(ctx))
```

模板写错、数据库临时不可用都会 panic，把整条掷骰指令带崩。
建议包一层 `recover`：出错时只跳过属性行、写一条 warn 日志，掷骰本身照常返回。

### 2.4 前提与已知坑

| 坑 | 说明 |
|---|---|
| **必须开启 `officialQQUseMarkdown`** | 小号红字依赖 `msg_type=2`；关闭时客户端会显示原始公式 `$\scriptsize...$`。建议在不生效时打一条日志提示，便于排查 |
| `.sn none` / `.sn off` | 会把 `AutoSetNameTemplate` 清空，此时**不应显示**状态栏 |
| `.sn` 只有名字 | 只显示角色名，不要留空行；建议此时仍给小号红字（或按产品决定） |
| 不要写用户文案文件 | 状态栏在"最终返回文本 + 发送层"完成，**不要**改写 `SeaDice-text-template.yaml` |

### 2.5 建议开放的配置（可选）

* 开关（默认开/关由上游定）
* 颜色（现在固定 `#E5484D`）

### 2.6 验证清单

- [ ] 官方 QQ 群触发状态栏；OneBot / Telegram **不触发**
- [ ] `.sn dnd`、`.sn coc`、`.sn expr {$t玩家_RAW} HP{hp}/{hpmax}` 都能实时求值
- [ ] `.sn none` / `.sn off` 隐藏状态栏，但正文正常
- [ ] 改 HP 后再掷骰，顶部数值**自动变化**
- [ ] 属性含 `\ { } $ # % _ ^ &` 时不破坏数学片段
- [ ] 角色名只出现一次
- [ ] **FU / SH / 第三方扩展的检定也带状态栏**（这条最能验证挂载点是否正确）
- [ ] 普通回复（帮助 / 错误提示）**不带**状态栏
- [ ] 同一条回复不会出现两条状态栏

---

## 三、程序标题类文案硬编码，无法配置

### 3.1 现象

`dice/config.go` 里 `核心:骰子名字` 默认「海豹核心」，骰主可在 `text-template.yaml` 改：

```go
"核心": {
    "骰子名字": { {"海豹核心", 1} },
```

它被用在掷骰文案等**玩家侧**位置，例如"不支持连续检定 N 次，**海豹核心**觉得这太多了"。
这部分设计没问题。

**问题在于同一个名字出现在两个语义不同的地方：**

| 位置 | 语义 | 能否配置 |
|---|---|---|
| 掷骰文案、入群提示 | **玩家侧骰娘名字**（骰主常改成"罗兰"这类） | ✅ 走 `核心:骰子名字` |
| `.help`（无参数）第一行 | **程序自身标题** + 版本号 | ❌ 硬编码 `"海豹核心 " + VERSION` |
| `.bot about` 第一行 | 程序自身标题 | ❌ 硬编码 `SealDice %s` |

结果：骰主把 `核心:骰子名字` 改成「罗兰」后，`.help` 里**仍然是「海豹核心」**，
两边不一致；而想让 `.help` 跟着变，**没有任何配置项能做到**，只能改代码重新编译。

> 注意：**不建议**简单地把 `.help` 改成读 `核心:骰子名字`——
> 那会让标题变成"罗兰 1.6.2-dev+..."这种混搭（程序名 + 玩家侧骰娘名）。
> 两者语义不同，应该分开。

### 3.2 建议

任选其一（倾向第 1 种）：

**方案 1：把标题也纳入文案模板**，新增独立文案键，并保留硬编码兜底

```go
// 建议新增 核心:程序标题（默认「海豹核心」）与 核心:关于标题（默认「SealDice」）
title := strings.TrimSpace(DiceFormatTmpl(ctx, "核心:程序标题"))
if title == "" || strings.HasPrefix(title, "<%未知项") {
    title = "海豹核心"
}
text := title + " " + VERSION.String() + "\n"
```

**方案 2：只补文档与注释**，说明 `.help` / `.bot about` 的标题是程序名、
不随 `核心:骰子名字` 变化，避免骰主误以为改一处就够。

### 3.3 附带发现

入群强制邀请退群、被拉黑退群等提示里的程序名也是**硬编码**的，分散在：

```
dice/platform_adapter_gocq.go
dice/platform_adapter_onebot.go
dice/platform_adapter_walleq.go
```

```go
text := fmt.Sprintf("...感谢使用海豹核心。", groupInfo.InviteUserID)
```

这几处**用户可见**却改不了。建议改用 `{核心:骰子名字}` 占位符
（海豹文案引擎支持嵌套），或统一走新的 `核心:程序标题`。

---

## 四、官方 QQ 富媒体上传的三个问题

### 4.1 请求超时硬编码 3 秒（最要紧）

**位置**：`dice/platform_adapter_official_qq.go` 的 `connect`

```go
pa.Api = qqbot.NewOpenAPI(pa.AppID, pa.tokenSource).WithTimeout(3 * time.Second)
```

**为什么致命**：这个超时作用在 SDK 的 resty client 上，是**所有请求共用**的
（文本发送、富媒体上传、拉取机器人信息都算）。而用 `url` 方式上传富媒体时，
腾讯要**先把整个文件下载完**才返回响应头，3 秒基本必然超时：

```
official qq 发送群聊消息时，准备语音信息失败：
Post "https://api.sgroup.qq.com/v2/groups/<id>/files": context deadline exceeded
(Client.Timeout exceeded while awaiting headers)
```

**建议**：

* 放宽到 30~60 秒（官方文档写明"上传接口超时建议 ≥5 秒"，且 URL 上传要等平台
  下载完整个文件；几十 MB 的文件建议 ≥120 秒）；
* 更好的做法是做成配置项（如 `officialQQRequestTimeoutSec`），并在配置修正时把 `0`
  补成默认值——否则老配置文件里该字段为 0 会让 `SetTimeout(0)` 使**所有**请求立即超时。

### 4.2 文件消息（`file_type=4`）未实现，`[CQ:file]` 被静默丢弃

**现状**：

* 群聊 `sendQQGroupMsgRaw` 的类型分支只有 Text / Reply / At / Image(1) / Record(3)，
  **没有 `*message.FileElement`**；
* 单聊 `sendC2CMsgRaw` 同样；
* `uploadGroupMedia` / `uploadC2CMedia` 的签名本来就是 `(…, fileType int)`，
  只是**从没传过 `4`**；
* `SendFileToPerson` / `SendFileToGroup` 是空实现，只回一句
  「[尝试发送文件 %s，但不支持]」。

但平台是支持的（官方文档表格里"文件"在单聊/群聊都是收发 ✅，软/硬限制 200MB）。

**建议**：两处 switch 各加一个分支，`SendFileTo*` 改为构造真实文件消息：

```go
case *message.FileElement:
    // file_type=4：任意格式，发送后展示为文件卡片
    media, err := pa.uploadGroupMedia(qctx, groupID, elem, 4)
    // ...与 ImageElement 分支相同的"先发当前消息再起新消息"逻辑
```

```go
func (pa *PlatformAdapterOfficialQQ) SendFileToGroup(ctx *MsgContext, uid, path, flag string) {
    pa.SendToGroup(ctx, uid, fmt.Sprintf("[CQ:file,file=%s]", message.EscapeCQParam(path)), flag)
}
```

**三个坑**：

1. 别用 `message.SealCodeToCqCode` 生成这段 CQ 码——它只认
   `[img:/图:/文本:/语音:/视频:]`，**不认识 `file`**，会原样返回；
2. 路径里的逗号 / 方括号必须用 `message.EscapeCQParam` 转义（`&#44;` / `&#91;`），
   否则 CQ 参数会被截断成一个不存在的路径；
3. 频道（QQ-CH）官方不支持文件，**不要**动频道分支。

### 4.3 `file_info` 的处理：群聊与单聊不一致

**现状**：

```go
// uploadGroupMedia：有 base64 解码
decodedFileInfo, decodeErr := base64.StdEncoding.DecodeString(media.FileInfo)
if decodeErr != nil {
    decodedFileInfo = []byte(media.FileInfo)
}

// uploadC2CMedia：直接把 []byte 透传，没有解码
return &dto.MediaInfo{FileInfo: media.FileInfo}, nil
```

**群聊那步解码是必要的（这一点请勿"优化"掉）**——我们在自己的分支上踩过：

官方文档写的是「file_info 内部为序列化二进制，开发者无需解析，直接透传即可」，
但 **SDK 的字段类型**决定了必须解码一次：

| 环节 | 字段类型 | 内容 |
|---|---|---|
| 上传接口返回 | `dto.Media.FileInfo` = `string` | base64 文本 |
| 适配器传出 | `dto.MediaInfo.FileInfo` = `[]byte` | **解码后的字节** |
| 发送接口序列化 | Go `encoding/json` 对 `[]byte` | 自动再做一次 base64 |

"先解码、再由 JSON 编码一次"正好还原成发送接口 `media.file_info` 期望的 base64 字符串。

我们曾按文档注释把这步删掉，发送立刻报：

```
code:400, {"message":"请求参数file_info无效","code":40034032}
```

图片和语音**全部发不出去**。建议：**保留解码**，并给单聊路径补上同样的解码，让两者一致。

### 4.4（增强）本地文件分片上传，以保留文件名

**现象**：修好 4.2 后文件能发出去，但客户端显示「未命名」。

**根因**：`file_data`(base64) 方式的上传接口**请求体没有文件名字段**
（`dto.MessageMediaToCreate` 只有 `file_type` / `url` / `srv_send_msg` / `file_data`）。
官方文档的对照关系：

| 方式 | 字段 | 能否自定义文件名 |
|---|---|---|
| URL 上传 | `url` | ✅ 平台用 URL 路径末段 |
| `file_data` | `file_data` | ❌ |
| 分片上传 | `upload_id` + `file_name` | ✅ |

**建议**：为"本地文件"实现官方推荐的分片上传四步流程：

```
① POST /v2/{groups|users}/{id}/upload_prepare
     { file_type, file_size, file_name, md5, sha1, md5_10m }
     → upload_id + block_size + parts[]（index + presigned_url）
② PUT  <presigned_url>                逐片上传分片数据
③ POST /v2/{groups|users}/{id}/upload_part_finish
     { upload_id, part_index, block_size, md5 }
④ POST /v2/{groups|users}/{id}/files
     { file_type, srv_send_msg:false, file_name, upload_id }   → file_info
```

**易错点**：

* `file_size` / `block_size` 在官方文档里是**字符串**，不是数字；
* `md5_10m` 是**文件前 10002432 字节**的 MD5，不是整个文件的；
* 预签名 URL 自带鉴权，PUT 时**不要**附加 `Authorization` / `X-Union-Appid`，
  否则签名校验失败；
* 单聊与群聊的上传 / 预上传 / 分片端点**互相独立**，不能跨场景复用；
* botgo SDK **只封装了 `/files`**，`upload_prepare` 与 `upload_part_finish`
  需要适配器自己发请求；鉴权头与 SDK 一致：
  `Authorization: <token_type> <access_token>` + `X-Union-Appid: <appID>`；
* 从 `file://` URL 推断文件名时要**百分号解码**，否则带空格/中文的文件名会变成
  `%E5%B8%A6%20...`；
* 建议**默认关闭**、只对本地文件启用（远程 URL 仍走 URL 上传），
  万一出问题可以一键回退，不影响已经稳定的语音/图片。

### 4.5 验证方式

| 项 | 方法 |
|---|---|
| 超时 | 把超时设小（如 5 秒）复现 `context deadline exceeded`；设大后同一文件可正常发出 |
| 文件消息 | `[CQ:file,file=...]` 应产生一次 `file_type=4` 的上传请求，并在发送接口收到 `msg_type=7` + `media.file_info` |
| `file_info` | 构造返回 `AE86C5D3F0E14B238C656C0F6DD1D0479C`（32 位十六进制，恰好是合法 base64），断言最终发送值与接口返回一致 |
| 分片上传 | 用本地假服务器验证四步请求字段：大小与校验值正确、分片内容与源文件一致、合并请求带 `upload_id` 与 `file_name` |
| 不回归 | 图片 / 语音发送保持原样；频道分支未被改动 |

---

## 五、改动规模参考

| 能力 | 新增 | 修改 |
|---|---|---|
| 状态栏 | 一个约 170 行的文件 | `DiceFormatTmpl` 打标记、发送层两处消费、`MsgContext` 加字段、`ext_log.go` 抽取求值函数 |
| 文案模板 | 无 | `.help` / `.bot about` 两处取模板、若干硬编码改占位符 |
| 富媒体 | 分片上传约 450 行（可选） | 超时改配置、两处 switch 加文件分支、`SendFileTo*` 实现、单聊补 `file_info` 解码 |

---

## 六、提交前的隐私清单

如果要以 PR 形式提交，请确认以下内容**不会**进入 diff：

- [ ] `serve.yaml`、数据库、群聊日志、角色卡、自定义表情包
- [ ] QQ Bot Secret / AppSecret、API Key、签名私钥、webhook Token
- [ ] 服务器 IP、内网地址、恢复群、私有 API 地址
- [ ] 未经授权的头像、昵称、群号、QQ 号、自定义鉴定词
- [ ] 私人定制的默认文案（如自定义的骰娘名字、帮助文本）

建议把每个能力拆成**独立 PR**，便于上游 review。
