#!/usr/bin/env bash
set -euo pipefail

GO_VERSION="${GO_VERSION:-1.25.4}"
ROOT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)"
TOOLS_DIR="$ROOT_DIR/.tools"
GO_DIR="$TOOLS_DIR/go"
GO_BIN="$GO_DIR/bin/go"

detect_arch() {
  case "$(uname -m)" in
    x86_64|amd64)
      echo "amd64"
      ;;
    aarch64|arm64)
      echo "arm64"
      ;;
    *)
      echo "Unsupported architecture: $(uname -m)" >&2
      exit 1
      ;;
  esac
}

download_file() {
  local url="$1"
  local output="$2"

  if command -v curl >/dev/null 2>&1; then
    curl -fL "$url" -o "$output"
    return
  fi

  if command -v wget >/dev/null 2>&1; then
    wget -O "$output" "$url"
    return
  fi

  echo "Missing downloader: install curl or wget, or install Go manually." >&2
  exit 1
}

setup_go() {
  if command -v go >/dev/null 2>&1; then
    GO_CMD="$(command -v go)"
    return
  fi

  local go_arch
  go_arch="$(detect_arch)"
  local archive="$TOOLS_DIR/go${GO_VERSION}.linux-${go_arch}.tar.gz"
  local url="https://go.dev/dl/go${GO_VERSION}.linux-${go_arch}.tar.gz"

  mkdir -p "$TOOLS_DIR"

  if [ ! -x "$GO_BIN" ]; then
    if [ ! -f "$archive" ]; then
      echo "Downloading Go ${GO_VERSION} for linux/${go_arch}..."
      download_file "$url" "$archive"
    fi

    rm -rf "$GO_DIR"
    echo "Extracting Go ${GO_VERSION}..."
    tar -C "$TOOLS_DIR" -xzf "$archive"
  fi

  GO_CMD="$GO_BIN"
  export GOROOT="$GO_DIR"
  export PATH="$GO_DIR/bin:$PATH"
}

main() {
  local target_arch="${GOARCH:-$(detect_arch)}"

  cd "$ROOT_DIR"
  setup_go

  mkdir -p "$ROOT_DIR/bin"

  "$GO_CMD" version
  "$GO_CMD" mod tidy
  GOOS=linux GOARCH="$target_arch" CGO_ENABLED=0 \
    "$GO_CMD" build -o "$ROOT_DIR/bin/gpt2api-sidecar" ./cmd/sidecar
}

main "$@"
