# 上游 Issue 稿件（三篇）

用途：把本分支实现的三项能力整理成可直接提交到
[sealdice/sealdice-core](https://github.com/sealdice/sealdice-core) 的 issue / PR 说明。

每篇都是**独立**的，可以分别提交。稿件里引用的是**上游主分支的文件与函数名**
（不是本分支的行号），行号仅作定位参考、可能随上游变化。

提交前请自行确认：

- [ ] 上游当前版本是否已经实现（避免重复提）
- [ ] 稿件里没有夹带任何私人配置、密钥、服务器地址、群号、QQ 号
- [ ] 复现步骤在本机能跑通

---

# 稿件一：QQ 官方机器人缺少「虚拟角色状态栏」

## 标题建议

`[Feature] QQ 官方机器人无法改群名片，建议支持「虚拟角色状态栏」（读取 .sn 模板实时渲染）`

## 现象

QQ 官方机器人没有修改群成员名片的接口——
`dice/platform_adapter_official_qq.go` 里 `SetGroupCardName` 是空实现：

```go
func (pa *PlatformAdapterOfficialQQ) SetGroupCardName(_ *MsgContext, _ string) {}
```

因此玩家用 `.sn`（名片模板）设置的属性栏在官方 QQ 上**完全没有效果**：

```
.sn dnd        # 玩家设置了属性模板
.r 1d20        # 回复里看不到任何属性
```

而同一个玩家在 OneBot / Telegram / Discord 上是正常的，因为那些平台真的会改名片。

## 期望

在官方 QQ 的掷骰 / 鉴定回复**顶部**渲染一条"虚拟状态栏"，代替改名片：

```
$\scriptsize\textcolor{#E5484D}{\text{侦探 HP12/12 AC16}}$
侦探的"侦查"检定结果为: (1)=1/50 成功
```

* 属性行用 QQ Markdown 的数学片段实现**小号红字**；
* 角色名与鉴定正文保持正常字号。

## 建议实现（最小改动）

### 1. 把「只计算 .sn 模板」从改名里抽出来

现在 `SetPlayerGroupCardByTemplate`（`dice/ext_log.go`）同时做两件事：求值模板 + 调平台改名。
抽出一个纯求值函数即可复用：

```go
// 只求值，不调用 SetGroupCardName
func EvalPlayerGroupCardTemplate(ctx *MsgContext, tmpl string) (string, error)

// 原函数改为：先求值，再改名
func SetPlayerGroupCardByTemplate(ctx *MsgContext, tmpl string) (string, error)
```

### 2. 从 .sn 模板里抽出「属性部分」

`.sn` 模板的通用形态是「玩家名占位符 + 属性表达式」：

```
{$t玩家_RAW} HP{hp}/{hpmax} AC{ac}
{$t玩家_RAW} SAN{理智} HP{生命值}/{生命值上限} DEX{敏捷}
{$t玩家_RAW}
```

把玩家名占位符删掉、只对剩下的部分求值，就得到属性行：

```go
var playerPlaceholderRe = regexp.MustCompile(`\{\s*\$t玩家(?:_RAW)?\s*\}`)
attrPart := playerPlaceholderRe.ReplaceAllString(ctx.Player.AutoSetNameTemplate, "")
```

**关键点：不要为每种规则硬编码字段表**。这样 `.sn coc` / `.sn dnd` /
`.sn expr ...` / 未来新增的写法全部自动兼容。

### 3. 组装小号红字

```go
`$\scriptsize\textcolor{#E5484D}{\text{<转义后的"角色名 属性行">}}$`
```

角色名与属性都放在同一个片段内即可整体显示为小号红字。

### 4. 表达式里的特殊字符必须转义

属性值里可能出现 `\ { } $ # % _ ^ & ~`，它们会提前结束数学片段或被当成 LaTeX 命令。
建议整体替换成全角字符，并把换行压成空格（保证状态栏始终是单行）：

```go
var mathEscapeReplacer = strings.NewReplacer(
    `\`, `＼`, `{`, `｛`, `}`, `｝`, `$`, `＄`, `#`, `＃`,
    `%`, `％`, `_`, `＿`, `^`, `＾`, `&`, `＆`, `~`, `～`,
    "\r\n", " ", "\n", " ", "\r", " ", "\t", " ",
)
```

### 5. 挂载位置：建议放在发送层，不要逐个规则改（重要）

> 这条是我们在实现中**踩过的坑**，建议上游直接按正确做法落地。

最初我们是逐个在 `ext_coc7.go` / `ext_dnd5e.go` / `builtin_commands.go` 里调
「加状态栏」，结果 **FU / SH / 第三方 JS 扩展的检定全都没有状态栏**——
因为这些规则走的是各自的检定实现。

正确做法是两段式：

1. `DiceFormatTmpl`（`dice/rollvm_migrate.go`）在渲染「最终回复」模板时打标记：

   ```go
   var finalReplyTmplKeys = map[string]bool{
       "核心:骰点": true, "核心:骰点_多轮": true,
       "COC:检定": true, "COC:检定_多轮": true, "COC:理智检定": true,
   }
   if finalReplyTmplKeys[s] {
       ctx.OfficialQQStatusBarPending = true   // MsgContext 上新增的字段
   }
   ```

2. 发送层消费标记（`dice/im_helpers.go` 的 `replyGroupRawNoCheck` /
   `replyPersonRawNoCheck`，两个都要，暗骰走私聊）：

   ```go
   text = withOfficialQQCharacterStatusBar(ctx, text)   // 内部读标记并在顶部插入，随后清除标记
   ```

好处：任何规则系统（含未来新增的扩展）只要用这些核心模板渲染检定结果，
就自动带状态栏；帮助文本、错误提示等不受影响（它们不是这些模板）。

### 6. 只在官方 QQ 生效

```go
func isOfficialQQEndpoint(ep *EndPointInfo) bool {
    return ep != nil && ep.Platform == "QQ" && ep.ProtocolType == "official"
}
```

OneBot / Telegram / Discord 等平台行为必须**完全不变**。

### 7. 渲染要有 panic 兜底

状态栏挂在每条掷骰回复的热路径上，而求值链内部有若干 `lo.Must` 断言
（例如 `loadAttrValueByName` 里的 `lo.Must(ctx.Dice.AttrsManager.LoadByCtx(ctx))`）。
模板写错或数据库临时不可用会 panic，把整条掷骰指令带崩。
建议包一层 `recover`，出错时只跳过属性行并写 warn 日志。

## 前提 / 坑

* **必须开启 `officialQQUseMarkdown`**（`msg_type=2`），否则客户端会显示原始公式
  `$\scriptsize...$`。可在状态栏不生效时给一条日志提示，便于排查。
* `.sn none` / `.sn off` 会把 `AutoSetNameTemplate` 清空，此时应**不显示**状态栏。
* `.sn` 只设了名字（没有属性）时，只显示角色名，不要留空行。
* 建议开放配置项（开关 + 颜色）给骰主，但不要写入 / 改写用户的
  `SeaDice-text-template.yaml`。

## 参考实现规模

新增一个文件约 170 行 + 上述 5 处小改动。我们这边的实现与测试：
`dice/official_qq_character_roll_markdown.go`、
`dice/official_qq_character_roll_markdown_test.go`（12 个测试，含"任意规则系统的检定
自动带状态栏""普通回复不带""标记只消费一次""属性实时更新""特殊字符转义"）。

---

# 稿件二：自定义文案（text-template）相关

## 标题建议

`[Feature] 建议开放「核心:骰子名字」以外的程序标题文案，或明确其可自定义范围`

## 现象 / 痛点

`dice/config.go` 里 `核心:骰子名字` 的默认值是「海豹核心」：

```go
"核心": {
    "骰子名字": {
        {"海豹核心", 1},
    },
```

它被用在多处**用户可见**的位置（如"不支持连续检定 N 次，**海豹核心**觉得这太多了"），
骰主可以在 `text-template.yaml` 里改名——这部分设计没问题。

**问题在于同名文案出现在两个语义不同的地方**：

| 位置 | 语义 | 是否走模板 |
|---|---|---|
| 掷骰文案、入群提示等 | **玩家侧骰娘名字**（骰主常改成"罗兰"这类） | ✅ 走 `核心:骰子名字` |
| `.help`（无参数）第一行 | **程序自身标题** + 版本号 | ❌ 硬编码 `"海豹核心 " + VERSION` |

结果：骰主把 `核心:骰子名字` 改成「罗兰」之后，`.help` 里仍然显示「海豹核心」，
两边不一致；骰主如果想让 `.help` 也跟着变，**没有任何配置能做到**（只能改代码重新编译）。

类似地，`.bot about` 第一行是硬编码的 `SealDice %s`，同样无法配置。

## 建议

任选一种（我们倾向第 1 种）：

1. **把两个标题也纳入文案模板**，例如新增
   `核心:程序标题`（默认「海豹核心」）与 `核心:关于标题`（默认「SealDice」），
   同时保留现有硬编码作为兜底：

   ```go
   title := strings.TrimSpace(DiceFormatTmpl(ctx, "核心:程序标题"))
   if title == "" || strings.HasPrefix(title, "<%未知项") {
       title = "海豹核心"
   }
   text := title + " " + VERSION.String() + "\n"
   ```

2. **只补文档与注释**：说明 `.help` / `.bot about` 的标题是程序名、不随
   `核心:骰子名字` 变化，避免骰主误以为改一处就够。

## 附带发现（建议一并修）

入群强制邀请退群、被拉黑退群等提示里的程序名也是**硬编码**的：

```
dice/platform_adapter_gocq.go
dice/platform_adapter_onebot.go
dice/platform_adapter_walleq.go
```

```go
text := fmt.Sprintf("...感谢使用海豹核心。", groupInfo.InviteUserID)
```

这几处**用户可见**，却和其他文案不一致（改不了）。建议改成 `{核心:骰子名字}`
占位符（海豹的文案引擎支持嵌套 `{核心:骰子名字}`），或统一走新的 `核心:程序标题`。

---

# 稿件三：官方 QQ 富媒体上传的三个问题

## 标题建议

`[Bug] QQ 官方机器人：请求超时 3 秒导致语音/文件必然超时；文件消息未实现；file_info 处理不一致`

## 3.1 请求超时硬编码 3 秒 → 语音/文件基本发不出去（最要紧）

**位置**：`dice/platform_adapter_official_qq.go` 的 `connect`

```go
pa.Api = qqbot.NewOpenAPI(pa.AppID, pa.tokenSource).WithTimeout(3 * time.Second)
```

**为什么要命**：这个超时作用在 SDK 的 resty client 上，是**所有请求共用**的。
而用 `url` 方式上传富媒体时，腾讯要**先把整个文件下载完**才返回响应头，
3 秒基本必然超时：

```
official qq 发送群聊消息时，准备语音信息失败：
Post "https://api.sgroup.qq.com/v2/groups/<id>/files": context deadline exceeded
(Client.Timeout exceeded while awaiting headers)
```

**建议**：

* 至少放宽到 30~60 秒（官方文档写明「上传接口超时建议 ≥5 秒」，
  且 URL 上传要等平台下载完整文件；发几十 MB 文件建议 ≥120 秒）；
* 更好的是做成配置项（如 `officialQQRequestTimeoutSec`），并在反序列化/配置修正时
  把 `0` 补成默认值——否则老配置文件里该字段为 0 会让 `SetTimeout(0)` 使所有请求立即超时。

## 3.2 文件消息（`file_type=4`）未实现，`[CQ:file]` 被静默丢弃

**现状**：

* 群聊 `sendQQGroupMsgRaw` 的类型分支只有 Text / Reply / At / Image(1) / Record(3)，
  没有 `*message.FileElement`；
* 单聊 `sendC2CMsgRaw` 同样；
* `uploadGroupMedia` / `uploadC2CMedia` 的签名本来就是 `(…, fileType int)`，只是从没传过 `4`；
* `SendFileToPerson` / `SendFileToGroup` 是空实现，只回一句
  「[尝试发送文件 %s，但不支持]」。

但平台是支持的（文档表格里"文件"单聊/群聊都是收发 ✅，软/硬限制 200MB）。

**建议**：两处 switch 各加一个分支，`SendFileTo*` 改为构造真实文件消息：

```go
case *message.FileElement:
    // file_type=4：任意格式，发送后展示为文件卡片
    media, err := pa.uploadGroupMedia(qctx, groupID, elem, 4)
    // ...与 ImageElement 分支相同的拼接逻辑
```

```go
func (pa *PlatformAdapterOfficialQQ) SendFileToGroup(ctx *MsgContext, uid, path, flag string) {
    pa.SendToGroup(ctx, uid, fmt.Sprintf("[CQ:file,file=%s]", message.EscapeCQParam(path)), flag)
}
```

**两个坑**：

* 别用 `message.SealCodeToCqCode` 生成这段 CQ 码，它只认
  `[img:/图:/文本:/语音:/视频:]`，不认识 `file`；
* 路径里的逗号 / 方括号必须用 `message.EscapeCQParam` 转义（`&#44;` / `&#91;`），
  否则 CQ 参数会被截断成一个不存在的路径；
* 频道（QQ-CH）场景官方不支持文件，**不要**动频道分支。

## 3.3 `file_info` 的处理：群聊与单聊不一致（并附一条"别改"的说明）

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

**为什么群聊那步解码是必要的**（**这一点请勿"优化"掉**，我们在自己的分支上踩过）：

官方文档写的是「file_info 内部为序列化二进制，开发者无需解析，直接透传即可」，
但 SDK 的字段类型决定了必须解码一次：

| 环节 | 字段类型 | 内容 |
|---|---|---|
| 上传接口返回 | `dto.Media.FileInfo` = `string` | base64 文本 |
| 适配器传出 | `dto.MediaInfo.FileInfo` = `[]byte` | **解码后的字节** |
| 发送接口序列化 | Go `encoding/json` 对 `[]byte` | 自动再做一次 base64 |

「先解码、再由 JSON 编码一次」正好还原成发送接口 `media.file_info` 期望的 base64 字符串。
我们曾按文档注释把这一步删掉，结果发送立刻报：

```
code:400, {"message":"请求参数file_info无效","code":40034032}
```

图片和语音**全部发不出去**。所以建议：**保留解码**，并给单聊路径补上同样的解码，让两者一致。

## 3.4（可选增强）本地文件分片上传，以保留文件名

**现象**：修复 3.2 后文件能发出去，但客户端显示「未命名」。

**根因**：`file_data`(base64) 方式的上传接口请求体没有文件名字段
（`dto.MessageMediaToCreate` 只有 `file_type` / `url` / `srv_send_msg` / `file_data`）。
官方文档的对照关系：

| 方式 | 字段 | 能否自定义文件名 |
|---|---|---|
| URL 上传 | `url` | ✅ 平台用 URL 路径末段 |
| `file_data` | `file_data` | ❌ |
| 分片上传 | `upload_id` + `file_name` | ✅ |

**建议**：为「本地文件」实现官方推荐的分片上传四步流程：

```
① POST /v2/{groups|users}/{id}/upload_prepare
     { file_type, file_size, file_name, md5, sha1, md5_10m }
     → upload_id + block_size + parts[]（index + presigned_url）
② PUT  <presigned_url>                逐片上传
③ POST /v2/{groups|users}/{id}/upload_part_finish
     { upload_id, part_index, block_size, md5 }
④ POST /v2/{groups|users}/{id}/files
     { file_type, srv_send_msg:false, file_name, upload_id }   → file_info
```

**注意点**（都容易写错）：

* `file_size` / `block_size` 在官方文档里是**字符串**；
* `md5_10m` 是**文件前 10002432 字节**的 MD5，不是整个文件的；
* 预签名 URL 自带鉴权，PUT 时**不要**附加 `Authorization` / `X-Union-Appid`，
  否则签名校验失败；
* 单聊与群聊的上传 / 预上传 / 分片端点**互相独立**，不能跨场景复用；
* botgo SDK 只封装了 `/files`，`upload_prepare` 与 `upload_part_finish`
  需要适配器自己发请求（鉴权头与 SDK 一致：`Authorization: <token_type> <token>`
  与 `X-Union-Appid: <appID>`）；
* 从 `file://` URL 推断文件名时要做**百分号解码**，否则带空格/中文的文件名会变成
  `%E5%B8%A6%20...`；
* 建议默认关闭（配置开关），只对本地文件启用，远程 URL 仍走 URL 上传。

## 验证方式（供 reviewer 参考）

* 超时：把 `officialQQRequestTimeoutSec` 设小（如 5）复现 `context deadline exceeded`；
  设大后同一文件可正常发出。
* 文件消息：`[CQ:file,file=...]` 应产生一次 `file_type=4` 的上传请求，
  并在发送接口收到 `msg_type=7` + `media.file_info`。
* `file_info`：人为构造返回 `AE86C5D3F0E14B238C656C0F6DD1D0479C`（32 位十六进制，
  恰好是合法 base64），断言最终发送的值与接口返回一致（解码后再编码等价）。
* 分片上传：用本地假服务器验证四步请求的字段（大小与校验值、分片内容与源文件一致、
  合并请求带 `upload_id` 与 `file_name`）。

---

## 附：本分支对应的实现与测试（供 PR 参考）

| 能力 | 实现 | 测试 |
|---|---|---|
| 状态栏 | `dice/official_qq_character_roll_markdown.go` | `dice/official_qq_character_roll_markdown_test.go` |
| 分片上传 | `dice/platform_adapter_official_qq_chunked.go` | `dice/platform_adapter_official_qq_chunked_test.go` |
| 富媒体修复 | `dice/platform_adapter_official_qq.go` | `dice/platform_adapter_official_qq_media_test.go` |
| 模板求值抽取 | `dice/ext_log.go`（`EvalPlayerGroupCardTemplate`） | — |

> 提交 PR 前请先把本分支的私人改动（身份绑定、`.help` 改名、Docker/CI、自定义文案）
> 剔除，只保留上面这几项与上游主分支相关的能力。
