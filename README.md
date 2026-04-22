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

## 当前边界

- 目前只做图片接口
- `POST /v1/chat/completions` 仍然返回 `501`
- 修改 `config.yaml` 里的 `models`、`api_keys`、`accounts` 后，需要重启 sidecar
- 当前没有实现配置热重载

## 安全提示

- 不要提交 `config.yaml`
- 不要提交真实的 ChatGPT `auth_token`
- 对外公开仓库时只保留 `config.example.yaml`

## 致谢

这个项目的思路与部分逆向实现来源于上游 `gpt2api`，这里只是把它裁成了一个更适合集成场景的轻量 sidecar。
