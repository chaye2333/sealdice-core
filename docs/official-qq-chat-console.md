# 官方 QQ 聊天控制台

> 在 WebUI 里像用桌面版 QQ 一样，直接看官方 QQ 机器人收到的消息、并以机器人身份发消息。
> 只覆盖官方 QQ（`QQ / official`）这一侧；民间 bot（OneBot、Gocq、Lagrange 等）不参与。

入口：WebUI → 扩展功能 → **聊天控制台**（`#/mod/chat-console`）。

---

## 一、能做什么

| 能力 | 说明 |
|---|---|
| 会话列表 | 官方机器人参与过的群聊与私聊，显示群名/昵称、最后一条消息、未读数、在线状态 |
| 消息流 | 收到的消息、骰子自动回复、骰主从控制台发出的消息，三种来源用不同颜色区分 |
| 图片/语音/文件 | 收到的附件以图片预览 / 链接展示（官方链接有有效期，过期后只能看到链接）；发送支持图片/语音/视频/文件，上限 30MB |
| 发送 | 以机器人身份发文字；群聊支持完全放开，私聊受官方主动消息额度限制 |
| 引用回复 | 右键「回复」→ 引用条 + 正文。官方主动消息挂不了 `msg_id`，所以是**伪引用**：正文前加「回复 某人：原文」 |
| @ 群成员 | 群聊输入框旁「@成员」，从最近收到过消息的成员里选；发送时转成官方 `<qqbot-at-user>` 标签 |
| 撤回 | 只能撤回**机器人自己发的**消息，官方限制时间；撤回后历史里显示"该消息已被撤回" |
| 禁言 / 解禁 | 群成员消息右键「禁言」，支持 1 分钟 / 10 分钟 / 1 小时 / 1 天 / 解除；需要机器人是群管理员 |
| 存储管理 | 顶部「存储」按钮：查看总占用 / 每个群占用（字节、条数、天数），调整保留天数（0 = 永久），一键清空单个会话或全部历史 |
| 实时 | 新消息通过 SSE 自动出现；SSE 断开时前端降级为定时轮询 |
| 改名 | 群名拉不到或被官方省略号截断时，可以手动给会话起名 |

## 二、做不到什么（平台限制，不是 bug）

- **群消息是否完整取决于群主授权**：官方群主在群设置里开启"允许机器人获取群消息"后，控制台才能看到全部群消息；
  未授权的群只能看到 @ 机器人的消息，界面上会显示 `仅 @ 消息` 标签。
- **无法回溯历史**：官方不提供历史消息接口，控制台只记录功能启用之后收到的消息。
- **私聊主动消息有额度**：群聊的主动消息在群主授权后基本放开，私聊仍有额度/频率限制；
  发送失败时界面会显示具体原因（额度、权限、鉴权、内容不合法）。
- **不镜像频道（Guild）**：本功能只做群聊与私聊。
- **不记录被拦截的消息**：消息在适配器入口就记录了，早于拦截/打码，这一点和"拦截"功能互相独立。
- **引用是"伪引用"**：官方主动消息不能挂 `msg_id`，所以引用只会以「回复 某人：原文」的形式出现在正文里，
  不是 QQ 客户端里那种真引用块（被动回复窗口只有 5 分钟 / 5 条，控制台做不到）。
- **撤回只能撤回机器人自己的消息**，且受官方时间限制；撤回群里别人的消息需要管理员权限，官方未开放。
- **禁言需要机器人是群管理员**，不能禁言群主/其他管理员，单次最长 30 天，接口 60 次/分钟。
- **踢人、改群名片、退群**：官方没有开放对应接口（适配器里是空实现），控制台也做不了。

## 三、数据存在哪

```
<dataDir>/chat-console/
├── console.db        # 独立的 SQLite 库：会话元信息 + 消息（不动 data.db）
├── console.db-wal    # SQLite 的 WAL，正常现象
├── console.db-shm
├── settings.json     # 保留天数（可调，0 = 永久保留）
└── legacy-jsonl/     # v1.1 及以前的 index.json + 按天 jsonl，自动导入后挪到这里，可删
```

- **为什么用 SQLite**：500 个群的公开机器人，jsonl 每次翻页都要扫整个日文件、撤回要顺着文件倒着找，
  长期用会越来越慢；SQLite 用 `(conv_key, ts)` 和 `msg_id` 索引，翻页 / 撤回 / 按天删除都是走索引。
