# Runtime installation and diagnosis

[POLICY.md](../POLICY.md) owns repository rules and
[the operations contract](../skills/crw-run/references/operations.md) owns the operational ones.
This page describes the runtime entry point that installs and diagnoses the MCP bridge and the
session relay, and it is written against that contract's clause numbers so a reader can check a
claim against the rule it came from.

Two entry points exist and they are deliberately not one:

| Command | Installs | Contract |
| --- | --- | --- |
| `python3 scripts/install.py --check` or `--apply` | Skill links into Codex | OPS-2.3 |
| `python3 scripts/runtime_install.py` | The bridge, the relay, the MCP registration and the Linear hook | OPS-2.4, OPS-6.3 |

The first is unchanged by this page. It stays standard-library-only and idempotent, it refuses to
replace an existing directory or a foreign link, and runtime installation is never folded into it.
The runtime entry point reuses its `LINKED`, `MISSING` and `CONFLICT` vocabulary so one word means
one thing across both layers, and it reads the skill-link layer by running `scripts/install.py
--check` rather than by reimplementing it.

## The one definition

`scripts/crw_runtime/components.json` is the single compatibility definition OPS-1.1 requires.
Both installation and diagnosis read it; neither carries a second copy of a revision, a version or
a digest.

It carries only what this checkout can prove about itself. Every field is either re-derived from
the checkout at check time or marked with the OPS-0 status word that says it was not:

| Field | How it is established |
| --- | --- |
| `subdirectory`, `packageLocation` | Paths in this repository |
| `subdirectoryTree`, `packageTree` | `git rev-parse HEAD:<path>` |
| `sourceDigest` | The OPS-1.2 walk over the package directory |
| `version`, `requiresPython` | Read from the component's `pyproject.toml` |
| `upstream` remote, revision, tree and licence | `recorded`: carried from the import, not re-derivable here |
| `measuredPoints` | Empty, and `unmeasured`: this repository has exercised no combination |

`python3 scripts/runtime_install.py verify-definition` re-derives every derivable field and fails on
any disagreement, so the definition cannot drift from the source it describes. It runs in CI through
[scripts/ci/contracts.py](../scripts/ci/contracts.py). The upstream revision is not derivable from
this checkout, because the import brought source rather than history, so it is instead required to
appear in [packages/README.md](../packages/README.md), which is the provenance narrative OPS-1.5
says is retained rather than replaced. That keeps one machine-readable owner without letting the
prose and the definition disagree.

The repository commit is deliberately absent from the file. A commit SHA recorded inside the commit
it names is self-referential, so it is measured at run time and reported, never committed.

What the definition does **not** carry is as important. Installed locations, entry points,
interpreters, host names and measured points are host facts. OPS-3.2 makes a real record a private
receipt, so the runtime entry point writes those to a host record outside this repository and this
repository never commits one.

## The host record

The host record at `${XDG_STATE_HOME:-~/.local/state}/codex-relay-workflow/host-record.json` is the
other half of the definition and is never committed. It holds the repository commit and tree
measured at run time, checkout cleanliness, one entry per install location
(`location`, `installMode`, `entryPoint`, `entryPointTarget`, `environment`, `interpreter`,
`integrity`, `reachedVia`) and the measured points.

This is what makes reuse reachable. Under OPS-1.3 a point means the combination was exercised, so
no amount of reading bytes produces one, and a component whose bytes match but whose combination
nobody has run classifies `unmeasured` and is preserved rather than reused.

`runtime_install.py measure` is the operation that produces a point, and it has to run something
real. Starting a process is not exercising a combination: the bridge's entry point starts a stdio
server and never contacts the App Server, so a recipe built on startup would record success against
an unreachable host. `measure` therefore runs two actual operations under the resolved interpreter:

| Component | Operation | What makes it an exercise |
| --- | --- | --- |
| Relay | `codex-session-relay --socket <sock> --state <dir> doctor` | `actorReachability.socketConnect` is a real connect and must equal `ok` |
| Bridge | `packages/codex-thread-bridge/scripts/check_connection.py --socket <sock>` | The package's own read-only check starts the MCP server, lists its tools and calls `get_capabilities`, which is an App Server round trip |

That check is invoked, never modified or reimplemented. A connection, protocol or tool-call failure
records **no** qualifying point and the run reports why. The point records
`{interpreter, codexCli, appServer, host, date, measuredBy, method}` bound to one install location,
one combination and the `sourceDigest` it was measured against, so it is evidence tied to the bytes
it covers rather than an independently editable expectation. A point recorded against another
interpreter is a different combination and does not satisfy this one. Points are appended, never
replaced.

