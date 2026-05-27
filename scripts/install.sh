#!/usr/bin/env bash
# trd prerequisite check. Detects OS, tells the user how to install
# anything missing, and does not try to be clever with sudo.
#
# Supports: Linux (Debian/Ubuntu, Fedora/RHEL, Arch), macOS (Homebrew), WSL.
# Windows (non-WSL) is intentionally unsupported.
set -euo pipefail

say()  { printf '%s\n' "$*"; }
warn() { printf '\033[33m[warn]\033[0m %s\n' "$*" >&2; }
fail() { printf '\033[31m[fail]\033[0m %s\n' "$*" >&2; exit 1; }
ok()   { printf '\033[32m[ok]\033[0m   %s\n' "$*"; }

detect_os() {
  if grep -qi microsoft /proc/version 2>/dev/null; then echo "wsl"; return; fi
  case "$(uname -s)" in
    Linux)  echo "linux"  ;;
    Darwin) echo "darwin" ;;
    *)      echo "unknown" ;;
  esac
}

detect_linux_family() {
  if command -v apt-get >/dev/null; then echo "debian"; return; fi
  if command -v dnf     >/dev/null; then echo "fedora"; return; fi
  if command -v pacman  >/dev/null; then echo "arch";   return; fi
  echo "unknown"
}

OS=$(detect_os)
[[ "$OS" == "unknown" ]] && fail "unsupported OS $(uname -s). WSL, Linux, and macOS only."

FAMILY=""
if [[ "$OS" == "linux" || "$OS" == "wsl" ]]; then
  FAMILY=$(detect_linux_family)
fi

say "OS: $OS ${FAMILY:+(family: $FAMILY)}"
say

hint_install() {
  local pkg="$1"
  case "$OS" in
    darwin) echo "  brew install $pkg" ;;
    linux|wsl)
      case "$FAMILY" in
        debian) echo "  sudo apt-get update && sudo apt-get install -y $pkg" ;;
        fedora) echo "  sudo dnf install -y $pkg" ;;
        arch)   echo "  sudo pacman -S --needed $pkg" ;;
        *)      echo "  (install $pkg via your package manager)" ;;
      esac
      ;;
  esac
}

# Resolve the right package name for libopus dev headers (varies by distro).
opus_dev_package() {
  case "$OS" in
    darwin) echo "opus opusfile" ;;
    linux|wsl)
      case "$FAMILY" in
        debian) echo "libopus-dev libopusfile-dev" ;;
        fedora) echo "opus-devel opusfile-devel" ;;
        arch)   echo "opus opusfile" ;;
        *)      echo "opus opusfile (-dev/-devel as appropriate)" ;;
      esac
      ;;
  esac
}

check_bin() {
  local name="$1" pkg="${2:-$1}"
  if command -v "$name" >/dev/null; then
    ok "$name: $(command -v "$name")"
    return 0
  fi
  warn "$name is missing"
  say  "    install with:"
  hint_install "$pkg"
  return 1
}

# Check that pkg-config can find a library (.pc file). Returns 0 if found.
check_pkg() {
  local pc="$1" hint="$2"
  if ! command -v pkg-config >/dev/null; then
    warn "pkg-config is missing — needed to detect $pc"
    say  "    install with:"
    hint_install "pkg-config"
    return 1
  fi
  if pkg-config --exists "$pc" 2>/dev/null; then
    ok "$pc: $(pkg-config --modversion "$pc")"
    return 0
  fi
  warn "$pc development headers are missing"
  say  "    install with:"
  hint_install "$hint"
  return 1
}

missing=0

check_bin git || missing=$((missing+1))

# Go is required — every make target builds the dispatcher from source.
if command -v go >/dev/null; then
  ok "go: $(go version | awk '{print $3}')"
else
  warn "go is missing (required to build trd)"
  say  "    install:"
  say  "      https://go.dev/dl/   (Go 1.22+)"
  missing=$((missing+1))
fi

# CGo build dependencies for libopus (audio codec).
check_pkg opus     "$(opus_dev_package)" || missing=$((missing+1))
check_pkg opusfile "$(opus_dev_package)" || missing=$((missing+1))

# tmux is recommended for `make setup` (keeps the dispatcher alive across
# an SSH disconnect). Without it you'll need your own process supervisor.
if command -v tmux >/dev/null; then
  ok "tmux: $(command -v tmux)  (used by 'make setup' to keep trd alive)"
else
  say "[info] tmux not found. Optional — without it, run 'trd start' under systemd or another supervisor."
fi

# unzip is required by both the bun and omp installers.
check_bin unzip || missing=$((missing+1))

# bun is required by omp (the install script and runtime both need it).
if command -v bun >/dev/null; then
  ok "bun: $(bun --version) ($(command -v bun))"
else
  warn "bun is missing (required by omp)"
  say  "    install:"
  say  "      curl -fsSL https://bun.sh/install | bash"
  say  "    see https://bun.sh"
  missing=$((missing+1))
fi

# omp (the headless agent). The dispatcher spawns `omp -p` per message.
if command -v omp >/dev/null; then
  ok "omp: $(command -v omp)"
else
  warn "omp (oh-my-pi agent) is missing"
  say  "    install:"
  say  "      curl -fsSL https://omp.sh/install | sh"
  say  "    see https://github.com/oh-my-pi/oh-my-pi"
  missing=$((missing+1))
fi

# SSH key for private repo access (optional but very common).
if [[ -f "$HOME/.ssh/id_ed25519" || -f "$HOME/.ssh/id_rsa" || -f "$HOME/.ssh/id_ecdsa" ]]; then
  ok "ssh key present in ~/.ssh/"
else
  warn "no SSH private key found in ~/.ssh/"
  say  "    generate with:"
  say  "      ssh-keygen -t ed25519 -C \"$(whoami)@$(hostname)\""
  say  "    then add the .pub to GitHub / your git host."
  say  "    (not counted as missing — public repos work fine without it)"
fi

# Warn (don't fail) if ~/.local/bin isn't on PATH — `make install` puts
# the binary there.
case ":$PATH:" in
  *":$HOME/.local/bin:"*) ok "PATH includes \$HOME/.local/bin" ;;
  *) warn "\$HOME/.local/bin is not on \$PATH — 'trd' won't be found after install"
     say  "    add to your shell rc:"
     say  "      export PATH=\"\$HOME/.local/bin:\$PATH\""
     ;;
esac

say
if (( missing > 0 )); then
  warn "$missing prerequisite(s) missing. Install them and re-run this script."
  exit 1
fi

ok "all prerequisites satisfied"
say
say "Next steps:"
say "  1. (optional) make install-models    # ~230MB of whisper + TTS models"
say "  2. make setup TELEGRAM_BOT_TOKEN=<your-token>"
