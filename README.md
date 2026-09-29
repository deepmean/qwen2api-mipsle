# qwen2api（MIPS32 移植版）

把 [Qwen](https://chat.qwen.ai) 网页版封装成 **OpenAI 兼容 API** 的极简网关，
交叉编译为单一静态二进制，运行在 **MIPS32 小端 OpenWrt 路由器**上
（MediaTek MT7621 / `ramips`，实测机型 JCG-Q20）。

上游 [deepmean/Qwen2API](https://github.com/deepmean/Qwen2API) 是 Bun/Node 应用。
Bun 没有 MIPS32 后端，OpenWrt 24.10 也没有 `mipsel_24kc` 的 `node` 包，
因此核心对话链路用 **Go 纯标准库重写**，交叉编译为静态二进制。

## 接口范围

| 接口 | 说明 |
| --- | --- |
| `GET /v1/models` | 实时拉取 Qwen 模型目录；拉取失败时回退到静态列表 |
| `POST /v1/chat/completions` | 支持 `stream: true` 与 `stream: false` |
| `GET /health` | 运行时长、账号数量、各账号登录状态 |

为控制内存占用，以下**未移植**：Anthropic 兼容层（`/v1/messages`）、
工具调用协议、图片/视频生成、Web 管理面板。

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
- **`feature_config.output_schema: "phase"`** —— 关键字段。缺失则上游不再返回
  `delta.phase`，所有增量帧被丢弃。
- **两通道输出** —— 推理内容进 `reasoning_content`，正文进 `content`，与上游默认
  一致（`legacyReasoningInContent = false`）。
- **单消息扁平化** —— 网页接口只接受一条当前消息，多轮对话折叠为 JSONL 历史块
  加最后一轮。
- **模型名规范化**（`model.go`）—— 剥离客户端附加的 `[1M]` 等后缀与
  `-thinking`/`-search` 行为后缀，并用实时目录把别名映射为规范 ID。

## 构建

```sh
export GOOS=linux GOARCH=mipsle GOMIPS=softfloat CGO_ENABLED=0
go build -trimpath -ldflags "-s -w" -o dist/qwen2api-linux-mipsle .
```

`GOMIPS=softfloat` 是**必须的**：MT7621 无 FPU，硬浮点构建会在运行时崩溃。
产物约 6 MB，静态链接，仅用 Go 标准库，无需下载任何模块。

```sh
go test ./...   # 含 Cookie 编码器与上游 JS 的交叉校验
```

## 部署（OpenWrt）

| 路径 | 内容 |
| --- | --- |
| `/opt/qwen2api/qwen2api` | 可执行二进制 |
| `/opt/qwen2api/qwen2api.conf` | `API_KEY`、`SERVICE_PORT` 等（权限 600） |
| `/opt/qwen2api/accounts.txt` | `email:password` 行（权限 600） |
| `/etc/init.d/qwen2api` | procd 服务脚本 |

```sh
scp dist/qwen2api-linux-mipsle root@ROUTER:/opt/qwen2api/qwen2api
scp deploy/init.d-qwen2api       root@ROUTER:/etc/init.d/qwen2api
chmod +x /opt/qwen2api/qwen2api /etc/init.d/qwen2api
/etc/init.d/qwen2api enable
/etc/init.d/qwen2api start
logread -e qwen2api
```

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
| `main.go` | HTTP 服务器、路由、鉴权、流式转发、外置调度 |
| `attach.go` | 上下文外置编排：会话切分、前缀缓存、复用匹配、熔断、压缩兜底 |
| `upload.go` | STS Token → OSS 上传 → parse → 轮询；`message.files` 描述符 |
| `context.go` | 有损压缩（附件不可用时的兜底） |
| `qwen.go` | 上游客户端：登录、建会话、补全、模型目录、浏览器头构造 |
| `sse.go` | SSE 解码 + delta 规范化（含 `thinking_summary` 通道） |
| `ssxmod.go` | SSXMOD Cookie 生成（LZW + 自定义 Base64 + 指纹） |
| `model.go` | 模型名规范化与别名解析 |
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
