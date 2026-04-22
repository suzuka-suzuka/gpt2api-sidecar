# gpt2api-sidecar

A lightweight Go image sidecar that keeps the reverse-engineered `chatgpt.com` image pipeline, while exposing a small OpenAI-compatible API for local projects like Sakura.

This repository is intentionally small. It does not include the full SaaS layers from the upstream `gpt2api` project such as MySQL, Redis, billing, admin UI, account management, or RBAC. It focuses on one job:

- keep the Go reverse layer
- expose `/v1/images/*` in an OpenAI-compatible shape
- return real image bytes that downstream apps can send directly

## What It Provides

- `GET /healthz`
- `GET /v1/models`
- `POST /v1/images/generations`
- `POST /v1/images/edits`
- `GET /v1/blobs/:id`
- `POST /v1/chat/completions`
  - currently returns `501` because this sidecar is image-only for now

## Why This Exists

The upstream `gpt2api` project contains several layers mixed together:

- reverse-engineered ChatGPT upstream client
- OpenAI-compatible gateway
- SaaS platform features

For integration into existing bots or plugins, that full stack is often unnecessary. This sidecar extracts the practical core for image generation and image editing, while staying easy to deploy beside another application.

## Reverse Layer Kept Here

The sidecar still uses the important parts of the upstream reverse stack:

- `uTLS` transport and browser-like TLS fingerprinting
- `sentinel/chat-requirements`
- PoW token flow
- `/f/conversation`
- reference image upload
- conversation polling
- signed image download resolution

## Repository Layout

- `cmd/sidecar`
  - executable entrypoint
- `internal/server`
  - HTTP API and OpenAI-compatible response layer
- `internal/runner`
  - image workflow orchestration
- `internal/pool`
  - in-memory account pool
- `internal/upstream/chatgpt`
  - copied and adapted reverse client pieces
- `scripts`
  - helper scripts for Windows and Linux
- `deploy/systemd`
  - example service file for Linux

## Configuration

Copy the example file first:

```bash
cp config.example.yaml config.yaml
```

Required fields:

- `auth.api_keys`
  - API keys that downstream clients use to call this sidecar
- `accounts[].auth_token`
  - ChatGPT web access token used by the reverse upstream client
- `accounts[].proxy_url`
  - optional, but required if your account must reach `chatgpt.com` through a proxy

If `device_id` and `session_id` are empty on first start, the sidecar generates them and persists them back to `config.yaml` to keep account fingerprinting stable.

## Quick Start

### Windows

```powershell
Copy-Item .\config.example.yaml .\config.yaml
.\scripts\run.ps1
```

What `run.ps1` does:

- creates `config.yaml` if missing
- refuses to start if `auth_token` is empty
- downloads portable Go into `.tools\go` if `go` is not installed
- runs `go mod tidy`
- starts the sidecar

### Linux

See [LINUX.md](./LINUX.md) for the full Linux guide.

Quick version:

```bash
chmod +x ./scripts/run.sh ./scripts/build.sh
./scripts/run.sh
```

## Build

### Windows

```powershell
.\scripts\build.ps1
```

### Linux

```bash
./scripts/build.sh
```

The compiled binary is written to:

```text
./bin/gpt2api-sidecar
```

## Example Sakura Integration

Point the Sakura image config to this sidecar:

```yaml
provider: openai_compat
model: gpt-image-2
api: sakura-sidecar-key
baseURL: http://127.0.0.1:46321/v1
```

If Sakura runs on a different machine, replace `127.0.0.1` with your sidecar host IP or domain, and update:

- `server.listen`
- `server.public_base_url`

## Health Checks

```bash
curl http://127.0.0.1:46321/healthz
curl http://127.0.0.1:46321/v1/models \
  -H "Authorization: Bearer sakura-sidecar-key"
```

## Notes and Limits

- this sidecar is image-only for now
- `POST /v1/chat/completions` still returns `501`
- changing `config.yaml` values such as `models`, `api_keys`, or `accounts` requires a restart
- config hot reload is not implemented

## Security Notes

- do not commit `config.yaml`
- do not commit real ChatGPT `auth_token` values
- use `config.example.yaml` as the public template

## Credits

This project is derived from the reverse and gateway ideas in the upstream `gpt2api` project, but intentionally stripped down into a focused sidecar for integration use cases.
