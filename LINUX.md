# Linux Usage

This sidecar can run on Linux directly. The simplest flow is:

```bash
cd /opt/gpt2api-sidecar
cp config.example.yaml config.yaml
```

Then edit `config.yaml`:

- Fill `accounts[0].auth_token`
- Fill `proxy_url` if your account must access `chatgpt.com` through a proxy
- If Sakura is on another machine, change:
  - `server.listen` to `0.0.0.0:46321`
  - `server.public_base_url` to your real public or LAN URL

## Start

```bash
chmod +x ./scripts/run.sh ./scripts/build.sh
./scripts/run.sh
```

What `run.sh` does:

- Creates `config.yaml` from `config.example.yaml` if missing
- Refuses to start if `auth_token` is still empty
- Uses system `go` if present
- Otherwise downloads portable Go into `.tools/go`
- Runs `go mod tidy`
- Starts the sidecar with `go run`

Default listen address:

```text
http://127.0.0.1:46321
```

## Build Binary

```bash
chmod +x ./scripts/build.sh
./scripts/build.sh
./bin/gpt2api-sidecar -config ./config.yaml
```

The build script outputs a Linux binary at:

```text
./bin/gpt2api-sidecar
```

## Sakura Config

Point Sakura image config to:

```yaml
provider: openai_compat
model: gpt-image-2
api: sakura-sidecar-key
baseURL: http://127.0.0.1:46321/v1
```

If Sakura is not on the same machine, replace `127.0.0.1` with the Linux host IP or domain.

## Health Check

```bash
curl http://127.0.0.1:46321/healthz
curl http://127.0.0.1:46321/v1/models \
  -H 'Authorization: Bearer sakura-sidecar-key'
```

## Notes

- This lightweight sidecar is image-only for now.
- `POST /v1/chat/completions` still returns `501`.
- If you change `config.yaml` fields like `models`, `api_keys`, or `accounts`, restart the sidecar. It does not hot reload config.
