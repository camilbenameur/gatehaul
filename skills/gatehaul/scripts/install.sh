#!/usr/bin/env bash
# Installs the gatehaul CLI from a GitHub release, after checking it against the
# release's SHA256SUMS. Needs gh (logged in) and sha256sum or shasum.
#
#   install.sh             the newest release, pre-releases included
#   install.sh v0.1.0      a given release
#
# GATEHAUL_BIN_DIR sets where it goes (default: ~/.local/bin).
set -euo pipefail

REPO=camilbenameur/gatehaul
BIN_DIR=${GATEHAUL_BIN_DIR:-$HOME/.local/bin}

fail() {
  printf 'install.sh: %s\n' "$1" >&2
  if [ -n "${2:-}" ]; then
    printf 'Fix: %s\n' "$2" >&2
  fi
  exit 1
}

command -v gh >/dev/null 2>&1 || fail "gh is not installed" "install it from https://cli.github.com, then run gh auth login"

case "$(uname -s)" in
  Linux) os=linux ;;
  Darwin) os=darwin ;;
  MINGW* | MSYS* | CYGWIN*) os=windows ;;
  *) fail "unsupported system $(uname -s)" "build from source: go install github.com/$REPO/cmd/gatehaul@latest" ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) fail "unsupported architecture $(uname -m)" "build from source: go install github.com/$REPO/cmd/gatehaul@latest" ;;
esac
ext=''
if [ "$os" = windows ]; then
  ext='.exe'
fi

tag=${1:-}
if [ -z "$tag" ]; then
  # gh release download without a tag skips pre-releases: take the newest release of any kind.
  tag=$(gh release list -R "$REPO" --limit 1 --json tagName --jq '.[0].tagName') || fail "cannot list the releases of $REPO" "gh auth status"
fi
[ -n "$tag" ] || fail "no release found in $REPO" "build from source: go install github.com/$REPO/cmd/gatehaul@latest"

asset="gatehaul_${tag}_${os}_${arch}${ext}"
tmp=$(mktemp -d)
trap 'rm -rf -- "$tmp"' EXIT

gh release download "$tag" -R "$REPO" -D "$tmp" -p "$asset" -p SHA256SUMS || fail "cannot download $asset from release $tag" "gh release view $tag -R $REPO"

want=$(awk -v f="$asset" '$2 == f || $2 == "*" f { print $1 }' "$tmp/SHA256SUMS")
[ -n "$want" ] || fail "$asset is not listed in SHA256SUMS" "report it at https://github.com/$REPO/issues"
if command -v sha256sum >/dev/null 2>&1; then
  got=$(sha256sum "$tmp/$asset" | awk '{ print $1 }')
else
  got=$(shasum -a 256 "$tmp/$asset" | awk '{ print $1 }')
fi
[ "$got" = "$want" ] || fail "checksum mismatch for $asset (got $got, want $want)" "do not use this file; report it at https://github.com/$REPO/issues"

mkdir -p "$BIN_DIR"
dest="$BIN_DIR/gatehaul$ext"
cp "$tmp/$asset" "$dest.tmp"
chmod +x "$dest.tmp"
mv -f "$dest.tmp" "$dest"

printf 'installed %s (%s, sha256 %s) to %s\n' "$tag" "$os/$arch" "$got" "$dest"
case ":$PATH:" in
  *":$BIN_DIR:"*) ;;
  *) printf 'note: %s is not on PATH; call %s directly, or add the directory to PATH\n' "$BIN_DIR" "$dest" ;;
esac
"$dest" version
