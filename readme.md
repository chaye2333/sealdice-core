# 鲸娘与豹 · sealdice-core 分支

> 一个以「QQ 官方机器人」为中心的 sealdice-core 分支：
> 让**官 bot 与民间 bot 的数据实时共通**，并把官 bot 缺的那部分能力补齐
> （虚拟角色状态栏、发送文件保留文件名……）。
> 代码主要由 AI 写成，**仅供体验**；用之前请读完这份说明，并做好数据备份。

---

## ⚠️ 先看这里

- **没有改动数据库结构**：全分支没有新增表、没有新增字段，换回官方主线可以直接沿用同一份 `data/`。
- **本项目跟随海豹主线**：定期 `git fetch upstream && git merge upstream/master`，
  冲突一律「主线优先」。最近一次同步：**2026-09-16**（已并入上游 #1811 / #1817 / #1819）。
- 但「跟随主线」不等于零风险：绑定功能侵入了 `dice/ext_log.go`、
  `dice/platform_adapter_official_qq.go`、`dice/builtin_commands.go` 等文件，
  上游大改这些地方时仍然需要手工合并。
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

### 2. 验证码（防抢号）

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

### 3. 虚拟角色状态栏（官 bot 专属）

官方机器人**改不了群名片**，所以用 `.sn` 模板渲染成一行小号红字挂在回复顶部：

```
$\scriptsize\textcolor{#E5484D}{\text{马丁·弗卢吉尔 SAN69 HP12/12 DEX65}}$
```

- 名字取自**当前绑定的角色卡**，`.pc tag` 换卡后立刻跟着变
- 属性实时求值，不硬编码任何规则系统（COC / DND / FU / 自定义 `.sn expr` 都行）
- `.sn off` 可彻底关掉；**需要开启「官方QQ使用Markdown」**，否则状态栏自动隐藏
- 只对官方 QQ 端点生效，OneBot / TG / Discord 等平台回复格式完全不变

### 4. 官方 QQ 适配增强

| 改动 | 说明 |
|---|---|
| 分片上传 | `officialQQChunkedUploadEnable` 打开后，本地文件（≥1MB）走分片上传，**能保留文件名**；默认关 |
| 请求超时可配 | `officialQQRequestTimeoutSec`（默认 60 秒，与上游一致），5~600 |
| 掉线防崩 | 连接失败后端点 `Enable` 仍为 true，旧实现发消息会 nil 接口 panic 并**冲掉整条 websocket**；现在发送入口有守卫 |
| 邮件 | SMTP 支持 `host` 或 `host:port`，默认 465（隐式 TLS）、587 强制 STARTTLS，失败会如实报错 |

### 5. 其它

- `.help` 第一行固定为 `鲸娘与豹 <版本号>`
- 私聊（`PG-` 伪群号）也参与归一：官方私聊与旧号私聊共用同一份属性/默认卡
- `.group doctor` / `.bind doctor` 一键自检所有绑定

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

产物在 `dist-build\sealdice-core.exe`（脚本会先把管理界面装进 `static/frontend` 再编译）。
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
| `officialQQRequestTimeoutSec` | `60` | 官方 QQ 请求超时（5~600） |
| `officialQQChunkedUploadEnable` | `false` | 本地文件分片上传（保留文件名） |
| `logMultiBotDedupWindowSec` | `5` | **建议保持 5**：调大并不能跨 bot 去重，只会把同一人窗口内的重复正文并成一条 |

已删除的历史配置项：`identityBindQuestionCount`、`identityBindKeepQuiz`、
`identityBindFailCooldownSec`、`identityBindUseEmailCode`（答题路径与邮箱开关都已取消；
残留的旧键会被静默忽略）。

---

## 更详细的文档

- [`docs/identity-bind-and-ghcr.md`](docs/identity-bind-and-ghcr.md)
  —— 绑定与验证码的完整说明、镜像构建、常见问题与排查手册
- [`docs/how-to-revert-to-upstream.md`](docs/how-to-revert-to-upstream.md)
  —— **回退到官方主线**的操作手册：数据可见性、`serve.yaml` 兼容性、改动清单

回退只需把镜像换成官方版本，`data/` 不用动；但绑定期间产生的数据全部记在**旧身份**名下，
回退后官方侧读不到（数据没丢，只是不带绑定功能的版本不会做归一），详见上面那份文档。

---

## 已知限制

- 群绑定的确认人是**旧群邀请人**：邀请人换号或没开 QQ 邮箱时，需要骰主手动确认兜底
- 群绑定要求旧群对象在内存里（骰子进过那个群），否则拿不到邀请人
- **同一个群里同时挂官 bot 与民间 bot 时无法去重**：同一条消息会记两遍，日志量 ×2
  （一般用法是官 bot 在官方群、民间 bot 在旧群，不受影响）
- 群配置（前缀、自定义回复、牌堆、扩展开关）**不共享**，只共享日志与玩家数据
- 群绑定后只看得到归一后（旧群）那一份日志；官方群自己那份**不会被删**，解绑或回退后又能看到

---

## 授权

fork 自 [sealdice/sealdice-core](https://github.com/sealdice/sealdice-core)，
沿用上游的 **MIT License**（见 [LICENSE](LICENSE)）。上游版权归 sealdice 项目所有。

上游文档与手册：<https://sealdice.com> · 官方群 524364253
