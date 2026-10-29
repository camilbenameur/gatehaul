# gatehaul

**Offload your coding agents' builds and tests to remote sandboxes, and get proof they actually ran.**

> **Status: early development.** Only `gatehaul doctor` and `gatehaul version` work today (milestone M1); the rest is planned.

## Why

Agents such as Claude Code or Codex run builds and test suites hundreds of times a day. On a laptop this is slow and noisy. Remote, it brings three problems that existing tools only partly solve:

- **Tool timeouts.** An agent cannot block for 20 minutes on a test run. It needs asynchronous jobs (`run`, then `wait`) that outlive its session.
- **False greens.** A gate that ran zero tests, used a missing toolchain or tested a stale commit must not pass. The agent is the one actor the gate has to constrain.
- **Secrets.** The sandbox runs code written by an agent. It should hold no credentials at all.

## What v0.1 will do

- **Zero configuration** on top of an authenticated [`gh`](https://cli.github.com/). The repository, account, hosts and test command are all inferred.
- **Your own GitHub Codespaces** as sandboxes, behind one queue spread over several machines. SSH and Docker backends come later.
- **Commits in, patches out.** Code is sent as an incremental `git bundle`. Generated files come back as a sanitized patch applied to your working tree.
- **Asynchronous jobs** that fit agent tool timeouts: `run` blocks for a bounded time, then hands back a job id for `wait` and `log`.
- **Gates with proof of execution**: zero tests is a failure, the test count can only go down with explicit human approval, and the verdict is tied to the commit. A false green exits with code 4.
- **No secrets in the sandbox.** Authentication stays in `gh` on your machine. GitHub's injected tokens are stripped from the job environment.
- **A Claude Code `Stop` hook** that refuses to end a turn without a green gate on `HEAD`.

Today:

```bash
go install github.com/camilbenameur/gatehaul/cmd/gatehaul@latest
gatehaul doctor                  # checks gh, auth, the codespace scope, git, the repo, the gate and the hosts
gatehaul doctor --json           # the same for agents, with where each inferred value came from
```

Planned for v0.1:

```bash
gatehaul up -n 2                 # create two codespaces (asks first: it uses your quota)
gatehaul run -- go test ./...    # send HEAD, queue, run; returns a job id if it takes long
gatehaul wait                    # wait for the pending job of HEAD
gatehaul gate                    # run the inferred gate and compute the verdict locally
```

## Fair use

gatehaul is meant for **your own account and your own repositories**, within your Codespaces quota. It is not a shared build farm.

## License

[Apache-2.0](LICENSE)
