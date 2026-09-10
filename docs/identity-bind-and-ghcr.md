# QQ身份绑定 + 虚拟角色状态栏 + GHCR 自动构建：使用与部署说明

面向：把海豹从 NapCat（OneBot）迁移到 **QQ 官方机器人**，希望老玩家的角色卡 / 日志不丢，
并且官方机器人也能显示角色属性的人。

本说明只讲怎么用、怎么发布镜像、怎么验收。改动清单见文末。

---

## 一、这个功能解决什么问题

| | 旧（NapCat / OneBot） | 新（QQ 官方机器人） |
|---|---|---|
| 玩家 ID | `QQ:123456` | `OpenQQ:<机器人UIN>-<MemberOpenID>` |
| 群 ID | `QQ-Group:789` | `OpenQQ-Group:<机器人UIN>-<GroupOpenID>` |

官方机器人的 `OpenID` 和 QQ 号**没有任何换算关系**，所以迁移后：

* 老玩家的角色卡（`.st` / `.pc`）读不到了；
* 老群的日志（`.log list` / `.log get`）看不到了。

本功能让玩家在官方 bot 里**自证身份**，然后把新身份"指"回旧身份：

* 绑定后**只做读取回退**：读旧数据、写新数据；
* 一条历史数据都不会被搬动、修改，`.unbind` 之后立刻恢复原样；
* **只有 QQ 官方机器人端点**会用到绑定结果，你其它的 OneBot 群行为完全不变。

---

## 二、先决条件

* 你已经能用官方机器人正常收发消息（`.r 1d20` 有反应）。
* 迁移前的旧数据**还在同一个数据库里**（也就是同一个 `data` 目录）。
  如果你已经换过服务器/重建过数据库，旧数据不存在，本功能就没有数据可读——这是正常的，不是 bug。
* 旧数据里玩家确实有角色卡（`.pc list` 能看到），或旧群确实有日志。

---

## 三、开启功能

### 3.1 怎么打开（重要：界面上暂时看不到开关）

**先说结论：目前管理界面里找不到这个开关，这是正常的。**

原因是前端来源：本仓库的 Dockerfile 会把 `static/frontend` 用 `go:embed` 打进二进制，
而这份前端是**从官方 `sealdice-ui` release 下载的预编译产物**
（见 `static/gen/download-fe.go`）。官方前端里当然没有我新加的配置项，
所以界面不会渲染出这个分组。后端 API 其实是完整的（`DiceConfig` 会把整个
`Config` 返回给前端，包含这几个字段），缺的只是界面控件。

**所以现在请改配置文件开启**，路径在容器内是 `/app/data/default/serve.yaml`
（宿主机上就是你映射的 `./data/default/serve.yaml`）。

打开该文件，找到文件末尾区域，加入下面三行（如果已存在就改值）：

```yaml
identityBindEnable: true
identityBindQuestionCount: 1
identityBindCooldownSec: 60
```

然后重启容器：

```bash
docker restart sealdice-core
```

**不用担心被界面覆盖**：`DiceConfigSet`（`api/dice_config.go`）只处理请求里出现过的
键，界面保存设置时不会碰这几个键；而 `saveLocked` 会把整个 `Config` 写回去，
所以手改的值会一直保留。

### 3.2 配置项说明

| 配置项 | 配置文件字段 | 默认 | 说明 |
|---|---|---|---|
| 启用身份与日志绑定 | `identityBindEnable` | `false` | 总开关，默认关闭，需手动打开 |
| 验证题目数量 | `identityBindQuestionCount` | `1` | 1~5，超过 5 会被收敛到 5 |
| 绑定冷却时间(秒) | `identityBindCooldownSec` | `60` | 同一用户两次发起绑定的最小间隔，0 表示不限制 |

题目数量怎么选：

* `1` 题最省事，适合信任度高的群；
* 老玩家角色卡名比较独特时，`1` 题已经足够（要在他自己的多张卡里选对）；
* 想更严格可以调到 `2`~`3`，玩家要连着答对。

### 3.3 怎么验证开关生效