## Installing the runtime

`runtime_install.py install` refuses unless `verify-definition` passes, then resolves an
interpreter that satisfies both components' `requires-python`. The controller itself runs on
Python 3.10 for CI and never selects itself for a runtime that requires 3.11 or newer; when no
suitable interpreter exists it refuses and names the requirement.

The environment is created as a new directory, so an existing one is never overwritten. Each
module's imported location is then read back from the interpreter rather than assumed, because an
editable install leaves nothing under site-packages and a copied install does, and its OPS-1.2
digest is computed from whatever the interpreter actually resolved.

The candidate is then exercised, and the recorded pointer moves only after a qualifying point
exists for it. OPS-2.4 sequences an update as measure, install, measure again, and the second
measurement is the one that produces the point; promoting before it would select a runtime that
imports cleanly and fails the moment it is used. A candidate whose exercise fails stays unselected
and the previously selected runtime remains selected. A failure at any step leaves the previous
runtime in place, and nothing here removes, moves or recreates the store: update failure and store
loss are different accidents and the recovery for one must not cause the other.

## Installation ownership

Classification reads the four OPS-2.1 signals and nothing else: where the entry point actually
resolves, the checkout's commit, tree and cleanliness, whether the definition agrees with the
installed bytes, and what the Codex configuration registers. A signal that cannot be read is
reported as unreadable and stops classification; it never counts as a signal that agreed.

Cleanliness is read across the whole checkout, not only the component's subdirectory. An
uncommitted change to a root script or to another package leaves every package digest untouched
while the checkout is no longer the revision the definition names, and only the checkout-wide
reading catches it.

The five OPS-2.2 classes are evaluated in their fixed order and the first match wins:
`conflict`, `fork`, `foreign`, `unmeasured`, `own`. Only `own` is reused. Because the committed
definition carries no measured points, a host install whose bytes match still classifies
`unmeasured` until the host record carries a point for the combination it runs under. That is the
intended answer, not a gap to close by relaxing the rule: OPS-1.3 refuses to let a matching digest
stand in for a run nobody performed. `measure` is the supported way out, and it is the only one.

Nothing outside a recorded path is ever overwritten. A foreign relay, a local fork, an existing
directory, an existing link and an MCP name already registered with a different command are each
reported with both values, and the run changes nothing.

## MCP registration

The server is registered as `[mcp_servers.<name>]` in `<CODEX_HOME>/config.toml`, the supported
configuration path, through `runtime_install.py register-mcp`. Registration is append-only and
idempotent: an identical registration is reported `LINKED` and nothing is written, an absent one is
appended, and a different command or argument list is reported `CONFLICT` and nothing is written.
Every other table in the file, including other MCP servers and hook settings, is preserved byte for
byte, which the command checks by requiring the prior content to be an exact prefix of the new file
rather than by asserting it.

The reader is a deliberately small table-header scanner, not a general TOML parser, because the
`validate` and `tests` jobs run on Python 3.10 where `tomllib` does not exist. It models exactly one
shape, `[mcp_servers.<name>]`. A server's name is the first segment after `mcp_servers.`, and any
deeper segments are that server's own sub-tables: a real configuration on this host carries
`[mcp_servers.oracle.env]` and `[mcp_servers.codex-thread-bridge.tools.create_thread]`, and reading
either as a server name would append a duplicate registration for a server that is already there.
Bare and quoted spellings of a name normalise to one name.

Everything else makes the file unreadable, and an unreadable file is never appended to: an
array-of-tables header, a dotted or inline `mcp_servers` assignment, an unterminated multi-line
string, or the same server defined twice. Where `tomllib` is importable it additionally
cross-checks the scanner's reading and reports a disagreement as an unreadable signal, but the
Python 3.10 branch is protected by negative fixtures rather than by `tomllib`.

The reader walks characters rather than counting delimiters, because a quote only means what it
means outside a string. A single-quoted literal value can legitimately contain the three-quote
sequence that opens a multi-line string, and counting delimiters reads that as a fence which never
closes, silently skipping every table after it. Basic-string escapes are decoded before any value is
compared, an escape the reader does not model is reported rather than guessed at, and a member
assignment inside a parent `[mcp_servers]` table is unreadable for the same reason a dotted one is.

The property that fixes is a round trip, not three cases: **what the writer emits, the reader reads
back unchanged, and a rerun then answers `LINKED`** — including values carrying backslashes, quotes,
control characters and the three-quote sequence.

