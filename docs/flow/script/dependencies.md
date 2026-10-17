
## Dependencies and their supply-chain check

The loader and runtime add two direct dependencies and promote one. Each was checked as the slopsquatting gate
(`plugins/crw/skills/crw-dev-security`, section 6.5) asks: the module exists on the Go module proxy with a real release
history, its maintainer and source repository are the plausible ones, a Go module runs no install script (`go build` runs
only the compiler on the imported packages; the module's `go:generate` lines never run), and the `go.mod` and `go.sum`
diff was read line by line.

| Module | Pinned to | Why this pin | License | Check |
| --- | --- | --- | --- | --- |
| `github.com/dop251/goja` | `v0.0.0-20260917113740-793a2a65c13b` (commit `793a2a65c13b`, 2026-09-17) | The module publishes no tags, so the pin is a commit: the maintainer's merge of pull request 739 (2026-09-17), more than three weeks old at the time of the change. Two later commits older than two weeks (`for await` support and a `PromiseResolve` fix for promise subclasses) are not relied on. | MIT (Dmitry Panov, from Robert Krimen's otto) | History since 2016-11-04 (623 commits), maintainer `dop251`, source `github.com/dop251/goja`. |
| `github.com/evanw/esbuild` | `v0.28.2` (2026-08-08) | The newest tagged release older than two weeks. Only `pkg/api` is imported, so the build uses esbuild as a pure Go library. | MIT (Evan Wallace) | 438 tagged releases since v0.8.0 (2020-10-28), maintainer `evanw`, source `github.com/evanw/esbuild`. |
| `github.com/google/jsonschema-go` | `v0.4.3` (already in `go.mod`) | Moved from indirect to a direct dependency: the engine validates every call result with it. | MIT | Unchanged pin. |

goja brings four indirect modules: `github.com/dlclark/regexp2/v2` v2.5.2 (MIT), `github.com/go-sourcemap/sourcemap` v2.1.3
(BSD-2-Clause), `github.com/google/pprof` (Apache-2.0, a test-time dependency of goja) and `golang.org/x/text` v0.3.8
(BSD-3-Clause). Both direct modules build with `CGO_ENABLED=0`, so the release matrix needs no change.
