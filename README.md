# Twitter 媒体下载 Bot

把推文链接发给 Telegram Bot，Bot 下载其中的图片 / 视频 / GIF 并发回聊天。使用 Go 编写，Long Polling 模式，无需公网 IP、域名或 HTTPS 证书。

- 数据源：[FxTwitter](https://github.com/FixTweet/FxTwitter) 公开 API，无需登录、无需 cookie
- 支持 `x.com` / `twitter.com` 的 `/status/<id>` 链接，也支持直接发送推文 ID
- 一条消息可包含多个链接（最多 5 条），逐条处理
- 图片取原图（`name=orig`），视频 / GIF 取直链
- 照片优先由 Telegram 按原 URL 拉取（约 5 MB 以内），避免服务器下载和上传；失败自动回退
- 多张媒体合并为媒体组发送；超过 Telegram 限制的媒体直接提示并跳过
- 白名单授权；心跳健康检查、offset 持久化、网络错误自动重试
- 下载到临时目录，发送后立即删除，不在服务器保留媒体文件
- 来源可扩展：新增站点只需实现 `Source` 接口并注册一行

## 快速开始

### 1. 创建 Bot

在 Telegram 中找 [@BotFather](https://t.me/BotFather)，发送 `/newbot`，按提示设置名称，记下返回的 token。

### 2. 获取自己的用户 ID

给 [@userinfobot](https://t.me/userinfobot) 发送任意消息，即可获取你的数字用户 ID。Bot 不会回复未授权账号，因此必须提前配置。

### 3. 配置

```bash
cp .env.example .env
vi .env
```

至少填写：

```dotenv
BOT_TOKEN=123456789:AAH...
ALLOWED_USER_IDS=你的用户ID
```

### 4. 启动

```bash
mkdir -p state
docker compose up -d --build
docker compose logs -f
```

看到 `bot started` 和 `polling started` 即启动成功。

### 5. 使用

用授权账号私聊 Bot：

- 发送推文链接 → Bot 以 Reply 方式回一条状态消息，实时显示获取、下载、上传进度；有媒体时处理完删除状态消息，媒体以 Reply 发送，第一条媒体的 caption 是作者、正文与 Source
- 直接发送推文 ID（纯数字）→ 同上
- 纯文字推文 → 状态消息最终变为帖子内容
- 一条消息包含多个链接 → 每条链接各自一条 Reply 状态消息
- `/start` 或 `/help` → 使用说明

## 配置项

| 环境变量 | 必填 | 默认值 | 说明 |
| --- | --- | --- | --- |
| `BOT_TOKEN` | 是 | — | @BotFather 提供的 token |
| `ALLOWED_USER_IDS` | 是 | — | 授权用户 ID，英文逗号分隔 |
| `STATE_DIR` | 否 | `/state` | offset、心跳、临时文件目录 |
| `TMP_DIR` | 否 | `$STATE_DIR/tmp` | 下载临时目录 |
| `MAX_UPLOAD_MB` | 否 | `50` | 单个媒体上限（MB），范围 1–50 |
| `FXTWITTER_BASE` | 否 | `https://api.fxtwitter.com` | FxTwitter API 基址 |
| `POLL_TIMEOUT_SECONDS` | 否 | `60` | 长轮询超时，范围 5–120 |
| `LOG_LEVEL` | 否 | `info` | `debug` / `info` / `warn` / `error` |
| `TIMING_TRACE` | 否 | `off` | `off` / `summary` / `verbose`，每条消息的分段耗时日志 |
| `TZ` | 否 | `Asia/Shanghai` | 容器时区 |
| `HTTPS_PROXY` / `HTTP_PROXY` | 否 | — | 访问 Telegram / Twitter 所需代理 |

## 耗时诊断

默认关闭，设置 `TIMING_TRACE=summary` 后，每条包含链接的消息处理完成会输出一行 `TIMING summary`：

```text
TIMING summary trace=42:99 poll=0.9s queue=3ms status=306ms refs=1 fx=1.4s fx_tries=1 fx_sleeps=0 fx_http=200 fx_code=200 dl=392ms dl_bytes=81.0KB dl_rate=207.0KB/s dl_tries=1 dl_sleeps=0 dl_http=200 copy=201ms fsync=9ms rename=1ms reuse=true up=1.2s up_kind=photo up_via=file up_size=81.0KB up_rate=450.0KB/s up_tries=1 up_http=200 wrote=180ms ttfb=900ms retry_after=0 edit=2ms total=2.1s recv=1.2s skew=+0.4s
```

主要字段：

- `poll`：按 Telegram 时间估算的「用户发送 → Bot 接收」延迟；`skew` 是本机与 Telegram 的时钟差估算。
- `queue` / `status`：任务在队列中等待、发送每条链接 Reply 状态消息的耗时之和。
- `edit`：阶段与进度编辑的累计耗时（含下载/上传进度刷新）。
- `fx`：FxTwitter 获取总耗时（含重试和退避）；`fx_tries` / `fx_http` / `fx_code` 是尝试次数与响应码，`fx_sleeps` 是重试休眠。
- `dl` / `copy` / `fsync` / `rename`：媒体下载总耗时、网络写入耗时、落盘 fsync、重命名；`dl_bytes` / `dl_rate` 是大小与速度，`reuse` 表示是否复用了连接。
- `up` / `up_via` / `up_rate` / `wrote` / `ttfb` / `retry_after`：Telegram 上传总耗时（含重试）、发送方式（`url` 表示由 Telegram 自己拉取，`file` 表示本地上传）、上传速度、请求体写完的时刻、首字节时刻（含服务器处理）、429 退避时长。
- `total`：从用户发送到处理完成；`recv`：从 Bot 接收到处理完成。

需要逐阶段明细时设置 `TIMING_TRACE=verbose`，会额外输出 `TIMING step`（阶段）和 `TIMING http`（每次 HTTP 尝试的 DNS/TCP/TLS/TTFB/字节数/连接复用）日志。`LOG_LEVEL=debug` 还会输出 offset 文件 fsync 耗时。

## 限制

- Telegram Bot API 单文件上传上限 **50 MB**。超过的媒体不会发送，Bot 会在失败说明 Reply 中给出原媒体链接。
- Telegram `sendPhoto` 的照片上限是 **10 MB**。10–50 MB 的照片会自动改用「文件」发送（不压缩、不改变内容）。
- 照片默认由 Telegram 按 URL 拉取；Bot API 对 URL 照片的限制约为 **5 MB**，超过或拉取失败时会回退为下载后上传。
- Telegram 上传固定使用 HTTP/1.1，避免丢包线路上 HTTP/2 流控进一步拖慢大文件。
- FxTwitter 对高频请求可能返回 429；Bot 会重试并在消息之间限速。仍失败时稍后重发链接即可。
- 转发帖、引用帖会按 FxTwitter 返回的原帖内容发送。

## 新增来源

`source.go` 定义了来源接口：

```go
type Source interface {
    Name() string
    FindRefs(text string) []Match
    Fetch(ctx context.Context, ref Ref) (*Post, error)
}
```

新增站点（例如 Bluesky）时：

1. 新建 `bluesky.go`，实现 `Name` / `FindRefs` / `Fetch`；
   - `FindRefs` 返回链接在文本中的位置和 `Ref`；
   - `Fetch` 返回 `Post{CanonicalURL, Media}`，媒体只填 `URL` / `Type` / `Filename`；
   - 帖子不存在或已删除时返回包装了 `ErrPostUnavailable` 的错误。
2. 在 `bot.go` 的 `run()` 中注册：`newRegistry(newFxTwitter(...), newBluesky(...))`。

下载、大小限制、发送、进度提示都由通用流程处理，无需改动。

下载器默认只允许 `https://*.twimg.com` 的媒体地址（防 SSRF）。新增来源如果使用其他 CDN，需要在 `download.go` 中扩展 `isTwimgURL` 或为该来源配置允许列表。

## 项目结构

```
main.go        配置、入口、健康检查
bot.go         轮询、鉴权、worker 队列、消息处理
timing.go      分段耗时与 TIMING 时间线
source.go      来源接口、注册表、跨来源类型
twitter.go     FxTwitter 来源实现
render.go      帖子信息（作者/正文/Source）渲染与 caption 截断
status.go      Reply 状态消息、阶段与下载/上传进度
download.go    流式下载、大小限制、重试、临时文件
media.go       Telegram 上传、媒体组、照片降级、URL 发送、429 重试
Dockerfile     多阶段构建
docker-compose.yml
```

## 本地开发

```bash
BOT_TOKEN=... ALLOWED_USER_IDS=... STATE_DIR=./state go run .
```

运行测试：

```bash
go test -race ./...
```

检查心跳：

```bash
STATE_DIR=./state go run . -healthcheck
```
