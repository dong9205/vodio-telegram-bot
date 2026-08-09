# Telegram 智能媒体归档站

把 Telegram 变成私人媒体收件箱：转发一条消息，程序会自动分类并把图片、视频与说明保存到本地磁盘或 NAS，同时提供 Telegram 风格的 Web 预览。

推荐使用 **Bot + MTProto** 模式：Bot 负责接收和通知，MTProto 用户账号负责下载，从而避开 Bot API 的大文件下载限制。

预构建镜像：`registry.cn-hangzhou.aliyuncs.com/dong9205/vodio-telegram-bot:latest`。每次推送 `main` 分支时，GitHub Actions 会先运行测试，再构建并发布 amd64/arm64 镜像；推送 `v1.2.3` 形式的标签还会发布对应版本标签。仓库维护者需要配置 `ALIYUN_REGISTRY_USERNAME` 和 `ALIYUN_REGISTRY_PASSWORD` 两个 GitHub Actions Secrets。

## 功能概览

- 支持视频、`video/*` 文档、图片、图文、普通文本和 Telegram 相册；
- 使用 OpenAI 兼容接口，根据 caption、文件名、MIME type 和大小自动分类；
- 每条消息或相册保存为独立归档目录，并生成 `description.txt`；
- MTProto 支持大文件下载、实时进度、失败重试和 `.part` 断点续传；
- Web 页面按分类组织聊天，可直接查看图片、播放视频和阅读原始描述；
- 视频归档完成后，Bot 可私聊发送速度、耗时、保存位置及剩余队列；
- 单一 Go 程序、内嵌前端，适合 Docker、fnOS、群晖、威联通和普通 Linux 主机。

## 工作方式

```mermaid
flowchart LR
    A["发送或转发给 Bot"] --> B["Bot 转入私人归档群"]
    C["直接发送到归档群"] --> D["MTProto 监听新消息"]
    B --> D
    D --> E["AI 根据文本元数据分类"]
    E --> F["下载媒体并生成说明"]
    F --> G["保存到本地或 NAS"]
    F --> H["Web 展示进度与预览"]
    G --> I["Bot 发送完成通知"]
```

Web 页面使用以下层级：

| Telegram 风格界面 | 对应归档数据 |
| --- | --- |
| 左侧一个聊天 | 一个 AI 分类目录，例如 `Movies`、`Adult`、`Learning/Python` |
| 右侧一条消息 | 该分类下的一个归档目录 |
| 消息中的媒体组 | 同一条 Telegram 消息或相册中的图片、视频 |

媒体组会沿用 Telegram `message_id` 顺序。AI 只接收 caption、文件名、MIME type、来源类型和文件大小等文本元数据，不会上传图片或视频内容。

## 运行模式

| 模式 | 输入方式 | 大文件 | Web 任务与预览 | Bot 完成通知 | 适用场景 |
| --- | --- | --- | --- | --- | --- |
| 仅 Bot | 私聊 Bot | 受 Bot API 限制 | 不进入 MTProto 任务面板 | 基础保存回复 | 小文件、快速试用 |
| 仅 MTProto | 直接发到归档群 | 支持 | 支持 | 不支持 | 不需要 Bot 入口 |
| Bot + MTProto | 私聊 Bot 或直接发群 | 支持 | 支持 | 支持 | **推荐** |

下面以推荐的 **Bot + MTProto** 模式为例。

## Docker 快速开始

### 1. 准备账号和凭据

需要准备：

