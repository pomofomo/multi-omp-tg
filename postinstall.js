#!/usr/bin/env node
// Picks the right prebuilt trd binary for this platform and chmod +x it.
// If the binary isn't in the npm tarball (dev install), fall back to `go build`.
"use strict";

const fs = require("node:fs");
const path = require("node:path");
const { spawnSync } = require("node:child_process");

const platform = process.platform; // "linux", "darwin", "win32"
const arch = process.arch; // "x64", "arm64", ...

const archMap = { x64: "amd64", arm64: "arm64" };
const osMap = { linux: "linux", darwin: "darwin" };

if (!osMap[platform] || !archMap[arch]) {
  console.error(`[trd] unsupported platform ${platform}/${arch}. Build from source: go build ./cmd/trd`);
  process.exit(0); // don't fail npm install — user can still build from source
}

const targetName = `trd-${osMap[platform]}-${archMap[arch]}`;
const root = __dirname;
const source = path.join(root, "bin", targetName);
const dest = path.join(root, "bin", "trd");

function chmodx(p) {
  try { fs.chmodSync(p, 0o755); } catch (e) { /* ignore */ }
}

if (fs.existsSync(source)) {
  try {
    if (fs.existsSync(dest)) fs.unlinkSync(dest);
    fs.copyFileSync(source, dest);
    chmodx(dest);
    console.log(`[trd] installed ${targetName} -> bin/trd`);
    process.exit(0);
  } catch (e) {
    console.error(`[trd] copy failed:`, e.message);
  }
}

// Fallback: build from source if go is available and cmd/trd exists.
// trd requires cgo (sherpa-onnx, libopus) — set CGO_ENABLED=1 explicitly
// so cross-compile-style envs don't silently produce a broken binary.
const cmdDir = path.join(root, "cmd", "trd");
if (fs.existsSync(cmdDir)) {
  const go = spawnSync("go", ["build", "-o", dest, "./cmd/trd"], {
    cwd: root,
    stdio: "inherit",
    env: { ...process.env, CGO_ENABLED: "1" },
  });
  if (go.status === 0) {
    chmodx(dest);
    console.log("[trd] built from source -> bin/trd");
    process.exit(0);
  }
  console.error("[trd] go build failed; you can build manually with: CGO_ENABLED=1 go build -o bin/trd ./cmd/trd");
} else {
  console.error(`[trd] no prebuilt binary for ${platform}/${arch}, and no source tree found.`);
}
