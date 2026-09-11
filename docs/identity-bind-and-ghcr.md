# QQ身份绑定 + 双向数据共通 + 虚拟角色状态栏 + GHCR 自动构建：使用与部署说明

> `.help` 第一行显示 `鲸娘与豹 <版本号>`，第二行是 fork 说明（该fork版本主要是适配官bot的功能，代码鲸鱼写的。）

面向：把海豹从 NapCat（OneBot）迁移到 **QQ 官方机器人**，希望老玩家的角色卡 / 日志不丢，
并且**官方 bot 与民间 bot 能共用同一份数据**、官方机器人也能显示角色属性的人。

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

本功能让玩家在官方 bot 里**自证身份**，然后把新身份"归一"到旧身份：

* 规范 ID **固定是旧身份**，旧数据原地不动，一条都不搬；
* 归一之后，**官方 bot 与民间 bot 读写的是同一份数据**（角色卡、属性、日志）；
* 因此不是「两边各存一份再同步」，而是本来就只有一份，不存在分叉问题；
* `.unbind` 之后立刻恢复原样；
* **只有 QQ 官方机器人端点 + 参与过绑定的身份**会走归一，你其它的 OneBot 群行为完全不变。

**两个维度，需要两个绑定**（详见 3.2.1）：

| 绑定 | 维度 | 影响 |
|---|---|---|
| `.bind <旧QQ号>` | 用户 | 角色卡、属性、`.sn` 模板 |
| `.group bind <旧群号>` | 群 | 日志状态、日志内容 |

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
| **使用私聊验证码验证** | `identityBindUseVerificationCode` | **`true`** | **防抢号的关键**，默认开启。由民间 bot 给被声明的旧 QQ 号发私聊验证码，只有真正持有该号的人能确认。详见 3.2.2 |
| 验证码位数 | `identityBindCodeLength` | `6` | 4~8 |
| 验证码有效期(秒) | `identityBindCodeExpireSec` | `600` | 60~3600，默认 10 分钟 |
| 验证码后仍要求答题 | `identityBindKeepQuiz` | `false` | 想要双重验证时打开 |
| 验证题目数量 | `identityBindQuestionCount` | `1` | 1~5，超过 5 会被收敛到 5（仅答题路径使用） |
| 绑定冷却时间(秒) | `identityBindCooldownSec` | `60` | 同一用户两次发起绑定的最小间隔，0 表示不限制 |
| 答错锁定时长(秒) | `identityBindFailCooldownSec` | `43200` | 答错后多久不能再发起绑定，默认 **12 小时**（仅答题路径使用） |
| 日志去重窗口(秒) | `logMultiBotDedupWindowSec` | `5` | 同一条玩家消息多久内只记一次。**默认 5 = 上游原行为**；同群同时挂官方 bot 与民间 bot 时才需要调大（见第七点八） |

### 3.2.2 私聊验证码：为什么必须有它

**答题解决不了抢号问题。** 答题（角色卡名 / 日志名）只能拦住"完全不知道你信息的人"；
只要对方知道你的旧 QQ 号、又碰巧或猜到你的卡名，就能把你的整套数据
（角色卡、属性、日志）接管过去。

验证码解决的是**归属证明**，而且必须是**跨两个 bot** 的，因为：

* 官方 bot 这边**无法**确认"你就是旧 QQ 号 123456"——它只有一个 OpenID；
* 但民间 bot 可以：**它能给那个号码发私聊**。

所以流程是：

```
1. 用户在官方 bot 发起        .bind 123456
2. 官方 bot 生成验证码，登记一条挑战
3. 后台任务通过民间 bot 给 123456 发私聊验证码
4. 拿到验证码的人在私聊里把它回给民间 bot
5. 校验通过 → 建立绑定（新身份 = 官方侧发起者）
```

只有真正持有那个旧 QQ 号的人才能收到私聊，所以这一步无法伪造。

群绑定同理：验证码发给**旧群的邀请人**（把骰子拉进旧群的人），由他确认。

**设计上刻意保留的性质**：

* 绑定**始终由官方 bot 侧发起**，民间 bot 只负责投递与确认；
* 验证码只认**被声明的那个号**发来的回复，别人就算知道验证码也没用；
* 只在**私聊**里认验证码，群聊里的纯数字消息绝不被吞掉；
* 单条验证码允许失败 5 次，超过即作废（比"答错锁 12 小时"体验好得多）；
* 失败与成功都会留一条挑战记录，便于排查。

**什么时候要关掉它**：它依赖民间 bot（OneBot 连接）在线。
如果你**已经放弃民间 bot 的运营、打算纯用官 bot 做数据迁移**，
就没有可用的连接来发私聊验证码，这时必须关掉它，改用答题验证。

关闭方法（管理界面「身份绑定 → 使用私聊验证码验证」取消勾选）：

```yaml
identityBindUseVerificationCode: false
```

关掉之后流程完全回到答题，行为与之前一致。

### 3.2.1 双向数据共通（重要）

绑定之后，**官方身份与旧 QQ 号 / 官方群与旧群读写的是同一份数据**，而不是各存一份。

实现方式是「规范 ID（canonical ID）归一」，不是数据同步：

* 规范 ID **固定是旧身份**（迁移前的 QQ 号 / 旧群）；
* 旧号、旧群那一侧**原地不动**，一条数据都不搬；
* 官方那一侧的所有数据访问都先归一成旧 ID，于是两边算出来的 key 完全一样；
* 因此不存在「两边数据分叉」的问题——本来就只有一份。

要让数据**完全**共通，需要**两个绑定同时生效**：