- Telegram Bot Token：在 [@BotFather](https://t.me/BotFather) 中发送 `/newbot` 创建；
- Telegram user ID：用于限制 Bot 使用者和接收完成通知；
- Telegram `api_id`、`api_hash`：在 [my.telegram.org/apps](https://my.telegram.org/apps) 申请；
- 一个正常的 Telegram 用户账号：供 MTProto 登录；
- 一个只用于归档的私人群：把 Bot 和 MTProto 登录账号都加入该群；
- OpenAI 或兼容服务 API Key；
- Docker 与 Docker Compose。

获取自己的 user ID：先私聊新 Bot 并发送 `/start`，在项目尚未运行时访问：

```text
https://api.telegram.org/bot<你的Token>/getUpdates
```

在返回结果中找到 `message.from.id`。调用地址含有 Bot Token，使用后不要分享浏览器或终端历史。

### 2. 创建归档群并获取群 ID

1. 创建私人群，例如 `Video Archive Inbox`；
2. 把 Bot 和 `MT_PHONE` 对应的用户账号加入群；
3. 确认 Bot 有权在群里发送消息；
4. 在群里发送一条消息，再通过 Bot API `getUpdates` 查看 `message.chat.id`。

Bot API 群 ID 通常是负数，超级群常见格式为 `-100...`。推荐将同一个 ID 写入：

```env
BOT_RELAY_CHAT_ID=-1001234567890
MT_INBOX_CHAT_ID=-1001234567890
```

`MT_INBOX_CHAT_ID` 同时接受 MTProto 日志中的正数 `peer_id` 和 Bot API 的负数 chat ID。

如果暂时不知道群 ID，可先设为 `0`。程序会记录收到消息的 `peer_id`，但发现模式不会执行归档：

```env
MT_INBOX_CHAT_ID=0
```

### 3. 配置 `.env`

复制示例：

```bash
cp .env.example .env
```

Windows PowerShell：

```powershell
Copy-Item .env.example .env
```

推荐配置：

```env
# Bot 负责入口和通知，MTProto 负责下载
BOT_ENABLED=true
MT_ENABLED=true

TELEGRAM_BOT_TOKEN=1234567890:replace-with-your-bot-token
ALLOWED_USER_IDS=123456789
BOT_RELAY_CHAT_ID=-1001234567890

MT_API_ID=123456
MT_API_HASH=replace-with-your-api-hash
MT_PHONE=+8613800000000
# MT_PASSWORD=replace-with-your-2fa-password
MT_INBOX_CHAT_ID=-1001234567890
MT_SESSION_FILE=/data/telegram-session/session.json
# 可选：让 MTProto 登录、监听和媒体下载使用 SOCKS5 代理
# MT_PROXY_URL=socks5://192.168.1.10:7890

STORAGE_ROOT=/data/telegram-videos

DASHBOARD_ENABLED=true
DASHBOARD_ADDR=127.0.0.1:9090
# DASHBOARD_STATE_FILE=/data/telegram-videos/.dashboard/tasks.json

AI_PROVIDER=openai
OPENAI_API_KEY=sk-replace-with-your-api-key
# OPENAI_BASE_URL=https://api.openai.com/v1
# AI_MODEL=gpt-4o-mini

# 可选：仅作用于 Bot API、Bot 文件下载和 AI 请求，不作用于 MTProto
# HTTP_PROXY_URL=http://127.0.0.1:7890
```

Docker Compose 会把以下容器内配置固定为持久化路径：

```text
STORAGE_ROOT=/data/telegram-videos
MT_SESSION_FILE=/data/telegram-session/session.json
DASHBOARD_ADDR=0.0.0.0:9090
```

### 4. 首次登录 MTProto

第一次启动需要交互式输入 Telegram 登录验证码：

```bash
docker compose run --rm -it tg-video-archiver
```

看到提示后输入 Telegram 收到的验证码：

```text
Telegram login code:
```

如果账号启用了两步验证，还需要正确配置 `MT_PASSWORD`。看到 `MTProto archiver started` 后按 `Ctrl+C` 停止；Session 已保存，后续通常不必再次验证。

不要让两个实例同时使用同一个 Bot Token 或 `MT_SESSION_FILE`。

### 5. 后台运行

```bash
docker compose up -d --build
docker compose logs -f tg-video-archiver
```

查看容器状态：

```bash
docker compose ps
```

### 6. 打开 Web 预览

Docker Compose 默认映射端口 `9090`：

```text
http://NAS地址:9090
```

本机运行时默认地址是：

```text
http://127.0.0.1:9090
```

Web 页面当前没有账号认证，只应在本机或可信局域网中访问，不要直接暴露到公网。

## 日常使用

### 发送给 Bot

1. 私聊 Bot 并点击 **Start**；
2. 发送或转发文本、图片、视频或相册；
3. Bot 将内容转入归档群；
4. MTProto 下载、分类并保存；
5. 视频完成后，Bot 私聊发送下载报告。

### 直接发送到归档群

也可以绕过 Bot，直接把内容发送或转发到 `MT_INBOX_CHAT_ID` 对应的群。若 Bot 同时启用，视频完成后仍会向 `ALLOWED_USER_IDS` 发送通知。

当前版本只监听程序运行期间收到的新消息，不主动扫描很早以前的群历史。

### Web 归档预览

Web 页面提供：

- 按 AI 分类聚合聊天；
- 同一分类中的多个归档按时间排列为多条消息；
- 原始描述、归档标题、媒体数量和文件大小；
- 图片预览和浏览器原生视频播放；
- Telegram 相册顺序编号；
- 下载进度、实时速度和失败原因；
- “继续下载”和“重新下载”；
- 全部、进行中、已保存和失败状态筛选。

媒体接口只允许读取 `STORAGE_ROOT` 内、任务记录中明确列出的图片和视频。

## 文件保存结构

一条普通消息或一个 Telegram 相册对应一个独立归档目录：

```text
data/telegram-videos/
└── Learning/
    └── Telegram/
        └── telegram-tutorial/
            ├── description.txt
            ├── image-01.jpg
            └── video-02.mp4
```

- `Learning/Telegram` 是 AI 分类目录，最多两级；
- `telegram-tutorial` 是本条消息的归档目录；
- `description.txt` 包含原始说明、分类结果、媒体信息和成功/失败文件列表；
- 下载未完成时，目录中可能存在隐藏的 `.part` 文件；
- 同名文件和目录不会被静默覆盖。

AI 优先使用以下顶级分类：

`Movies`、`TV`、`Anime`、`Music`、`Sports`、`Tech`、`Learning`、`News`、`Comedy`、`Food`、`Travel`、`Gaming`、`Adult`、`Personal`、`Unsorted`。

AI 失败、超时或返回无效结果时，内容会保存到 `Unsorted`，不会因此停止下载。

## 断点续传与重试

MTProto 下载失败时会保留确定名称的 `.part` 文件：

- **继续下载**：沿用原归档目录，把部分文件回退到安全的 512 KB 分块边界，再从该偏移继续请求；最多重复下载一个分块；
- **重新下载**：丢弃对应部分文件，从头开始本次失败内容；
- 升级到断点续传版本之前的旧失败任务没有 `.part` 文件，只能重新下载；
- Telegram 文件引用可能过期。如果长时间后继续或重试仍失败，请重新转发原消息。

任务历史保存在 `DASHBOARD_STATE_FILE`。其中包含重试所需的 Telegram 文件引用，应当视为敏感数据。

## 完成通知

同时启用 Bot 和 MTProto 时，包含视频的归档成功后会向所有 `ALLOWED_USER_IDS` 发送类似报告：

```text
✅ 视频下载完成

标题：Telegram 教程
视频数量：1
传输大小：428 MB
平均速度：2.6 MB/s
开始时间：2026-08-09 10:00:00
结束时间：2026-08-09 10:02:44
下载耗时：2 分 44 秒
归档位置：/data/telegram-videos/Learning/Telegram/telegram-tutorial

队列状态：
正在下载：1 个视频（1 个任务）
等待处理：3 个视频（2 个任务）
```

Telegram 不允许 Bot 主动私聊从未启动过它的用户。`ALLOWED_USER_IDS` 中的用户必须先私聊 Bot 并点击 **Start**。

## 配置参考

### Bot

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `BOT_ENABLED` | 自动判断 | 启用 Bot 入口；仅 MTProto 模式应明确设为 `false` |
| `TELEGRAM_BOT_TOKEN` | 无 | `BOT_ENABLED=true` 时必填 |
| `ALLOWED_USER_IDS` | 无 | Bot 白名单和通知接收者；英文逗号分隔 |
| `BOT_RELAY_CHAT_ID` | `0` | 非零时将 Bot 收到的内容转入归档群；为 `0` 时由 Bot API 直接下载 |
| `TELEGRAM_API_BASE_URL` | `https://api.telegram.org` | 官方或自建 Bot API Server 根地址 |

### MTProto

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `MT_ENABLED` | `false` | 启用用户账号 MTProto 监听与下载 |
| `MT_API_ID` | 无 | `MT_ENABLED=true` 时必填 |
| `MT_API_HASH` | 无 | `MT_ENABLED=true` 时必填 |
| `MT_PHONE` | 无 | 国际格式手机号，例如 `+8613800000000` |
| `MT_PASSWORD` | 空 | Telegram 两步验证密码 |
| `MT_SESSION_FILE` | `data/telegram-session/session.json` | 登录 Session 文件，必须持久化并保密 |
| `MT_INBOX_CHAT_ID` | `0` | 监听目标；支持正数 `peer_id` 或负数 Bot API chat ID；`0` 为发现模式 |
| `MT_PROXY_URL` | 空 | MTProto SOCKS5 代理；支持 `socks5://`、`socks5h://` 及可选用户名/密码 |

### 存储、AI、Web 与网络

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `STORAGE_ROOT` | 无 | 必填；本地或 NAS 归档根目录 |
| `AI_PROVIDER` | `openai` | 当前仅支持 OpenAI 兼容 Chat Completions API |
| `OPENAI_API_KEY` | 无 | 当前启动必填 |
| `OPENAI_BASE_URL` | `https://api.openai.com/v1` | OpenAI 兼容 API 根地址 |
| `AI_MODEL` | `gpt-4o-mini` | 分类模型 |
| `DASHBOARD_ENABLED` | `true` | 启用内嵌 Web 页面 |
| `DASHBOARD_ADDR` | `127.0.0.1:9090` | Web 监听地址；Compose 覆盖为 `0.0.0.0:9090` |
| `DASHBOARD_STATE_FILE` | `STORAGE_ROOT/.dashboard/tasks.json` | 任务历史和私密重试数据 |
| `HTTP_PROXY_URL` | 空 | Bot API、Bot 文件下载和 AI 请求使用的 HTTP/HTTPS 代理 |

未设置 `HTTP_PROXY_URL` 时，Go HTTP 客户端仍会遵循标准的 `HTTP_PROXY`、`HTTPS_PROXY` 和 `NO_PROXY`。MTProto 不使用这些 HTTP 代理；如需代理 MTProto，请单独设置 `MT_PROXY_URL`：

```env
# 无认证；未写端口时默认为 1080
MT_PROXY_URL=socks5://192.168.1.10:7890

# 用户名/密码认证（特殊字符需要 URL 编码）
# MT_PROXY_URL=socks5://username:password@192.168.1.10:7890
```

代理地址必须能从容器内部访问。容器中的 `127.0.0.1` 指向容器自身，不是 Docker 主机。启用成功后日志会出现 `MTProto proxy enabled`，且不会记录代理密码。

## 本地运行与开发

需要 Go 1.24 或更新版本：

```bash
go mod download
go run ./cmd/tg-video-archiver
```

程序会读取当前目录的 `.env`；已存在的系统环境变量优先于 `.env` 中的同名配置。

运行测试：

```bash
go test ./...
```

gotd 包含体积很大的生成代码。低内存环境可以先限制包级并发：

```bash
GOMAXPROCS=2 go test -p 1 ./...
```

Windows PowerShell：

```powershell
$env:GOMAXPROCS=2
go test -p 1 ./...
```

如果编译器仍然耗尽虚拟内存，请增加系统页面文件，或在内存更充足的 CI/主机上运行完整测试。

## 常见问题

### 启动时报缺少配置

- 至少启用 `BOT_ENABLED` 或 `MT_ENABLED` 中的一种模式；
- `BOT_ENABLED=true` 时必须提供 `TELEGRAM_BOT_TOKEN` 和 `ALLOWED_USER_IDS`；
- `MT_ENABLED=true` 时必须提供 `MT_API_ID`、`MT_API_HASH` 和 `MT_PHONE`；
- `STORAGE_ROOT` 和 `OPENAI_API_KEY` 当前始终必填。

### `PHONE_NUMBER_INVALID`

- `MT_PHONE` 必须是 Telegram 账号实际使用的手机号；
- 使用完整国际格式，例如 `+8613800000000`；
- 不要包含空格、短横线，也不要填 Bot Token、user ID 或群 ID。

### Bot 回复已入队，但没有保存

- 确认 `BOT_RELAY_CHAT_ID` 与 `MT_INBOX_CHAT_ID` 指向同一个群；
- 确认 Bot 和 MTProto 登录账号都在该群中；
- 检查日志是否出现 `MTProto target message received`；
- 临时设置 `MT_INBOX_CHAT_ID=0`，通过 discovery 日志核对 `peer_id`；
- 在 Web 页面查看失败原因，并选择继续或重新下载。

### 下载很慢

- 单个 MTProto 文件当前使用单路串行下载，程序同时最多处理两个归档任务；
- MTProto 不使用 `HTTP_PROXY_URL`；需要代理时设置 `MT_PROXY_URL`，登录、监听和媒体下载会统一走 SOCKS5；
- 用同一机器上的 Telegram Desktop 下载同一文件，可帮助判断是否为网络路由问题；
- `FLOOD_PREMIUM_WAIT` 表示 Telegram 账号侧触发限制；
- `engine forcibly closed: context canceled` 通常表示程序或 MTProto 引擎被关闭。

### Web 页面无法访问

- Docker Compose 默认映射 `9090:9090`；
- 本地运行默认只监听 `127.0.0.1:9090`；
- NAS 部署时确认容器运行正常，并允许可信局域网访问 9090；
- 不要为了方便而直接将端口暴露到公网。

### AI 分类总是进入 `Unsorted`

- 检查 `OPENAI_API_KEY`；
- 兼容服务的 `OPENAI_BASE_URL` 通常应以 `/v1` 结尾；
- 确认 `AI_MODEL` 是服务实际支持的模型；
- AI 只使用文本元数据，caption 和有效文件名不足时可能无法判断。

### 没有收到 Bot 完成通知

- 确认 Bot 和 MTProto 同时启用；
- 确认 user ID 在 `ALLOWED_USER_IDS` 中；
- 先私聊 Bot 并点击 **Start**，确认没有屏蔽 Bot；
- 当前通知只针对包含视频且成功完成的 MTProto 归档。

### 看不到“继续下载”

只有失败任务留下有效 `.part` 文件时才会显示“继续下载”。旧任务、归档目录丢失或没有部分文件时，应选择“重新下载”。

## 安全建议

- 不要提交 `.env`，也不要公开 Bot Token、`api_hash`、手机号、2FA 密码或 Session；
- `MT_SESSION_FILE` 可以代表 Telegram 登录会话，应像密码一样保护；
- `DASHBOARD_STATE_FILE` 包含 Telegram 文件引用，不要共享；
- 只把 Bot 和 MTProto 账号加入专用私人归档群；
- `ALLOWED_USER_IDS` 只填写可信用户；
- Web 页面没有登录认证，不要把 9090 端口直接暴露到公网；
- 项目会清洗 AI 返回的目录和文件名，拒绝绝对路径、盘符、`..` 和控制字符；
- 媒体预览只能读取 `STORAGE_ROOT` 内任务明确记录的文件；
- 下载先写入 `.part` 或临时文件，完成后再移动为正式文件。

## 当前限制

- 不扫描历史消息，只处理程序运行期间收到的新消息；
- 单个 MTProto 文件为单路下载；
- MTProto 不支持普通 HTTP 代理或 Telegram MTProxy，目前仅支持通过 `MT_PROXY_URL` 配置 SOCKS5；
- 断点续传只适用于新版本创建并保留 `.part` 文件的任务；
- AI 只分析文本元数据，不分析图片、视频画面或音频；
- Web 页面没有账号登录；
- `OPENAI_API_KEY` 当前是启动必填项，即使只希望使用回退分类也必须配置。

## 官方参考

- [Telegram Bot 创建教程](https://core.telegram.org/bots/tutorial)
- [Telegram Bot 功能与 BotFather](https://core.telegram.org/bots/features#botfather)
- [Telegram Bot API](https://core.telegram.org/bots/api)
- [申请 Telegram api_id 与 api_hash](https://core.telegram.org/api/obtaining_api_id)
- [Telegram API 服务条款](https://core.telegram.org/api/terms)
