# gatehaul technical specification (v0.1)

This document describes what `gatehaul` does and how: the interface, the protocol between the laptop and the host, the runner, the job states and patch sanitising. The code refers to its sections (for example `docs/SPEC.md §5.2`).

## 1. Purpose

In any GitHub repository, once `gh` is logged in, `gatehaul run -- go test ./...` works without a configuration file. Jobs run in the user's codespaces, outlive the agent session that started them, and never turn green without proof of execution.

The v0.1 backend is GitHub Codespaces, driven by `gh`. The `Transport` and `Lifecycle` interfaces (§6) will allow other hosts (SSH, Docker) to be added without a redesign.

## 2. What is guessed, and how

| Information | Source | Fallback when missing |
|---|---|---|
| `gh` binary and its version | `gh --version` (minimum 2.40) | `doctor` explains how to install or update it |
| Account and authentication | `gh auth status --active --json hosts` | `doctor` → `gh auth login` |
| `codespace` scope | Scopes field of `gh auth status` | `doctor` → `gh auth refresh -h github.com -s codespace` |
| `owner/repo` repository | `git remote get-url origin` (then `upstream`, then the only GitHub remote). The URL is parsed whether it is SSH or HTTPS. | `--repo owner/repo`. When no remote points to GitHub, the error says what the remotes point to (`origin is <host>`, `a local path`, or no remote at all) and its `Fix:` leads with the read-only fixes: run gatehaul from the repository's checkout, or pass `--repo`. The usual cause is the wrong directory, so `git remote add` is only mentioned for a checkout with no remote, as a step for the user. |
| What is sent | The **commit** of `HEAD`, or the reference passed as an argument. A dirty working tree triggers a warning, never an upload: modified tracked files (`dirty_tree`: not committed, so not tested) are told apart from untracked files (`untracked_files`: not in git, so not sent; a softer message, since it is often junk such as a crash dump). | Clear error: "commit first" |
| Usable machines | `gh codespace list --repo R --json name,displayName,state,machineName,repository,lastUsedAt`, filtered on the **display prefix `gatehaul-`** | `gatehaul up` (§4) |
| Default machine type | `gh api repos/R/codespaces/machines` → the smallest allowed machine | `--machine` |
| What the host already knows | The probe (§5.1) returns the `refs/gatehaul/*` references and the runner version. The controller derives the base of the incremental bundle from them. | Full bundle |
| Gate *(planned)* | `gatehaul.toml` if it exists. Otherwise, detection: `go.mod` → `go test -json ./...`; `package.json` with a `test` script → `npm test`; `Cargo.toml` → `cargo test`; `pyproject.toml` → `pytest --junitxml`. `doctor` also searches subdirectories (breadth first, 3 levels, skipping dot directories, `node_modules`, `vendor`, `target`, `dist`, `build`, `venv`, `__pycache__`) when the root has nothing, and reports `gate_in_subdirectory` with a runnable `gatehaul run --cd <dir> -- <cmd>`. The gate is only used by `gatehaul gate`; `gatehaul run -- <cmd>` never needs it, so a missing gate never blocks `run`. | `gatehaul gate -- <cmd>` with the regex parser |
| Minimum test threshold *(planned)* | **Ratchet**: the largest number of tests already seen on the branch or its base, and never 0. It is stored **on the laptop**, in the ref `refs/gatehaul/ratchet/<key>`, updated by compare-and-swap (`git update-ref <ref> <new> <old>`). | `min_tests` in `gatehaul.toml` |
| Gate policy *(planned)* | Fingerprint of the effective policy (stages, commands, parsers, thresholds, allowed skips), approved on first use and stored in the local cache | Any later change requires `--accept-drop` (code 4 otherwise) |

**What is never guessed**, because the decision belongs to a human: creating a codespace (which uses quota), lowering a threshold or changing the gate policy, and deleting anything. `gatehaul.toml` lives in a repository that the agent edits: the agent may write its first version, but may never approve, on its own, a relaxation of its own gate.