在官方 bot 群里发 `.bind`（不带参数），如果返回的是帮助文本、而不是
「身份绑定功能未开启」，说明开关已经生效。

---

## 四、玩家怎么用

### 4.1 `.bind` 绑定身份

在官方机器人的群里：

```
.bind 123456 789
```

* `123456` = 玩家**迁移前的 QQ 号**
* `789` = 你们**迁移前的群号**（旧群的群号，不是官方 bot 的群）

骰娘会从旧数据里找出这个 QQ 号的角色卡名，出选择题：

```
正在验证 QQ:123456 在 QQ-Group:789 的身份，共 1 题。
请按顺序回答下面的问题，把每题的选项序号连起来回复即可，例如 `1234`：
1. 你的角色卡名是（选项：调查员甲 / 调查员乙 / 空白卡）
回复 `.bind cancel` 可以取消本次绑定。
```

玩家回复选项序号（多题就连着写，例如 `132`）：

```
.bind 1
```

答对：

```
绑定成功！
当前身份 OpenQQ:100-xxxx 现在会读取 QQ:123456 的历史数据。
如需解除请发送 `.unbind`。
```

其它子指令：

| 指令 | 作用 |
|---|---|
| `.bind help` | 查看帮助 |
| `.bind status` | 查看自己当前绑定状态 |
| `.bind cancel` | 取消正在进行的问答 |
| `.bind list` | 查看所有绑定记录（需要管理权限） |
| `.unbind` | 解除自己的绑定 |

### 4.2 `.log bind` 绑定群日志

日志绑定是**群级**的，需要群主/管理员操作：

```
.log bind 789
```

同样会出**日志名**的选择题（选项里带 `[其它群]` 前缀的是干扰项）：

```
.log bind 1
```

成功之后：

* `.log list` / `.log get` / `.log stat` / `.log export` 都会去读**旧群**的日志；
* `.log new` / `.log on` / `.log end` 这些**写入**操作仍然记在当前新群，不会污染旧数据。

| 指令 | 作用 |
|---|---|
| `.log bindstatus` | 查看当前群的日志绑定 |
| `.log unbind` | 解除当前群的日志绑定（需要管理权限） |

### 4.3 安全设计（为什么不怕被冒领）

* 题目答案来自**旧数据**，只有真正拥有过那个 QQ 号的人才知道；
* 答错会立刻作废本次问答，并计入冷却；
* 绑定成功后骰娘会通过 `ctx.Notice` 给你发**通知**（和 `.send` 用的是同一套通知通道），
  你会在第一时间知道谁绑了谁；
* 绑定关系**只影响读取**，权限判定（master / 群管）始终用真实的新身份，不会因为绑定而提权。

---

## 五、把这套改动发布成 Docker 镜像

本机不需要装 Go / Node / GCC —— 构建交给 GitHub。

### 5.1 一次性准备

1. 确认改动已经提交并推送到你的 fork：

   ```bash
   git add -A
   git commit -m "feat: QQ官方机器人身份与日志绑定"
   git push origin main
   ```

2. 打开 GitHub 仓库页面 → **Actions** 标签页。
3. 第一次推送到 `main` 会自动触发 `Build & Push Docker image (GHCR)`。
4. 构建完成后，镜像地址是：

   ```
   ghcr.io/chaye2333/sealdice-core:latest
   ```

   （`chaye2333` 换成你的 GitHub 用户名；GitHub 用户名有大写字母时这里要全小写。）

5. 首次发布后，去仓库右侧 **Packages** → 点进这个包 → **Package settings** →
   把可见性改成 **Public**（或者保持 Private，但那样服务器上要先 `docker login ghcr.io`）。

### 5.2 云服务器上拉取并运行

```bash
# 拉取镜像
docker pull ghcr.io/chaye2333/sealdice-core:latest

# 先备份！先备份！先备份！
docker stop sealdice-core 2>/dev/null || true
cp -a ./data ./data.backup-$(date +%Y%m%d)

# 用 compose 起（推荐，docker-compose.example.yml 已给好）
docker compose -f docker-compose.example.yml up -d

# 或者直接 run
docker run -d --name sealdice-core \
  -p 3211:3211 \
  -v "$PWD/data:/app/data" \
  -v "$PWD/extra:/app/extra" \
  -e TZ=Asia/Shanghai \
  --restart unless-stopped \
  ghcr.io/chaye2333/sealdice-core:latest
```

