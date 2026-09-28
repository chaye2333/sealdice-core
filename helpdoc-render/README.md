# helpdoc-render —— 帮助文档图片化渲染后端

给海豹（本 fork）的「帮助文档图片化」功能出图的小服务。**纯 Go、无浏览器、低内存**：
常驻内存十几 MB 量级，渲染一张 820×1000 的图大约需要 3~5MB 临时内存。

## 它解决什么

TRPG 帮助文档里可能有敏感词，或者一堆正常词组合起来像宗教传播。海豹主程序在
**命中敏感词**或**正文超过设定长度**时，会把正文发到这个服务渲染成图片，
再以图片消息发出去（而不是把文本直接交给平台）。

## 接口契约

```
POST /render
Content-Type: application/json
Authorization: Bearer <token>        # 启动时用 -token 设置；不设则不校验

{"text": "帮助文档正文（Markdown 子集）", "format": "markdown", "width": 820, "reason": "censor"}

→ 200 image/png，响应体是图片字节（响应头带 Cache-Control: no-store）
→ 413 正文超过 -max-text-chars
→ 其它 4xx/5xx + 纯文本错误说明
```

```
GET /health → 200 "ok"
```

主程序侧的对应实现见 `dice/help_image_render.go`；任何失败（连不上、超时、非 200、
返回不是图片、图太大）都会**回退成文本发送**，不会让帮助功能不可用。

支持的 Markdown 子集：标题（`#`~`######`）、项目符号与有序列表、引用块、围栏代码块、
行内代码、`**加粗**`、`[文字](链接)`、分隔线。CJK 按字折行，西文尽量在空格处断。

## 编译（独立 module，不参与主程序构建）

本目录自带 go.mod，与海豹主程序**互不影响**：主程序构建、镜像、CI 都不会编译它。

```bash
# 在仓库根目录
cd helpdoc-render && go build -o helpdoc-render .
```

## 字体（必须）

渲染中文需要中文字体，服务会自动探测常见路径（Noto CJK / 文泉驿 / macOS 苹方 /
Windows 微软雅黑）。Linux 上装一个即可：

```bash
apt install -y fonts-noto-cjk        # Debian / Ubuntu
apk add --no-cache font-noto-cjk     # Alpine
```

也可以显式指定：`-font /path/to/font.ttc`，或用环境变量 `HELPDOC_RENDER_FONT`。

## 运行

```bash
./helpdoc-render -addr 127.0.0.1:3212 -token 换成你自己的随机串
```

常用参数：

| 参数 | 默认 | 说明 |
|---|---|---|
| `-addr` | `127.0.0.1:3212` | 监听地址。**建议只监听回环**，用同一个 compose 网络或 nginx 反代暴露 |
| `-font` | 自动探测 | 字体文件（`.ttf`/`.otf`/`.ttc`） |
| `-token` | 空 | 设置后要求 `Authorization: Bearer <token>` |
| `-font-size` | `17` | 正文字号 |
| `-line-height` | `1.65` | 行高倍数 |
| `-padding` | `26` | 页面内边距 |
| `-max-height` | `20000` | 单图最大高度，超过则截断并提示 |
| `-max-text-chars` | `20000` | 正文最大字符数，超过直接报 413 |
| `-concurrency` | `2` | 并发渲染上限（限制峰值内存） |

### systemd

```ini
[Unit]
Description=SealDice helpdoc render backend
After=network.target

[Service]
ExecStart=/opt/helpdoc-render/helpdoc-render -addr 127.0.0.1:3212 -token 你的令牌
Restart=always
RestartSec=3
# 内存上限兜底：一般用不到这么多
MemoryMax=256M
User=nobody

[Install]
WantedBy=multi-user.target
```

### docker compose（与海豹同一个网络）

```yaml
services:
  helpdoc-render:
    image: alpine:3.20
    # 把编译好的二进制挂进去；也可以自己打个镜像
    volumes:
      - ./helpdoc-render:/usr/local/bin/helpdoc-render:ro
    command: ["/usr/local/bin/helpdoc-render", "-addr", "0.0.0.0:3212", "-token", "你的令牌"]
    environment:
      - HELPDOC_RENDER_FONT=/usr/share/fonts/opentype/noto/NotoSansCJK-Regular.ttc
    restart: unless-stopped
    # 不需要对外暴露端口：海豹容器通过服务名访问
```

海豹那边（WebUI → 帮助文档设置）填：`http://helpdoc-render:3212/render` + 同一个令牌。

## 自测

```bash
curl -s -X POST http://127.0.0.1:3212/render \
  -H 'Content-Type: application/json' -H 'Authorization: Bearer 你的令牌' \
  -d '{"text":"# 标题\n\n正文","width":820}' -o out.png
file out.png   # 应为 PNG image data
```

## 已知限制

- 只支持 Markdown 子集，复杂排版（表格、嵌套列表、图片）不渲染；
- 图片不能复制、不能被读屏软件识别，这是"绕开文本审核"的代价；
- 每次请求都现场渲染、**不做缓存**（按需求"一次性临时用"），所以文档越长、调用越频繁，
  CPU 开销越明显；并发上限默认 2，可按机器配置调整。