## 3. Interface

| Command | Effect | Exit codes | State |
|---|---|---|---|
| `gatehaul doctor [--json]` | Checks how `next:` lines will name gatehaul (`path`), `gh`, authentication, the scope, git ≥ 2.31, the repository, the commit, the gate and the presence of `gatehaul-*` codespaces. For each problem, returns **the command that fixes it**. | 0 ok · 5 to fix | done |
| `gatehaul up [-n 1] [--machine M] [--max 2] [--yes] [--dry-run]` | Creates the missing `gatehaul-<i>` codespaces, with explicit `--idle-timeout`, `--retention-period` and `--default-permissions`, and prints the cost. Asks for confirmation in a terminal; **without a terminal and without `--yes`, fails with a `Fix:` line** instead of waiting for an answer that an agent will never give. | 0 · 5 | done |
| `gatehaul down [--delete]` | Stops (or deletes) the `gatehaul-*` codespaces whose queue is empty | 0 | done |
| `gatehaul run [<ref>] [--prio P] [--timeout D] [--block 5m] [--cd DIR] -- <cmd>` | Sends the commit, queues and runs. Blocks for at most `--block`, then hands back control with the id. `--cd DIR` runs `<cmd>` in `DIR`, a directory of the repository **relative to its root** (default: the root); `-C` stays the local directory gatehaul starts from. `DIR` is validated on the laptop (relative, no `..` escaping the root, no drive or control character; usage error with a `Fix:` otherwise). The controller puts a guard line before the command (`cd -- DIR 2>/dev/null \|\| { echo '…' >&2; exit 1; }`, `DIR` shell-quoted when needed), so the runner protocol is unchanged, the fingerprint covers it, the job record keeps the command as typed (retry hints read `gatehaul run --cd DIR -- <cmd>`), and a directory missing from the commit fails the job with a clear message. | 0 green · 1 red · 3 running · 5 infra | done |
| `gatehaul gen [<ref>] [--cd DIR] [--allow-sensitive] -- <cmd>` | Like `run`, then applies to the local working tree the patch of the files that changed, after sanitising (§7) | same, plus 6 patch refused | done |
| `gatehaul apply [<id>] [--allow-sensitive]` | Replays the patch application of a finished `gen`; never applies twice | 0 · 3 · 5 · 6 | done |
| `gatehaul wait [<id>...] [--any\|--all] [--block 5m]` | Idempotent, never cancels. Returns `completed_ids` and `pending_ids`. Without an id, targets the one unfinished job of `HEAD`; a unique prefix of the ULID is enough. Applies the patch of a `gen` that has just finished. | as `run` | done |
| `gatehaul log [<id>] [--grep RE] [--since BYTES] [--tail N]` | Full or filtered log, with a byte cursor returned on every call and a cap per response (100 KiB) to protect the agent's context | 0 | done |
| `gatehaul status [<id>]` · `gatehaul queue` | **Read-only**, never waits, and **never starts a stopped host** (waking a codespace costs quota, and a stopped one runs nothing). Without an id (`queue` is the same): one line per host with its state; a stopped or starting host is reported `stopped (jobs not checked)` (or its state) without any connection; a running host is probed once (§5.1) and lists its running job and its queue in order, each with id, state, queue position, age, ref and the first characters of its command (untrusted data). With an id: the job's state, from the local cache when it has ended, else from one `wait` call with a slice of 0 and no log tail when its host is running, else the last state seen from here, marked as not checked. A `next:` line when something is left to do: `wait` on the unfinished jobs dispatched from this machine, `log` for a failed job, `apply` for a `gen` whose patch is not applied, the command again when its host is gone. An unknown id is an error (`no_such_job`). | 0 · 5 | done |
| `gatehaul cancel [<id>]` | TERM to the session group, KILL after 10 s, then a sweep of the processes marked `GATEHAUL_JOB` (the runner already supports this) | 0 | planned |
| `gatehaul gate [<ref>] [--profile light\|full] [--accept-drop]` | Runs the gate. **The verdict is computed on the laptop.** | 0 · 1 · 3 · **4 false green** · 5 | planned |
| `gatehaul hook claude-stop` | Claude Code `Stop` hook: exits 2 if the last green gate is not on `HEAD` | 0 · 2 | planned |

