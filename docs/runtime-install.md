# Runtime installation and diagnosis

[POLICY.md](../POLICY.md) owns repository rules and
[the operations contract](../skills/crw-run/references/operations.md) owns the operational ones.
This page describes the runtime entry point that installs and diagnoses the MCP bridge and the
session relay, and it is written against that contract's clause numbers so a reader can check a
claim against the rule it came from.

Two entry points exist and they are deliberately not one:

| Command | Installs | Contract |
| --- | --- | --- |
| `python3 scripts/install.py` | Skill links into Codex | OPS-2.3 |
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
| `upstream` | `recorded`: carried from the import, not re-derivable here |
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

## Installation ownership

Classification reads the four OPS-2.1 signals and nothing else: where the entry point actually
resolves, the checkout's commit, tree and cleanliness, whether the definition agrees with the
installed bytes, and what the Codex configuration registers. A signal that cannot be read is
reported as unreadable and stops classification; it never counts as a signal that agreed.

The five OPS-2.2 classes are evaluated in their fixed order and the first match wins:
`conflict`, `fork`, `foreign`, `unmeasured`, `own`. Only `own` is reused. Because the committed
definition carries no measured points, a host install whose bytes match still classifies
`unmeasured` and is preserved rather than reused. That is the intended answer, not a gap to close
by relaxing the rule: OPS-1.3 refuses to let a matching digest stand in for a run nobody performed.

Nothing outside a recorded path is ever overwritten. A foreign relay, a local fork, an existing
directory, an existing link and an MCP name already registered with a different command are each
reported with both values, and the run changes nothing.

## MCP registration

The server is registered as `[mcp_servers.<name>]` in `<CODEX_HOME>/config.toml`, the supported
configuration path. Registration is append-only and idempotent: an identical registration is
reported `LINKED` and nothing is written, an absent one is appended, and a different command or
argument list is reported `CONFLICT` and nothing is written. Every other table in the file,
including other MCP servers and hook settings, is preserved byte for byte.

The reader is a deliberately small table-header scanner, not a general TOML parser, because the
`validate` and `tests` jobs run on Python 3.10 where `tomllib` does not exist. It models exactly one
shape and refuses everything else: an array-of-tables header, a dotted or inline `mcp_servers`
assignment, or an unterminated multi-line string makes the file unreadable, and an unreadable file
is never appended to. Where `tomllib` is importable it additionally cross-checks the scanner's
reading and reports a disagreement as an unreadable signal rather than proceeding.

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
registration alone; observed tool names are supplied explicitly with `--observed-tool` and recorded
as what they are. Every record names the destination it measured and whether that destination was
temporary, so a temporary-destination proof cannot be read as a claim about a host.

## Trial mode

Diagnosis creates no work. `deliveryAccepted` requires an attempt that recorded a returned turn id,
which means creating one, so it reports `not_applicable` unless `--trial` is given. Trial mode is
the only mode that registers a relationship and sends, it names the scope it acted in, and it is
never implied by any other flag.

## One shared service and one store

OPS-3.1 puts one relay service and one durable store behind an entire operating scope, which is one
host, one OS user and one App Server. A second repository or a second project installs into that
same scope and reuses the same service and the same store; nothing here creates a daemon or a store
per project, per repository or per parent.

Diagnosis therefore reports the resolved scope, the store directory and database, the service owner
and whether a service is running, by calling the relay's own `doctor` and `service status` rather
than by rediscovering any of it. Equality of path strings is not proof under OPS-3.4, so the report
carries `doctor`'s `stateDirectory` and its relationship count together: a count of zero where an
assignment is expected means the process is pointed somewhere else.

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

## What none of this establishes

Running the entry point against a temporary destination proves what it did there. It is not
evidence about a host's real Codex home, its installed runtime, its MCP registration or its
operational database. Installation success, MCP registration, tool exposure, App Server
connection, delivery acceptance and service activation are six separate facts under OPS-6.1 and
none of them is read from another.
