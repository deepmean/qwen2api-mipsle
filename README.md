# qwen2api (mipsle port)

A minimal [Qwen](https://chat.qwen.ai) web → OpenAI-compatible gateway, ported to
run on **MIPS32 little-endian OpenWrt routers** (MediaTek MT7621 / `ramips`).

Upstream [deepmean/Qwen2API](https://github.com/deepmean/Qwen2API) is a Bun/Node
application. Neither Bun nor Node has a MIPS32 backend, and OpenWrt 24.10 carries
no `node` package for `mipsel_24kc`, so the core chat path was **reimplemented in
Go** and cross-compiled as a single static binary.

## Scope

Implemented:

| Endpoint | Notes |
| --- | --- |
| `GET /v1/models` | Live catalogue from Qwen, static fallback if the fetch fails |
| `POST /v1/chat/completions` | `stream: true` and `stream: false` |
| `GET /health` | Uptime, account count, per-account sign-in state |

Deliberately **not** ported, to keep the resident set small: the Anthropic
compatibility layer (`/v1/messages`), the tool-calling protocol, image/video
generation, and the web admin panel.

## Long-context attachments

Qwen's edge (Aliyun WAF) rejects a request body at roughly 128 KiB with a
`FAIL_SYS_USER_VALIDATE` challenge under HTTP 200. An agent client like Claude
Code resends its whole conversation every turn (system prompt + tool schemas +
history ≈ 950 KB), so it crosses that line within a few exchanges.

Rather than dropping history, the port externalizes it exactly as upstream does:
once the body exceeds the threshold, the conversation history is uploaded to
Qwen as a document and referenced from `message.files`. The model reads the
attachment, so nothing is lost.

```
POST /api/v1/files/getstsToken   -> temporary Aliyun OSS credentials + object key
PUT  https://<bucket>.<endpoint>/<key>   (OSS Signature-V1, HMAC-SHA1)
POST /api/v2/files/parse         -> ask Qwen to parse the document
POST /api/v2/files/parse/status  -> poll until parsed
```

Only the parse calls are rate-limited by the WAF (counted per egress IP), so the
uploaded history is cached per session (`attach.go`) and reused: when a later
turn's history still starts with the lines that were baked into the cached
document, only the new tail is sent inline and no upload happens. One parse
covers many turns.

Compaction (`context.go`) remains as the fallback for when an upload is
unavailable, and a breaker pauses uploads for 5 minutes after the WAF repeatedly
challenges the parse call — backing off, not retrying into the challenge.

Verified on device: a 2 MB / 264-message Claude Code-shaped request is
externalized (90 KB inline, the rest attached) and the model recalls a token
buried at the start of the history; subsequent turns reuse the same attachment.

## What the port preserves

The parts of upstream that actually decide whether a request succeeds were kept
faithfully, because Qwen's WAF fingerprints the client:

- **`ssxmod_itna` / `ssxmod_itna2` cookies** — the LZW + custom-base64 encoding of
  a 37-field browser fingerprint (`ssxmod.go`). Cross-checked byte-for-byte
  against the upstream JavaScript; see `ssxmod_test.go`.
- **Per-account browser identity** — device id, screen, locale, WebGL renderer and
  Chrome version are derived deterministically from a SHA-256 of the account
  email, so one account always presents one coherent identity.
- **Header profile** — `user-agent`, `sec-ch-ua`, `sec-ch-ua-platform`,
  `accept-language`, `sec-fetch-*`, `timezone`, `source: web`, `version: 0.2.81`.
- **`feature_config.output_schema: "phase"`** — load-bearing. Without it the
  upstream stops emitting `delta.phase` and every incremental frame is discarded.
- **Two-phase output** — reasoning goes to `reasoning_content`, the answer to
  `content`, matching upstream's default (`legacyReasoningInContent = false`).
- **Single-message flattening** — the web endpoint accepts exactly one current
  message, so multi-turn conversations are collapsed into a JSONL history block
  plus the final turn.

## Build

```sh
export GOOS=linux GOARCH=mipsle GOMIPS=softfloat CGO_ENABLED=0
go build -trimpath -ldflags "-s -w" -o dist/qwen2api-linux-mipsle .
```

`GOMIPS=softfloat` is required: MT7621 has no FPU, and a hard-float build crashes
at runtime. The result is a ~6MB statically linked binary. Only the Go standard
library is used, so no module downloads are needed.

```sh
go test ./...   # includes the upstream cross-validation for the cookie encoder
```

## Deploy (OpenWrt)

| Path | Contents |
| --- | --- |
| `/opt/qwen2api/qwen2api` | the binary |
| `/opt/qwen2api/qwen2api.conf` | `API_KEY`, `SERVICE_PORT` (mode 600) |
| `/opt/qwen2api/accounts.txt` | `email:password` lines (mode 600) |
| `/etc/init.d/qwen2api` | procd service |

```sh
scp dist/qwen2api-linux-mipsle root@ROUTER:/opt/qwen2api/qwen2api
scp deploy/init.d-qwen2api       root@ROUTER:/etc/init.d/qwen2api
chmod +x /opt/qwen2api/qwen2api /etc/init.d/qwen2api
/etc/init.d/qwen2api enable
/etc/init.d/qwen2api start
logread -e qwen2api
```

Then fill in credentials and restart:

```sh
vi /opt/qwen2api/accounts.txt     # email:password
/etc/init.d/qwen2api restart
```

To survive firmware upgrades, add the deployment paths to `/etc/sysupgrade.conf`:

```
/opt/qwen2api/
/etc/init.d/qwen2api
/etc/rc.d/S96qwen2api
/etc/rc.d/K10qwen2api
```

## Use

```sh
curl http://192.168.26.1:3000/v1/models

curl http://192.168.26.1:3000/v1/chat/completions \
  -H 'Authorization: Bearer sk-your-key' \
  -H 'Content-Type: application/json' \
  -d '{"model":"qwen3.6-plus","stream":true,
       "messages":[{"role":"user","content":"hello"}]}'
```

## Configuration

| Env | Default | Meaning |
| --- | --- | --- |
| `SERVICE_PORT` | `3000` | Listen port |
| `API_KEY` | *(unset)* | Comma-separated bearer tokens. Unset leaves the port open — a warning is logged at startup |
| `QWEN_CHAT_PROXY_URL` | `https://chat.qwen.ai` | Upstream origin |
| `ACCOUNTS_FILE` | `/opt/qwen2api/accounts.txt` | Credentials file |
| `MAX_CONCURRENT` | `8` | In-flight chat requests |
| `AGENT_CONTEXT_PREFIX_TTL_SECONDS` | `1800` | How long an uploaded history attachment may be reused |
| `AGENT_CONTEXT_PREFIX_MAX_ENTRIES` | `8` | Sessions whose attachments are cached |
| `LOG_LEVEL` | `INFO` | `DEBUG`/`INFO`/`WARN`/`ERROR` |

## Notes

- The router must be able to reach `chat.qwen.ai`. A mainland-China egress is
  ideal — the WAF treats it as a normal client. If the egress is elsewhere the
  fingerprint (`zh-CN`, `Asia/Shanghai`) will look inconsistent and requests may
  be challenged.
- Only `gzip` is advertised in `Accept-Encoding`; the Go standard library cannot
  decode `br`/`zstd`, and asking for them would produce undecodable bodies.
- Tokens are signed in lazily and re-signed 5 minutes before expiry. A `401`/`403`
  from upstream invalidates the token so the next request re-authenticates.
- Request bodies are never logged; only the byte size and the resolved model name,
  so an empty reply can still be traced without recording the user's prompts.