- 库文件**独立于** `data.db`：不改任何既有 schema，删掉 `chat-console/` 目录等于清空控制台，回退安全。
- 表：`chat_console_conversations`（会话）、`chat_console_messages`（消息），`PRAGMA journal_mode=WAL`。
- 保留天数**默认 30 天**，可在「存储」里改成任意天数（上限 3650），**0 = 永久保留**；
  清理每小时最多执行一次，改小保留天数时会立刻清理一次。
- 「清空全部历史」会 `DELETE` 后执行 `VACUUM` 把库文件真正缩回去；单个会话清空只删数据，空间由 SQLite 复用。
- 占用面板里的「合计占用」是**实际磁盘大小**（console.db + wal/shm + settings + legacy），
  每个会话的字节数按列长度 + 固定开销**估算**，两者不会完全对得上。
- 媒体只存 URL 等元信息，**不下载原件**（官方链接过期后失效）。
- 不写数据库（主库）、不参与身份绑定归一、不影响跑团日志；
- 内置备份（`data.db` / 牌堆 / 日志等）**不包含** `chat-console/`，不会撑大备份。

**从旧版升级**：v1.1 及以前用的是 `index.json` + 按天 jsonl，首次启动会**自动导入**到 `console.db`，
原始文件整体挪进 `legacy-jsonl/` 留一份（不直接删）；确认数据没问题后可以直接删掉这个目录，
它的占用也会出现在「存储」面板里。

**回退**：删掉整个 `chat-console/` 目录即可；旧版本程序不认识该目录，也不会读写。

## 四、接口

所有接口走 WebUI 的 AccessToken 鉴权（`token` 请求头或 `token` 查询参数）。

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/sd-api/chat-console/conversations` | 会话列表（按最后消息时间倒序） |
| GET | `/sd-api/chat-console/messages?key=&before=&limit=` | 历史消息，向前分页，按时间升序返回 |
| POST | `/sd-api/chat-console/send` | `{key, text, quote?, mentions?}` 以机器人身份发送 |
| POST | `/sd-api/chat-console/upload` | multipart：`key` + `file`，发送图片/语音/视频/文件（≤30MB） |
| POST | `/sd-api/chat-console/recall` | `{key, msgId}` 撤回机器人自己发的消息 |
| POST | `/sd-api/chat-console/mute` | `{key, memberOpenId, seconds}` 禁言（seconds=0 为解除） |
| GET | `/sd-api/chat-console/storage` | 保留天数 + 总占用 + 每个会话的占用/条数 |
| POST | `/sd-api/chat-console/settings` | `{retentionDays}` 调整保留天数（0 = 永久） |
| POST | `/sd-api/chat-console/purge` | `{key}` 清空单个会话；`{all:true}` 清空全部 |
| POST | `/sd-api/chat-console/read` | `{key}` 清未读 |
| POST | `/sd-api/chat-console/rename` | `{key, name}` 手动改名 |
| GET | `/sd-api/chat-console/events?token=` | SSE 实时推送（`message` / `recall` / `conversation` 事件） |

会话 key 形如 `group:<group_openid>`、`c2c:<user_openid>`，
与 dice 内部的 `OpenQQ-Group:<uin>-<openid>` 是两套 ID，互不干扰。

## 五、实现位置

- `dice/chat_console.go`：存储、会话索引、去重、保留策略、事件订阅；
- `dice/chat_console_official_qq.go`：官方 QQ 适配器侧的收发钩子、群信息补全、控制台发送、错误翻译；
- `api/chat_console.go`：REST + SSE；
- 前端：`sealdice-ui` 的 `src/components/mod/PageChatConsole.vue` 与 `src/api/chat_console/index.ts`。

设计稿见 `docs/official-qq-chat-console-design.md`。

## 六、排查

| 现象 | 排查 |
|---|---|
| 列表一直为空 | 官方 bot 是否已连接（账号设置里状态为在线）？连接后需要真的收到过一条消息才会建会话 |
| 群里只能看到 @ 消息 | 群主没在官方群设置里授权"获取全部群消息" |
| 群名显示 `群 XXXXXX` | `/v2/groups/{id}/info` 没拉到（权限/频率），可以手动改名，之后不会再被覆盖 |
| 私聊发送失败 | 官方主动消息额度或频率限制，等一会儿再试；错误里有原始返回 |
| 实时不更新 | 看标题右侧"实时连接中"；断开时会自动重连并降级轮询 |