> 保持 `./data` 挂载不变，数据库和 `serve.yaml` 就都在你手里，升级镜像不会丢数据。

### 5.3 私有包需要先登录

如果包是 Private，服务器上执行一次：

```bash
echo "<你的GitHub Personal Access Token，勾选 read:packages>" | \
  docker login ghcr.io -u <你的GitHub用户名> --password-stdin
```

### 5.4 怎么确认跑的是新版本

```bash
docker run --rm ghcr.io/chaye2333/sealdice-core:latest --version
```

输出的版本号里会带构建日期和短 hash（形如 `1.6.2-dev+20260910.abc1234`）。

### 5.5 以后改代码怎么更新

**你不需要在本地编译**，镜像由 GitHub 在云端构建：

```bash
# 在仓库目录里
git add -A
git commit -m "说明这次改了什么"
git push origin master          # 你的默认分支是 master，不是 main
```

推送后 GitHub Actions 会依次：跑测试 → 编译 WebUI → 编译 Linux 核心 → 打包镜像 →
推到 `ghcr.io/chaye2333/sealdice-core:latest`。然后服务器上：

```bash
docker pull ghcr.io/chaye2333/sealdice-core:latest
docker compose -f docker-compose.example.yml up -d
```

几点提醒：

* **默认分支是 `master`**。工作流同时监听 `master` 和 `main`，两个名字都能触发。
* 工作流文件是 `.github/workflows/docker-ghcr.yml`，可以在仓库 **Actions** 标签页看进度。
* 镜像构建前会先跑 `go test ./dice ./api`，**测试不过就不会推镜像**，避免把坏版本推上去。
* `latest` 只在默认分支上打；另外每个分支还会打一个同名 tag（如 `master`）。
* GitHub 用户名有大写字母时，GHCR 地址要全小写。

### 5.6 关于 WebUI：`ui/` 目录不是管理界面（重要）

仓库根目录的 **`ui/` 不是海豹的管理界面**，它是海豹自带的一个 Vue 脚手架样例页
（`HelloWorld.vue`、`TheWelcome.vue`、`counter.ts`），编译出来就是
`You've successfully created a project with Vite + Vue 3` 那个欢迎页。

真正的管理界面由 **`sealdice-ui`** 仓库产出，上游的约定是：

```
static/frontend/{index.html, favicon.svg, assets/...}   -> 被 static/static.go 的 go:embed 打进二进制
```

获取方式写在 README 里：

```bash
go generate ./...
```

它执行 `static/gen/download-fe.go`，从
`https://github.com/sealdice/sealdice-ui/releases/download/pre-release/sealdice-ui.zip`
下载官方前端产物并解压到 `static/frontend`。

**本仓库的 Dockerfile 已经按这套机制处理**：在 Go 构建阶段执行
`go generate ./static/...`，并断言 `index.html` 与 `assets/` 存在，
下载失败就直接让构建失败——不会再出现「镜像能起来、但页面是脚手架欢迎页」这种情况。

工作流里还有一个**烟雾测试**：启动容器抓首页，校验：

* 不是占位页（`not bundled in this checkout`）；
* 不是脚手架页（`Vite + Vue 3`）；
* 首页引用的 `assets/index-*.js` 能真实访问。

三者都通过才算构建成功。