**Output:** `--json` on every command. The default text is short and readable by an agent: first line `job=<id> state=<s> exit=<n>`, then the end of the log, then a `next: <command>` line when something is left to do: **no `next:` line means nothing is left to do** (a succeeded `run`, for example). The command names gatehaul so that it runs as written: `gatehaul` when the `gatehaul` found on `PATH` is the running binary; otherwise the running binary's absolute path (forward slashes, which bash, PowerShell and cmd accept; single-quoted when it holds spaces), since some agent sessions lack `~/.local/bin` on `PATH`. `GATEHAUL_NEXT=<prefix>` overrides it, for a wrapper script that adds a prelude or fixed options (`export GATEHAUL_NEXT='bash scripts/gatehaul.sh'`). The same prefix starts the `Fix:` lines that name a job (`wait`, `log` or `apply <id>`, or the command that runs a job again); `doctor` reports it in its `path` check, a warning (`not_on_path`) when `PATH` lacks `gatehaul` or holds another binary. A failed or unfinished job also gets one diagnostic line, `# kind=… stage=… code=… reason=…`, with only its non-empty fields; a succeeded job has none. Before sending, `run` and `gen` print one progress line on stderr, `sending <ref> @ <sha7> to gatehaul-1…` (with `, in <dir>` for `--cd`, and `(waking it, ~20 s)` or `(it is Starting; waiting for it)` when the host is not running). With `--json` that line is left out, since the report carries `ref` and `sha`; only `waking gatehaul-1…` or `gatehaul-1 is Starting; waiting for it…` is printed when the host is not running. The `--json` job objects carry `sha` and `ref` (and `dir` with `--cd`). Data goes to stdout, errors and progress to stderr. **Every error carries a `Fix:` line** and a stable `code` in the JSON (`outcome_unknown`, `scope_missing`, `host_replaced`, `patch_refused`, `stale_base`…). Job logs are preceded by a line that marks them as untrusted data.

**States and exit codes are two separate things.** States: `queued`, `running`, then a terminal state that is never rewritten (`succeeded`, `failed`, `timed_out`, `cancelled`, `lost`, `infra_failed`), with `failure_kind` (`user`, `infra`, `lost`, `gate`) and `stage`. There is a single truth table, in `internal/job/state.go`; an unknown state is an error, never a default value. Mapping: `succeeded` 0; `failed`, `timed_out` and `cancelled` 1; `queued` and `running` 3; `lost` and `infra_failed` 5. Only `run`, `gen`, `wait` and `gate` return 3; `log` and `status` return 0 as soon as they could read.

**Commands run with `pipefail`** (§5.2): a pipeline fails when any of its commands does, so `make lint 2>&1 | head -40` fails with status 141 (SIGPIPE) once `head` stops reading, even when `make lint` passed. To trim output, read the log with `log --tail N` or `--grep RE` instead. When a command fails (`kind=user stage=command`) with a status above 128, the reason spells out the signal (128 + N): `status 141 (SIGPIPE: a reader such as "| head" closed the pipe early; jobs run with pipefail, …)`, `137 (SIGKILL: killed, often out of memory, or by a timeout inside the command)`, `143 (SIGTERM: …)`, and a generic `usually killed by signal N` for the others. Only an overrun of the job's own `--timeout` gives `timed_out`.

**Job id:** `<host number>-<ULID>`, for example `2-01JB…`. The number comes from the display name `gatehaul-2`: routing works without a local index, and the index is only a cache.

**Idempotent submission:** the controller makes the ULID and writes `jobs/<id>/dispatch.json` in its cache *before* opening the connection. A `submit` replayed with the same id and the same fingerprint returns the existing job (`exists`); with a different fingerprint, it answers `conflict`. If the answer is lost, the exit code is 5 with `outcome_unknown` and `next: gatehaul wait <id>`: never a job failure, never a blind replay.