Writing is held under an exclusive lock for the whole read-modify-write, re-reads immediately before
replacing, and replaces by temp file. That coordinates runs of this command with each other and
removes truncation. It cannot coordinate with an editor that does not take the same lock, so it is
not called compare-and-swap: a writer ignoring the lock can still land in the remaining window. The
hook file is written the same way, with the same stated limit.

## Six results that never imply one another

Diagnosis reports the six OPS-6.1 fields separately, in the OPS-6.2 shape, each with its own
evidence, exact command, acting process and measurement time. A field with no timed observation
behind it reports `unknown`; no time is ever invented or copied from another field.

| Field | Established by | Never established by |
| --- | --- | --- |
| `installed` | The component classifies `own` | The package importing somewhere |
| `mcpExposed` | Registration plus tool names observed in a live session | A configuration entry |
| `connected` | `doctor` reporting `actorReachability.socketConnect` as `ok` | A socket file on disk |
| `deliveryAccepted` | An attempt that recorded a returned turn id | A dispatch or an absent error |
| `verificationComplete` | Every OPS-6.4 condition at once | A completed turn or a green check |
| `alwaysActive` | A supervised runtime surviving a host restart | Any of the five above |

A live session is the only thing that can list MCP tools, so `mcpExposed` stays `not_verified` on a
registration alone. It reaches `verified` from real tool names only: either `--observed-tool`
supplied by a caller that is itself in a live session, or the tool list the bridge's own
`check_connection.py` returns, which is an actual MCP session over stdio. Every record names the
destination it measured and whether that destination was temporary, so a temporary-destination
proof cannot be read as a claim about a host.

Two further results are reported beside those six and are never merged into them, because importing
and preserving settings are separately falsifiable:

| Result | Established by |
| --- | --- |
| `imported` | The module imported under the resolved interpreter, carrying the `__file__` it resolved to, so an import satisfied by another copy is visible |
| `settingsPreserved` | Every other table in `config.toml` and every other hook entry byte-identical before and after |

## Trial mode

Diagnosis creates no work. `deliveryAccepted` requires an attempt that recorded a returned turn id,
which means creating one, so it reports `not_applicable` unless `--trial` is given. Trial mode is
the only mode that registers a relationship and sends, it names the scope it acted in, and it is
never implied by any other flag.

The send has to go far enough to produce the evidence. `emit` stores the receipt and enqueues the
delivery; the attempt itself happens in `deliver`. So the trial registers, emits, and then runs a
bounded `deliver` for that one event, and the field's evidence is the attempt's returned turn id.
An emitted receipt's own turn id never satisfies it. The authorized recipient and the requested
settings are recorded before the send, and the settings the host reported back are recorded with the
result.

Everything the trial needs is checked before its first command, so an incomplete trial writes
nothing: an artifact, a dispatch turn, a recipient equal to the parent task, and the recipient's
authorized settings. Each of those was previously discovered at the relay, after rows had already
been written. Settings are supplied with `--recipient-settings`, or acknowledged with
`--settings-already-recorded` when they are already authorized on the host; that acknowledgement is
recorded as the caller's own unverified claim, because every relay read constructs a store and there
is no read-only way to confirm it from here. A false acknowledgement can still reach the relay and
leave rows behind.

Getting that far takes more than three commands, and each of the extra ones exists because the relay
refuses the send without it. Measured against a running App Server, the sequence is:

| Step | Why the send needs it |
| --- | --- |
| `assignment-find` | Runs first, before anything is written. A lookup run afterwards could find the relationship the trial itself just created, which says nothing about the store |
| `settings-record` | A send is withheld until the recipient's authorized settings are on record, because preserving them is what the delivery checks against |
| `register` | Creates the relationship and opens its first generation |
| `generation-open` | Replays that same dispatch request id to read the generation number back; it opens no second generation |
| `generation-bind` | The generation `register` opened is unbound, and an unbound generation cannot be emitted against |
| `admit-turn` | Only the anchor turn is admitted by default; a turn the child actually ran is a continuation |
| `emit` | Stores the receipt and enqueues it. It carries an artifact, because a reviewable receipt with an empty manifest is refused |
| `deliver` | The attempt itself, and the only step that can return the turn id this field needs |

Those invocations are built as data rather than inline, so a test can compare them against the
required arguments the relay's own parser declares without performing a delivery. That check reads
the parser statically, because the relay declares a newer Python than this repository runs its own
checks with, and it covers every command the trial can send rather than the ones a fixture happened
to build.

## One shared service and one store

OPS-3.1 puts one relay service and one durable store behind an entire operating scope, which is one
host, one OS user and one App Server. A second repository or a second project installs into that
same scope and reuses the same service and the same store; nothing here creates a daemon or a store
per project, per repository or per parent.

