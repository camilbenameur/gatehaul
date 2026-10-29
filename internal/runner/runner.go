package runner

import (
	"crypto/sha256"
	_ "embed" // runner.sh is embedded in the binary
	"encoding/hex"
	"fmt"
	"strings"
)

//go:embed runner.sh
var script []byte

var scriptSum = sha256.Sum256(script)

// Script returns the runner, the bash script that runs on the host.
func Script() []byte {
	return append([]byte(nil), script...)
}

// Version identifies the runner: the first 12 hex digits of the sha256 of its
// script. The runner is installed as runner-<Version>.sh.
func Version() string {
	return hex.EncodeToString(scriptSum[:])[:12]
}

// safeRoot reports whether root can be placed between single quotes in the
// preamble: an absolute path without quote, control character or "..".
func safeRoot(root string) bool {
	if !strings.HasPrefix(root, "/") || len(root) < 2 {
		return false
	}
	for _, r := range root {
		if r == '\'' || r < 0x20 || r == 0x7f {
			return false
		}
	}
	for _, part := range strings.Split(root, "/") {
		if part == ".." {
			return false
		}
	}
	return true
}

// Preamble returns the first part of the stdin of "bash -s" (see the package
// documentation): a function and the line that calls it. An unsafe root is
// replaced by an empty one, which the preamble reports as install_failed on the
// remote side, with the usual BEGIN/END reply.
func Preamble(root string) []byte {
	if !safeRoot(root) {
		root = ""
	}
	var b strings.Builder
	b.WriteString(preambleHead)
	fmt.Fprintf(&b, "  local root='%s' ver='%s' want='%s' len=%d\n",
		root, Version(), hex.EncodeToString(scriptSum[:]), len(script))
	b.WriteString(preambleBody)
	return []byte(b.String())
}

// preambleHead only names the function: HELLO is printed as soon as the
// function runs, before anything can fail.
const preambleHead = "gh1_preamble() {\n"

// preambleBody is the rest of the function and the line that calls it. Every
// message is a constant without quote or backslash, so that it can be put in
// JSON as is.
const preambleBody = `  printf 'GATEHAUL1 HELLO %s\n' "$ver"
  gh1_fail() {
    if [ -n "$root" ] && [ -d "$root" ]; then
      printf '%s: %s\n' "$2" "$3" >"$root/failed" 2>/dev/null || true
    fi
    printf 'GATEHAUL1 BEGIN\n{"schema":1,"error":{"code":"%s","message":"%s"}}\nGATEHAUL1 END 1\n' "$1" "$3"
    exit 1
  }
  case $root in
    /?*) ;;
    *) gh1_fail install_failed root 'the runner root must be an absolute path' ;;
  esac
  case $root in
    */../* | */..) gh1_fail install_failed root 'the runner root must not contain ..' ;;
  esac
  local tool
  for tool in sha256sum mkdir mv chmod rm; do
    command -v "$tool" >/dev/null 2>&1 || gh1_fail missing_tool tools "missing tool: $tool"
  done
  umask 077
  mkdir -p -- "$root" 2>/dev/null || gh1_fail install_failed mkdir 'cannot create the runner directory'
  chmod 700 -- "$root" 2>/dev/null || true
  local probe="$root/.exec-probe.$$"
  printf '#!/bin/sh\nexit 0\n' >"$probe" 2>/dev/null || gh1_fail install_failed write 'cannot write in the runner directory'
  chmod 700 -- "$probe" 2>/dev/null || true
  if ! "$probe" >/dev/null 2>&1; then
    rm -f -- "$probe"
    gh1_fail noexec exec 'a script cannot run from the runner directory, which looks mounted noexec: remount it with exec, or recreate the host'
  fi
  rm -f -- "$probe"
  local runner="$root/runner-$ver.sh" buf= got=
  IFS= LC_ALL=C read -r -d '' -N "$len" buf || true
  if [ "${#buf}" -ne "$len" ]; then
    gh1_fail install_failed read 'the runner was cut while it was being read'
  fi
  if [ -f "$runner" ]; then
    got=$(sha256sum <"$runner") || got=
    got=${got%% *}
    if [ "$got" != "$want" ]; then
      rm -f -- "$runner"
    fi
  fi
  if [ ! -f "$runner" ]; then
    gh1_tmp="$runner.tmp.$$"
    trap 'rm -f -- "$gh1_tmp"' EXIT
    printf '%s' "$buf" >"$gh1_tmp" || gh1_fail install_failed write 'cannot write the runner'
    got=$(sha256sum <"$gh1_tmp") || gh1_fail install_failed verify 'cannot compute the checksum of the runner'
    got=${got%% *}
    if [ "$got" != "$want" ]; then
      gh1_fail install_failed verify 'the runner checksum does not match'
    fi
    chmod 700 -- "$gh1_tmp" 2>/dev/null || true
    mv -f -- "$gh1_tmp" "$runner" || gh1_fail install_failed install 'cannot install the runner'
    trap - EXIT
  fi
  rm -f -- "$root/failed" 2>/dev/null || true
  buf=
  exec env GATEHAUL_ROOT="$root" bash "$runner"
}
gh1_preamble
`