## 4. Codespace lifecycle

- **Naming:** the display name `gatehaul-<i>` is the label. The real (random) name is never hard-coded: it is rediscovered on each call, with a 60 s cache. Two codespaces with the same number are an error with a `Fix:`.
- **Creation** (`gatehaul up` only, never implicit): `gh codespace create -R R -b <default branch> -m M -d gatehaul-<i> --idle-timeout 30m --retention-period 72h --default-permissions`. A detached job with no open SSH session dies at the idle timeout (measured: shutdown 5 min after the last session with `--idle-timeout 5m`), whereas an open session, even a silent one, keeps the host awake; hence 30 min by default and `wait` in slices.
- **Wake-up:** if a chosen host is `Shutdown`, the first `gh codespace ssh` starts it (about 20 s measured). The controller prints `sending main @ 0ca67e7 to gatehaul-2 (waking it, ~20 s)…` (`waking gatehaul-2…` with `--json`).
- **Shutdown:** `gatehaul down`. A host whose queue is not empty is left in place, with the reason.
- **Cap:** by default, `up` refuses to go beyond 2 codespaces (Codespaces terms of use). `--max` raises it.
- **Concurrent creation:** `up` takes a local file lock during `list` then `create`, and reads the list again after getting the lock: two agents started at the same time do not exceed the cap. Any host still listed holds its number, even while it is being deleted.
- **States:** those of `gh` (`Available`, `Starting`, `ShuttingDown`, `Provisioning`, `Shutdown`, `Failed`…) are sorted into `Running`, `Busy`, `Stopped`, `NotFound` or `Failed`. An unknown state is never `Running`: it marks the host concerned as unusable, without blocking the others. `GH_HOST` is respected and `gh auth switch` is never called.
- **Replacement:** `/workspaces/.gatehaul/host.id`, drawn at random at install time, is returned by `probe` and copied into every job. A codespace deleted then recreated under the same name gives `lost` with `host_replaced`, not "unknown job".

## 5. Host side: the runner

A single script, `runner.sh`, is embedded in the binary with `go:embed`. It is installed under `/workspaces/.gatehaul/runner-<short sha256>.sh`. `/workspaces` is persistent; `/tmp` is wiped each time the codespace stops.

**The `GATEHAUL1` protocol.** The remote command line is always `bash -s`; everything goes through stdin, in this order:

1. A preamble, called on a single line so that bash has read all of it before executing anything. It prints `GATEHAUL1 HELLO <version>` (proof that the remote script started), reads exactly the length of the runner with `LC_ALL=C read -N`, installs it if it is missing or if its sha256 does not match (written to a `.tmp` then `mv`, a single winner), checks that the root is not mounted `noexec`, then runs the runner.
2. The runner itself (a few dozen KB).
3. The request header: a `GATEHAUL1 <subcommand>` line, `key=value` lines, then an empty line. Values are restricted to `[A-Za-z0-9._:,/+=-]`; free text (command, search pattern) is base64 in a key ending in `_b64`. The runner validates every value and never evaluates any of them.
4. The payload, for `submit` only: exactly `bundle_len` bytes of git bundle.

The response is `GATEHAUL1 BEGIN`, one line of JSON, then `GATEHAUL1 END <rc>`. The controller ignores remote shell noise before `HELLO`. A stream with `HELLO` but no `END` was cut: for a `submit`, the outcome is unknown (§3). A cut before `HELLO` can be retried, since nothing was done.

```
/workspaces/.gatehaul/           0700 ; the runner runs with umask 077, the job command with umask 022
  runner-<hash>.sh
  host.id              random host identifier (§4)
  failed               present only if the install failed
  history.jsonl        past durations
  repo.git/            bare repository; refs/gatehaul/<key> = last commit received
  wt/<key>/            one worktree per branch
  jobs/<id>/           bundle, cmd.sh, meta.json, log, started, lock, sid, manifest.json, patch, exit.json
  queue/<prio>-<seq>-<id>   waiting tickets
  lock                 lock for the execution turn
```