Diagnosis therefore reports the resolved scope, the store directory and database, the service owner
and whether a service is running, by calling the relay's own `doctor` and `service status` rather
than by rediscovering any of it.

`doctor` is called three times, because one call cannot answer all three questions.

A call that selects a store explicitly skips sibling discovery altogether and reports
`checked: false`, because the caller already decided which participants share that directory. That
applies to `CODEX_SESSION_RELAY_STATE` exactly as it applies to `--state`, so the **discovery** call
passes the socket, no `--state`, and a sanitized environment with that variable removed; inheriting
it would silently produce an empty conflict inventory. The **selected** call uses the explicit
`--state` for the store actually in use.

The third call is targeted at the root of the state home. Discovery enumerates child directories
only, so a `relay.sqlite3` sitting directly in `<state home>/codex-session-relay` is invisible to
both calls above. A host can be in exactly that state, so it is inspected explicitly rather than
left out of the inventory. All three results are reported, a `checked: false` is preserved as not
checked rather than as none found, and no candidate is adopted.

A relay build that reports no `siblingStores` at all is a third answer again, and it is reported as
its own: an installed relay older than the revision that added sibling reporting emits nothing for
that field, and rendering that silence as an empty inventory would hide exactly the conflict the
inventory exists to surface. The summary distinguishes not reported, not checked, and checked.

Because a relay cannot always answer, the inventory also lists every relay database and operations
ledger visible under the state home, each marked as listed rather than identified. Listing files is
not rediscovering anything: nothing opens a database, chooses between candidates or decides which
one serves a socket, and that judgement stays with the relay. It is there so a host whose installed
relay is too old to report siblings still sees every store it has.

Equality of path strings is not proof under OPS-3.4. Proof is `doctor` from each participating
process reporting the same `stateDirectory` together with `assignment-find --issue` returning the
expected relationship. A relationship count is reported as the weaker observation it is: a count of
zero where an assignment is expected means the process is pointed somewhere else, and a nonzero
count from a different populated database would satisfy a count check while proving nothing.

That lookup constructs a writable store, and a diagnosis constructs none, so plain `diagnose` does
not run it and says so rather than claiming it happened. It runs in the trial, before any write, and
its result is compared against the relationship the caller independently supplies with
`--expect-relationship`; a store that does not hold it stops the trial before anything is written.
`diagnose --assignment-lookup` runs the same lookup on its own where that is wanted, and is
documented as constructing a store. One command cannot produce the reading from every participating
process that OPS-3.4 also asks for, and the report says so.

Every subsequent call sets both selectors, `--state` and `CODEX_SESSION_RELAY_STATE`, to the same
resolved absolute path, because under OPS-3.3 the flag alone moves the store while leaving the
adapter's `operations-<scope>.sqlite3` ledger behind. That ledger is reported as its own artifact.

Other stores beside the resolved one are reported, never adopted and never hidden. `doctor` already
distinguishes stores that record no socket from stores claiming the same socket, and a host can
hold both alongside a separate `operations-<scope>.sqlite3` ledger, which OPS-3.3 explains is
selected differently from the store. A host in that state is reported as ambiguous with its
candidates listed, because adopting one on a guess is how the wrong store gets served.

## The Linear hook

`runtime_install.py hook` installs the next-step Linear hook into the user hook file. Under OPS-6.3
a hook's identity is `<source>:<event>:<matcher-index>:<hook-index>` and Codex records a trusted
hash against it, so installation appends at the end and never inserts: inserting renumbers every
later hook in the same file and detaches the trusted hash recorded against the old identity. For
the same reason a hook is disabled rather than deleted, and no content is silently updated.

The command records the identity, the trusted hash, the hook file path with its SHA-256 and the
issue that installed it, then reads the registration back. Installed, enabled and observed to have
fired are three separate claims and are reported as three. Installation is not activation: this
command never enables a daemon, and `alwaysActive` is a separate field with separate evidence.

What survives a hook installation is every existing identity and its hook content, so the trusted
hash Codex recorded against each one stays attached. The file bytes do not: the document is
reserialized. The MCP registration is the one that preserves bytes, by appending and leaving the
prior content as an exact prefix.

## What none of this establishes

Running the entry point against a temporary destination proves what it did there. It is not
evidence about a host's real Codex home, its installed runtime, its MCP registration or its
operational database. `installed`, `mcpExposed`, `connected`, `deliveryAccepted`,
`verificationComplete` and `alwaysActive` are six separate facts under OPS-6.1, and `imported` and
`settingsPreserved` are two more beside them. None of the eight is read from another.