| 绑定 | 负责的维度 | 影响 |
|---|---|---|
| `.bind <旧QQ号>` | **用户**维度 | 角色卡、属性、`.sn` 名片模板 |
| `.group bind <旧群号>` | **群**维度 | 日志状态（开没开/叫什么）、日志内容 |

只做一个会有半边读不到：

* 只做个人绑定：角色卡通了，但日志仍然在两个群各记一份；
* 只做群绑定：日志通了，但每个玩家的角色卡还是各看各的。

`.bind status` / `.group status` / `.group doctor` 都会提示当前缺哪一半。

题目数量怎么选：

* `1` 题最省事，适合信任度高的群；
* 老玩家角色卡名比较独特时，`1` 题已经足够（要在他自己的多张卡里选对）；
* 想更严格可以调到 `2`~`3`，玩家要连着答对。

**答错锁定**用于防止用猜测的方式套别人的角色卡名 / 日志名：

* 答错一次立即进入锁定，默认 **12 小时**，期间不能再次发起绑定；
* 个人绑定与群绑定的锁定**互相独立**，一个答错不影响另一个；
* 想放宽就调小这个值（例如 `3600` = 1 小时）；
* 注意：填 `0` 会被当作「缺省」而补回 12 小时，这是故意的，避免配置失误导致无惩罚。

### 3.3 怎么验证开关生效

在官方 bot 群里发 `.bind`（不带参数），如果返回的是帮助文本和当前绑定状态、
而不是「身份绑定功能未开启」，说明开关已经生效。

---

## 四、玩家怎么用

### 4.1 `.bind` 绑定个人身份（全局，与群无关）

在官方机器人的群里：

```
.bind 123456
```

* `123456` = 玩家**迁移前的 QQ 号**。

**旧群号是可选参数**：`.bind 123456 789` 也能用，但那只影响提示文案，不影响验证，
因为角色卡是按 `owner_id`（旧 QQ 号）查询的，跟群没有关系。
个人绑定**全局生效**——在任何官方群里绑一次，所有官方群都通用。

骰娘会从旧数据里找出这个 QQ 号的角色卡名，出选择题：

```
正在验证旧身份 QQ:123456，共 1 题。
请按顺序回答下面的问题，把每题的选项序号连起来回复即可，例如 `1234`：
1. 你的角色卡名是（选项：调查员甲 / 调查员乙 / 空白卡）
回复 `.bind cancel` 可以取消本次绑定。
```

玩家回复选项序号（多题就连着写，例如 `132`）：

```
.bind 1
```

> **卡在答题环节了？** 发 `.bind cancel` 取消个人问答，或 `.group cancel` 一次清掉
> 个人和群的问答。取消只是作废这次问答，**不会**动已存在的绑定记录。

**两种绑定不会互相干扰**：`.bind` 与 `.group bind` 使用不同的存储键，可以同时使用。
想确认自己现在是什么状态，用 `.bind status`（个人）和 `.group status`（群）分别查看。

其它子指令：

| 指令 | 作用 |
|---|---|
| `.bind help` | 查看帮助 |
| `.bind` / `.bind status` | 查看自己当前绑定状态 |
| `.bind cancel` | 取消进行中的问答 |
| `.bind reset` | 同上，并清掉群绑定的问答 |
| `.bind list` | 查看所有绑定记录（需要管理权限） |
| `.unbind` | 解除自己的个人绑定 |

### 4.2 `.group bind` 绑定整个群

群绑定是把**整个官方群**绑到迁移前的旧群上，绑定之后群里的日志读取会指向旧群。
它需要群主/管理员操作：

```
.group bind 789
```

日志题是**填空**，会把该旧群真实的日志名列出来，让你回复其中一个：

```
正在验证旧群 QQ-Group:789，共 1 题。
请按顺序回答下面的问题，直接回复答案文字即可：
1. 这是你们团在旧群的日志名，请回复其中一个（本群共有 2 个日志：追书人 / 第一话；回复其中一个即可）
回复 `.bind cancel` 可以取消本次绑定；答错会被锁定，请勿尝试猜测。
```

```
.group bind 追书人
```

> **为什么不做成选择题？** 早期版本用「真日志名 + 假的 `[其它群] xxx` 干扰项」出选择题，
> 结果干扰项一眼就能看穿，等于把答案直接告诉玩家。现在改为填空，且填**任意一个**
> 该群真实日志名都算对，既不需要玩家猜，也没法把答案摆在他面前。
>
> 也可以简写成 `.group 789`，不带 `bind` 一样识别。

卡住了同样用 `.group cancel` 取消。**答错会锁定 12 小时**（可用
`identityBindFailCooldownSec` 调整）。

成功之后：

* `.log list` / `.log get` / `.log stat` / `.log export` 都会去读**旧群**的日志；
* `.log new` / `.log on` / `.log end` 这些**写入**操作仍然记在当前新群，不会污染旧数据。

| 指令 | 作用 |
|---|---|
| `.group bind <旧群号>` | 发起群绑定（需要管理权限） |
| `.group status` | 查看当前群的绑定 |
| `.group unbind` | 解除当前群的绑定（需要管理权限） |
| `.group cancel` | 取消进行中的问答（个人+群） |

> 兼容写法：`.log bind` / `.log unbind` / `.log bindstatus` 与上面完全等价，继续可用。

#### 个人绑定与群绑定是两套独立的东西

这一点很重要，也是早期版本的一个缺陷：