**`<key>`** replaces the raw branch name: a `[a-z0-9-]` slug of at most 32 characters, followed by 6 hex digits of the sha256 of the full ref (`a/b`, `A` and `é` do not collide). The controller computes it, and the runner validates it again before any path concatenation. Every state file is written to a `.tmp` then moved with `mv`, carries a `schema: 1` field, and `exit.json` is **written last, once only**.

### 5.1 Runner subcommands

Each one is **a single SSH round trip**, which costs about 5 s with `gh codespace ssh` (measured, warm).

| Subcommand | Role |
|---|---|
| `probe` | Returns the runner version, `host_id`, `boot_id`, the queue depth, the running job and its age, the unfinished jobs (`jobs`: the running one, then at most 50 queued ones in queue order, each with its id, state, queue position, age, priority, branch key, kind and the first 512 bytes of its command in base64), the `refs/gatehaul/*`, the tools present, the free disk space and whether a token variable is present. **Also starts reconciliation** (§5.3). |
| `submit` | Receives the header and the bundle. Idempotent (§3). Verifies the bundle (`git bundle verify`, `list-heads` contains the expected SHA, `transfer.fsckObjects`, size capped at 2 GiB), fetches it, writes the ticket, detaches the job with `setsid` and returns the id. If a prerequisite of the incremental bundle is missing, it answers `need_full` and the controller sends a full bundle under the same id. |
| `wait` | Waits at most 60 s for one or more jobs to finish, then returns their state and the end of their log. Imposed order: read `exit.json`, drain the log to the end, then answer. The controller chains independent slices; no orphan wait is left on the host. |
| `log` | Full log, filtered (`grep -E`) or from a byte cursor, cut at the last line end, with `cursor` and `truncated` |
| `patch` | The patch of a `gen` that succeeded, in base64, with its recomputed sha256 and its size (8 MiB at most); `no_patch` otherwise |
| `cancel` | TERM to the session group, KILL after 10 s, `GATEHAUL_JOB` sweep, ticket removal. Writes `cancelled` only if `exit.json` does not exist yet. |
| `gc` | Deletes jobs finished more than N days ago |

### 5.2 Life of a job (`__job` mode, detached)

1. `git fetch` of the bundle into `repo.git`, then update of `refs/gatehaul/<key>`. This step writes only to `repo.git`.
2. **Waiting for the turn**: a `flock` loop on the global lock, with the ticket at the head of `queue/`, sorted by (priority, sequence). The wait is capped at 60 min.
3. **Under the lock only**: `git worktree prune`, worktree reset (`reset --hard` and `clean -ffdx`, keeping a short list of caches), then `checkout --detach <sha>`. A worktree that `rm` cannot delete (files owned by another user) is set aside, and `gc` purges it.
4. **Check that HEAD equals the sent SHA.** Otherwise, `infra_failed` with `stage=verify-sha`.
5. Write `started` and the job identity (`sid`, start time read from `/proc/<pid>/stat`, `boot_id`), then run `setsid --wait timeout --signal=TERM --kill-after=10s <duration> bash -o pipefail cmd.sh`, under umask 022, with `GATEHAUL_JOB=<id>` in the environment. **The token variables (`GITHUB_TOKEN`, `GH_TOKEN`, `GITHUB_CODESPACE_TOKEN`…), `GIT_ASKPASS`, `SSH_ASKPASS` and `SSH_AUTH_SOCK` are removed**, and the *credential helper* is disabled. Known limit: a codespace also keeps tokens in readable files under `/workspaces/.codespaces/shared/`, which the command can read; run only trusted projects there. The job holds `jobs/<id>/lock` for its whole life, but this descriptor is closed for the command, so that a daemon started by the build does not inherit the lock. The execution timeout (`--timeout`) is distinct from the wait for the turn; only an actual overrun of this timeout gives `timed_out`.
6. TERM to the session group, KILL after 10 s, then a bounded sweep of `/proc/*/environ` to kill whatever carries `GATEHAUL_JOB=<id>`, including processes that called `setsid` again. If any remain, the reason mentions `cleanup_unresolved`.
7. For a `gen`: the patch is computed **against the verified SHA, never against HEAD** (the command may have committed or switched branch), with a private index: `read-tree <sha>`, `add -A` (leaving out untracked files that existed before the command, such as the kept caches), then `diff --cached --binary --full-index --no-ext-diff --no-textconv --no-renames <sha>`, with explicit `--git-dir` and `--work-tree`. The computation is bounded (120 s, 8 MiB); beyond that, the job ends `failed` at stage `patch` with a clear reason. Its `patch_sha256` and size go into `exit.json`. The worktree is then reset.
8. Write `manifest.json`: verified SHA, durations, cores, `uname`, toolchain versions read in the job's shell, and `tree_dirty_after` (a gate that modifies tracked files does not give a green). Append the duration to `history.jsonl`.
9. **Write `exit.json` last** (`schema`, `state`, `exit_code`, `failure_kind`, `reason`, `stage`, `truncated`, start and end times), then remove the ticket. The log is capped at 16 MiB while running: beyond that, the command is stopped (`failed`, `truncated`), because cutting the log afterwards would invalidate the cursors already returned by `log --since`.

