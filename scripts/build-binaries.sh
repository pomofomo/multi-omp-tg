#!/usr/bin/env bash
# Build trd for the current host.
#
# Cross-compile is intentionally NOT attempted here. The build depends on
# cgo (sherpa-onnx for whisper/TTS, libopus for the audio codec), and
# producing portable binaries needs a target-specific toolchain plus the
# matching shared libraries on the target host. Build on each deployment
# host instead.
set -euo pipefail

cd "$(dirname "$0")/.."
mkdir -p bin

echo "building bin/trd for $(go env GOOS)/$(go env GOARCH)"
CGO_ENABLED=1 go build -trimpath -ldflags="-s -w" -o bin/trd ./cmd/trd

echo "done:"
ls -lh bin/trd
