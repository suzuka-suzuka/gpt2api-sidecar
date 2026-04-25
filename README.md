# gpt2api-sidecar

一个轻量的 Go 图片 sidecar。

它保留了 `chatgpt.com` 图片链路里最关键的逆向部分，同时对下游暴露一个小而实用的 OpenAI 兼容接口，方便像 Sakura 这样的项目直接接入。

这个仓库是刻意“瘦身”过的版本，不包含上游 `gpt2api` 里的完整 SaaS 层，例如：

- MySQL
- Redis
- 计费
- 管理后台
- 账号管理面板
- RBAC 权限系统

它只专注做一件事：

- 保留 Go 逆向层
- 提供 `/v1/images/*` 的 OpenAI 兼容接口
- 返回真实图片字节，方便下游直接发送

## 当前提供的接口

- `GET /healthz`
- `GET /v1/models`
- `POST /v1/images/generations`
- `POST /v1/images/edits`
- `GET /v1/blobs/:id`
- `POST /v1/chat/completions`
  - 当前固定返回 `501`，因为这个 sidecar 暂时只做图片能力

## 这个项目的定位

上游 `gpt2api` 仓库把三层东西放在了一起：

- ChatGPT 网页逆向客户端
- OpenAI 兼容网关
- SaaS 平台能力

但如果你只是想把图片能力接进自己的机器人、插件或本地服务，完整 SaaS 栈通常太重了。

这个 sidecar 就是把“最有复用价值”的那层单独抽出来，变成一个能陪跑在你主项目旁边的小服务。

## 保留了哪些逆向能力

当前 sidecar 仍然保留这些关键部分：

- `uTLS` 传输层和浏览器风格 TLS 指纹
- `sentinel/chat-requirements`
- PoW 流程
- `/f/conversation`
- 参考图上传
- 会话轮询
- 图片下载签名 URL 解析

## 目录结构

- `cmd/sidecar`
  - 程序入口
- `internal/server`
  - HTTP API 和 OpenAI 兼容响应层
- `internal/runner`
  - 图片流程编排
- `internal/pool`
  - 内存版账号池
- `internal/upstream/chatgpt`
  - 复制并裁剪过的逆向客户端代码
- `scripts`
  - Windows / Linux 启动与构建脚本
- `deploy/systemd`
  - Linux 的 `systemd` 示例

## 配置说明

先复制示例配置：

```bash
cp config.example.yaml config.yaml
```

必须填写的字段：

- `auth.api_keys`
  - 下游调用这个 sidecar 时使用的 API Key
- `accounts[].auth_token`
  - ChatGPT 网页侧的 `access_token`
- `accounts[].proxy_url`
  - 可选；如果你的账号必须走代理访问 `chatgpt.com`，这里要填

### 模型配置

`models` 是下游可见的模型列表。每一项都有三个字段：

- `id`
  - 下游请求里传的模型名，例如 `gpt-image-2` 或 `gpt-5-3`
- `upstream`
  - 真实发给 `chatgpt.com` `/f/conversation` 的模型 slug
- `type`
  - 当前图片接口只接受 `image`；`chat` 会被 `/v1/models` 列出，但 `/v1/chat/completions` 仍然返回 `501`

`auto` 不是 `/backend-api/models` 返回的真实模型，而是让 `chatgpt.com` 自己选择图片路由。免费账号通常建议保留一个 `upstream: auto` 的图片模型；想测试真实模型 slug 时，可以把它们按下面这样配置：

```yaml
models:
  - id: gpt-image-2
    upstream: auto
    type: image
  - id: gpt-5-3
    upstream: gpt-5-3
    type: image
```

我在 2026-04-25 用当前两个账号请求 `https://chatgpt.com/backend-api/models`，返回的真实 slug 是：

| slug | title | max_tokens |
|---|---|---:|
| `gpt-5-3` | GPT-5.3 | 52815 |
| `gpt-5-3-instant` | GPT-5.3 Instant | 52815 |
| `gpt-5-4-thinking` | GPT-5.4 Thinking | 262144 |
| `gpt-5-5-thinking` | GPT-5.5 Thinking | 262144 |
| `gpt-5-2` | GPT-5.2 | 43815 |
| `gpt-5-2-instant` | GPT-5.2 Instant | 43815 |
| `gpt-5-2-thinking` | GPT-5.2 Thinking | 262144 |
| `gpt-5-1` | GPT-5.1 | 35815 |
| `gpt-5` | GPT-5 | 34815 |
| `gpt-5-mini` | GPT-5-mini | 32767 |
| `gpt-5-3-mini` | GPT-5.3 Mini | 52815 |
| `gpt-5-4-t-mini` | GPT-5.4 Thinking Mini | 262144 |
| `o3` | o3 | 196608 |
| `research` | Deep Research | 34815 |
| `agent-mode` | Agent | 262144 |

