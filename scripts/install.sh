#!/usr/bin/env bash
# trd prerequisite check. Detects OS, tells the user how to install anything
# missing, and does not try to be clever with sudo.
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

missing=0

check_bin git  || missing=$((missing+1))
# tmux is optional in the headless-omp port — only used by `make setup`
# to keep the dispatcher running across an SSH disconnect. Recommend it
# but don't fail without it.
if command -v tmux >/dev/null; then
  ok "tmux: $(command -v tmux) (optional, used by make setup)"
else
  say "[info] tmux not found. Optional — without it, run 'trd start' under a process supervisor of your choice."
fi

# omp (the headless agent). The dispatcher spawns `omp -p` per message.
if command -v omp >/dev/null; then
  ok "omp: $(command -v omp)"
else
  warn "omp (oh-my-pi agent) is missing"
  say  "    install with:"
  say  "      npm install -g @oh-my-pi/pi-coding-agent"
  say  "    or see https://github.com/oh-my-pi/oh-my-pi"
  missing=$((missing+1))
fi

# SSH key for private repo access.
if [[ -f "$HOME/.ssh/id_ed25519" || -f "$HOME/.ssh/id_rsa" || -f "$HOME/.ssh/id_ecdsa" ]]; then
  ok "ssh key present in ~/.ssh/"
else
  warn "no SSH private key found in ~/.ssh/"
  say  "    generate one with:"
  say  "      ssh-keygen -t ed25519 -C \"$(whoami)@$(hostname)\""
  say  "    then add the .pub to GitHub / your git host."
  missing=$((missing+1))
fi

# Go (only needed to build trd from source; optional for end users).
if command -v go >/dev/null; then
  ok "go: $(go version | awk '{print $3}')"
else
  say "[info] go not found. That's fine — you only need it to build trd from source."
fi

say
if (( missing > 0 )); then
  warn "$missing prerequisite(s) missing. Install them and re-run this script."
  exit 1
fi

ok "all prerequisites satisfied"
say
say "Next: build trd (if developing from source)"
say "  make build     # or: go build -o bin/trd ./cmd/trd"
say
say "Or install via npm (when published):"
say "  npm install -g telegram-repo-dispatcher"
