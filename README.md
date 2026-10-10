# qwen2api（MIPS32 移植版）

[![build](https://github.com/deepmean/qwen2api-mipsle/actions/workflows/build.yml/badge.svg)](https://github.com/deepmean/qwen2api-mipsle/actions/workflows/build.yml)

把 [Qwen](https://chat.qwen.ai) 网页版封装成 **OpenAI 兼容 API** 的极简网关，
交叉编译为单一静态二进制，运行在 **MIPS32 小端 OpenWrt 路由器**上
（MediaTek MT7621 / `ramips`，实测机型 JCG-Q20）。

上游 [deepmean/Qwen2API](https://github.com/deepmean/Qwen2API) 是 Bun/Node 应用。
Bun 没有 MIPS32 后端，OpenWrt 24.10 也没有 `mipsel_24kc` 的 `node` 包，
因此核心对话链路用 **Go 纯标准库重写**，交叉编译为静态二进制。

二进制由 **GitHub Actions 云编译**产出，本地无需安装 Go（见[构建](#构建)）。

## 接口范围

| 接口 | 说明 |
| --- | --- |
| `GET /v1/models` | 实时拉取 Qwen 模型目录，并按 `chat_type` 能力合成 `-image` 变体；拉取失败时回退到静态列表 |
| `POST /v1/chat/completions` | 支持 `stream: true` 与 `stream: false`；`-image` 后缀走生图 |
| `POST /v1/images/generations` | OpenAI 生图接口，返回图片 URL（或 `b64_json`） |
| `GET /health` | 运行时长、账号数量、各账号登录状态 |

为控制内存占用，以下**未移植**：Anthropic 兼容层（`/v1/messages`）、
工具调用协议、图片编辑与视频生成、Web 管理面板。

## 生图（t2i）

上游目录只返回裸 id，`qwen3.8-max-image` 并不存在——`-image` 是本网关与 Qwen
网页端共同用来选择"这个模型的生图能力"的**后缀约定**。因此 `/v1/models` 会读取
上游 meta 里的 `chat_type`，凡是声明了 `t2i` 的模型都追加一个 `-image` 变体：

```
qwen3.8-max          ->  t2t（文本）
qwen3.8-max-image    ->  t2i（生图）
```

生图结果**不需要独立传输**：上游把它当作普通 completion 流返回，图片 URL 落在
`choices[0].delta.content` 上，`phase` 为 `image_gen`（实测响应约 1.5 KB）：

```
data: {"choices":[{"delta":{"role":"assistant",
  "content":"https://cdn.qwenlm.ai/output/.../xxx.png?key=...",
  "phase":"image_gen","status":"typing"}}]}
```

两条路径都能取到图片：`-image` 后缀经 `/v1/chat/completions` 把 URL 作为
`content` 返回；`/v1/images/generations` 则返回 OpenAI 形状的 `data[].url`。

### 内存与并发

实测（JCG-Q20 / MT7621，250 MB RAM，约 73 MB 可用）：

| 场景 | 峰值增量 | 结果 |
| --- | --- | --- |
| 单张生图 | +6 ~ 20 MB | 正常 |
| 4 MiB × 4 并发缓冲 | — | OOM 被杀 |

生图 URL 本身很轻，真正的开销在 `response_format: "b64_json"`——它要把 PNG 拉进
内存再 base64 展开（4/3 倍）。所以生图有**独立于** `MAX_CONCURRENT` 的闸门
`IMAGE_MAX_CONCURRENT`，默认 **2**；`b64_json` 的下载另受 `IMAGE_MAX_BYTES`
约束（默认 8 MiB）。超出的请求返回 `429`。

## 长上下文附件

Qwen 边缘（阿里云 WAF）在请求体接近 128 KiB 时，会以 HTTP 200 返回
`FAIL_SYS_USER_VALIDATE` 风控挑战，而不是正常回答。像 Claude Code 这类
Agent 客户端每一轮都会重发完整对话（系统提示词 + 工具定义 + 历史 ≈ 950 KB），
几轮之后必然越线。

本项目不丢弃历史，而是像上游一样把历史**外置**：请求体超过阈值后，
把对话历史上传为 Qwen 文档，并在 `message.files` 中引用。模型读取附件，
内容不丢失。

```
POST /api/v1/files/getstsToken   -> 临时阿里云 OSS 凭证 + 对象 key
PUT  https://<bucket>.<endpoint>/<key>   （OSS Signature-V1，HMAC-SHA1 签名）
POST /api/v2/files/parse         -> 请求 Qwen 解析文档
POST /api/v2/files/parse/status  -> 轮询直到解析完成
```

WAF 只对 parse 调用按出口 IP 限流，所以上传的历史会按会话缓存并复用
（`attach.go`）：当后续轮次的历史仍以已上传内容的前缀开头时，只内联新尾段，
不再上传。**一次 parse 覆盖多轮**。

上传不可用时回退到有损压缩（`context.go`）；WAF 连续挑战 parse 时熔断器
暂停上传 5 分钟——是退避，不是反复撞挑战。

设备实测：2 MB / 264 条消息的 Claude Code 量级请求被外置（90 KB 内联，
其余进附件），模型能准确召回埋在历史开头的 token；后续轮次复用同一附件。

## 移植保留的关键细节

Qwen 的 WAF 会对客户端做指纹识别，因此决定请求成败的部分被忠实保留：

- **`ssxmod_itna` / `ssxmod_itna2` Cookie** —— 37 字段浏览器指纹经 LZW + 自定义
  Base64 编码（`ssxmod.go`）。已与上游 JavaScript **逐字节**交叉校验，见
  `ssxmod_test.go`。
- **按账号派生浏览器身份** —— 设备 ID、屏幕、语言、WebGL 渲染器、Chrome 版本
  均由账号邮箱的 SHA-256 确定性派生，同一账号始终呈现同一套一致身份。
- **请求头画像** —— `user-agent`、`sec-ch-ua`、`sec-ch-ua-platform`、
  `accept-language`、`sec-fetch-*`、`timezone`、`source: web`、`version: 0.2.81`。
  版本号刻意**不跟随**线上 FE 构建（2026-10 已到 0.3.12）：接口只接受与 ssxmod
  指纹模板一致的版本，单独把版本改成 0.3.12 会让 `chats/new` 返回
  `Unauthorized`。改版本号必须同步升级 `ssxmod.go` 的指纹模板。
- **`feature_config.output_schema: "phase"`** —— 关键字段。缺失则上游不再返回
  `delta.phase`，所有增量帧被丢弃。
- **两通道输出** —— 推理内容进 `reasoning_content`，正文进 `content`，与上游默认
  一致（`legacyReasoningInContent = false`）。
- **单消息扁平化** —— 网页接口只接受一条当前消息，多轮对话折叠为 JSONL 历史块
  加最后一轮。
- **模型名规范化**（`model.go`）—— 剥离客户端附加的 `[1M]` 等后缀与
  `-thinking`/`-search` 行为后缀，并用实时目录把别名映射为规范 ID。

## 构建

**二进制由 GitHub Actions 云编译产出，本地不需要安装 Go。**

工作流：[`.github/workflows/build.yml`](.github/workflows/build.yml)

| 触发 | 行为 |
| --- | --- |
| push 到 `main` | 测试 + 编译，并更新滚动预发布 `latest` |
| 打 `v*` 标签 | 测试 + 编译，并发布正式 release（自动生成 release notes） |
| Pull Request | 仅测试 + 编译（不发布） |
| 手动 | Actions 页面 → build → Run workflow |

固定下载地址（可直接拉到路由器）：

```sh
wget -O /tmp/qwen2api.new \
  https://github.com/deepmean/qwen2api-mipsle/releases/download/latest/qwen2api-linux-mipsle
chmod +x /tmp/qwen2api.new
```

工作流分两个 job，`build` 依赖 `test`：

1. **test** —— `go vet ./...` 与 `go test ./...`。测试含 Cookie 编码器与上游
   JavaScript 的**逐字节**交叉校验，是最容易在无声中失效的部分。
2. **build** —— 以 `GOOS=linux GOARCH=mipsle GOMIPS=softfloat CGO_ENABLED=0`
   交叉编译，然后**校验产物**再上传。

### 产物校验

编译后会解析 ELF 并断言，任何一条不符即失败，坏二进制进不了 release：

| 断言 | 不满足的后果 |
| --- | --- |
| `ELF32` | 内核拒绝执行 |
| little endian | 内核拒绝执行 |
| `Machine: MIPS` | 内核拒绝执行 |
| `FP ABI: Soft float` | **能启动，首次用浮点即崩溃** |
| 无 `PT_INTERP`（静态） | 路由器上没有匹配的解释器 |

`GOMIPS=softfloat` 是**必须的**：MT7621 无 FPU，硬浮点构建会在运行时崩溃。
Go 链接器在 `GOMIPS=softfloat` 时把 `.MIPS.abiflags` 的 `fp_abi` 写成 `3`，
binutils 将其显示为 `FP ABI: Soft float`。

产物约 6 MB，静态链接，仅用 Go 标准库，**无需下载任何模块**。

### 本地构建（可选）

仅在需要本地调试时使用；仓库不依赖本地 Go：

```sh
export GOOS=linux GOARCH=mipsle GOMIPS=softfloat CGO_ENABLED=0
go build -trimpath -ldflags "-s -w" -o dist/qwen2api-linux-mipsle .
go test ./...
```

> 注意：本地构建会把当前 git 状态写进 `.go.buildinfo`（`vcs.revision` /
> `vcs.modified`）。因此**同一份源码在本地与 CI 产出的二进制哈希不同**——
> 这是 `-trimpath` 保留 VCS 戳的正常现象，不是构建不一致。CI 从干净树构建，
> `vcs.modified=false`，是更规范的那一份。

## 部署（OpenWrt）

| 路径 | 内容 |
| --- | --- |
| `/opt/qwen2api/qwen2api` | 可执行二进制 |
| `/opt/qwen2api/qwen2api.conf` | `API_KEY`、`SERVICE_PORT` 等（权限 600） |
| `/opt/qwen2api/accounts.txt` | `email:password` 行（权限 600） |
| `/etc/init.d/qwen2api` | procd 服务脚本 |

```sh
# 直接从 CI 下载（推荐）
wget -O /tmp/qwen2api.new \
  https://github.com/deepmean/qwen2api-mipsle/releases/download/latest/qwen2api-linux-mipsle
scp /tmp/qwen2api.new          root@ROUTER:/opt/qwen2api/qwen2api
scp deploy/init.d-qwen2api     root@ROUTER:/etc/init.d/qwen2api
chmod +x /opt/qwen2api/qwen2api /etc/init.d/qwen2api
/etc/init.d/qwen2api enable
/etc/init.d/qwen2api start
logread -e qwen2api
```

> 升级前先备份现网二进制（`cp qwen2api qwen2api.bak`），并核对哈希：
> 用 `md5sum` 比对路由器上的文件与 release 中的文件，确认部署的正是 CI 产物。

填入账号后重启：

```sh
vi /opt/qwen2api/accounts.txt     # email:password
/etc/init.d/qwen2api restart
```

> 注意：procd 只在启动时读取 `qwen2api.conf`，改配置后**必须重启**。

要跨固件升级保留，把以下路径加入 `/etc/sysupgrade.conf`：

```
/opt/qwen2api/
/etc/init.d/qwen2api
/etc/rc.d/S96qwen2api
/etc/rc.d/K10qwen2api
```

## 使用

```sh
curl http://192.168.26.1:3000/v1/models

curl http://192.168.26.1:3000/v1/chat/completions \
  -H 'Authorization: Bearer sk-your-key' \
  -H 'Content-Type: application/json' \
  -d '{"model":"qwen3.8-max","stream":true,
       "messages":[{"role":"user","content":"你好"}]}'
```

## 配置

| 环境变量 | 默认值 | 说明 |
| --- | --- | --- |
| `SERVICE_PORT` | `3000` | 监听端口 |
| `API_KEY` | *（未设置）* | 逗号分隔的 Bearer Token。未设置则端口开放，启动时打印警告 |
| `QWEN_CHAT_PROXY_URL` | `https://chat.qwen.ai` | 上游地址 |
| `ACCOUNTS_FILE` | `/opt/qwen2api/accounts.txt` | 账号文件 |
| `MAX_CONCURRENT` | `8` | 并发对话请求上限 |
| `IMAGE_MAX_CONCURRENT` | `2` | 并发生图请求上限（独立闸门，见"内存与并发"） |
| `IMAGE_MAX_BYTES` | `8388608` | `b64_json` 单张图片下载上限（字节） |
| `AGENT_CONTEXT_PREFIX_TTL_SECONDS` | `1800` | 已上传历史附件的可复用时长 |
| `AGENT_CONTEXT_PREFIX_MAX_ENTRIES` | `8` | 缓存附件的会话数 |
| `LOG_LEVEL` | `INFO` | `DEBUG` / `INFO` / `WARN` / `ERROR` |

### 账号文件格式

```
# 每行一个凭证：email:password
user1@example.com:MyP@ssw0rd
user2@example.com:pass:with:colons
# 空行与 # 开头的行被忽略
```

密码本身可含 `:`，只有**第一个**冒号作为分隔符。

## 架构

| 文件 | 职责 |
| --- | --- |
| `main.go` | HTTP 服务器、路由、鉴权、流式转发、外置调度、生图端点与图片并发闸门 |
| `attach.go` | 上下文外置编排：会话切分、前缀缓存、复用匹配、熔断、压缩兜底 |
| `upload.go` | STS Token → OSS 上传 → parse → 轮询；`message.files` 描述符 |
| `context.go` | 有损压缩（附件不可用时的兜底） |
| `qwen.go` | 上游客户端：登录、建会话、补全、模型目录、浏览器头构造 |
| `sse.go` | SSE 解码 + delta 规范化（含 `thinking_summary` 与 `image_gen` 通道） |
| `ssxmod.go` | SSXMOD Cookie 生成（LZW + 自定义 Base64 + 指纹） |
| `model.go` | 模型名规范化与别名解析（含 `-image` → `t2i`） |
| `pool.go` | 账号轮询池 + JWT 懒登录与刷新 |
| `transport.go` | `net/http` 传输层（显式 gzip/deflate 解压） |
| `config.go` | 配置加载与账号文件解析 |

## 注意事项

- 路由器需能访问 `chat.qwen.ai`。**中国大陆出口最理想**——WAF 视其为正常客户端。
  出口在别处时，指纹（`zh-CN`、`Asia/Shanghai`）会显得不一致，请求可能被挑战。
- `Accept-Encoding` 只声明 `gzip` / `deflate`；Go 标准库无法解码 `br`/`zstd`，
  声明了反而会得到无法解析的响应体。
- Token 懒登录，并在过期前 5 分钟重新登录。上游返回 `401`/`403` 时失效该 token，
  下次请求重新认证。
- **不记录请求体**，只记录字节数与解析出的模型名——出现空回复时仍可定位，
  同时不落盘用户提示词内容。

## 许可证

本项目是 [deepmean/Qwen2API](https://github.com/deepmean/Qwen2API)（ISC）的重新实现，
详见 [LICENSE](LICENSE)。
