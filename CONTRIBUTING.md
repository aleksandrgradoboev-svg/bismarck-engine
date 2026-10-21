# Contributing

Bug reports, test runs on your platform, ports of pi changes and fixes are all welcome. This file
says how the project checks its own work, so a contribution can pass the same bar.

## Before you start

- **Open an issue first** for anything bigger than a fix: a port of a pi change, a new package, a
  new dependency. The project tracks pi closely, and some of pi is out of scope on purpose (see
  "Differences from pi" in the [README](README.md)).
- **New third-party modules need a ruling.** The build may reach only the modules listed in
  `internal/policy` (the standard library, `golang.org/x/{image,text,sys}` and `wazero`), and a test
  enforces it; the SQLite driver lives in its own module, `durable/storage/sqlite/modernc`.
- **No cgo** in the root module; the same test checks it.

## Build and test

```bash
gofmt -l .                 # must print nothing
go vet ./...
go test ./...              # live-API tests skip without keys
(cd durable/storage/sqlite/modernc && go test ./...)
```

CI also runs, and a change to concurrent code should pass locally first:

```bash
go test -cpu 1 ./durable/codemode/... ./durable/harness/...   # one processor: goroutines run one at a time
go test -race ./durable/... ./codemode/... ./mcp/...           # needs cgo and a C compiler
```

The `stress` workflow repeats suites under the race detector (`workflow_dispatch`, or push a
`stress/**` branch) when a test looks flaky.

## What a good change carries

- **One step per pull request.** A pull request ports one pi change, fixes one defect or adds one
  thing; a step that grows past about 1,500 lines of code is split before work starts.
- **A test that fails without the change.** For a fix: the test fails on the old code with the
  defect's own symptom. For a race: a delay that widens the window (a sleep in the code under test,
  removed before commit) shows the old code failing and the new one passing.
- **Sabotage for new mechanisms.** Turn each guard off in turn (replace a condition with `true` or
  `false`, drop a call) and check that some test fails. A guard no test notices is either dead code
  or an untested promise; say which in the pull request.
- **pi as the reference.** A port says which pi commit it follows (`port(area): … (upstream
  <commit>)`), and a deliberate difference from pi is written down: in the README for user-visible
  ones, in `docs/sync-v1.2.0.md` section 4 for durable.
- **No version bump.** The version lives in the git tag; releases are cut separately and announced
  on the [Releases page](https://github.com/aleksandrgradoboev-svg/bismarck-engine/releases).

## Reporting a bug

Say what you ran, on which OS and Go version, and paste the failing output in full. For a flaky
test, the failure message matters more than the reproduction: run it with `-count=50` (and `-cpu 1`
or `-race` if that is where it failed) and paste the first failure.

## License

By contributing you agree that your contribution is licensed under the project's
[MIT License](LICENSE).
