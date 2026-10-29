#!/usr/bin/env bash
# Trap handlers (on_exit and the job's cleanup) are invoked indirectly.
# shellcheck disable=SC2317
# gatehaul runner: the remote half of gatehaul (docs/SPEC.md section 5).
#
# The controller sends this script on the stdin of "bash -s", after a short
# preamble that installs it under $GATEHAUL_ROOT and runs it. It then reads one
# request from stdin (wire format: internal/runner/protocol.go) and answers on
# its original stdout with
#
#   GATEHAUL1 BEGIN
#   <one line of JSON>
#   GATEHAUL1 END <rc>
#
# Everything else it prints goes to stderr. The hidden mode "__job <id>" is the
# detached job that submit starts (docs/SPEC.md 5.2).
#
# ASCII only, LF line endings. No eval: free text is only ever decoded into a
# file or kept in a variable that is passed as one argument.
set -euo pipefail

ORIG_LC_ALL_SET=0
ORIG_LC_ALL=
if [ "${LC_ALL+x}" = x ]; then
  ORIG_LC_ALL_SET=1
  ORIG_LC_ALL=$LC_ALL
fi
export LC_ALL=C
shopt -u patsub_replacement 2>/dev/null || true

SCHEMA=1
MAX_BUNDLE_BYTES=2147483648
MAX_LOG_FILE=16777216
MAX_LOG_CHUNK=102400
MAX_WAIT_S=60
TURN_TIMEOUT_S=3600
GRACE_S=60

RE_ID='^[1-9][0-9]{0,3}-[0-9A-HJKMNP-TV-Z]{26}$'
RE_KEY='^[a-z0-9-]{1,32}-[0-9a-f]{6}$'
RE_SHA='^[0-9a-f]{40}$'
RE_FP='^[0-9a-f]{64}$'
RE_UINT='^[0-9]{1,10}$'
RE_B64='^[A-Za-z0-9+/]*={0,2}$'
RE_VALUE='^[A-Za-z0-9._:,/+=-]*$'
RE_REQUEST='^GATEHAUL1 ([a-z_]+)$'

# Entries of a worktree that "git clean" keeps between jobs (build caches).
CLEAN_EXCLUDES=(-e /node_modules -e /.gradle -e /target -e /.venv)

ROOT=${GATEHAUL_ROOT:-}
JOBS=$ROOT/jobs
QUEUE=$ROOT/queue
REPO=$ROOT/repo.git
WTS=$ROOT/wt
SELF=$(readlink -f -- "${BASH_SOURCE[0]}")

HOST_ID=
SUB=
RESPONDED=0
COMMITTED=0
SUBMIT_JD=
TMPD=
UNRESOLVED=0
RECONCILED=()
JPIDS=()
declare -A H=()

# ------------------------------------------------------------------ helpers

have() { command -v "$1" >/dev/null 2>&1; }

iso_now() { date -u +%Y-%m-%dT%H:%M:%SZ; }

boot_id() {
  local b=''
  if [ -r /proc/sys/kernel/random/boot_id ]; then
    read -r b </proc/sys/kernel/random/boot_id || true
  fi
  printf '%s' "$b"
}