> 想改管理界面本身，要去 [sealdice-ui](https://github.com/sealdice/sealdice-ui) 仓库改，
> 不是在 sealdice-core 的 `ui/` 里改。改完发布后重新构建本镜像即可生效。

### 5.7 关于 pnpm 依赖授权（另一个踩过的坑）

（如果以后你决定改成从 `ui/` 或 `sealdice-ui` 源码构建前端，会需要这段）

`ui/pnpm-workspace.yaml` 里的 `allowBuilds` 是**必须的**：

```yaml
allowBuilds:
  '@tailwindcss/oxide': true
  esbuild: true
```

pnpm 10 以后默认不执行依赖的安装脚本，而 `esbuild` 和 `@tailwindcss/oxide` 都是原生模块，
没有这一步 `pnpm install --frozen-lockfile` 会以 `ERR_PNPM_IGNORED_BUILDS` 失败，
进而让整个镜像构建中断。如果用 Dockerfile 里的 `COPY` 只拷了 `package.json` 与
`pnpm-lock.yaml`，镜像内是看不到这个授权文件的，必须把它一起 COPY 进构建上下文。

---

## 六、上线验收清单

按顺序做，任何一步不对都别急着放开给玩家：

- [ ] **备份**：原镜像 tag、`./data` 整个目录都留一份。
- [ ] `docker run --rm <镜像> --version` 能看到预期版本号。
- [ ] 启动后管理界面能打开：`http://<服务器IP>:3211`。
- [ ] 确认 **官方QQ使用Markdown（officialQQUseMarkdown）已开启**（虚拟状态栏依赖它）。
- [ ] 管理界面里把 **QQ身份与日志绑定** 打开，题目数量设 1、冷却设 60。
- [ ] 官方 bot 群里发 `.r 1d20`，确认基本功能正常。
- [ ] **状态栏**：测试号发 `.sn coc`（或 `.sn dnd`），再发 `.r 1d20`，
      确认回复顶部出现**小号红字属性行**，下一行是角色名，且字号正常。
- [ ] **状态栏实时更新**：`.st 生命值-3` 之后再发一次 `.r 1d20`，确认顶部数值变了。
- [ ] **状态栏开关**：`.sn none` 后发 `.r 1d20`，确认顶部不再显示属性行和角色名。
- [ ] 用一个**有旧角色卡**的测试号：`.bind <旧QQ号> <旧群号>`，确认出题、答对、绑定成功。
- [ ] 绑定后 `.pc list` / `.st show` 能读到**旧角色卡**。
- [ ] 用一个**答错**的号验证：会提示"答案不正确"，且**不会**产生绑定。
- [ ] 立刻再发起一次，确认出现"操作过于频繁"（冷却生效）。
- [ ] `.log bind <旧群号>` 后 `.log list` 能看到旧群日志。
- [ ] `.unbind` 之后 `.pc list` 恢复成绑定前的状态（证明没有动过数据）。
- [ ] 找一个 **OneBot / NapCat 群**发 `.r 1d20` 和 `.bind`，
      确认行为完全没变、`.bind` 会被拒绝、回复里**没有** `\scriptsize` 之类的公式。
- [ ] 看核心日志：没有 `panic`、没有数据库报错、没有"渲染 QQ 官方角色状态栏失败"的 warn。

---

## 七、常见问题

**Q：`.bind` 提示"身份绑定功能未开启"？**
去管理界面打开开关，或改 `serve.yaml` 的 `identityBindEnable: true` 后重启。

**Q：`.bind` 提示"身份绑定仅用于 QQ 官方机器人"？**
这是设计如此：只有官方 bot 的 OpenID 需要回指旧身份。OneBot 平台本来就用 QQ 号，无需绑定。

**Q：提示"找不到 QQ:xxx 的角色卡"？**
三种可能：① 旧 QQ 号填错；② 旧群号填错（要用 `QQ-Group:` 后面的数字）；
③ 这个号在旧数据里确实没有角色卡。可以先在旧数据里 `.pc list` 确认。

**Q：为什么题目只有 1 题？**
按 `identityBindQuestionCount` 出题，但题数不会超过该玩家**实际拥有的角色卡数量**。
只有一张卡时就只能出 1 题。

**Q：绑定之后属性写入写到哪里？**
绑定后 `AttrsManager.LoadByCtx` 回退读旧身份，因此 `.st` 这类**在当前角色卡上修改**的指令
会落在**旧身份那张卡**上——这正是"卡还是我的卡"的预期效果。
而 `.pc new` 新建角色卡走的是当前新身份，会挂在官方 bot 的 ID 下。
绑定只增加"读取回退"，不会创建、复制或删除任何数据行。

**Q：解除绑定后旧数据会怎样？**
什么都没变。绑定只是一张"新 ID → 旧 ID"的对照表（`identity-bindings.json`），
`.unbind` 删掉一行对照表而已，旧数据从未被修改过。

**Q：`.log bind` 之后新日志去哪了？**
新日志（`.log new`）仍然记在当前新群。绑定只影响**读取**旧日志。

**Q：绑定关系存在哪？**
`data/default/identity-bindings.json`，纯文本，可以直接看、可以备份。
删除这个文件等于解除所有绑定。

---

## 七点五、QQ 官方机器人虚拟角色状态栏

### 为什么需要它

QQ 官方机器人**没有修改群成员名片的接口**（`SetGroupCardName` 在官方 QQ 适配器里是空实现），
所以 `.sn` 设置的属性名片在官方 bot 里完全没有效果。本功能改为把 `.sn` 模板当作
"虚拟角色状态栏"，直接画在掷骰 / 检定回复的顶部。

### 效果

```
$\scriptsize\textcolor{#E5484D}{\text{HP12/12 AC16}}$
调查员甲
调查员甲的"侦查"检定结果为: (1)=1/50 成功
```

* 属性行是 QQ Markdown 的数学片段，客户端渲染成**小号红字**；
* 角色名和鉴定正文在片段之外，保持**正常字号**；
* HP / SAN / AC 等属性变化后，**下一条回复自动用新值**，无需任何额外操作。

### 怎么用

什么都不用配。玩家照常用 `.sn`：

| 玩家操作 | 状态栏表现 |
|---|---|
| `.sn dnd` | 顶部显示 DND 属性小号红字 + 角色名 |
| `.sn coc` | 同上，COC 属性 |
| `.sn expr {$t玩家_RAW} HP{hp}/{hpmax} AC{ac}` | 按自定义模板实时求值 |
| `.sn` （只设名字） | 只显示角色名，不显示属性行 |
| `.sn none` / `.sn off` | 整个状态栏隐藏 |

实现上**不硬编码任何规则系统的字段**：它把模板里的玩家名占位符（`{$t玩家_RAW}` / `{$t玩家}`）
摘掉，只对剩下的部分求值，所以 COC、DND、自定义 `.sn expr`、以及未来新增的写法都能兼容。

### 重要前提：必须开启官方 QQ Markdown

小号红字依赖 QQ 的 **markdown 消息类型（msg_type=2）**。请确认：

```
管理界面 → 扩展设置 → 官方QQ使用Markdown（officialQQUseMarkdown）= 开启
```

如果关闭，官方 QQ 会按普通文本发送，用户会看到原始的 `$\scriptsize...$` 公式。
（`identityBindEnable` 与这个状态栏是两套独立功能，互不影响。）

### 生效范围

状态栏**只在 QQ 官方机器人端点**渲染。OneBot（NapCat / go-cqhttp）、Telegram、Discord 等
平台的回复格式**完全不变**——它们本来就能改群名片，不需要虚拟状态栏。

具体挂载位置（只在这些"最终回复"上加，帮助文本和错误提示不受影响）：

| 指令 | 位置 |
|---|---|
| `.r` / `.rd` / `.roll` 等 | `dice/builtin_commands.go` 的骰点收尾 |
| `.rx` 相关 | 同上 |
| `.drl` 骰池抽取 | `dice/ext_fun.go` |
| `.ra` / `.rc` COC 检定 | `dice/ext_coc7.go` |
| `.sc` 理智检定 | `dice/ext_coc7.go` |
| `.rd` DND 检定 | `dice/ext_dnd5e.go` |

### 常见问题

**Q：顶部没有属性行，只有角色名？**
玩家的 `.sn` 模板里可能只有 `{$t玩家_RAW}`，没有属性表达式。让他重新 `.sn coc` 或
`.sn expr {$t玩家_RAW} HP{hp}/{hpmax}` 即可。

**Q：显示成原始公式 `$\scriptsize...$`？**
说明消息不是以 markdown 类型发出的。开启 `officialQQUseMarkdown` 后重试。

**Q：属性改了但顶部没变？**
状态栏每次回复都重新求值，不会缓存。如果没变，检查是否改到了别的角色卡
（`.pc list` 确认当前绑定的是哪张卡）。

**Q：属性里有特殊字符会怎样？**
反斜杠、花括号、`$`、`#`、`%`、`_`、`^`、`&`、`~` 会被自动换成全角字符，
不会破坏 Markdown 数学片段；属性里的换行会被压成空格，保证状态栏始终是两行。

**Q：模板写错了会不会让骰子崩掉？**
不会。状态栏渲染有 panic 兜底，出错时只跳过属性行并写一条 warn 日志，掷骰本身照常返回。

---

## 八、本次改动的文件清单

新增：

| 文件 | 作用 |
|---|---|
| `dice/ext_identity_bind.go` | 绑定核心：存储、出题、验证、回退读取、`.bind`/`.log bind` 逻辑 |
| `dice/ext_identity_bind_test.go` | 22 个测试：出题、答案解析、冷却、持久化、平台隔离、指令全流程 |
| `dice/official_qq_character_roll_markdown.go` | 官方 QQ 虚拟角色状态栏：模板求值、小号红字、转义 |
| `dice/official_qq_character_roll_markdown_test.go` | 状态栏测试：渲染、转义、实时更新、平台隔离、真实掷骰端到端 |
| `Dockerfile` | 多阶段构建：编译 WebUI → 编译核心（`CGO_ENABLED=0`）→ 精简运行镜像 |
| `.dockerignore` | 避免把 `data`、二进制等打进镜像 |
| `.github/workflows/docker-ghcr.yml` | 自动测试 + 构建 + 推送到 GHCR |
| `docker-compose.example.yml` | 云服务器部署模板 |

修改：

| 文件 | 改动 |
|---|---|
| `dice/dice_config.go` | 新增 3 个绑定配置字段 + `FixIdentityBindConfig()` 收敛取值范围 |
| `dice/dice_config_default.go` | 默认值：绑定关闭、1 题、冷却 60 秒 |
| `dice/dice.go` | `Dice.IdentityBindStore` 字段与初始化 |
| `dice/builtin_commands.go` | 注册全局指令 `.bind` / `.unbind`；骰点收尾挂状态栏 |
| `dice/ext_log.go` | 抽出 `EvalPlayerGroupCardTemplate`；`.log bind` 系列；读操作群回退 |
| `dice/dice_attrs_manager.go` | `LoadByCtx` 支持绑定后的属性读取回退 |
| `api/dice_config.go` | WebUI 保存这三个配置项 |
| `dice/ext_fun.go` | `.drl` 骰池抽取挂状态栏 |
| `dice/ext_coc7.go` | `.ra` / `.rc` / `.sc` 挂状态栏 |
| `dice/ext_dnd5e.go` | DND 检定挂状态栏 |

---

## 九、按你的要求未实现的部分

你明确要求**不做**下面两项，交给用户自己在自定义文本框里写：

* **【大成功】【极难成功】等结果文案转 Markdown 灰色引用框** —
  用户可以自己在 `SeaDice-text-template.yaml` 里把结果文案写成 `> 成功` 这类引用行。
* **本地 GIF / 图片与 Markdown 拆分发送** — 不做发送层改动，避免影响既有行为。

其余仍可后续增强的点：

* 绑定关系目前是**全局生效**（绑一次所有群都能读到旧身份）。若你希望每个群独立，
  需要把 `identityBindUserKey` 的键加上群维度。
* 日志读取回退要求旧群对象在内存里（骰娘进过那个群）。如果旧群从未被官方 bot 加载过，
  需要额外补一个"按群号构造只读 GroupInfo"的路径。
* 状态栏目前跟在玩家**当前保存的** `.sn` 模板上。若想让状态栏独立于 `.sn` 排版，
  可以再开放 `$tQQ角色属性行` / `$tQQ角色状态栏` 之类的文案变量让用户自定义位置。