All the runner's git calls go through a single wrapper: `-c core.hooksPath=/dev/null`, `-c credential.helper=`, `-c gc.auto=0`, `-c protocol.allow=never` (except `file`), `init --bare --template=`, `GIT_TERMINAL_PROMPT=0`, `LC_ALL=C`, and `rev-parse --verify --end-of-options <rev>^{commit}`, refusing any reference that starts with `-`.

### 5.3 Reconciliation (on every `probe`, `wait`, `log` and `cancel`)

- The `jobs/<id>/lock` lock is free, `started` exists and `exit.json` does not: the job is `lost` (codespace restarted, process killed). Its `exit.json` is written and its ticket removed. **A job whose `started` exists is never relaunched.**
- A ticket without `started` whose process has disappeared (different `boot_id`, or free lock after a 60 s grace period) is **relaunched once**, at its place in the queue. Lost a second time without having started, it ends `infra_failed` at stage `queue`.
- Only what matches the triple (`sid`, start time, `boot_id`) is signalled or counted as alive: a pid proves nothing after a reboot.
- The global lock held by a dead process is released by `flock`.
- `git worktree prune` cleans up ghost worktrees.

## 6. Laptop side: the Go controller

```
cmd/gatehaul/               main, subcommand routing (stdlib flag)
internal/out/               text and JSON output, stable codes, Fix: lines
internal/clock/             injectable clock: no real waiting in tests
internal/ghx/               running gh and git behind a Runner interface (argv, stdin, timeout, never a shell), classification of gh errors
internal/detect/            repository, remote, ref → SHA, working tree state, gate detection
internal/doctor/            cumulative diagnosis
internal/host/              Host; Transport, Lifecycle and Backend interfaces
internal/host/codespaces/   Lifecycle (list/create/stop/delete) + Transport (gh codespace ssh -c N -- bash -s)
internal/host/fake/         local bash -s Transport in a temporary folder (tests and CI)
internal/host/hosttest/     conformance suite run against each backend
internal/runner/            embedded runner.sh, preamble, protocol, client
internal/bundle/            incremental base, git bundle create
internal/job/               ids, states, local job cache
internal/lock/              portable file lock (gofrs/flock)
internal/patch/             sanitising (§7) and patch application
internal/ctl/               run, gen, apply, wait, log, status, up, down
```

**External dependencies:** `gofrs/flock` (portable file lock, Windows included) and `golang.org/x/sys` (detecting a real terminal). No SSH in Go, no GitHub API client: `gh` does everything, and it is the one that holds the token. `gatehaul` never reads, stores or prints a token.