# js STRING: prints STRING as a JSON string literal (ASCII only).
js() {
  local s=$1
  s=${s//\\/\\\\}
  s=${s//\"/\\\"}
  s=${s//$'\n'/\\n}
  s=${s//$'\r'/\\r}
  s=${s//$'\t'/\\t}
  s=${s//[^ -~]/?}
  printf '"%s"' "$s"
}

# emit RC JSON: the only thing the runner writes on the original stdout.
emit() {
  printf 'GATEHAUL1 BEGIN\n%s\nGATEHAUL1 END %s\n' "$2" "$1" >&3
  RESPONDED=1
}

ok() { emit 0 "$1"; }

# die CODE MESSAGE: answers with an ErrorResponse and stops. Never call it
# from a command substitution.
die() {
  local code=$1 msg=$2 j1 j2
  j1=$(js "$code")
  j2=$(js "$msg")
  emit 1 "{\"schema\":$SCHEMA,\"error\":{\"code\":$j1,\"message\":$j2}}"
  exit 1
}

on_exit() {
  if [ -n "$TMPD" ]; then
    rm -rf -- "$TMPD"
  fi
  if [ "$COMMITTED" = 0 ] && [ -n "$SUBMIT_JD" ]; then
    rm -rf -- "$SUBMIT_JD"
  fi
  # A stream without END means "outcome unknown" to the controller: once a
  # submit has committed, that is the honest answer to an unexpected stop.
  if [ "$RESPONDED" = 0 ] && [ "$COMMITTED" = 0 ]; then
    emit 1 '{"schema":1,"error":{"code":"internal","message":"the runner stopped unexpectedly"}}'
    exit 1
  fi
}

# put FILE TEXT: atomic write (a .tmp file, then mv). Returns 1 if the text
# could not be written (a full disk leaves an empty file, which is not kept).
put() {
  local tmp=$1.tmp.$$
  if ! { printf '%s\n' "$2" >"$tmp" && [ -s "$tmp" ]; } 2>/dev/null; then
    rm -f -- "$tmp"
    return 1
  fi
  mv -f -- "$tmp" "$1"
}

# put_once FILE TEXT: creates FILE only if it does not exist yet (the link is
# atomic). Returns 1 when FILE exists: terminal files are never rewritten, and
# 2 when the text could not be written (a full disk): nothing is created then.
put_once() {
  local f=$1 tmp=$1.tmp.$$
  if ! { printf '%s\n' "$2" >"$tmp" && [ -s "$tmp" ]; } 2>/dev/null; then
    rm -f -- "$tmp"
    return 2
  fi
  if ln -- "$tmp" "$f" 2>/dev/null; then
    rm -f -- "$tmp"
    return 0
  fi
  rm -f -- "$tmp"
  return 1
}

# jget FILE KEY: the value of a flat key written by this runner (a string or a
# number without quotes or commas), or nothing.
jget() {
  local body re
  if [ ! -r "$1" ]; then
    return 0
  fi
  body=$(<"$1")
  re="\"$2\":\"?([^\",}]*)"
  if [[ $body =~ $re ]]; then
    printf '%s' "${BASH_REMATCH[1]}"
  fi
  return 0
}

# file_age FILE: seconds since FILE was modified (a large number if missing).
file_age() {
  local m
  if m=$(stat -c %Y -- "$1" 2>/dev/null); then
    echo $((EPOCHSECONDS - m))
  else
    echo 999999999
  fi
}

# gitw ARGS: every git call goes through here.
gitw() {
  # No automatic gc (it would detach and keep our stdout open); fd 3 is closed
  # for git for the same reason. The explicit gc is in cmd_gc.
  GIT_TERMINAL_PROMPT=0 git -c core.hooksPath=/dev/null -c commit.gpgsign=false \
    -c credential.helper= -c protocol.allow=never -c protocol.file.allow=always \
    -c gc.auto=0 -c maintenance.auto=false "$@" 3>&-
}

# lock_refs / unlock_refs: one writer at a time on refs/gatehaul/* (two submits
# on the same branch would otherwise collide on the ref's own git lock). The
# descriptor is closed before the job is detached.
lock_refs() {
  exec 5>"$ROOT/refs.lock"
  flock -w 60 5 || die internal "another submit holds the refs lock"
}

unlock_refs() {
  exec 5>&-
}

# rev_commit REV: prints the commit id of REV in repo.git, or fails.
rev_commit() {
  case $1 in
    -*) return 1 ;;
  esac
  gitw -C "$REPO" rev-parse --verify --quiet --end-of-options "$1^{commit}"
}

# ------------------------------------------------------------------ request

read_header() {
  local line key val n=0
  IFS= read -r line || die bad_request "empty request"
  [[ $line =~ $RE_REQUEST ]] || die bad_request "the first line is not a GATEHAUL1 request"
  SUB=${BASH_REMATCH[1]}
  while IFS= read -r line; do
    if [ -z "$line" ]; then
      return 0
    fi
    n=$((n + 1))
    if [ "$n" -gt 64 ]; then
      die bad_request "too many header lines"
    fi
    if [ "${#line}" -gt 2000000 ]; then
      die bad_request "header line too long"
    fi
    [[ $line == *=* ]] || die bad_request "header line without ="
    key=${line%%=*}
    val=${line#*=}
    [[ $key =~ ^[a-z0-9_]{1,32}$ ]] || die bad_request "invalid header key"
    [[ $val =~ $RE_VALUE ]] || die bad_request "invalid characters in $key"
    H[$key]=$val
  done
  die bad_request "the request header is not terminated"
}

# req_match KEY REGEX: the header value of KEY must exist and match REGEX.
req_match() {
  local v=${H[$1]:-}
  [[ $v =~ $2 ]] || die bad_request "invalid or missing $1"
}

# get_uint KEY DEFAULT MAX: sets REPLY to the integer value of KEY.
get_uint() {
  local v=${H[$1]:-$2}
  [[ $v =~ $RE_UINT ]] || die bad_request "invalid $1"
  v=$((10#$v))
  if [ "$v" -gt "$3" ]; then
    die bad_request "$1 is too large"
  fi
  REPLY=$v
}

# ------------------------------------------------------------------ layout

load_host_id() {
  HOST_ID=
  if [ -r "$ROOT/host.id" ]; then
    HOST_ID=$(<"$ROOT/host.id")
  fi
}

init_root() {
  local id tmp
  umask 077
  mkdir -p -- "$ROOT" "$JOBS" "$QUEUE" "$WTS" || die install_failed "cannot create the runner directories"
  chmod 700 -- "$ROOT" "$JOBS" "$QUEUE" "$WTS" 2>/dev/null || true
  if [ ! -s "$ROOT/host.id" ]; then
    tmp=$ROOT/host.id.tmp.$$
    id=$(head -c 16 /dev/urandom | od -An -tx1 | tr -d ' \n')
    printf '%s\n' "$id" >"$tmp"
    mv -n -- "$tmp" "$ROOT/host.id" || true
    rm -f -- "$tmp"
  fi
  load_host_id
  if have git && [ ! -d "$REPO" ]; then
    tmp=$ROOT/repo.git.tmp.$$
    gitw init --bare --quiet --template= "$tmp" || die install_failed "cannot create repo.git"
    mv -T -- "$tmp" "$REPO" 2>/dev/null || rm -rf -- "$tmp"
  fi
}

# ------------------------------------------------------------------ job state

# lock_free ID: true if the job's lock file exists and nobody holds it.
lock_free() {
  local f=$JOBS/$1/lock
  [ -e "$f" ] || return 1
  (exec 9<"$f" && flock -n 9) 2>/dev/null
}

# launch_boot ID: the boot id under which the process of job ID was last
# launched (by submit, or by a replay), or nothing.
launch_boot() {
  local jd=$JOBS/$1 b=''
  if [ -r "$jd/replays" ]; then
    read -r _ b _ <"$jd/replays" || true
  fi
  if [ -z "$b" ] || [ "$b" = - ]; then
    b=$(jget "$jd/meta.json" boot_id)
  fi
  printf '%s' "$b"
}

# job_alive ID: false once the job is finished or its process is gone. A job
# that was just launched gets GRACE_S seconds to take its lock, unless the host
# restarted since: no process survives a new boot_id.
job_alive() {
  local id=$1 jd=$JOBS/$1 lb cur ref=$JOBS/$1/meta.json
  if [ ! -d "$jd" ] || [ -e "$jd/exit.json" ]; then
    return 1
  fi
  if [ -e "$jd/lock" ] && ! lock_free "$id"; then
    return 0
  fi
  if [ -e "$jd/started" ]; then
    return 1
  fi
  lb=$(launch_boot "$id")
  cur=$(boot_id)
  if [ -n "$lb" ] && [ -n "$cur" ] && [ "$lb" != "$cur" ]; then
    return 1
  fi
  if [ -e "$jd/replays" ]; then
    ref=$jd/replays
  fi
  [ "$(file_age "$ref")" -lt "$GRACE_S" ]
}

# spawn_job ID: detaches "__job ID": its own session, no inherited descriptors,
# so that it survives the end of this ssh session.
spawn_job() {
  # LC_ALL is exported here, so the job process gets the caller's own value
  # back (or none): it passes that on to the command.
  local -a lc=(-u LC_ALL)
  if [ "$ORIG_LC_ALL_SET" = 1 ]; then
    lc=("LC_ALL=$ORIG_LC_ALL")
  fi
  env "${lc[@]}" "GATEHAUL_ROOT=$ROOT" setsid bash -- "$SELF" __job "$1" </dev/null >/dev/null 2>&1 3>&- &
  disown 2>/dev/null || true
}

# relaunch_job ID: a job that never started lost its process (docs/SPEC.md
# 5.3). It is launched again once; the count is the file "replays", created
# atomically so that two reconcilers cannot both do it. Returns 1 when the job
# was already launched again before: the caller gives up on it.
relaunch_job() {
  local id=$1 jd=$JOBS/$1 rc=0 boot ticket
  if [ -e "$jd/replays" ]; then
    return 1
  fi
  boot=$(boot_id)
  put_once "$jd/replays" "1 ${boot:--} $EPOCHSECONDS" || rc=$?
  if [ "$rc" = 0 ]; then
    ticket=$QUEUE/$(jget "$jd/meta.json" prio)-$(jget "$jd/meta.json" seq)-$id
    if [ ! -e "$ticket" ]; then
      printf '%s\n%s\n' "$boot" "$EPOCHSECONDS" >"$ticket" 2>/dev/null || true
    fi
    spawn_job "$id"
  fi
  return 0
}

# job_state ID: queued, running or the terminal state.
job_state() {
  local jd=$JOBS/$1
  if [ -e "$jd/exit.json" ]; then
    jget "$jd/exit.json" state
  elif [ -e "$jd/started" ]; then
    printf 'running'
  else
    printf 'queued'
  fi
}

# write_exit ID STATE KIND STAGE REASON [EXIT_CODE [TRUNCATED [DIRTY [EXTRA]]]]
# writes exit.json once. Returns 1 if it already existed.
write_exit() {
  local id=$1 state=$2 kind=$3 stage=$4 reason=$5
  local code=${6:-null} trunc=${7:-false} dirty=${8:-false} extra=${9:-}
  local jd=$JOBS/$1 sha started='' ended body
  local jid jstate jcode jkind jstage jreason jsha jhost jstarted jended
  sha=$(jget "$jd/meta.json" sha)
  if [ -r "$jd/started" ]; then
    read -r _ started _ <"$jd/started" || true
  fi
  ended=$(iso_now)
  jid=$(js "$id")
  jstate=$(js "$state")
  jkind=$(js "$kind")
  jstage=$(js "$stage")
  jreason=$(js "$reason")
  jsha=$(js "$sha")
  jhost=$(js "$HOST_ID")
  jstarted=$(js "$started")
  jended=$(js "$ended")
  jcode=$code
  body="{\"schema\":$SCHEMA,\"id\":$jid,\"state\":$jstate,\"exit_code\":$jcode,\"failure_kind\":$jkind,\"stage\":$jstage,\"reason\":$jreason,\"sha\":$jsha,\"host_id\":$jhost,\"started_at\":$jstarted,\"ended_at\":$jended,\"truncated\":$trunc,\"tree_dirty_after\":$dirty$extra}"
  put_once "$jd/exit.json" "$body"
}

log_size() { stat -c %s -- "$1/log" 2>/dev/null || echo 0; }

# tail_b64 FILE BYTES: the last BYTES of FILE, base64 on one line.
tail_b64() {
  if [ "$2" -gt 0 ] && [ -s "$1" ]; then
    tail -c "$2" -- "$1" | base64 -w0
  fi
}

# queue_pos ID: 1 is next; 0 when the job is not waiting for its turn.
queue_pos() {
  local t name tid n=0
  for t in "$QUEUE"/*; do
    [ -e "$t" ] || continue
    name=${t##*/}
    tid=${name#*-}
    tid=${tid#*-}
    [ -e "$JOBS/$tid/started" ] && continue
    job_alive "$tid" || continue
    n=$((n + 1))
    if [ "$tid" = "$1" ]; then
      printf '%s' "$n"
      return 0
    fi
  done
  printf '0'
}

# status_json ID TAIL_BYTES: the JobStatus of ID. exit.json is read first, then
# the log, so that a terminal answer never carries a truncated log.
status_json() {
  local id=$1 tailb=$2 jd=$JOBS/$1 body size tail sha started='' state qpos=0
  if [ -e "$jd/exit.json" ]; then
    body=$(<"$jd/exit.json")
    body=${body%\}}
    size=$(log_size "$jd")
    tail=$(tail_b64 "$jd/log" "$tailb")
    printf '%s,"queue_pos":0,"log_size":%s,"log_tail_b64":"%s"}' "$body" "$size" "$tail"
    return 0
  fi
  sha=$(jget "$jd/meta.json" sha)
  if [ -e "$jd/started" ]; then
    state=running
    read -r _ started _ <"$jd/started" || true
  else
    state=queued
    qpos=$(queue_pos "$id")
  fi
  size=$(log_size "$jd")
  printf '{"schema":%s,"id":"%s","state":"%s","exit_code":null,"failure_kind":"","stage":"","reason":"","sha":"%s","host_id":"%s","queue_pos":%s,"started_at":"%s","ended_at":"","log_size":%s,"log_tail_b64":"","truncated":false,"tree_dirty_after":false}' \
    "$SCHEMA" "$id" "$state" "$sha" "$HOST_ID" "$qpos" "$started" "$size"
}

# ------------------------------------------------------------------ processes

# signal_session ID SIG: signals the job's session, if the leader is still the
# process that was recorded: the (sid, start time, boot_id) triple must match,
# because a pid alone proves nothing after a reboot.
signal_session() {
  local jd=$JOBS/$1 sid='' start='' sboot='' cur f
  [ -r "$jd/sid" ] || return 0
  read -r sid start sboot _ <"$jd/sid" || true
  [[ $sid =~ ^[0-9]+$ ]] || return 0
  cur=$(boot_id)
  if [ -n "$sboot" ] && [ "$sboot" != - ] && [ "$sboot" != "$cur" ]; then
    return 0
  fi
  [ -r "/proc/$sid/stat" ] || return 0
  read -ra f <"/proc/$sid/stat" || return 0
  if [ "${f[5]:-}" = "$sid" ] && [ "${f[21]:-}" = "$start" ]; then
    kill "-$2" -- "-$sid" 2>/dev/null || true
  fi
  return 0
}

# job_pids ID: sets JPIDS to the processes whose environment carries
# GATEHAUL_JOB=ID, including those that called setsid again.
job_pids() {
  local f p
  JPIDS=()
  while IFS= read -r f; do
    p=${f#/proc/}
    p=${p%/environ}
    [[ $p =~ ^[0-9]+$ ]] || continue
    if [ "$p" != "$$" ] && [ "$p" != "${BASHPID:-0}" ]; then
      JPIDS+=("$p")
    fi
  done < <(LC_ALL=C grep -laxzF -- "GATEHAUL_JOB=$1" /proc/[0-9]*/environ 2>/dev/null || true)
}

# kill_job_procs ID: TERM, KILL after 10 s, then look again. Sets UNRESOLVED=1
# and returns 1 if something survives.
kill_job_procs() {
  local id=$1 i
  UNRESOLVED=0
  signal_session "$id" TERM
  job_pids "$id"
  if [ "${#JPIDS[@]}" -eq 0 ]; then
    return 0
  fi
  kill -TERM -- "${JPIDS[@]}" 2>/dev/null || true
  for ((i = 0; i < 20; i++)); do
    sleep 0.5
    job_pids "$id"
    if [ "${#JPIDS[@]}" -eq 0 ]; then
      return 0
    fi
  done
  signal_session "$id" KILL
  kill -KILL -- "${JPIDS[@]}" 2>/dev/null || true
  for ((i = 0; i < 10; i++)); do
    sleep 0.5
    job_pids "$id"
    if [ "${#JPIDS[@]}" -eq 0 ]; then
      return 0
    fi
  done
  UNRESOLVED=1
  return 1
}

# ------------------------------------------------------------------ reconciliation

# settle_dead ID: the job has no live process and no exit.json.
settle_dead() {
  local id=$1 jd=$JOBS/$1 reason
  if [ -e "$jd/started" ]; then
    reason="the job's process is gone before it finished (host restarted or process killed)"
    if ! kill_job_procs "$id"; then
      reason="$reason; cleanup_unresolved"
    fi
    if write_exit "$id" lost lost reconcile "$reason" null; then
      RECONCILED+=("$id")
    fi
  else
    # Never started: its process is gone, but nothing ran, so it is launched
    # again once (docs/SPEC.md 5.3) and keeps its place in the queue.
    if relaunch_job "$id"; then
      return 0
    fi
    write_exit "$id" infra_failed infra queue "the job never started, even after it was launched again (host restarted or runner killed); submit it again" null || true
  fi
  rm -f -- "$QUEUE"/*-"$id"
}

reconcile_job() {
  local id=$1
  if [ ! -e "$JOBS/$id/meta.json" ]; then
    return 0
  fi
  if [ ! -e "$JOBS/$id/exit.json" ] && ! job_alive "$id"; then
    settle_dead "$id"
  fi
}

reconcile_all() {
  local d id t name
  for d in "$JOBS"/*/; do
    [ -d "$d" ] || continue
    id=${d%/}
    id=${id##*/}
    [[ $id =~ $RE_ID ]] || continue
    reconcile_job "$id"
  done
  for t in "$QUEUE"/*; do
    [ -e "$t" ] || continue
    name=${t##*/}
    id=${name#*-}
    id=${id#*-}
    if [ ! -e "$JOBS/$id/meta.json" ] || [ -e "$JOBS/$id/exit.json" ]; then
      if [ -e "$JOBS/$id/exit.json" ] || [ "$(file_age "$t")" -ge "$GRACE_S" ]; then
        rm -f -- "$t"
      fi
    fi
  done
  git_prune_worktrees
}

git_prune_worktrees() {
  if [ -d "$REPO" ]; then
    gitw -C "$REPO" worktree prune >/dev/null 2>&1 || true
  fi
}

# ------------------------------------------------------------------ probe

cmd_probe() {
  local version git_v='' tools='' sep='' t disk=0 line tok=false depth=0 running=null
  local refs='' rsep='' name sha d id age ep rec='' csep='' out
  init_root
  RECONCILED=()
  reconcile_all

  version=$(sha256sum -- "$SELF")
  version=${version:0:12}

  if have git; then
    git_v=$(git --version 2>/dev/null) || git_v=
    git_v=${git_v#git version }
    git_v=${git_v%% *}
  fi

  for t in flock setsid timeout base64 sha256sum git; do
    if have "$t"; then
      tools+="$sep\"$t\":true"
    else
      tools+="$sep\"$t\":false"
    fi
    sep=,
  done

  if line=$(df -Pk -- "$ROOT" 2>/dev/null | tail -n 1); then
    read -r _ _ _ disk _ <<<"$line" || true
  fi
  [[ $disk =~ ^[0-9]+$ ]] || disk=0

  if [ -n "${GITHUB_TOKEN:-}${GH_TOKEN:-}${GITHUB_CODESPACE_TOKEN:-}" ]; then
    tok=true
  fi

  for t in "$QUEUE"/*; do
    [ -e "$t" ] || continue
    name=${t##*/}
    id=${name#*-}
    id=${id#*-}
    if [ ! -e "$JOBS/$id/started" ] && job_alive "$id"; then
      depth=$((depth + 1))
    fi
  done

  for d in "$JOBS"/*/; do
    [ -d "$d" ] || continue
    id=${d%/}
    id=${id##*/}
    [[ $id =~ $RE_ID ]] || continue
    if [ -e "$d/started" ] && [ ! -e "$d/exit.json" ] && ! lock_free "$id"; then
      read -r ep _ <"$d/started" || ep=$EPOCHSECONDS
      [[ $ep =~ ^[0-9]+$ ]] || ep=$EPOCHSECONDS
      age=$((EPOCHSECONDS - ep))
      running="{\"id\":\"$id\",\"age_s\":$age}"
      break
    fi
  done

  if [ -d "$REPO" ]; then
    while read -r name sha; do
      if [[ $name =~ $RE_KEY ]] && [[ $sha =~ $RE_SHA ]]; then
        refs+="$rsep\"$name\":\"$sha\""
        rsep=,
      fi
    done < <(gitw -C "$REPO" for-each-ref --format='%(refname:lstrip=2) %(objectname)' refs/gatehaul/ 2>/dev/null || true)
  fi

  for id in "${RECONCILED[@]}"; do
    rec+="$csep\"$id\""
    csep=,
  done

  out="{\"schema\":$SCHEMA,\"runner\":\"$version\",\"host_id\":\"$HOST_ID\",\"boot_id\":\"$(boot_id)\",\"git\":$(js "$git_v"),\"tools\":{$tools},\"disk_free_kb\":$disk,\"github_token_env\":$tok,\"queue_depth\":$depth,\"running\":$running,\"refs\":{$refs},\"reconciled\":[$rec]}"
  ok "$out"
}

# ------------------------------------------------------------------ submit

# submit_existing ID FP: same id replayed. Never returns.
submit_existing() {
  local id=$1 fp=$2 have_fp state
  have_fp=$(jget "$JOBS/$id/meta.json" fingerprint)
  if [ "$have_fp" != "$fp" ]; then
    die conflict "job $id already exists with a different fingerprint"
  fi
  reconcile_job "$id"
  state=$(job_state "$id")
  ok "{\"schema\":$SCHEMA,\"id\":\"$id\",\"status\":\"exists\",\"state\":\"$state\"}"
  exit 0
}

cmd_submit() {
  local id key sha fp kind prio tmo blen b64 jd t tries=0 a b
  local got vout vrc heads hsha href refname fout seq boot ticket

  req_match id "$RE_ID"
  req_match key "$RE_KEY"
  req_match sha "$RE_SHA"
  req_match fp "$RE_FP"
  id=${H[id]}
  key=${H[key]}
  sha=${H[sha]}
  fp=${H[fp]}
  kind=${H[kind]:-}
  case $kind in
    run | gen) ;;
    *) die bad_request "kind must be run or gen" ;;
  esac
  get_uint prio 5 9
  prio=$REPLY
  get_uint timeout_s 0 86400
  tmo=$REPLY
  if [ "$tmo" -lt 1 ]; then
    die bad_request "timeout_s must be at least 1"
  fi
  get_uint bundle_len 0 9999999999
  blen=$REPLY
  b64=${H[cmd_b64]:-}
  [[ $b64 =~ $RE_B64 ]] || die bad_request "cmd_b64 is not base64"
  if [ -z "$b64" ] || [ $((${#b64} % 4)) -ne 0 ]; then
    die bad_request "cmd_b64 is empty or malformed"
  fi

  for t in flock setsid timeout git base64 sha256sum; do
    have "$t" || die missing_tool "missing tool: $t"
  done

  init_root
  jd=$JOBS/$id

  # Idempotency first: the same id is either the same job or a conflict.
  until mkdir -- "$jd" 2>/dev/null; do
    if [ -s "$jd/meta.json" ]; then
      submit_existing "$id" "$fp"
    fi
    tries=$((tries + 1))
    if [ "$tries" -ge 15 ]; then
      a=$(file_age "$jd")
      b=$(file_age "$jd/bundle")
      if [ "$a" -ge 120 ] && [ "$b" -ge 120 ] && [ ! -s "$jd/meta.json" ]; then
        rm -rf -- "$jd"
        tries=0
        continue
      fi
      die bad_request "job $id is being submitted by another connection"
    fi
    sleep 1
  done
  SUBMIT_JD=$jd

  if [ "$blen" -gt "$MAX_BUNDLE_BYTES" ]; then
    die bundle_too_large "the bundle is larger than $MAX_BUNDLE_BYTES bytes"
  fi

  if [ "$blen" -gt 0 ]; then
    if ! head -c "$blen" >"$jd/bundle"; then
      die disk_full "cannot store the bundle"
    fi
    got=$(stat -c %s -- "$jd/bundle")
    if [ "$got" -ne "$blen" ]; then
      die bundle_invalid "short bundle: received $got of $blen bytes"
    fi
    vrc=0
    vout=$(gitw -C "$REPO" bundle verify "$jd/bundle" 2>&1) || vrc=$?
    if [ "$vrc" -ne 0 ]; then
      case $vout in
        *prerequisite*) die need_full "the host lacks commits the bundle builds on; send a full bundle" ;;
      esac
      die bundle_invalid "git bundle verify failed: ${vout: -300}"
    fi
    heads=$(gitw -C "$REPO" bundle list-heads "$jd/bundle" 2>/dev/null) || die bundle_invalid "cannot list the refs of the bundle"
    refname=
    while read -r hsha href _; do
      if [ "$hsha" = "$sha" ]; then
        refname=$href
        break
      fi
    done <<<"$heads"
    if [ -z "$refname" ]; then
      die sha_mismatch "the bundle does not hold commit $sha"
    fi
    case $refname in
      HEAD | refs/*) ;;
      *) die bundle_invalid "unexpected ref name in the bundle" ;;
    esac
    lock_refs
    fout=$(gitw -c transfer.fsckObjects=true -C "$REPO" fetch --quiet --no-tags --force "$jd/bundle" "$refname:refs/gatehaul/$key" 2>&1) \
      || die bundle_invalid "git fetch failed: ${fout: -300}"
    unlock_refs
    rm -f -- "$jd/bundle"
  else
    rev_commit "$sha" >/dev/null || die need_full "the host does not have commit $sha; send a bundle"
    lock_refs
    gitw -C "$REPO" update-ref "refs/gatehaul/$key" "$sha" || die internal "cannot update refs/gatehaul/$key"
    unlock_refs
  fi
  got=$(rev_commit "$sha") || die sha_mismatch "commit $sha is missing after the fetch"
  if [ "$got" != "$sha" ]; then
    die sha_mismatch "commit $sha is missing after the fetch"
  fi
  # Keeps the commit alive for the job even if the branch ref moves on.
  gitw -C "$REPO" update-ref "refs/gatehaul-jobs/$id" "$sha" || die internal "cannot pin commit $sha"

  printf '%s' "$b64" | base64 -d >"$jd/cmd.sh.tmp" 2>/dev/null || die bad_request "cmd_b64 is not valid base64"
  mv -f -- "$jd/cmd.sh.tmp" "$jd/cmd.sh"
  : >"$jd/log"

  seq=$(date +%s%N)
  boot=$(boot_id)
  put "$jd/meta.json" "{\"schema\":$SCHEMA,\"id\":\"$id\",\"key\":\"$key\",\"sha\":\"$sha\",\"kind\":\"$kind\",\"prio\":$prio,\"timeout_s\":$tmo,\"fingerprint\":\"$fp\",\"host_id\":\"$HOST_ID\",\"boot_id\":\"$boot\",\"seq\":\"$seq\",\"created_epoch\":$EPOCHSECONDS}" \
    || die disk_full "cannot write the job record"
  COMMITTED=1

  ticket=$QUEUE/$prio-$seq-$id
  if ! { printf '%s\n%s\n' "$boot" "$EPOCHSECONDS" >"$jd/ticket.tmp" && mv -f -- "$jd/ticket.tmp" "$ticket"; } 2>/dev/null; then
    # Nothing stays committed: the job directory goes away with this answer.
    rm -f -- "$jd/ticket.tmp"
    COMMITTED=0
    die disk_full "cannot write the queue ticket"
  fi

  spawn_job "$id"

  ok "{\"schema\":$SCHEMA,\"id\":\"$id\",\"status\":\"accepted\",\"state\":\"queued\"}"
}

# ------------------------------------------------------------------ wait

cmd_wait() {
  local raw=${H[ids]:-} id done_n limit tailb all=0 deadline out='' sep=''
  local -a ids=()
  init_root
  if [ -z "$raw" ]; then
    die bad_request "missing ids"
  fi
  IFS=, read -ra ids <<<"$raw" || true
  if [ "${#ids[@]}" -gt 64 ]; then
    die bad_request "too many ids"
  fi
  for id in "${ids[@]}"; do
    [[ $id =~ $RE_ID ]] || die bad_request "invalid job id"
    [ -e "$JOBS/$id/meta.json" ] || die unknown_job "no such job: $id"
  done
  get_uint timeout_s 0 100000
  limit=$REPLY
  if [ "$limit" -gt "$MAX_WAIT_S" ]; then
    limit=$MAX_WAIT_S
  fi
  get_uint tail_bytes 0 9999999999
  tailb=$REPLY
  if [ "$tailb" -gt "$MAX_LOG_CHUNK" ]; then
    tailb=$MAX_LOG_CHUNK
  fi
  if [ "${H[all]:-0}" = 1 ]; then
    all=1
  fi

  deadline=$((SECONDS + limit))
  while :; do
    done_n=0
    for id in "${ids[@]}"; do
      reconcile_job "$id"
      if [ -e "$JOBS/$id/exit.json" ]; then
        done_n=$((done_n + 1))
      fi
    done
    if [ "$all" = 1 ]; then
      if [ "$done_n" -eq "${#ids[@]}" ]; then
        break
      fi
    elif [ "$done_n" -ge 1 ]; then
      break
    fi
    if [ "$SECONDS" -ge "$deadline" ]; then
      break
    fi
    sleep 1
  done

  for id in "${ids[@]}"; do
    out+="$sep$(status_json "$id" "$tailb")"
    sep=,
  done
  ok "{\"schema\":$SCHEMA,\"jobs\":[$out]}"
}

# ------------------------------------------------------------------ log

cmd_log() {
  local id jd since maxb tl pat terminal=0 state size start end len rawlen
  local keep partial lastb trunc=false cursor rc=0 raw out data
  init_root
  req_match id "$RE_ID"
  id=${H[id]}
  jd=$JOBS/$id
  [ -e "$jd/meta.json" ] || die unknown_job "no such job: $id"
  get_uint since 0 9999999999
  since=$REPLY
  get_uint max_bytes "$MAX_LOG_CHUNK" 9999999999
  maxb=$REPLY
  if [ "$maxb" -lt 1 ] || [ "$maxb" -gt "$MAX_LOG_CHUNK" ]; then
    maxb=$MAX_LOG_CHUNK
  fi
  get_uint tail_lines 0 100000
  tl=$REPLY
  pat=${H[grep_b64]:-}
  [[ $pat =~ $RE_B64 ]] || die bad_request "grep_b64 is not base64"

  TMPD=$(mktemp -d -p "$ROOT" tmp.XXXXXX)
  raw=$TMPD/raw
  out=$TMPD/out
  : >"$TMPD/pat"
  if [ -n "$pat" ]; then
    printf '%s' "$pat" | base64 -d >"$TMPD/pat" 2>/dev/null || die bad_request "grep_b64 is not valid base64"
  fi

  reconcile_job "$id"
  if [ -e "$jd/exit.json" ]; then
    terminal=1
  fi
  state=$(job_state "$id")
  size=$(log_size "$jd")

  if [ "$tl" -gt 0 ]; then
    # The end of the log: the last tail_lines lines of what is left to read.
    end=$size
    start=$((size - maxb))
    if [ "$start" -lt "$since" ]; then
      start=$since
    fi
    if [ "$start" -gt "$end" ]; then
      start=$end
    fi
    # "truncated" stays false: the cursor of a tail is the end of the log, so
    # there is nothing to continue from (asking for the rest returns nothing).
  else
    start=$since
    if [ "$start" -gt "$size" ]; then
      start=$size
    fi
    end=$((start + maxb))
    if [ "$end" -gt "$size" ]; then
      end=$size
    fi
  fi
  len=$((end - start))

  : >"$raw"
  if [ "$len" -gt 0 ]; then
    tail -c "+$((start + 1))" -- "$jd/log" | head -c "$len" >"$raw" || true
  fi
  rawlen=$(stat -c %s -- "$raw")
  if [ "$tl" -eq 0 ] && [ $((start + rawlen)) -lt "$size" ]; then
    trunc=true
  fi

  # Cut at the last newline, unless the job is over and this is the end.
  keep=$rawlen
  if [ "$rawlen" -gt 0 ]; then
    lastb=$(tail -c 1 -- "$raw" | od -An -tx1 | tr -d ' \n')
    if [ "$lastb" != 0a ] && ! { [ "$terminal" = 1 ] && [ $((start + rawlen)) -ge "$size" ]; }; then
      partial=$(tail -n 1 -- "$raw" | wc -c)
      if [ "$partial" -lt "$rawlen" ]; then
        keep=$((rawlen - partial))
      elif [ "$rawlen" -lt "$maxb" ]; then
        keep=0
      fi
    fi
  fi
  if [ "$keep" -lt "$rawlen" ]; then
    truncate -s "$keep" -- "$raw"
  fi
  cursor=$((start + keep))

  # The first line is probably cut when the end of the log was asked for.
  if [ "$tl" -gt 0 ] && [ "$start" -gt "$since" ] && [ "$start" -gt 0 ]; then
    tail -n +2 -- "$raw" >"$TMPD/raw2" || true
    mv -f -- "$TMPD/raw2" "$raw"
  fi

  if [ -s "$TMPD/pat" ]; then
    LC_ALL=C grep -a -E -f "$TMPD/pat" -- "$raw" >"$out" || rc=$?
    if [ "$rc" -gt 1 ]; then
      die bad_request "invalid grep pattern"
    fi
  else
    cp -- "$raw" "$out"
  fi
  if [ "$tl" -gt 0 ]; then
    tail -n "$tl" -- "$out" >"$TMPD/out2" || true
    mv -f -- "$TMPD/out2" "$out"
  fi
  data=$(base64 -w0 <"$out")
  ok "{\"schema\":$SCHEMA,\"id\":\"$id\",\"state\":\"$state\",\"data_b64\":\"$data\",\"cursor\":$cursor,\"size\":$size,\"truncated\":$trunc}"
}

# ------------------------------------------------------------------ cancel

# wait_job_gone ID SECS: true once nobody holds the job's lock (at once if the
# job has no lock file), false if it is still held after SECS seconds.
wait_job_gone() {
  local f=$JOBS/$1/lock
  [ -e "$f" ] || return 0
  (exec 9<"$f" && flock -w "$2" 9) 2>/dev/null
}

cmd_cancel() {
  local id jd i
  init_root
  req_match id "$RE_ID"
  id=${H[id]}
  jd=$JOBS/$id
  [ -e "$jd/meta.json" ] || die unknown_job "no such job: $id"
  reconcile_job "$id"
  if [ ! -e "$jd/exit.json" ]; then
    # The same lock arbitrates with the job's own transition to started.
    exec 6>"$jd/state.lock"
    flock 6
    if [ -e "$jd/exit.json" ]; then
      :
    elif [ ! -e "$jd/started" ]; then
      write_exit "$id" cancelled user queue "cancelled before it started" null || true
      rm -f -- "$QUEUE"/*-"$id"
    else
      : >"$jd/cancel"
    fi
    exec 6>&-
    if [ ! -e "$jd/exit.json" ]; then
      kill_job_procs "$id" || true
      # The job shell sees the cancel flag and writes exit.json itself.
      for ((i = 0; i < 40; i++)); do
        if [ -e "$jd/exit.json" ]; then
          break
        fi
        job_pids "$id"
        if [ "${#JPIDS[@]}" -gt 0 ]; then
          kill -KILL -- "${JPIDS[@]}" 2>/dev/null || true
        fi
        sleep 0.5
      done
      # The job shell may still be writing the manifest and the patch: its
      # lock is released when it is gone, and only then is exit.json ours.
      if [ ! -e "$jd/exit.json" ] && wait_job_gone "$id" 60; then
        write_exit "$id" cancelled user command "cancelled by the user" null || true
        rm -f -- "$QUEUE"/*-"$id"
      fi
    fi
  fi
  ok "$(status_json "$id" 0)"
}

# ------------------------------------------------------------------ gc

cmd_gc() {
  local days removed=0 f d id
  init_root
  get_uint days 7 3650
  days=$REPLY
  while IFS= read -r f; do
    d=${f%/exit.json}
    id=${d##*/}
    [[ $id =~ $RE_ID ]] || continue
    rm -rf -- "$d"
    if [ -d "$REPO" ]; then
      gitw -C "$REPO" update-ref -d "refs/gatehaul-jobs/$id" >/dev/null 2>&1 || true
    fi
    removed=$((removed + 1))
  done < <(find "$JOBS" -mindepth 2 -maxdepth 2 -name exit.json -mmin "+$((days * 1440))" 2>/dev/null || true)
  find "$ROOT" -maxdepth 1 -name 'tmp.*' -mmin +1440 -exec rm -rf {} + 2>/dev/null || true
  find "$WTS" -maxdepth 1 -name '.trash-*' -mmin +60 -exec rm -rf {} + 2>/dev/null || true
  gitw -C "$REPO" gc --quiet >/dev/null 2>&1 || true
  ok "{\"schema\":$SCHEMA,\"removed\":$removed}"
}

# ------------------------------------------------------------------ the job

JD=
JID=
JKEY=
JSHA=
JKIND=
JTIMEOUT=
JCREATED=
TICKET_NAME=
WT=
PREP_ERR=
LOG_TRUNC=false
TREE_DIRTY=false
PATCH_FIELD=

# Runs in the job's own shell; "$2" is where the session id is recorded.
# shellcheck disable=SC2016 # expanded by the child shell, on purpose
SESSION_WRAPPER='read -ra f </proc/$$/stat; printf "%s\n" "${f[5]}" >"$2"; exec bash -o pipefail "$1"'

job_exit() {
  trap - EXIT
  if [ ! -e "$JD/exit.json" ]; then
    write_exit "$JID" infra_failed infra runner "the runner stopped unexpectedly" null || true
  fi
  if [ -n "$TICKET_NAME" ]; then
    rm -f -- "$QUEUE/$TICKET_NAME"
  fi
  if [ -d "$REPO" ]; then
    gitw -C "$REPO" update-ref -d "refs/gatehaul-jobs/$JID" >/dev/null 2>&1 || true
  fi
}

# finish_early STATE KIND STAGE REASON: the job ends before its command ran.
finish_early() {
  write_exit "$JID" "$1" "$2" "$3" "$4" null || true
  exit 0
}

# head_ticket: the name of the first ticket whose job is alive.
head_ticket() {
  local t name tid
  for t in "$QUEUE"/*; do
    [ -e "$t" ] || continue
    name=${t##*/}
    tid=${name#*-}
    tid=${tid#*-}
    if job_alive "$tid"; then
      printf '%s' "$name"
      return 0
    fi
  done
  return 0
}

# wait_turn: 0 when this job holds the global lock at the head of the queue,
# 2 if it was cancelled meanwhile, 1 after TURN_TIMEOUT_S.
wait_turn() {
  local deadline=$((EPOCHSECONDS + TURN_TIMEOUT_S)) head
  while :; do
    if [ -e "$JD/exit.json" ] || [ ! -e "$QUEUE/$TICKET_NAME" ]; then
      return 2
    fi
    head=$(head_ticket)
    if [ "$head" = "$TICKET_NAME" ]; then
      if flock -w 2 7; then
        return 0
      fi
    else
      sleep 1
    fi
    if [ "$EPOCHSECONDS" -ge "$deadline" ]; then
      return 1
    fi
  done
}

reset_worktree() {
  gitw -C "$WT" reset --hard --quiet >/dev/null 2>&1 || return 1
  gitw -C "$WT" clean -ffdx --quiet "${CLEAN_EXCLUDES[@]}" >/dev/null 2>&1 || return 1
}

# discard_worktree: removes $WT. Files that belong to another user (a build in
# a container, sudo) cannot be removed, but a rename only needs write access to
# wt/: what is left is moved aside under a name that cmd_gc cleans up later.
discard_worktree() {
  rm -rf -- "$WT" 2>/dev/null || true
  if [ -e "$WT" ] || [ -L "$WT" ]; then
    mv -- "$WT" "$WTS/.trash-$JKEY-$(date +%s%N)" 2>/dev/null || true
  fi
}

# prepare_worktree: only called under the global lock.
prepare_worktree() {
  local out
  WT=$WTS/$JKEY
  PREP_ERR=
  umask 022
  gitw -C "$REPO" worktree prune >/dev/null 2>&1 || true
  if [ -e "$WT/.git" ] && reset_worktree; then
    if gitw -C "$WT" checkout --detach --force --quiet "$JSHA" >/dev/null 2>&1; then
      umask 077
      return 0
    fi
  fi
  discard_worktree
  gitw -C "$REPO" worktree prune >/dev/null 2>&1 || true
  if out=$(gitw -C "$REPO" worktree add --detach --force --quiet "$WT" "$JSHA" 2>&1); then
    umask 077
    return 0
  fi
  PREP_ERR=${out: -200}
  umask 077
  return 1
}

tool_versions() {
  local out='' t v
  cd -- "$WT" 2>/dev/null || return 0
  for t in git go node python3; do
    have "$t" || continue
    v=
    case $t in
      go) v=$(timeout 5 go version 2>&1 | head -n 1) || true ;;
      *) v=$(timeout 5 "$t" --version 2>&1 | head -n 1) || true ;;
    esac
    out+="${out:+,}$(js "$t"):$(js "$v")"
  done
  printf '%s' "$out"
}

job_main() {
  local id=${1:-} rc=0 head i sid='' start='' f ts
  local state kind stage reason code ended duration queue_s cores unamestr manifest
  local -a unset_args set_args

  [[ $id =~ $RE_ID ]] || exit 2
  [ -n "$ROOT" ] || exit 2
  JD=$JOBS/$id
  [ -d "$JD" ] || exit 2
  JID=$id
  umask 077

  # Held for the whole life of the job: this is how liveness is tested. A probe
  # that tests the lock holds it for a moment, hence the wait; a process that
  # still cannot take it means that another one runs this job, and leaves it be.
  exec 8>"$JD/lock"
  flock -w 10 8 || exit 0

  # From here on this process owns the job: whatever stops it is recorded.
  load_host_id
  JKEY=$(jget "$JD/meta.json" key)
  JSHA=$(jget "$JD/meta.json" sha)
  JKIND=$(jget "$JD/meta.json" kind)
  JTIMEOUT=$(jget "$JD/meta.json" timeout_s)
  JCREATED=$(jget "$JD/meta.json" created_epoch)
  TICKET_NAME=$(jget "$JD/meta.json" prio)-$(jget "$JD/meta.json" seq)-$id
  trap job_exit EXIT
  [[ $JKEY =~ $RE_KEY ]] || exit 2
  [[ $JSHA =~ $RE_SHA ]] || exit 2
  [[ $JTIMEOUT =~ ^[0-9]+$ ]] || exit 2
  [[ $JCREATED =~ ^[0-9]+$ ]] || JCREATED=$EPOCHSECONDS

  # 2. Wait for the turn.
  exec 7>"$ROOT/lock"
  wait_turn || rc=$?
  case $rc in
    0) ;;
    2) exit 0 ;;
    *) finish_early infra_failed infra queue "gave up waiting for a turn after 60 minutes" ;;
  esac

  # 3. Under the lock only: the worktree.
  if ! prepare_worktree; then
    finish_early infra_failed infra worktree "cannot prepare the worktree: $PREP_ERR"
  fi

  # 4. HEAD must be the commit that was sent.
  head=
  head=$(gitw -C "$WT" rev-parse --verify --quiet --end-of-options 'HEAD^{commit}') || head=
  if [ "$head" != "$JSHA" ]; then
    finish_early infra_failed infra verify-sha "the worktree is at ${head:-nothing}, not at $JSHA"
  fi

  # 5. started, then the command.
  exec 6>"$JD/state.lock"
  flock 6
  if [ -e "$JD/exit.json" ]; then
    exit 0
  fi
  if [ -e "$JD/cancel" ]; then
    finish_early cancelled user queue "cancelled before it started"
  fi
  local started_epoch=$EPOCHSECONDS started_iso boot
  started_iso=$(iso_now)
  boot=$(boot_id)
  put "$JD/started" "$started_epoch $started_iso ${boot:--}" \
    || finish_early infra_failed infra runner "cannot record the start of the job (disk full?)"
  exec 6>&-

  unset_args=(-u GITHUB_TOKEN -u GH_TOKEN -u GITHUB_CODESPACE_TOKEN -u GH_ENTERPRISE_TOKEN
    -u GITHUB_ENTERPRISE_TOKEN -u GIT_ASKPASS -u SSH_ASKPASS -u SSH_AUTH_SOCK -u GATEHAUL_ROOT)
  set_args=("GATEHAUL_JOB=$id" GIT_CONFIG_COUNT=1 GIT_CONFIG_KEY_0=credential.helper GIT_CONFIG_VALUE_0=)
  if [ "$ORIG_LC_ALL_SET" = 1 ]; then
    set_args+=("LC_ALL=$ORIG_LC_ALL")
  else
    unset_args+=(-u LC_ALL)
  fi

  (
    umask 022
    cd -- "$WT" || exit 97
    exec 7>&- 8>&- </dev/null >>"$JD/log" 2>&1
    exec env "${unset_args[@]}" "${set_args[@]}" \
      setsid --wait timeout --signal=TERM --kill-after=10s "$JTIMEOUT" \
      bash -c "$SESSION_WRAPPER" gatehaul-job "$JD/cmd.sh" "$JD/sid.new"
  ) &
  local cmd_pid=$!

  for ((i = 0; i < 50; i++)); do
    if [ -s "$JD/sid.new" ] || ! kill -0 "$cmd_pid" 2>/dev/null; then
      break
    fi
    sleep 0.1
  done
  if [ -s "$JD/sid.new" ]; then
    read -r sid <"$JD/sid.new" || true
    if [[ $sid =~ ^[0-9]+$ ]] && read -ra f <"/proc/$sid/stat" 2>/dev/null; then
      start=${f[21]:-}
    fi
    put "$JD/sid" "${sid:--} ${start:--} ${boot:--}" || true
  fi

  # While the command runs, the log may not outgrow MAX_LOG_FILE: it shares the
  # disk with the user's work, and a log that is cut afterwards would invalidate
  # the cursors already handed out. Past the limit the command is stopped.
  rc=0
  LOG_TRUNC=false
  # The children of this loop must not hold the job's lock (fd 8): a probe
  # that finds the lock free is how a dead job is recognised.
  while kill -0 "$cmd_pid" 2>/dev/null; do
    if [ "$(log_size "$JD" 8>&-)" -gt "$MAX_LOG_FILE" ]; then
      LOG_TRUNC=true
      kill_job_procs "$id" || true
      break
    fi
    sleep 0.2 8>&-
  done
  wait "$cmd_pid" || rc=$?
  ended=$EPOCHSECONDS

  # 6. Nothing the command started may outlive it.
  UNRESOLVED=0
  kill_job_procs "$id" || true

  state=succeeded
  kind=
  stage='command'
  reason=
  code=null
  if [ -e "$JD/cancel" ]; then
    state=cancelled
    kind=user
    reason="cancelled by the user"
  elif [ "$LOG_TRUNC" = true ]; then
    state=failed
    kind=user
    reason="the command wrote more than $MAX_LOG_FILE bytes of output and was stopped"
  elif [ "$rc" -eq 0 ]; then
    code=0
  elif { [ "$rc" -eq 124 ] || [ "$rc" -eq 137 ]; } && [ $((ended - started_epoch)) -ge "$JTIMEOUT" ]; then
    # Only a run that lasted the whole timeout is a timeout: 124 and 137 are
    # also what a command's own timeout or an out-of-memory kill gives.
    state=timed_out
    kind=user
    reason="the command ran over its timeout of $JTIMEOUT seconds"
  else
    state=failed
    kind=user
    code=$rc
    reason="the command exited with status $rc"
  fi
  if [ "$UNRESOLVED" = 1 ]; then
    reason="${reason:+$reason; }cleanup_unresolved"
  fi

  # 7. gen: the patch, then a clean worktree.
  TREE_DIRTY=false
  ts=$(gitw -C "$WT" status --porcelain --untracked-files=no 2>/dev/null) || ts=
  if [ -n "$ts" ]; then
    TREE_DIRTY=true
  fi
  PATCH_FIELD=
  if [ "$JKIND" = gen ] && [ "$state" = succeeded ]; then
    if gitw -C "$WT" add -A -N >/dev/null 2>&1 \
      && gitw -C "$WT" diff --binary --full-index --no-ext-diff --no-textconv --no-renames \
        --src-prefix=a/ --dst-prefix=b/ HEAD >"$JD/patch.tmp" 2>/dev/null; then
      mv -f -- "$JD/patch.tmp" "$JD/patch"
      ts=$(sha256sum -- "$JD/patch")
      PATCH_FIELD=",\"patch_sha256\":\"${ts%% *}\""
    else
      rm -f -- "$JD/patch.tmp"
      state=infra_failed
      kind=infra
      stage='patch'
      code=null
      reason="cannot compute the patch"
    fi
  fi
  if [ "$JKIND" = gen ]; then
    reset_worktree || true
  fi

  # 8. manifest.json and history.jsonl.
  duration=$((ended - started_epoch))
  queue_s=$((started_epoch - JCREATED))
  cores=$(nproc 2>/dev/null) || cores=0
  unamestr=$(uname -sr 2>/dev/null) || unamestr=
  manifest="{\"schema\":$SCHEMA,\"id\":\"$id\",\"sha\":\"$JSHA\",\"verified_head\":\"$head\",\"host_id\":\"$HOST_ID\",\"started_at\":\"$started_iso\",\"ended_at\":\"$(iso_now)\",\"duration_s\":$duration,\"queue_s\":$queue_s,\"cores\":$cores,\"uname\":$(js "$unamestr"),\"tools\":{$(tool_versions)},\"tree_dirty_after\":$TREE_DIRTY}"
  put "$JD/manifest.json" "$manifest"
  printf '{"schema":%s,"id":"%s","kind":"%s","state":"%s","queue_s":%s,"duration_s":%s,"ended":%s}\n' \
    "$SCHEMA" "$id" "$JKIND" "$state" "$queue_s" "$duration" "$EPOCHSECONDS" >>"$ROOT/history.jsonl" || true

  # 9. exit.json last; the ticket is removed by job_exit.
  write_exit "$id" "$state" "$kind" "$stage" "$reason" "$code" "$LOG_TRUNC" "$TREE_DIRTY" "$PATCH_FIELD" || true
  exit 0
}

# ------------------------------------------------------------------ main

main() {
  if [ "${1:-}" = "__job" ]; then
    job_main "${2:-}"
    exit 0
  fi
  exec 3>&1 1>&2
  trap on_exit EXIT
  case $ROOT in
    /?*) ;;
    *) die bad_request "GATEHAUL_ROOT must be an absolute path" ;;
  esac
  read_header
  case $SUB in
    probe) cmd_probe ;;
    submit) cmd_submit ;;
    wait) cmd_wait ;;
    log) cmd_log ;;
    cancel) cmd_cancel ;;
    gc) cmd_gc ;;
    *) die bad_request "unknown subcommand: $SUB" ;;
  esac
  exit 0
}

main "$@"