| | `.bind`（个人身份绑定） | `.group bind`（群绑定） |
|---|---|---|
| 绑的是什么 | 某个**人的**新旧身份 | 某个**群的**新旧身份 |
| 解决什么 | 这个人的**角色卡/属性**读不到 | 这个群的**日志**读不到 |
| 需要什么参数 | 旧 QQ 号（旧群号可选） | 旧群号 |
| 作用范围 | **全局**，所有官方群通用 | 只对当前群生效 |
| 存储键 | `user\|<官方用户ID>` | `group\|<官方群ID>` |
| 权限 | 任何用户 | 群主 / 管理员 |
| 出题依据 | 该 QQ 名下的**角色卡名** | 该旧群里的**日志名** |
| 查看状态 | `.bind status` | `.group status` |
| 解除 | `.unbind` | `.group unbind` |

**两者使用不同的存储键，互不覆盖、互不阻断，可以在同一个群里同时使用。**
推荐的操作顺序是：管理员先把群绑好（`.group bind <旧群号>`），群成员再各自做
`.bind <旧QQ号>`。两个都做完之后，成员既能看到自己的角色卡，也能看到旧群的日志。

卡在答题环节时，`.bind cancel` 取消个人问答，`.group cancel` 一次清掉两种问答。

### 4.3 安全设计（为什么不怕被冒领）

* 题目答案来自**旧数据**，只有真正拥有过那个 QQ 号 / 那个群的人才知道；
* 答错会立刻作废本次问答，并**锁定 12 小时**（`identityBindFailCooldownSec`），防止穷举猜测；
* 绑定成功后骰娘会通过 `ctx.Notice` 给你发**通知**（和 `.send` 用的是同一套通知通道），
  你会在第一时间知道谁绑了谁；
* 绑定关系**只影响读取**，权限判定（master / 群管）始终用真实的新身份，不会因为绑定而提权。
* 群绑定需要管理权限，普通成员无法改群级数据指向。

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

**本仓库的 Dockerfile 现在有两种取前端的方式**，用构建参数 `UI_FROM_SOURCE` 切换：

| `UI_FROM_SOURCE` | 前端来源 | 用途 |
| --- | --- | --- |
| `1`（默认） | 从 `chaye2333/sealdice-ui` 拉源码，在 `node:22-alpine` 阶段现场 `pnpm run build-only` 编译 | 镜像里带上我们新增的设置项（官方QQ超时/分片上传、身份绑定） |
| `0` | 走上游机制：`go generate ./static/...`，从 `sealdice-ui` 官方 pre-release 下载 zip | 想要一份「纯上游」的镜像时用 |

```bash
# 默认：带新设置项的前端
docker build -t sealdice-core:local .

# 纯上游前端
docker build -t sealdice-core:local --build-arg UI_FROM_SOURCE=0 .
```

前端源码来源也可以用参数覆盖（默认已指向你的 fork）：

```bash
docker build -t sealdice-core:local \
  --build-arg UI_REPO=chaye2333/sealdice-ui \
  --build-arg UI_REF=cc3f080 .
```

`UI_REF` 默认写死为 `master`，所以它会被算进 Docker 的层缓存键：
**改了 UI 并推到 fork 后，要重新构建镜像，需要在 `Dockerfile` 里把 `UI_REF` 改成新的短 hash
（或者构建时传 `--build-arg UI_REF=<新hash>`）**，否则 Docker 会直接复用旧的 UI 层。
镜像上也打了标签方便核对：`docker inspect <镜像> --format '{{json .Config.Labels}}'`
能看到 `sealdice.ui.repo` / `sealdice.ui.ref`。

无论走哪条路，Go 构建阶段都会断言 `static/frontend/index.html` 与 `static/frontend/assets/` 存在，
缺了就直接让构建失败——不会再出现「镜像能起来、但页面是脚手架欢迎页」这种情况。

工作流里还有一个**烟雾测试**：启动容器抓首页，校验：

* 不是占位页（`not bundled in this checkout`）；
* 不是脚手架页（`Vite + Vue 3`）；
* 首页引用的 `assets/index-*.js` 能真实访问。

三者都通过才算构建成功。