**Transport:** the runner client knows whether the remote script started (the `HELLO` line). Before it starts (codespace waking up, SSH refused), it retries with an increasing delay for at most 5 minutes, except for a permanent `gh` error (missing, not logged in, scope missing), which is raised at once with its `Fix:`. After it starts, it never retries. A `gh` error never proves that a host is absent. SSH options are passed after `--` (`-T`, `BatchMode=yes`, `ForwardAgent=no`, `ClearAllForwardings=yes`, `ConnectTimeout=8`, `ServerAliveInterval=15`), I/O is bounded in both directions, and the `gh` process tree is killed on cancellation (on Windows, `gh` launches a child `ssh`).

**The *fake* backend is the second backend:** it runs the real runner in a local folder, which makes it possible to play the whole conformance suite in CI without a codespace.

## 7. Patch sanitising (`gen`, `apply`)

The patch comes from a host that runs code written by an agent: it is **hostile input**. It is sanitised on a **tree diff**, never on its text.

1. The sha256 of the received bytes must be the one the runner announced.
2. Before any call to git, the `GIT binary patch` sections are read: the declared size of a `literal` and the resulting size of a `delta` (read from the zlib header, without decompressing everything) are compared to the caps, so that a patch of a few bytes cannot make git balloon to several GiB.
3. The patch is applied to a **temporary index** (`GIT_INDEX_FILE`) and a **temporary object directory** (`GIT_OBJECT_DIRECTORY`, with the repository's objects read-only through `GIT_ALTERNATE_OBJECT_DIRECTORIES`): `read-tree <base>`, `apply --cached --binary`, `write-tree`. Nothing is written to the user's repository.
4. The result is read with `git diff-tree -r -z --raw --no-renames <base> <new tree>`, then each entry is validated:
   - modes limited to `100644` and `100755`: symbolic links and submodules refused;
   - relative paths, with no NUL, control character, backslash, `:`, `..` component, drive prefix, or trailing dot or space;
   - `.git` component refused regardless of case, of characters ignored by HFS+, and of NTFS aliases (`git~1`, `::$DATA` stream);
   - **Windows 8.3 aliases** refused in any component (`WORKFL~1`, `GATEHA~1.TOM`);
   - Unicode case collisions and file/directory conflicts refused, by querying git directory by directory rather than listing the whole tree;
   - LFS pointers refused; caps per file (16 MiB), in total (64 MiB) and in number of files (2000), applied to new blobs only;
   - binary file detected by a NUL byte in the first 8000 bytes (flagged, not refused).
5. **Sensitive paths**, compared in normalised form (case folded, ignorable characters removed): `gatehaul.toml`, `.claude/**`, `.codex/**`, `CLAUDE.md`, `AGENTS.md`, `.github/workflows/**`, `.githooks/**`, `.husky/**`, `Makefile`, `package.json`, `*.sh`, `.envrc`, `.gitattributes`, `.gitmodules`. They are listed in `sensitive_paths`. `gatehaul.toml`, workflows and hooks are **refused** without `--allow-sensitive`.
6. All problems are listed, never only the first. A refused patch exits with code 6, `patch_refused` and the list; nothing is applied.

**Application:**

- The patch is bound to its origin: the working tree must be the one the job started from (`wrong_repo` otherwise), and HEAD must equal the sent commit, or descend from it without having touched the patch's files (`stale_base`, code 6, otherwise).
- A per-job lock serialises `gen`, `wait` and `apply`; `applied.json` prevents any double application, and a patch already present in the tree (checked with `git apply --reverse --check`) is recognised as applied.
- Before writing, the files concerned are copied into the job's cache, with an `applying` marker. The application itself cannot be interrupted by Ctrl-C; if it fails, or if a marker is left after a crash, the original files are restored before any other action.
- `git apply --check` then `git apply --binary` write to the working tree, never to the index. A conflict exits with code 6, `patch_conflict` and `Fix: … gatehaul apply <id>`.
