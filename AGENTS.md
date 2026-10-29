# Notes for agents and contributors

The design lives in [docs/SPEC.md](docs/SPEC.md); read it before changing behaviour. These rules keep the code honest:

- **Nothing builds on the maintainer's laptop.** CI on GitHub Actions is the build. Do not add steps that assume a local toolchain.
- **Every warning or error carries a fix.** Use `out.Warn` / `out.Fail` with a stable `out.Code` and a `Fix` the reader can run. A test validates every check that `doctor` emits.
- **No shell, no hidden I/O.** External commands go through `ghx.Runner` with an argv and a timeout. Tests use `ghx.Fake`, which fails on any unscripted command.
- **No sleeping in tests.** Waiting goes through `clock.Clock`; tests use `clock.Fake`.
- **gh holds the credentials.** gatehaul never reads, stores or prints a token.
- **Stable codes.** Exit codes (`internal/out`) and error codes never change meaning once released; add new ones instead.
- **Commands print data on stdout** (text or `--json`) and errors on stderr, ending with a `Fix:` line.