> 想改管理界面本身，要去 [sealdice-ui](https://github.com/sealdice/sealdice-ui) 仓库改，
> 不是在 sealdice-core 的 `ui/` 里改。改完推到自己的 fork，再按上面的方式重新构建镜像即可生效。

### 5.6.1 管理界面上新增的开关（在你的 sealdice-ui fork 里）

因为这三个配置项只有 `serve.yaml` 里有，上游的管理界面并没有对应的输入框，
所以在你 fork 的 `sealdice-ui` 里补上了。位置：**杂项设置 → 官方QQ 区域**。

| 界面上的名字 | 配置键 | 默认 | 说明 |
| --- | --- | --- | --- |
| 官方QQ 请求超时（秒） | `officialQQRequestTimeoutSec` | 60 | 5~600，重启后生效 |
| 本地文件使用分片上传 | `officialQQChunkedUploadEnable` | 关 | 开启后本地文件保留文件名 |
| 启用身份绑定 | `identityBindEnable` | 关 | 总开关，关掉后 `.bind` 不可用 |
| 身份绑定题目数量 | `identityBindQuestionCount` | 1 | 1~5 |
| 身份绑定发起间隔（秒） | `identityBindCooldownSec` | 60 | 0~86400 |
| 身份绑定答错锁定（秒） | `identityBindFailCooldownSec` | 43200 | 1~86400，答错后锁定 |

改动只涉及两个文件：

* `src/api/dice/index.ts`：`DiceConfig` 类型里补上这 6 个字段；
* `src/components/misc/PageMiscSettings.vue`：加上对应的 `el-form-item`，
  并在 `submit()` 里对 4 个数字框做 `toNumber()`（数字框偶尔会回传字符串，
  后端按整数解析会失败）。

### 5.7 关于 pnpm 依赖授权（另一个踩过的坑）

从源码编译前端（也就是默认的 `UI_FROM_SOURCE=1`）会踩到这个坑。

`sealdice-ui` 的 `pnpm-workspace.yaml` 里的 `allowBuilds` 是**必须的**：

```yaml
allowBuilds:
  esbuild: true
  core-js: false
  vue-demi: false
```

pnpm 10 以后默认不执行依赖的安装脚本，而 `esbuild` 是原生模块，
没有这一步 `pnpm install --frozen-lockfile` 会以 `ERR_PNPM_IGNORED_BUILDS` 失败，
进而让整个镜像构建中断。所以 Dockerfile 里：

* 用 `curl` 拉 **整个仓库 tarball**（而不是只 COPY 几个文件），保证
  `pnpm-workspace.yaml`、`pnpm-lock.yaml` 都在；
* 用 `npm install -g pnpm@10` 固定 pnpm 大版本，**不用 corepack**：
  非交互环境下 corepack 会弹「是否下载 pnpm」的提示，在 CI 里会直接失败。

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
      确认回复顶部出现**角色名 + 属性**，整行都是小号红字，且属性变化后会自动更新。
- [ ] **状态栏实时更新**：`.st 生命值-3` 之后再发一次 `.r 1d20`，确认顶部数值变了。
- [ ] **状态栏开关**：`.sn none` 后发 `.r 1d20`，确认顶部不再显示属性行和角色名。
- [ ] **验证码流程（默认路径）**：用一个**有旧角色卡**的测试号 `.bind <旧QQ号>`，
      确认提示是「已通过民间 bot 给 xxx 发送了私聊验证码」而不是出题；
- [ ] 打开那个旧 QQ 号与**民间 bot** 的私聊，把验证码回过去，
      确认收到「验证通过」，并且官方 bot 群里也收到绑定成功通知。
- [ ] **抢号防护**：用另一个号拿到同一个验证码去回复，确认被拒绝
      （提示"这个验证码不是发给你的"），且**不产生**任何绑定。
- [ ] **验证码过期**：发起后等超过 `identityBindCodeExpireSec`（默认 10 分钟）再回复，确认已失效。
- [ ] **只用官 bot 的模式**：把「使用私聊验证码验证」关掉，再 `.bind <旧QQ号>`，
      确认重新回到答题流程，能正常绑定。
- [ ] 绑定后 `.pc list` / `.st show` 能读到**旧角色卡**。
- [ ] 用一个**答错**的号验证：会提示"答案不正确"并说明锁定时长，且**不会**产生绑定；
      紧接着再发起一次，确认显示"操作过于频繁"（12 小时锁定生效）。
- [ ] 立刻再发起一次，确认出现"操作过于频繁"（冷却生效）。
- [ ] `.bind cancel` 能取消进行中的问答，取消后可以重新发起。
- [ ] `.group bind <旧群号>` 后，题目应该是**填空**（列出真实日志名，让你回复其中一个），
      回复任意一个真实日志名即可通过；`.log list` 能看到旧群日志。
- [ ] `.unbind` 之后 `.pc list` 恢复成绑定前的状态（证明没有动过数据）。
- [ ] **同时使用验证**：先 `.bind` 绑定个人身份，再 `.group bind` 绑群，
      两条都应该成功；`.bind status` 与 `.group status` 分别显示各自的记录；
      `.group unbind` 只解除群绑定，个人绑定仍在（`.pc list` 仍读旧卡）。
- [ ] **双向共通（用户维度）**：官方 bot 用 `.st 生命值-3`，然后到旧群用民间 bot
      发 `.st show`，看到的应该是**同一个数值**；反过来改也一样。
- [ ] **双向共通（群维度）**：旧群 `.log on <名字>` 之后，官方群 `.log list` 能看到；
      官方群 `.log off` 之后，确认旧群 `.log list` 里**不再新增**记录（状态是同一份）。
- [ ] **跨群日志合并**：官方群发几条、旧群发几条，然后 `.log get`，
      确认两边的话都在**同一份**记录里（而不是各记一份）。
- [ ] **自检**：`.group doctor` 应该报「异常 0 条 / 所有绑定看起来都正常」。
      如果报「目标旧群不在内存里」，去那个旧群让骰子收到一条消息即可恢复。
- [ ] `.bind cancel` 能取消进行中的问答，取消后可以重新发起。
- [ ] `.group bind <旧群号>` 后，题目应该是**填空**（列出真实日志名，让你回复其中一个），
      回复任意一个真实日志名即可通过；`.log list` 能看到旧群日志。
- [ ] `.unbind` 之后 `.pc list` 恢复成绑定前的状态（证明没有动过数据）。
- [ ] 找一个 **OneBot / NapCat 群**发 `.r 1d20` 和 `.bind`，
      确认行为完全没变、`.bind` 会被拒绝、回复里**没有** `\scriptsize` 之类的公式。
- [ ] `.help` 第一行是 `鲸娘与豹 <版本号>`，第二行是 fork 说明。
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
绑定后属性读写都归一成**旧身份**，所以 `.st` 这类修改会落在**旧身份那张卡**上——
这正是"卡还是我的卡"的预期效果，而且民间 bot 那边立刻能看到同一个数值。
`.pc new` 新建角色卡同样归一，因此新卡会挂在旧 QQ 号名下，两边都能看到。
绑定不创建、不复制、不删除任何数据行，只是让两边算出同一个 key。

**Q：解除绑定后旧数据会怎样？**
什么都没变。绑定只是一张"新 ID → 旧 ID"的对照表（`identity-bindings.json`），
`.unbind` 删掉一行对照表而已，旧数据从未被修改过。

**Q：`.group bind` 之后新日志去哪了？**
记进**归一后的那份日志**（也就是旧群那份），官方群和旧群看到的是同一份记录。
这正是「日志双向共通」的要点：不是各记一份再合并，而是本来就只有一份。

**Q：绑定成功了但数据看起来没变化？**
先用 `.group doctor` 自检。最常见的原因是**目标旧群不在内存里**
（机器人很久没去过那个旧群），此时读取会静默回退到当前群。
让骰子去旧群收到一条消息即可恢复。

**Q：同时开了官方 bot 和民间 bot，日志记了两遍？**
见「七点八」。把 `logMultiBotDedupWindowSec` 调到 `30` 可跨连接去重，
但要接受"同一人窗口内发的两条相同消息会并成一条"这个代价。

**Q：绑定关系存在哪？**
`data/default/identity-bindings.json`，纯文本，可以直接看、可以备份。
里面同一条绑定会存两份索引（新旧 ID 各一份），这是为了让两侧都能查到，
展示和落盘时会自动去重，不用担心。
删除这个文件等于解除所有绑定。

---

## 七点五、QQ 官方机器人虚拟角色状态栏

### 为什么需要它

QQ 官方机器人**没有修改群成员名片的接口**（`SetGroupCardName` 在官方 QQ 适配器里是空实现），
所以 `.sn` 设置的属性名片在官方 bot 里完全没有效果。本功能改为把 `.sn` 模板当作
"虚拟角色状态栏"，直接画在掷骰 / 检定回复的顶部。

### 效果

角色名与属性在**同一行**，并且**整体都是小号红字**：

```
$\scriptsize\textcolor{#E5484D}{\text{调查员甲 SAN50 HP30/30 DEX60}}$
调查员甲使用了"50"书页: 拼点结果为: D100=28/50 【成功】
```

* 角色名与属性都放在同一个 QQ Markdown 数学片段里，客户端整体渲染成**小号红字**；
* 角色名排在属性前面，一眼能看到是谁在骰；
* HP / SAN / AC 等属性变化后，**下一条回复自动用新值**，无需任何额外操作。

> 如果你想让角色名**保持正常字号**（只有属性是小号红字），把
> `dice/official_qq_character_roll_markdown.go` 中 `officialQQCharacterStatusBar`
> 里的名字从 `\text{...}` 挪到片段外面即可。

### 怎么用

什么都不用配。玩家照常用 `.sn`：

| 玩家操作 | 状态栏表现 |
|---|---|
| `.sn dnd` | 顶部显示「角色名 + DND 属性」小号红字 |
| `.sn coc` | 同上，COC 属性 |
| `.sn expr {$t玩家_RAW} HP{hp}/{hpmax} AC{ac}` | 按自定义模板实时求值 |
| `.sn` （只设名字） | 只显示角色名（同样是小号红字） |
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

### 生效范围与挂载机制

状态栏**只在 QQ 官方机器人端点**渲染。OneBot（NapCat / go-cqhttp）、Telegram、Discord 等
平台的回复格式**完全不变**——它们本来就能改群名片，不需要虚拟状态栏。

**挂载是统一做的，不是逐个规则系统去改**（这点经历过一次返工）：

1. `DiceFormatTmpl` 在渲染「最终回复」模板时，给 `ctx.OfficialQQStatusBarPending` 打标记。
   目前被认定为最终回复的模板是：

   | 模板键 | 对应指令 |
   |---|---|
   | `核心:骰点` / `核心:骰点_多轮` | `.r` `.rd` `.roll` `.rx` `.drl` 等 |
   | `COC:检定` / `COC:检定_多轮` | `.ra` `.rc` `.rav` `.rcv` |
   | `COC:理智检定` | `.sc` |

2. 发送层 `replyGroupRawNoCheck` / `replyPersonRawNoCheck`（`dice/im_helpers.go`）
   消费这个标记，在文本最顶部补上状态栏，然后清除标记。

这样做的原因：规则系统在中文海豹里是**扩展**，可以自由增删。
早期版本是逐个在 `ext_coc7.go` / `ext_dnd5e.go` / `builtin_commands.go` 里手工挂载，
结果 **FU 等规则系统的检定就完全没有状态栏**。现在任何规则系统只要用上面这些核心
模板渲染检定结果，就会自动带上状态栏，包括第三方 JS 扩展。

> 如果你自己写的扩展用了**别的模板键**来做检定，把那个键加进
> `dice/rollvm_migrate.go` 的 `officialQQFinalReplyTmplKeys` 即可。

帮助文本、错误提示、`.pc` / `.st` 等回复不会带状态栏，因为它们不是上面这些模板。

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

## 七点六、官方 QQ 富媒体（语音 / 文件）修复

这一节修的是官方 QQ 适配器的媒体发送问题，和插件（点歌卡片语音等）兼容性直接相关。

### 7.6.1 请求超时 3 秒 → 60 秒（最要紧的一个）

**症状**：点歌发语音时报「准备语音信息失败：… context deadline exceeded」。

**原因**：适配器把 SDK 的请求超时硬编码成 3 秒：

```go
pa.Api = qqbot.NewOpenAPI(...).WithTimeout(3 * time.Second)
```

这个超时作用在 SDK 的 resty client 上，是**所有请求共用**的（文本、上传、拉机器人信息）。
而用 `url` 方式上传富媒体时，腾讯要**先把整个文件下载完**才返回响应头，
3 秒基本必然超时。

**改法**：默认提到 60 秒，并且做成可配置：

```yaml
officialQQRequestTimeoutSec: 60
```

| 值 | 说明 |
|---|---|
| 默认 `60` | 只发语音（4~5MB）足够，官方文档也建议上传超时 ≥5 秒 |
| `120` 以上 | 要发几十 MB 的大文件时建议调大 |
| 下限 `5` | 低于会被自动抬到 5 秒 |
| 上限 `600` | 高于会被收敛，避免误填把机器人卡死 |

> 调大超时只是「允许它慢慢下」，源站慢的话机器人会一直卡着不回复。
> 想彻底不等，用下面的本地文件方案让腾讯别去下载。

**替代方案（不用腾讯下载）**：插件配置 `voiceUseLocalFile=true` + `localCacheDir`，
插件先把音频下到本地，发 `[CQ:record,file=file:///...]`，适配器走 `file_data`(base64) 上传，
`url` 留空，腾讯不再下载，5 秒都够用。

### 7.6.2 文件消息（file_type=4）现在真的能发

原来适配器**没有实现文件元素的发送**，`[CQ:file,...]` 会被静默丢弃；
`SendFileToPerson` / `SendFileToGroup` 也只是回一句「尝试发送文件 xxx，但不支持」。

现在补齐了：

* 群聊 `sendQQGroupMsgRaw` 增加 `case *message.FileElement`，以 `file_type=4` 上传；
* 单聊 `sendC2CMsgRaw` 同样增加该分支；
* `SendFileToPerson` / `SendFileToGroup` 改为构造 `[CQ:file,file=...]` 交给正常发送链路。

需要注意：

* **文件路径必须在程序工作目录或系统临时目录内**，否则 `FilepathToFileElement`
  会以「路径受限」拒绝（日志里会写「CQ码资源路径受限，已跳过」）。
  日志导出用的 `os.CreateTemp("", ...)` 天然在临时目录里，可以直接用。
* 路径里的逗号 / 方括号会用 `message.EscapeCQParam` 转义（`&#44;` / `&#91;`），
  不会把参数截断。
* `file_data`(base64) 模式**腾讯不支持自定义文件名**；要自定义文件名必须走
  `upload_prepare` 分片上传。
* **频道（QQ-CH）场景不支持文件**，官方文档里就是 ❌，本补丁不动频道。

### 7.6.3 file_info 必须解码后再发（一次被回滚的"优化"）

这块**踩过一次坑**，写下来免得以后有人再改错。

上传接口返回的 `file_info`，**必须经过一次 base64 解码**再交给发送接口：

```go
decodedFileInfo, decodeErr := base64.StdEncoding.DecodeString(media.FileInfo)
if decodeErr != nil {
    decodedFileInfo = []byte(media.FileInfo)
}
return &dto.MediaInfo{FileInfo: decodedFileInfo}, nil
```

原因是类型叠加：

| 环节 | 字段类型 | 说明 |
|---|---|---|
| 上传接口返回 | `dto.Media.FileInfo` = `string` | 内容是 base64 文本 |
| 适配器传出 | `dto.MediaInfo.FileInfo` = `[]byte` | 装的是**解码后的字节** |
| 发送接口序列化 | Go 的 `encoding/json` | 对 `[]byte` 自动做一次 base64 |

所以「先解码、再由 JSON 编码一次」正好还原成接口期望的形态。

**曾经**有人（就是我）照着官方文档里「file_info 内部为序列化二进制，开发者无需解析，
直接透传即可」的注释，把这步解码当成 bug 删掉了，结果发送立刻开始报：

```
code:400, {"message":"请求参数file_info无效","code":40034032}
```

图片和语音**全部发不出去**。已回滚，并加了测试锁定这个行为：

* `TestOfficialQQUploadGroupMediaDecodesFileInfo`
* `TestOfficialQQUploadC2CMediaDecodesFileInfo`
* `TestOfficialQQUploadGroupMediaFallsBackWhenFileInfoNotBase64`

**另外顺手修了一个真实的不一致**：单聊路径（`uploadC2CMedia`）上游**从来没有做过这步解码**，
和群聊路径行为不同。现在两条路径统一走 `decodeOfficialQQFileInfo`。

> 教训：官方文档的这句话描述的是「file_info 的语义」，但没描述 SDK 的字段类型；
> 判断这类问题时，**能跑的实测行为优先于文档注释**。

### 7.6.4 文件名显示「未命名」→ 分片上传（可选）

**现象**：文件能发出去，但客户端收到的文件叫「未命名」。

**根因**：腾讯的上传接口有几种方式，能力不同——

| 方式 | 请求体字段 | 能否自定义文件名 |
|---|---|---|
| URL 上传 | `url` | ✅ 平台用 URL 路径末段当文件名 |
| `file_data`(base64) | `file_data` | ❌ **接口没有文件名字段** |
| 分片上传 | `upload_id` + `file_name` | ✅ 合并时可以指定 |

本地文件走的是 `file_data`，所以名字必然由腾讯生成。这是**接口能力差异**，不是实现 bug。

**解法**：分片上传（官方文档推荐的"大文件或本地文件"方案），四步：

```
① POST /v2/{groups|users}/{id}/upload_prepare
     { file_type, file_size, file_name, md5, sha1, md5_10m }
     → upload_id + block_size + parts[]（index + presigned_url）
② PUT  <presigned_url>                逐片上传分片数据（裸 PUT，不要带 Authorization）
③ POST /v2/{groups|users}/{id}/upload_part_finish
     { upload_id, part_index, block_size, md5 }
④ POST /v2/{groups|users}/{id}/files
     { file_type, srv_send_msg:false, file_name, upload_id }   → file_info
```

几个容易写错的点（代码注释里也标了）：

* `file_size` / `block_size` 在官方文档里是**字符串**，不是数字；
* `md5_10m` 是**文件前 10002432 字节**的 MD5，不是整个文件的；
* 预签名 URL 自带鉴权，PUT 时**不能**附加 `Authorization` / `X-Union-Appid`，否则签名校验失败；
* 单聊与群聊的上传/预上传/分片端点**互相独立**，不能跨场景复用。

**开关**：**默认关闭，必须显式打开**。在 `serve.yaml` 里加一行：

```yaml
officialQQChunkedUploadEnable: true
```

> ⚠️ **不打开这一行，文件照样能发，但名字仍是「未命名」**——
> 关着时走的是旧的 `file_data` 路径，而那条路径腾讯不给文件名。

**打开后的行为**：本地文件走分片（可保留文件名）；**远程 URL** 仍走 URL 上传；
语音/图片等既有路径**完全不变**。所以这个开关是安全的，出问题关掉即可回退。

**排查清单**（名字还是「未命名」时逐条看）：

- [ ] `serve.yaml` 里确实有 `officialQQChunkedUploadEnable: true`
      （**顶格**、不要缩进；也不要和已有的键重复——重复会导致
      `serve.yaml parse failed`，骰子会启动失败）
- [ ] 改完后**重启过**容器
- [ ] 日志里能搜到 `official qq 分片上传: 文件=... 大小=... 分片数=...`；
      **有这行才说明走的是分片路径**，没有就是开关没生效
- [ ] 发的是**本地文件**（`file://` 或普通路径）；远程 URL 设计上不走分片

> botgo SDK 只封装了 `/files` 一个端点，`upload_prepare` 与 `upload_part_finish`
> 由适配器自己发请求（`dice/platform_adapter_official_qq_chunked.go`），
> 鉴权方式与 SDK 保持一致。测试通过注入的假腾讯服务器端到端验证了整个四步流程。

---

## 七点八、两个 bot 同时开着会怎样

双向共通之后，最容易想到的问题就是「官方 bot 和民间 bot 同时在线会不会打架」。
结论要分三种情况，差别很大。

### 情况 A：同一个海豹实例，两个 bot 都在**同一个 QQ 群**

| 环节 | 会不会重复 | 原因 |
|---|---|---|
| 玩家消息记日志 | ⚠️ **会重复** | 两个连接各收到同一条消息 |
| 骰子自己的发言记日志 | ✅ 不会 | 每条回复只由发出它的那个 bot 触发一次 |
| 绑定记录 | ✅ 无冲突 | 一条绑定只有一个存储记录 |
| 属性 / 角色卡 | ✅ 天然正确 | 只按「群+人」查，与 bot 数量无关 |
| 自动回复（读日志） | ⚠️ 可能重复 | 重复记进的两条都会被算进去 |

重复的根因：去重键用的是 `msg.RawID`，而**两个 bot 收到同一条消息时 RawID 是各自的**，
根本对不上。原来 5 秒的窗口只能挡住「同一个连接的重复推送」。

需要这个场景时，把窗口调大：

```yaml
logMultiBotDedupWindowSec: 30
```

调大（> 5）之后才会切换成按「群 + 人 + 正文」**跨连接去重**。

> ⚠️ 代价：同一个人在窗口内发的两条**内容完全相同**的消息会被并成一条
> （例如连打两个「1」）。日志是永久记录，所以这个模式**默认不开**，
> 必须由使用者主动开启。一般人不会同时开两个 bot，保持默认即可。

### 情况 B：同一个海豹实例，官方 bot 在官方群、民间 bot 在旧群（**主流用法**）

**完全不会重复**，因为两边收到的是**不同群**的消息。而这正是双向共通真正发挥作用的场景：

* 官方群里的话 → 记进**归一后的那份日志**（旧群）
* 旧群里的话 → 也记进**同一份**
* 两边 `.log list` / `.log get` / `.log export` 看到的是同一份内容

两个平台给的时间戳都保留着，所以两段对话的先后顺序可以还原。

### 情况 C：两个**独立的海豹实例**各跑一个 bot

**不支持。** 两个实例的 `data` 目录互相独立，绑定记录文件和去重表都不共享。
这种情况下绑定会看起来「生效了但读不到数据」。必须让两种连接方式挂在**同一个实例**上。

### 顺带说明 .log 状态的归属

日志状态（开没开、叫什么名字、logID）本来挂在各自的 `GroupInfo` 上。
实现里把状态**统一挂在归一后的群对象**上，所以：

* 官方群 `.log on` → 旧群立刻跟着记
* 旧群 `.log off` → 官方群也停
* 顺序随意，谁先开都行

排查这块时如果遇到「关不掉」或「开了没生效」，先用 `.group doctor` 确认绑定本身正常。

---

## 八、本次改动的文件清单

新增：

| 文件 | 作用 |
|---|---|
| `dice/ext_identity_bind.go` | 绑定核心：**双向索引存储、规范 ID 归一**、出题、验证、自检、`.bind` / `.group bind` 逻辑 |
| `dice/ext_identity_bind_test.go` | 绑定与双向共通测试：出题、答案解析、冷却、持久化、平台隔离、指令全流程、两侧收敛同一 key、抢号拦截、日志状态/写入/读取共通、自检 |
| `dice/official_qq_character_roll_markdown.go` | 官方 QQ 虚拟角色状态栏：模板求值、小号红字、转义 |
| `dice/official_qq_character_roll_markdown_test.go` | 状态栏测试：渲染、转义、实时更新、平台隔离、真实掷骰端到端 |
| `dice/help_title_test.go` | 锁定 `.help` 第一行是固定标题、不跟随「核心:骰子名字」，并断言 fork 说明存在 |
| `Dockerfile` | 多阶段构建：编译 WebUI → 编译核心（`CGO_ENABLED=0`）→ 精简运行镜像 |
| `.dockerignore` | 避免把 `data`、二进制等打进镜像 |
| `.github/workflows/docker-ghcr.yml` | 自动测试 + 构建 + 推送到 GHCR |
| `docker-compose.example.yml` | 云服务器部署模板 |

修改：

| 文件 | 改动 |
|---|---|
| `dice/dice_config.go` | 新增绑定配置字段 + `logMultiBotDedupWindowSec` + `FixIdentityBindConfig()`；新增 `officialQQRequestTimeoutSec` + `FixOfficialQQConfig()` |
| `dice/dice_config_default.go` | 默认值：绑定关闭、1 题、冷却 60 秒、答错锁 12 小时、官方 QQ 请求超时 60 秒、日志去重窗口 5 秒（= 上游原行为） |
| `dice/dice.go` | `Dice.IdentityBindStore` 字段与初始化 |
| `dice/im_session.go` | `MsgContext.OfficialQQStatusBarPending`；**`MsgContext.DataUserID` / `DataGroupID`（数据层身份，全项目唯一收敛点）** |
| `dice/im_helpers.go` | 发送层统一消费状态栏标记（群聊 + 私聊）；**`GetPlayerInfoBySenderRaw` 里填充 `Data*ID`** |
| `dice/rollvm_migrate.go` | `DiceFormatTmpl` 在渲染最终回复模板时打状态栏标记 |
| `dice/builtin_commands.go` | 注册全局指令 `.bind` / `.unbind` / `.group`（含 `.groupbind`）；**`.pc` 系列（list/new/rename/save/load/untagAll/del）改用数据层 ID**；`.help` 标题改为 `鲸娘与豹` + fork 说明 |
| `dice/dice_attrs_manager.go` | `LoadByCtx` 改用 `identityBindDataUserID/GroupID`（属性读写双向共通） |
| `dice/ext_log.go` | `.log` 状态统一挂在归一后的群对象（`stateGroup`）；骰子/玩家发言都写归一后的群；删除/编辑按归一后的群查找；日志去重窗口可配置且**跨 bot 模式为可选**；抽出 `EvalPlayerGroupCardTemplate` |
| `dice/platform_adapter_official_qq.go` | `officialQQGroupIDPrefix` 常量；请求超时可配置；群聊/单聊支持 `[CQ:file]`（file_type=4）；`SendFileTo*` 真正发文件；统一 file_info 解码；新增 `apiDomainOverride` 测试钩子 |
| `api/dice_config.go` | WebUI 保存绑定配置项、官方 QQ 请求超时、日志去重窗口 |

新增测试（重点）：

| 文件 | 作用 |
|---|---|
| `dice/ext_identity_bind_test.go` | 出题、答案解析、冷却、答错锁定、持久化、平台隔离、群/个人独立、**两侧收敛同一 key**、**旧号侧也能解析**、**抢号被拒**、**只有用户绑定时两个群保持隔离**、**日志状态共通**、**日志写入归一**、**`.log off` 作用在归一后的对象上（已验证能抓到回归）**、**自检报告** |
| `dice/official_qq_character_roll_markdown_test.go` | 状态栏：渲染、转义、实时更新、平台隔离、任意规则系统通用挂载 |
| `dice/platform_adapter_official_qq_media_test.go` | 官方 QQ 富媒体：超时默认/收敛、file_info 解码、CQ:file 转义与往返、频道不受影响 |
| `dice/platform_adapter_official_qq_chunked.go` | 大文件分片上传：预上传/分片 PUT/分片完成/合并，保留文件名 |
| `dice/platform_adapter_official_qq_chunked_test.go` | 分片上传测试：假腾讯服务器端到端、失败处理、开关路由、文件名推断 |

---

## 九、按你的要求未实现的部分

你明确要求**不做**下面两项，交给用户自己在自定义文本框里写：

* **【大成功】【极难成功】等结果文案转 Markdown 灰色引用框** —
  用户可以自己在 `SeaDice-text-template.yaml` 里把结果文案写成 `> 成功` 这类引用行。
* **本地 GIF / 图片与 Markdown 拆分发送** — 不做发送层改动，避免影响既有行为。

其余仍可后续增强的点：

* **日志跨群读要求旧群对象在内存里**。归一之后如果目标旧群不在 `ServiceAtNew`，
  读取会静默回退（`.group doctor` 会报出来）。彻底解决需要补一条
  "按群号构造只读 GroupInfo" 的路径。
* **两个独立海豹实例各跑一个 bot 不支持**（见七点八情况 C）。
  要支持得让两个实例共享数据库或走 HTTP 互通，是另一个量级的改动。
* **群配置（前缀、自动回复、牌堆、扩展开关）目前不共享**，只有日志与玩家数据归一。
  想让官方群直接继承旧群调好的群配置，需要把 `group_info` 也纳入归一范围。
* 状态栏目前跟在玩家**当前保存的** `.sn` 模板上。若想让状态栏独立于 `.sn` 排版，
  可以再开放 `$tQQ角色属性行` / `$tQQ角色状态栏` 之类的文案变量让用户自定义位置。