这些 slug 已经写进 `config.example.yaml`。它们能否稳定出图取决于账号权限和上游路由；`gpt-image-2`/`auto` 是最保守的默认项，`gpt-5-3` 是当前账号返回的默认真实模型。

Queue settings:

- `server.queue_wait_timeout`
  - Maximum time a request may wait in the FIFO image queue before returning `503 queue_timeout`.
- `server.max_queue_size`
  - Maximum number of waiting requests. Additional requests return `503 queue_full`.
- `server.acquire_timeout`
  - 单个出图子任务等待可用账号的最长时间。
- `server.request_timeout`
  - 单个出图子任务拿到账号后等待上游返回图片的最长时间；示例值为 `4m`，适合约 2 分钟出图再留一段余量。

The sidecar sizes image worker slots from the number of loaded accounts. Requests above that capacity wait in a FIFO queue before entering the account pool.

图片请求策略：

- `n > 1` 时会拆成 `n` 个并发出图任务，每个任务独立抢一个账号并跑完整 ChatGPT 图片链路。
- 任意并发任务先返回的图片会先被聚合；凑够 `n` 张就立即返回，并取消剩余任务。
- 每个子任务拿到账号后最多等待 `server.request_timeout`；示例配置为 `4m`，超时的子任务会记为 `image_timeout`。
- 如果所有子任务都失败或超时，接口会返回错误；如果已经拿到部分图片但没凑够 `n` 张，会返回已拿到的图片，优先保证速度。

如果 `device_id` 和 `session_id` 为空，sidecar 首次启动时会自动生成，并回写到 `config.yaml`，用来保持账号指纹稳定。

## 下载安装后怎么启动

### Windows

如果你已经把仓库下载下来了，最直接就是：

```powershell
Copy-Item .\config.example.yaml .\config.yaml
```

然后编辑 `config.yaml`，至少填好：

- `accounts[0].auth_token`
- 如果需要代理，再填 `accounts[0].proxy_url`

接着启动：

```powershell
.\scripts\run.ps1
```

`run.ps1` 会自动做这些事：

- 如果没有 `config.yaml`，就从 `config.example.yaml` 复制一份
- 如果 `auth_token` 还是空的，就直接提示你，不会盲启动
- 如果本机没装 Go，就下载便携 Go 到 `.tools\go`
- 自动执行 `go mod tidy`
- 启动 sidecar

如果你想先编译再运行：

```powershell
.\scripts\build.ps1
.\bin\gpt2api-sidecar.exe -config .\config.yaml
```

### Linux

完整说明见 [LINUX.md](./LINUX.md)。

最短启动方式：

```bash
chmod +x ./scripts/run.sh ./scripts/build.sh
./scripts/run.sh
```

如果你想先编译：

```bash
./scripts/build.sh
./bin/gpt2api-sidecar -config ./config.yaml
```

## 默认监听地址

```text
http://127.0.0.1:46321
```

编译后的 Linux 二进制默认输出到：

```text
./bin/gpt2api-sidecar
```

## Sakura 接入示例

把 Sakura 的图片配置指向这个 sidecar：

```yaml
provider: openai_compat
model: gpt-image-2
api: sakura-sidecar-key
baseURL: http://127.0.0.1:46321/v1
```

如果 Sakura 和 sidecar 不在同一台机器上，把 `127.0.0.1` 改成 sidecar 所在机器的 IP 或域名，同时修改：

- `server.listen`
- `server.public_base_url`

## 健康检查

```bash
curl http://127.0.0.1:46321/healthz
curl http://127.0.0.1:46321/v1/models \
  -H "Authorization: Bearer sakura-sidecar-key"
```

`/healthz` now includes `queue.limit`, `queue.active`, and `queue.pending` so you can monitor current image queue pressure.

## 当前边界

- 目前只做图片接口
- `POST /v1/chat/completions` 仍然返回 `501`
- 单个出图子任务拿到账号后最多等待 `server.request_timeout`；示例配置为 `4m`，追求速度时建议用更大的账号池承接 `n > 1` 的并发拆分
- 修改 `config.yaml` 里的 `models`、`api_keys`、`accounts` 后，需要重启 sidecar
- 当前没有实现配置热重载

## 安全提示

- 不要提交 `config.yaml`
- 不要提交真实的 ChatGPT `auth_token`
- 对外公开仓库时只保留 `config.example.yaml`

## 致谢

这个项目的思路与部分逆向实现来源于上游 `gpt2api`，这里只是把它裁成了一个更适合集成场景的轻量 sidecar。
