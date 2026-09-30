# Runtime installation, update and diagnosis

[POLICY.md](../POLICY.md) owns repository rules and
[the operations contract](../plugins/crw/skills/crw-run/references/operations.md) owns the operational ones.
This page describes how the runtime behind the MCP bridge, the session relay and the completion
hook is installed, updated, rolled back and diagnosed, and it is written against that contract's
clause numbers so a reader can check a claim against the rule it came from.

The runtime is one Go binary, `crw`, shipped in a release archive. The plugin package declares the
server and the Stop hook and reaches the binary through the installer's pointer
([plugin packaging](plugin-packaging.md)); the package carries no runtime of its own.

Updating is the half that can lose something. A first install has nothing to destroy; a second one
is standing on a runtime somebody is using and a database nobody can rebuild, so most of what
follows is about what is read before anything moves and what is put back when it does not.
[Updating an installation](#updating-an-installation) is where that lives.

| Command | What it does | Contract |
| --- | --- | --- |
| `crw install install`, `crw install update` | Verify a release archive, install it as a new runtime directory, exercise it and move the owned pointer to it | OPS-2.4 |
| `crw install rollback [<dir>]` | Point the owned pointer back at the runtime the last promotion replaced, or at a runtime directory the host record lists | OPS-2.4 |
| `crw install remove <dir>` | Delete one runtime directory nothing selects, points at or runs out of | OPS-2.4 |
| `crw install register-mcp --owner plugin` | Write the bridge record the plugin's declared server reads | OPS-2.2 |
| `crw install hook --owner plugin` | Write the Stop settings the plugin's declared hook reads | OPS-6.3 |
| `crw install status`, `crw doctor`, `crw doctor retention-scan` | Read the installation, classify it and report the six check results; write nothing | OPS-2.1, OPS-2.2, OPS-6.1 |
| `crw-dev skills link --check` or `--apply` | Skill links into Codex, from a checkout | OPS-2.3 |

Every `crw install` and `crw doctor` command prints one JSON document. Runtime installation is never
folded into the skill links: `crw-dev skills link` belongs to the repository's development binary
because it links a checkout, and a release archive has none. It stays idempotent, it refuses to
replace an existing directory or a foreign link, and its `LINKED`, `MISSING` and `CONFLICT` words
mean the same thing wherever this page uses them. A plugin installation has no skill links at all.

The Python installer, `scripts/runtime_install.py`, still exists. It installs the Python fence
release and it is the development and rollback path until the Python execution path is removed;
[the Python fence installer](#the-python-fence-installer) is the one section of this page about it.
Moving a host from the Python runtime to this one is [the cutover](port/cutover.md), not an install
alone. The cutover moves the store's ownership. Where `crw install install`, which moves the pointer
(and on the relay host's first Go install replaces its Python-era Stop settings, once), falls among
its steps is not written yet; that order is an open item todo 42 settles
([the backlog](port/refactor-backlog.md#deferred-review-findings)).

## What an installation is

A release archive is `crw_<version>_<os>_<arch>.tar.gz`, published with a `SHA256SUMS` beside it
([releases](releases.md#binary-assets)). It holds `crw`, the three compatibility names
`codex-session-relay`, `codex-thread-bridge` and `crw-completion-hook` as links to it, and the
licences. `crw` dispatches on the name it was started under, so each name is the component it
names.

An installation of it is two things under the destination:

| Path | What it is |
| --- | --- |
| `<destination>/bin-<version>-<digest12>/bin/` | The runtime: `crw` and the three links, where `<digest12>` is the start of the archive's SHA-256 |
| `<destination>/current` | The owned pointer: a directory symlink naming the selected runtime directory |

The destination is `~/.local/share/crw-runtime` and nothing else. Both of the plugin's declared
commands name `$HOME/.local/share/crw-runtime/current/bin/` and nothing else, because `HOME` is the
one variable a hook and an MCP server both receive
([how hooks and MCP servers load](plugin-packaging.md#how-hooks-and-mcp-servers-load)). So
`crw install` has no `--dest`, and neither it nor the wiring honours `XDG_DATA_HOME`: every
`crw install` command acts on `<home>/.local/share/crw-runtime`, where `<home>` is `HOME`, or this
user's passwd entry when `HOME` is not set. A host record whose pointer names another link is
refused by every command before it acts, naming the repair, and `crw install status` reports that
reading as `destinationAgrees` ([decisions 11 and 38](port/decisions.md)). For a temporary or
isolated installation, run the commands under another `HOME`, and move with it everything `HOME`
does not decide, as the isolated-home integration test (`internal/runtime/integration`) does:
`CODEX_HOME` and `XDG_STATE_HOME` inside the same tree, `CODEX_SESSION_RELAY_SCOPE_DIR` set to a
directory there, and `CODEX_SESSION_RELAY_STATE` and `CODEX_SESSION_RELAY_MARKER_ROOT` unset or
pointed there too. Each is read on its own. A `CODEX_HOME` left naming another Codex home has the
install replace that home's Python-era Stop settings by a document whose adapter is under the
temporary pointer, so once the temporary tree is gone the Python bootstrap, which runs the adapter
the settings name, releases every Stop there without a record; an `XDG_STATE_HOME` left naming
another state home puts the temporary host record there, or is refused where the record there names
another pointer; and without `CODEX_SESSION_RELAY_SCOPE_DIR` the relay finds its scope registry from
this user's passwd entry, never from `HOME`. `crw doctor` and `crw doctor retention-scan` still take
`--dest`, to read another destination, never to install into one.

A path the commands cannot use as given is refused rather than guessed at. `HOME` has to be
absolute, hold no `..` and not start with exactly two slashes (`//home/...`, which pathlib keeps as
spelled and a lexical join folds to one; three or more fold to one in both). A relative
`XDG_STATE_HOME` is a usage error (exit 2) to every `crw install` command not given `--record`: read
against the working directory it would put the host record where nothing else looks. The doctor does
not read against the working directory either, but it reports rather than refuses: `crw doctor`
answers `hostRecordState` `ACCESS_ERROR` and exits 0, and `crw doctor retention-scan` lists the
relay state root under that state home as unreadable, leaves rows 3 and 6 unscanned and exits 0,
never clear. A path from `HOME`, `CODEX_HOME`, `XDG_STATE_HOME` or a path flag that holds a byte
that is not UTF-8 is a usage error naming where it came from, because a record or settings document
written with a replacement character names a file that does not exist. The execution policy path is
the one exception, recorded as `os.fsdecode` spells it
([the execution policy](#the-execution-policy-the-plugin-bridge-runs-under)).

The host record, `${XDG_STATE_HOME:-~/.local/state}/codex-relay-workflow/host-record.json`, says
which runtime is selected and records every install, its measured points and who placed the
pointer ([the host record](#the-host-record)). The two settings records the plugin's commands read
sit in the Codex home: `crw-bridge-mcp.json` for the server and `crw-completion-hook.json` for the
Stop hook.

Nothing here manages `PATH`. The declared server and hook name the pointer by absolute path, but a
skill command that runs `codex-session-relay` finds whatever `PATH` finds
([how skill commands reach the relay](#how-skill-commands-reach-the-relay)).

## Installing the runtime

`crw install` needs a `crw` to run it. The archive carries one, so unpack it anywhere temporary and
run that copy against the archive itself:

```sh
tar -xzf crw_<version>_<os>_<arch>.tar.gz -C <scratch>
<scratch>/crw install install --from crw_<version>_<os>_<arch>.tar.gz \
    --socket <app-server-socket>   # SHA256SUMS beside the archive, or --sums <file>
```

`--release <tag>` fetches the archive for this host's target and its `SHA256SUMS` from that GitHub
release instead of `--from`. Either way the archive has to be named for this host's operating system
and architecture, be listed exactly once in `SHA256SUMS` and hash to the listed digest, and nothing
under the destination or in the host record is created before all three hold.

What follows is one run, in this order, and the result lists the steps it took:

1. Claim the runtime directory with an exclusive `mkdir` and a claim file
   ([the claim a run leaves behind](#the-claim-a-run-leaves-behind)).
2. Unpack the archive into it and read the binary's digest.
3. Record the install entries.
4. Exercise the candidate through its own concrete executables, never through `current`, which
   still names the predecessor: the relay's `doctor` must report `actorReachability.socketConnect`
   as `ok`, and the bridge must answer an MCP session that lists its tools and calls
   `get_capabilities`. Both run against the App Server socket `--socket` names. The bridge falls
   back to `<CODEX_HOME>/app-server-control/app-server-control.sock` without it, but the relay has
   no default socket: its `doctor` answers `socketConnect` as `not configured`, so a run without
   `--socket` fails at `exercise the candidate` (exit 1) even with an App Server listening at that
   path. A run that cannot exercise the candidate records no point and promotes nothing.
5. Under the host-wide promotion lock: [read whether it is safe to swap](#reading-whether-it-is-safe-to-swap),
   establish that the pointer is this command's, refuse a second owner of the bridge or the Stop
   hook, and, where the Stop settings still name the Python adapter, replace them once by their Go
   variant ([one Stop settings document](#one-stop-settings-document)).
6. [Commit the selection, then replace the pointer](#the-order-a-swap-commits-in), and read the
   pointer back.
7. Settle the claim.

OPS-2.4 sequences an update as measure, install, measure again, and step 4 is the measurement that
produces the point; promoting before it would select a runtime that unpacks cleanly and fails the
moment it is used. A failure at any step up to the promotion leaves the previous runtime selected
and the pointer where it was ([what a failed update restores](#what-a-failed-update-restores)).
Nothing here removes, moves or recreates the store: update failure and store loss are different
accidents and the recovery for one must not cause the other.

Until todo 43 removes it, the Go build refuses to open a store in the relay's default state
directory unless `CRW_ALLOW_LIVE_STATE=1` is set
([the live-state guard](port/cutover.md#the-live-state-guard-until-todo-43)). This run is not
refused by it: the exercise and the swap gate read the store without opening it, through the
relay's `doctor` and `service status` and a catalog read that takes no lock. What the guard does
refuse is the runtime's use of that store afterwards: the relay commands the skills run, and the
Stop hook's guard whenever it has to read the store. So on a host whose store is the live one, an
install before todo 43 leaves a runtime that cannot serve it, and that host moves through
[the cutover](port/cutover.md) instead.

### The record is not the replacement

The staging claim is written last. It says this staging finished, and until the selection is
committed and the owned pointer names the runtime there is nothing finished to say, so by the time
writing it can fail the declared commands already reach the new runtime. The replacement has
happened and only its record has not, and those are reported as two outcomes rather than folded
into one.

| Field | Answers |
| --- | --- |
| `promoted` | whether this run replaced a runtime |
| `inService` | whether this runtime directory must be kept: true while the record selects it or the pointer names it, true once its claim has settled, and true when none of that could be read. False only when the readings say so |
| `claimSettled` | whether the claim recording it was written |
| `claim` | the claim's own outcomes (`settled`, `released`), its read-back and the selection snapshot that decided them |
| `recoveryRequires` | what has to be done next, under the same key a refusal reports it |

So `crw install install` has four exit statuses:

| Status | What this run changed | The record | What it means |
| --- | --- | --- | --- |
| `0` | it landed, or there was nothing to change | written | the run finished; `alreadyInstalled` says when this archive was already the selected runtime |
| `3` | it landed | not written | the pointer names the new runtime and the claim that records it did not settle |
| `1` | nothing | not written | refused, or failed and put back what it had changed; whatever the host selected and reached before, it still does |
| `2` | nothing | not written | a usage error, found before anything was read |

**Exit 3 is not a refusal and must not be read as one.** Non-zero here means the opposite of what it
means everywhere else in this command: the change landed, and a process may be running out of the
runtime it changed. A wrapper that reads every non-zero status as "nothing changed" would report the
old runtime as selected, or clean up a runtime that is in service. Key a cleanup decision on
`inService` and never on the status alone: a competing install can supersede this runtime between
the promotion and the result, and then status 3 is still correct about this run while `inService`
is false.

Which accident happened, and what to do about it, is in `recoveryRequires`, derived from the claim
as it reads back and from a selection snapshot taken under the promotion lock:

| What the result says | What to do |
| --- | --- |
| another run held the claim's lock | wait for that run; this call wrote nothing |
| the claim could not be read back | make it readable or remove it, then run the install again; the runtime is in service and must not be deleted |
| what this host selects could not be established | read the host record before acting |
| the record selects this runtime and the pointer does not name it | read the pointer before rerunning, because a rerun replaces that link first |
| the record selects this runtime | clear what stopped the write and run the same install again: it finishes an interrupted promotion and rebuilds nothing |
| the record no longer selects it, and something may still reach it | leave the directory alone |
| nothing selects it or points at it | nothing; another run moved the selection on, so do not rerun to settle it |

The snapshot is consistent, not durable. Nothing holds the promotion lock until an operator reads
the result, so re-read it before acting if time has passed.

## Updating an installation

`crw install update` is the same run as `crw install install`; the name says which one you meant.
A new archive is a new runtime directory, because the directory is named for the archive's version
and digest, and the predecessor is never removed by an update. It becomes the host record's
`outgoing` selection, which [`crw install rollback`](#rolling-back) returns to.

### The pointer is what moves

`<destination>/current` is a directory symlink, and everything that starts the runtime names a path
through it: the plugin's Stop command, its server launcher, and the three executables the settings
records name. Those strings are stable across every update, so an update rewrites none of them and
changes nothing Codex has trusted. It moves the link.

The pointer is a way to reach a runtime and never an identity. A process already started keeps the
runtime it started in after the pointer moves: a running bridge or daemon goes on running its own
directory's binary, which is why no update removes a predecessor. The next process started through
the pointer is the new runtime.

A registered command and the runtime it reaches are separate claims, and `crw doctor` reports them
separately: the pointer's state and target, which kind of runtime the target is (`go-binary`,
`python-venv`), and whether the record selects what the pointer names (`runtime.agrees`). A
`current` repointed by hand at another directory is caught by that comparison rather than passing
because the command strings are unchanged.

### One Stop settings document

`crw install` writes one plugin-owned Stop settings document, and it serves both runtime kinds
through the pointer. `adapterInterpreter` `/usr/bin/env` and `adapterEntryPoint`
`<destination>/current/bin/crw-completion-hook` run the Go hook on a Go runtime and the fence
release's `crw-completion-hook` console script on a Python `env-*` runtime, and `relayExecutable`
`<destination>/current/bin/codex-session-relay` is the Go link or the venv's console script. No
promotion and no rollback rewrites that document ([decision 18](port/decisions.md)).

The one rewrite is of a Python-era document, and only when the pointer moves onto a Go runtime. The
relay host's Python-era document names `.../current/bin/python3` as `adapterInterpreter`, a path
that is gone once the pointer leaves the virtual environment. So a move of the pointer onto a Go
runtime that finds one (an install, an update or a `crw install rollback`; on the relay host, its
first Go install) archives it, inside its promotion and before the pointer moves, beside the file
as `crw-completion-hook.json.superseded-<time>` (a hard link to the same file, or a copy where the
filesystem refuses one, and never deleted) and replaces it, in one rename,
by its Go variant: the same relay, marker root, database, journal, socket, mode and budget, with only
the two adapter keys moved. Both documents work while the pointer still names the venv, so a Stop
always finds settings it can run. Settings a user owns, or that name an adapter this command did not
write, are left as they are. A write lands only on the document it was decided from: one that
changed after it was read is never written over, and a replacement never destroys another writer's
bytes.

A run that fails after this step puts the Python-era document back, and says so under
`settings.undone`, only while the path still holds exactly the bytes this run wrote, and then by an
atomic exchange of the two names (`renameat2` `RENAME_EXCHANGE` on Linux, `renamex_np`
`RENAME_SWAP` on darwin). Where no atomic exchange exists, nothing is renamed over the active path:
both files stay, and the answer gives the `mv` that puts the found settings back by hand.

A rollback to a Python runtime never puts the archive back; it requires the venv to serve the one
document instead ([rolling back](#rolling-back)). To return to the Python-era document itself, do it
by hand and only while the pointer names the venv, after `crw install rollback <venv>`: take the
newest `<CODEX_HOME>/crw-completion-hook.json.superseded-*` whose `adapterInterpreter` ends in
`/current/bin/python3` and `mv` it over `crw-completion-hook.json`, one rename, so a Stop never
finds the path empty. The next move of the pointer onto a Go runtime, whether `crw install install`,
`update` or a `crw install rollback` onto one, archives it again and writes its Go variant.

### The claim a run leaves behind

The runtime directory's name is deterministic and it is created with an exclusive `mkdir`, which is
what proves a run owns it. A run killed outright would otherwise leave the directory behind and
every retry of the same archive would refuse at the existence check for ever.

A run leaves two files in the directory, because they answer two questions. The lock,
`.crw-staging-lock`, answers whether anybody is still building, and it is created once and never
replaced: an advisory lock belongs to an inode rather than to a name, so a lock on a file later
replaced by rename would sit on an unlinked inode while the next reader found the new one free. The
claim, `.crw-staging-claim.json`, answers what that run said it was doing, and it is rewritten when
the staging settles. Removing anything needs positive proof of ownership, so the claim has to carry
this command's marker (`crw install`, or `runtime_install.py` for a Python runtime directory), its
claim version and a state from the declared set. Readable JSON at that path is not proof.

| Observed | Answer |
| --- | --- |
| No claim, and the directory holds files | Somebody else's. Refused, nothing touched, even when the host record selects something inside it |
| No claim, and the directory is empty | Taken over as it stands |
| A claim of this command's, the lock held | Another run is building it. Refused, nothing touched |
| A claim of this command's, the lock free, nothing selecting or naming it | An abandoned staging. Removed (through a tombstone, as [remove](#removing-a-runtime) removes) and built again, but only under remove's rules, read again under the promotion lock: kept, and the run refused, while a process may run out of it, a relay daemon record cannot be read, or a registration names it or cannot be read. One the record's `outgoing` names was in service, so it is kept and its claim settled. Where there is no process table (darwin) it is kept too, and the answer gives the recovery for a staging that was never promoted |
| A claim, and whether anyone holds it could not be established | Kept, and reported with what recovery needs |
| A settled claim, every component selects it and the pointer names it | Already installed. Reported, nothing rebuilt |
| A settled claim the record selects for one component and not another | Refused, naming each component's selection; `crw install rollback <dir>` selects every component and swaps nothing |
| A selected runtime the host cannot launch as it stands (`bin/crw` not a regular file this user may execute, or a link that does not resolve to it) | Refused, with `repair`: the commands that restore it in place (`crw` extracted from the archive the directory is named for, `chmod 755`, `ln -sfn crw` for each link), after which the same install answers already installed. Its directory is named for the archive, so it cannot be built again beside itself |
| A settled claim, and nothing selects it any more | Kept. It is a runtime that was promoted once, and a process may still be running out of it |
| An unsettled claim for a runtime that IS selected | An interrupted promotion. Finished rather than rebuilt |
| A lock held with no claim written | A run between taking the lock and writing its claim. Refused, nothing touched |

Finishing an interrupted promotion asks a narrower question about the link than a promotion does:
not whether it agrees with the selection, which it cannot while the promotion is unfinished, but
whether it still names a runtime this host record accounts for. It asks OPS-4.4 again as well,
because the interrupted run recorded no gate verdict and every cell of the gate reads state that
moves while nobody is looking.

Liveness is the lock and never a recorded process id: inside a container sharing a kernel the same
process id under the same boot id is a different process. Where `flock` is unavailable the answer is
that nobody could tell, and an owner nobody could establish is never read as an owner that is gone.
Deciding and acting are one step, under the directory's own `<env>.crw-lock` and then the promotion
lock ([the lock order](#the-order-a-swap-commits-in)), so two retries that both find the same
abandoned staging cannot both act on it.

### Reading whether it is safe to swap

OPS-4.4 sequences an update around a daemon that is not running and open attempts that have been
reconciled. Three readings answer that, each filling only its own cell:

| Cell | The reading that answers it |
| --- | --- |
| `daemon` | the selected relay's `service status`, whose `running` is decided by the lock a supervisor holds |
| `inFlight` | whether a store is there at all, then the selected relay's `doctor`, whose `contents.openAttempts` counts in-flight and held-uncertain attempts |
| `storeSchema` | the store's own schema, read read-only from `sqlite_master` without opening the store for writing, against the schema the candidate binary declares |

The in-flight cell reads twice, and the order is the point. The relay reports contents unavailable
both for a store that is missing and for one it cannot read, and those are opposite answers here: an
absent store has no open attempt, an unreadable one has an unknown number.

The swap proceeds only when the daemon is established stopped, the open attempts are established
zero, and the schema is established compatible: the verdict is `ALLOWED`. A cell that answered no
decides `BLOCKED`, and a cell that could not be read decides `UNESTABLISHED`; both keep the existing
installation, and the refusal names the cells. This command never starts or stops a daemon. OPS-4.1
gives the service to the scope operator, so a running daemon is a refusal here and not something to
resolve.

`STOPPED` describes a moment that has already passed: the relay's liveness answer releases its lock
before returning. Taking the reading inside the promotion lock narrows the window to the promotion's
own length; it cannot close it, because that lock excludes other runs of this command and says
nothing to a supervisor.

### Why the schema reading compares statements and not versions

The relay declares schema version 1, has never raised it, and grows its schema through separate
`CREATE ... IF NOT EXISTS` statements. Every store therefore agrees with every candidate at version
one, and comparing versions would detect neither a downgrade nor an upgrade while looking exactly
like a check.

So the cell compares each object's `CREATE` statement in the store's `sqlite_master` with the
statements the candidate declares, keyed by kind and name (`index sync_ready`, not `sync_ready`),
over every object the catalog holds except the ones SQLite maintains for itself. Runs of whitespace
outside quoted text are normalised away and nothing else is.

| Answer | Observed | Decision |
| --- | --- | --- |
| `NO_STORE` | no store exists at the resolved selection | allowed, and reported as absence rather than as agreement |
| `AGREES` | the same schema objects, defined identically | allowed |
| `EXTENDS` | the candidate declares objects the store does not hold | refused |
| `DIFFERS` | a shared object is defined differently | refused |
| `NARROWS` | the store holds objects the candidate does not declare | refused |

The relay applies its whole schema on every write-open, so a candidate whose schema is not the
store's applies the difference the moment its daemon first starts. OPS-4.5 reserves that for its own
decision, with a copied backup of the whole state directory taken first, so an update never waves it
through. The Go and Python runtimes execute the same schema statements
([decision 14](port/decisions.md)), and no Go release changes them before the commit point
([cutover](port/cutover.md#commit-point)).

### The order a swap commits in

The selection in the host record and the pointer on disk are two truths, and both the order they
are written in and the lock they are written under are the safety argument. They are written inside
one critical section holding the promotion lock, and the selection is committed first.

The reverse order has a real failure: the link lands, the record write then fails, recovery reads a
selection that does not name this runtime, concludes the candidate was never promoted, removes it,
and leaves the declared commands pointing into a directory that no longer exists. OPS-4.4 requires
every state transition to be committed before its side effect. Recovery also refuses to remove a
runtime the pointer names, so neither truth alone can authorise deleting a runtime the other one is
still using. Every judgement the promotion makes is decided on state read inside that lock.

The promotion lock is an advisory lock on one host-wide file beside the host record,
`host-record.json.promotion-lock`, created once, never unlinked, and released by the operating
system when its owner dies. It excludes runs of this command and of the Python installer, which
take the same lock, and nothing else. A kill inside the window leaves a runtime that is selected
and unsettled, which the next run recognises and finishes rather than rebuilds.

Ownership of the pointer is established from the record before it is replaced. Renaming over an
existing symlink succeeds whoever created it, so a `current` this host record never recorded
placing is left alone; a real directory at that path fails the rename outright, which is the safe
direction. The pointer placed is then proved: it has to resolve, with no loop and nothing dangling,
to the runtime directory itself by identity, and `<pointer>/bin/crw` has to be a regular file this
user may execute.

Every `crw install` command takes its locks in one order ([decision 33](port/decisions.md)): a
runtime directory's `<env>.crw-lock` first, then the host-wide promotion lock, then the host
record's own `.crw-lock` inside both. The settings records' locks, a claim's and the launcher
copy's are leaves: nothing else is taken while one is held. It is runtime_install.py's order too.
Every wait before a command's first write ends when the command is interrupted (SIGINT, SIGTERM,
SIGHUP): it stops waiting, writes nothing and says so (`register-mcp` and `hook` answer the outcome
`interrupted`). A wait inside a sequence already under way, such as the restore after a failed
promotion or the entry drop after a directory was set aside, runs to completion, because stopping
there would leave the host half-written.

### What a failed update restores

A failed update leaves the previous runtime selected, the previous pointer target in place, the
Stop settings as it found them and the store exactly as it was. The result says which step failed
rather than only that something did: `failedStep` names it, `retriable` says whether the same
archive can be installed again, `residualPaths` names what the run left, and `recoveryRequires` what
has to happen first.

Putting a selection back is narrower than it sounds, and deliberately. The restoration runs under
the promotion's own lock and puts back only the entries that still name what this run wrote; an
entry another run has promoted since is left alone. Putting a selection back includes putting it
back to nothing, for a first install that had no previous selection, and putting the pointer back
includes putting it back to absence. A restoration that cannot be read back reports a residual
pointer and keeps the candidate rather than claiming the rollback completed.

A candidate that is selected, whose record could not be read, or that the pointer names, is kept.
Otherwise the run drops its install entries and removes the directory it created, through a
tombstone as [remove](#removing-a-runtime) does, and the result says whether that removal was
verified: verified means the same archive can be installed again; not verified names the residual
path and what recovery needs, with the original failure reported beside the cleanup failure.

Two failure points an update might be expected to have do not exist here. Registration is not part
of an update: the declared commands name the pointer, so nothing is registered again. And nothing
starts: OPS-4.1 gives the service to the scope operator, so this command refuses while a daemon runs
and never starts one.

This is a POSIX path. The runtime layout, the directory symlink and the advisory locks are POSIX
assumptions; Windows is out of scope rather than approximated.

## Rolling back

`crw install rollback` points the owned pointer back at the host record's `outgoing` selection, the
runtime the last promotion replaced. `crw install rollback <dir>` points it at a runtime directory
the record lists exactly, as an install entry's `environment`, instead; an empty `<dir>` is a usage
error. It takes the target's `<env>.crw-lock` and then the promotion lock
([the lock order](#the-order-a-swap-commits-in)), finds the target again there and refuses with
nothing written when the record moved it, and then applies a promotion's gate, ownership and
second-owner rules, commits the selection before the link, and proves the pointer it placed. The
runtime it leaves stays installed and becomes the outgoing selection, so a second rollback returns
to it. Where there is no outgoing selection, a bare rollback refuses and writes nothing.

The target may be a Go runtime or a Python `env-*` runtime the fence installer made. It has to be
launchable as it stands, judged without running it, and its claim has to be settled, or unsettled
with nobody holding it where the record shows a promotion committed it (an exit 3). A rollback
onto a Python runtime never rewrites the Stop settings; one onto a Go runtime replaces a Python-era
document as a promotion does ([one Stop settings document](#one-stop-settings-document)). Onto a
Python runtime it requires the venv to serve that one document before the pointer moves:
`bin/crw-completion-hook`, `bin/codex-session-relay` and `bin/codex-thread-bridge`, each one's `#!`
interpreter and the recorded interpreter all resolve to regular files this user may execute, the
hook script names `codex_session_relay.stopadapter`, the relay package the record lists there is the
fence release, and every path the settings name through the pointer exists in the venv. Otherwise it
refuses with nothing changed.

The plugin's declared commands do not follow it there. The native wiring runs
`current/bin/crw hook --plugin-launch` and execs `current/bin/codex-thread-bridge --plugin-launch`,
and a Python runtime has no `bin/crw` and a bridge that refuses the flag. So with the native payload
installed, a rollback onto a Python runtime would release every Stop without a record and stop the
server from starting. It refuses instead, with nothing written: before it commits, it reads every
cached plugin version's hook and MCP declarations the way `crw doctor`'s retention scan reads them
(row 5, the shell scripts they run included), and any command that runs a program through the
pointer with `--plugin-launch` keeps the pointer where it is. The refusal names the cached version
directory, each such command (`pluginLaunches`) and the repair. A cached declaration that cannot
be read or judged refuses too, since whether it launches that way is then unknown. Reinstall a
plugin revision whose declarations are the Python bootstrap first, then roll the runtime back
([update and roll back](plugin-packaging.md#update-and-roll-back)). Nothing checks this for a Go
runtime built before decision 26, which reads the flag as something else; the order is its only
guard.

Install rollback is not takeover rollback. `crw install rollback` moves which runtime the pointer
names, and onto a Python runtime it rewrites no settings. It does not move the store's ownership. After the cutover
the store is owned by the Go runtime, and handing it back to the Python fence release is
`crw relay takeover rollback --to python --python-relay <path>`, which names the Python relay by
absolute path and never through the pointer ([cutover rollback](port/cutover.md#rollback)). One does
not imply the other, and a return to Python needs both. The cutover runbook does not yet say in
which order they run, or where `crw install install` falls among the forward steps; both orders are
open items todo 42 settles
([the backlog](port/refactor-backlog.md#deferred-review-findings)). The one order
fixed today is the plugin payload's, above: the payload goes back before the runtime does.

## Removing a runtime

`crw install remove <dir>` deletes one `bin-*` or `env-*` runtime directory directly under the
destination, and only when nothing may still be using it. The directory is judged by file identity,
so naming it through a symlink, a bind mount or another spelling of the destination passes no check
its own name fails. It refuses a directory the record selects, one the pointer names or might name,
one with no claim this command or the Python installer wrote, one whose claim cannot be read, a
staging another run still holds, any directory a live process runs out of (its `/proc/<pid>/exe`,
its working directory, or what its arguments run, resolving inside it, and every relay daemon a
`daemon.json` or scope claim records alive), and any directory a registration the host reads names
a path inside: the Stop settings, the bridge record, the cached plugin declarations, the
`crw-stop-hook.py` copy, `hooks.json` and `config.toml`. A process it cannot rule out or a
registration it cannot read or judge refuses too, and the answer names it. A process whose working
directory cannot be read and that runs something by a relative path is one it cannot rule out,
whatever user runs it, unless that user (not root) is provably shut out of the directory by the
permissions of the directory or a parent. So on a host where a root agent runs a relative script
(every Azure VM's WALinuxAgent, for example) `remove` refuses, names the process, and gives the
manual recovery below.

The process reading is this host's process table, in this command's PID namespace, and every answer
that rests on it says so under `processTable`: a process in a container sharing the directory, or on
another host sharing a network home, is not seen, so run `crw install remove` where the runtime's
processes run. Where there is no process table it can read (darwin has no procfs), whether a relay
or a bridge still runs out of the directory cannot be established, and every remove refuses,
fail-closed, with the recovery by hand under `recoveryRequires`: stop the relay daemon started from
it (`<dir>/bin/codex-session-relay service stop`) and end every Codex session whose bridge it
started, delete the directory, then run `crw install status` to see that the host record and the
pointer still name the runtime you meant. Its install entries stay in the record, where a rollback
naming it is refused because the directory is gone.

A removal survives a kill. Under the directory's `<env>.crw-lock` and then the promotion lock, the
directory is renamed in one step to its tombstone, `<destination>/.crw-removing-<name>`; its install
entries leave the host record in one write, with an `outgoing` that names it; only then is the
tombstone deleted. A drop that cannot be written renames the directory back and refuses, and a
deletion that does not finish exits 3 naming the tombstone. A kill anywhere leaves either the whole
directory or a tombstone. `crw install status` lists every tombstone under `interruptedRemovals`,
and `crw install remove` of the tombstone, or of the runtime's name when only its tombstone is left,
finishes it once no process runs out of it and no registration names it. Do not delete a tombstone
by hand: finishing it also drops what the host record still lists under the runtime's name. A
tombstone that carries no claim of this command's is somebody else's directory: status reports it
`ours: false`, and nothing removes it.

What remove cannot see is a command fixed before the update that will start a process later. A
Python runtime stays while any live or resumable task can still spawn it, whatever owns the store
([retention](port/cutover.md#retention)). Read `crw doctor retention-scan` first, and remove an
`env-*` directory only once it reports no reference that resolves into it.

## Reading an installation

Three read-only commands, each answering a different question:

- `crw install status` answers what a replacement actually left: what the host record selects, what
  the owned pointer names and whether they agree, whether the record's pointer is the fixed
  destination's (`destinationAgrees`, with the repair when it is not), every runtime directory with
  its claim, every unfinished removal (`interruptedRemovals`), the outgoing selection, the promotion
  lock and the two settings records.
- `crw doctor` is the host diagnosis: the host record reading, which runtime kind is selected, the
  Codex CLI version against the one the wiring was measured on, the App Server observed through the
  selected bridge, the pointer, the promotion lock, the settings records and every executable they
  name, the [classification](#installation-ownership) of each component, the residue a later install
  would reclaim, the relay's own scope reading, and [the six results](#six-results-that-never-imply-one-another).
  `--temporary` records the destination as a temporary one, so a proof taken there is never read as
  a claim about a host.
- `crw doctor retention-scan` enumerates [the fixed retention surface](port/cutover.md#retention-scan-surface)
  and reports every reference that resolves to a Python interpreter or a `.py` path, with its source.
  It is the reading the retention rule waits on.

None of them takes a lock across the whole reading, so a host changing underneath is described in
pieces.

## How skill commands reach the relay

The skills run `codex-session-relay ...` as a shell command ([relay usage](../plugins/crw/skills/crw-run/references/relay.md)),
and that name is found on `PATH`. The installer does not manage `PATH`: it places the name in
`<destination>/current/bin/`, and nothing puts that directory on anyone's `PATH`. So either put it
there, ahead of any other copy, in the environment the Codex tasks run in, or run the relay by that
absolute path. Check what a task actually reaches:

```sh
command -v codex-session-relay
readlink -f "$(command -v codex-session-relay)"
readlink -f <destination>/current                  # the runtime directory the pointer selects
```

The first answer has to lie inside the second. On a Go runtime (`bin-<version>-<digest12>`) it is
that directory's `bin/crw`, which the name links to. On a host still on the Python fence release,
before the cutover, the pointer selects an `env-*` directory and the answer is its
`bin/codex-session-relay`, a console script of that environment.

A stale `codex-session-relay` earlier on `PATH`, such as a console script under `~/.local/bin` whose
shebang names a Python virtual environment in a development checkout, runs whatever that checkout
holds against the same store, which can be code from before the fence release and so outside the
cutover's fence. The retention scan does not read
`PATH`, so this reading is the one that finds it.

## The one definition

`scripts/crw_runtime/components.json` is the single compatibility definition OPS-1.1 requires: the
two components, their console-script names (the compatibility names beside `crw`), version,
licence, retained upstream provenance and the tool that identifies each one. The Go side,
`internal/runtime/definition`, carries only what a Go install needs, and a test keeps it equal to
the committed file. Release digests are not in it: they live in the release's `SHA256SUMS` and in
the host record ([decision 35](port/decisions.md)).

The file stays `definitionVersion` 1 and keeps its Python-shaped fields, `packageLocation`,
`requiresPython`, the package trees and `sourceDigest`, while the Python fence installer reads it;
`runtime_install.py verify-definition` re-derives every one of them in CI, so editing anything under
`packages/` means recording the derived trees again. The upstream revision is not derivable from
this checkout, because the import brought source rather than history, so it is required to appear
in [packages/README.md](../packages/README.md), the provenance narrative OPS-1.5 says is retained
rather than replaced.

What the definition does not carry is as important. Installed locations, entry points, host names
and measured points are host facts. OPS-3.2 makes a real record a private receipt, so they go to the
host record outside this repository and this repository never commits one.

## The host record

The host record is the other half of the definition and is never committed. It is `recordVersion`
1, which the Go and Python installers both read and write: Go install entries carry fields of their
own beside the Python-era entries, which keep theirs. It holds, per component, one entry per install and the measured points, plus what is
`selected`, the `outgoing` selection a rollback returns to, and the owned `pointer` with who placed
it and when.

A Go install entry records `location` (the runtime's `bin/`), `environment` (the runtime
directory), `entryPoint` (the component's compatibility name), `binaryDigest` and `integrity` (the
binary's SHA-256), `target`, `version`, `archiveDigest`, `reachedVia`, and `source`, the repository
commit and tree the binary was built from. An entry the Python installer wrote carries
`interpreter`, `interpreterPath` and `installMode` instead of the binary fields. `crw doctor` reads
both, and classifies only Go installs: a Python install keeps the Python installer's classification
until the Python path is removed.

Under OPS-1.3 a point means the combination was exercised, so no amount of reading bytes produces
one, and an install whose bytes match but whose combination nobody has run classifies `unmeasured`
and is preserved rather than reused. The install's exercise is what produces a point
([installing the runtime](#installing-the-runtime), step 4), and a connection, protocol or tool-call
failure records no point and says why. A Go point records `install`, `installDigest`, `codexCli`,
`host` and `appServer`, the dimensions it is matched on, beside `date`, `measuredBy`, `method` and
`archiveDigest`. Points are appended, never replaced.

The `appServer` dimension is the bridge's whole `get_capabilities` payload, serialized and compared
by exact string equality: the identity of the server a combination was exercised against is
everything the round trip reported, not a version string someone chose to trust. So a change to that
payload's shape changes the dimension, and a point measured before it does not cover a bridge built
after it. The old point is not wrong and is not discarded; it remains evidence about the build it
was taken against. The same property is why the payload carries no timestamp: a dimension that
varied between two calls to the same build would never match itself.

## Reading a record, and what happens when it cannot be read

Every record these commands read (the host record, the settings records, a claim, the Codex
configuration, the hook file) is read at a narrow boundary that turns a failure into an answer
rather than a crash. The answer is one of four states, decided by an ordered observation rather
than by a convenience test:

| State | What was observed |
| --- | --- |
| `ABSENT` | nothing exists at the path. The only state that may be read as a host with no history |
| `PRESENT` | it was read. An existing record with nothing in it is `PRESENT`, not `ABSENT` |
| `UNREADABLE` | something is there and its shape cannot be read: a directory or other non-regular file, a symlink whose target is established missing or looping, invalid UTF-8, unparseable JSON, or containers of the wrong type |
| `ACCESS_ERROR` | nothing could be established: a permission or I/O failure reaching the path, a symlink whose target could not be resolved, or a parent directory that cannot be traversed |

The distinction that matters most is the last row. Being unable to ask is not being told no, so a
failure to establish existence is never reported as absence, and a permission problem is never
reported as a malformed record. A refusal names what failed and where.

**What this guarantees:** the worst case for a record these commands read is a named refusal, not a
crash. **What it does not guarantee:** that a record which could have been read is never refused.
That direction is the safe one, and the refusal carries its reason.

- **"Nothing was written" is scoped to what can be guaranteed.** Malformed input detected before
  the first mutating step refuses and the target file's bytes are unchanged. A read-back failure
  after a write reports the write instead of denying it (`record_applied_unverified`,
  `config_applied_unverified`) and exits non-zero, because reporting a landed write as a refusal
  that wrote nothing invites a retry over a file that now exists.
- **A read-only diagnosis reports rather than refuses.** `crw doctor` names the failed reading in
  `hostRecordState` and `hostRecordReading` and continues with what it could still observe, and
  never reads an unreadable record as a clean host. Commands that write refuse outright.

### One writer for the host record

Every change to the host record takes the record's lock, loads the record inside it, applies the
caller's narrow delta and saves. A caller never hands back a record it loaded earlier: an install
that loaded the record, spent minutes unpacking and exercising a runtime, and then saved what it had
loaded would overwrite whatever another run committed in between, and holding a lock over that save
would not help, because the staleness is already inside the value. So a caller says what it learned
(this install, these points, this selection) and the merge happens against the record as it then
stands. A failed install drops only the install entries keyed to the directory it created and leaves
the selection exactly as found, because another run's successful promotion is not this run's to
undo.

## Installation ownership

`crw doctor` classifies each component of the runtime the pointer names from the OPS-2.1 signals:
whether its entry point resolves into a recorded install, whether the binary's digest matches the
recorded `binaryDigest`, whether a measured point covers the combination it runs under, and
whether a registration conflicts with it or the owned pointer names something other than what the
record selects. A Go install is an archive, not a checkout, so
the checkout signals do not exist for it: a fork is bytes that differ from the recorded digest. A
signal that cannot be read stops classification and is reported; it never counts as a signal that
agreed. A runtime a host cannot launch, a `bin/crw` that is not an executable regular file or a
compatibility name that does not resolve to it, is reported as such, because a digest says nothing
about either.

The five OPS-2.2 classes are evaluated in their fixed order and the first match wins: `conflict`,
`fork`, `foreign`, `unmeasured`, `own`. Only `own` is reused. An install whose bytes match and whose
combination has no point classifies `unmeasured`, and that is the intended answer, not a gap to
close by relaxing the rule: OPS-1.3 refuses to let a matching digest stand in for a run nobody
performed. Exercising it, which is what an install does, is the way out.

Nothing outside a recorded path is ever overwritten, and no predecessor is removed by an install or
an update.

## MCP registration

The plugin package declares the server, and installing the package registers it. What the package
cannot carry is where the runtime is and which execution policy the bridge runs under, so that goes
into `<CODEX_HOME>/crw-bridge-mcp.json`, which `crw install register-mcp --owner plugin` writes and
the launcher reads at every start ([the native wiring](plugin-packaging.md#the-native-wiring)). The
record names the owner, the server name, the bridge executable
(`<destination>/current/bin/codex-thread-bridge` unless `--bridge-command` says otherwise), its
arguments (`--bridge-arg`, repeatable), and, from version 2, the execution policy. `--dry-run`
reports what would be written and writes nothing.

Writing the record is not registering a server. On a host with no plugin installed the record is
inert, and the command says so. Registered, recorded and a tool actually called stay three claims.

### Who registers the server

One owner registers each surface. `--owner plugin` is the only owner `crw install` writes for:
the user-owned registration, a `[mcp_servers]` table in `config.toml`, retired with the Python
installer. The refusal still runs both ways, because a host can acquire the other half from
elsewhere. A record is refused while the Codex configuration already starts this bridge, under this
server's name or under any other, and the refusal names that entry: the plugin's declaration beside
it would run a second bridge. The launcher stands down for a record that names another owner. A
record or configuration that could not be read refuses rather than defaulting, because installing on
an unanswered question is how the second bridge arrives. Every writer of the record takes the
ownership lock beside it (`crw-mcp-ownership`) while it writes.

### The execution policy the plugin bridge runs under

Codex starts a plugin-declared server with the App Server's own environment. Measured on Codex
Desktop 0.154.0 for Linux, that is `HOME LANG LOGNAME PATH SHELL USER` and nothing else. A bridge
started that way reads no execution policy: `get_capabilities` reports `presence_only` with no
roles, and a child created through the Desktop tools is never asked whether it runs its role's pair.
`--execution-policy` gives the plugin-owned record the one fact that closes that gap:

    crw install register-mcp --owner plugin --execution-policy /path/to/execution-policy.json

The record becomes version 2 and gains `executionPolicy`, with two fields: `path`, the file as given
(expanded and made absolute, not resolved, like the relay's own launch declaration), and `digest`,
the SHA-256 of the bytes this run read. It never carries what the file says. Before anything is
written, the file goes through the bridge's own parser in this binary, so a policy the bridge would
refuse to start under is refused here instead, as `execution_policy_unreadable`. The output reports
the mode and the declared role pairs, the same values `get_capabilities` discloses, and says which
parser judged them: the installed runtime parses the file again every time it starts and decides for
itself.

At every start the launcher reads the record and does one of two things. It refuses and exits 2,
naming the record and the repair, when the file is missing, is not a regular file, cannot be read,
or no longer hashes to `digest`, and also when its own environment already names a different policy
file or digest. Otherwise it starts the bridge with `CODEX_THREAD_BRIDGE_EXECUTION_POLICY` set to
`path` and `CODEX_THREAD_BRIDGE_EXECUTION_POLICY_DIGEST` set to `digest`, and the bridge refuses to
start if the bytes it parses hash to anything else. Refusing to start is the visible failure. The
declaration marks the server not required, so the session continues without the bridge's tools, and
it never continues with a bridge that checks no role. A version-1 record names no policy and starts
the bridge with the environment the launcher was given.

The policy is part of the registration's identity, so the only rerun that succeeds is an identical
one, answered `record_unchanged`. Any other difference is refused as `record_differs` and nothing is
written: another file, the same file with other contents, a rerun that drops the flag, or adding a
policy to a version-1 record. The refusal names the repair: move the record aside by hand, then run
`crw install register-mcp` again. Every edit to the policy file, including adding an exception,
therefore has two consequences. The relay picks the edit up when its daemon restarts. The bridge
record has to be moved aside and registered again, and a thread started in between has no bridge
tools. The digest is what lets a changed file fail visibly instead of being enforced unregistered.

A created record is reported only after the policy file has been hashed again, following the write,
and still matched. A mismatch immediately before the write writes nothing and answers
`record_policy_changed`; a mismatch after it gets the same answer and the move-aside repair, and the
record stays, because the launcher refuses its stale digest at every start. A removal by path cannot
exclude a writer that does not take the ownership lock, such as an editor, so nothing here removes a
record.

Codex starts the server once for each thread it loads. That was observed on Codex Desktop 0.154.0:
one App Server process with a separate bridge child per thread, and the child's start time matching
the thread's creation to the second. A record written now therefore takes effect for threads started
afterwards, with no App Server restart, and a thread already running keeps the bridge it spawned.
Neither is established on a host until a new thread's `get_capabilities` reports the digest the
record names. A package replacement may or may not reach threads that are already running;
[what two replacements measured](plugin-packaging.md#what-two-replacements-measured) says what was
seen, and [updating safely](plugin-packaging.md#updating-safely) gives the order and the checks.

## Six results that never imply one another

`crw doctor` reports the six OPS-6.1 fields under `checks.results`, in the OPS-6.2 shape, each with
its own evidence, command, acting process and measurement time. A field with no timed observation
behind it reports `unknown`; no time is ever invented or copied from another field.

| Field | Established by | Never established by |
| --- | --- | --- |
| `installed` | Every component classifies `own` | The runtime being present |
| `mcpExposed` | Tool names observed in a live Codex session | A record or a declaration |
| `connected` | The relay's `doctor` reporting `actorReachability.socketConnect` as `ok` | A socket file on disk |
| `deliveryAccepted` | An attempt that recorded a returned turn id | A dispatch or an absent error |
| `verificationComplete` | Every OPS-6.4 condition at once | A completed turn or a green check |
| `alwaysActive` | A supervised runtime surviving a host restart | Any of the five above |

A live session is the only thing that can list the tools a Codex session exposes, and the
diagnosis's own session with the bridge is not Codex's, so `mcpExposed` stays `not_verified` from
`crw doctor`; read it in a task. `deliveryAccepted` needs work created and delivered, which a
diagnosis never does, so it is `not_applicable` unless a trial produced it. `settingsPreserved` is
reported beside the six and never merged into them: it answers that the diagnosis wrote nothing.

## One shared service and one store

OPS-3.1 puts one relay service and one durable store behind an entire operating scope, which is one
host, one OS user and one App Server. A second repository or a second project installs into that
same scope and reuses the same service and the same store; nothing here creates a daemon or a store
per project, per repository or per parent. `crw doctor` reports the resolved scope, the store and
the service by calling the relay's own `doctor` rather than by rediscovering any of it, and reports
other stores beside the resolved one without adopting any of them. Equality of path strings is not
proof under OPS-3.4; proof is `doctor` from each participating process reporting the same state
directory together with `assignment-find --issue` returning the expected relationship.

## The completion hook

The plugin package declares the Stop hook that catches a managed turn ending without the records a
completion needs, and `crw install hook --owner plugin` writes the settings it reads,
`<CODEX_HOME>/crw-completion-hook.json`:

| Setting | Written as |
| --- | --- |
| `owner`, `configVersion`, `event` | `plugin`, `1`, `Stop` |
| `adapterInterpreter`, `adapterEntryPoint` | `/usr/bin/env` and `<destination>/current/bin/crw-completion-hook`, so a cached Python launcher that runs `[adapterInterpreter, adapterEntryPoint, <settings>]` reaches the Go hook too ([decision 18](port/decisions.md)) |
| `relayExecutable` | `<destination>/current/bin/codex-session-relay`, or `--relay-command` |
| `mode` | `observe` (`--mode`), which classifies and records and never holds a turn |
| `timeoutSeconds` | the adapter's own budget (`--guard-timeout`, default 5), under the registered timeout (`--timeout`, default 10, at most 10) |
| `journalRoot`, `journalPolicy` | `<CODEX_HOME>/crw-completion-hook/journal` (`--journal-root`) and `every_invocation` |
| `markerRoot`, `dbPath`, `socketPath` | the relay's own resolution, or `--marker-root`, `--db-path` and `--socket` |
| `isolationAssertedBy` | required with `--mode hold`, and nothing else |

The decision is not made in the hook. [The hook contract](../plugins/crw/skills/crw-run/references/hook-contract.md)
fixes the rules and the relay's `guard-evaluate` implements them; `crw hook` is the piece between the
host and that guard, and it exits 0 on every path, because exit 2 is the host's blocking code.

The settings are written before anything could read them, and every precondition is checked before
any write: the event, the budget against the registered timeout, a hook file that already registers
this adapter for Stop (the user-owned registration, refused by name, because two registrations run
twice on every Stop), settings already there that this command cannot act on, and
`CRW_COMPLETION_HOOK_CONFIG`, refused while it is set, because the plugin's hook reads only the
Codex home and never that override. Settings that already say something else are refused rather
than overwritten, because they carry the mode. On a Go host, Python-era settings are replaced only
by settings that record the same host facts; other flags answer `config_differs`, naming the
differing fields and the repair: rerun with the flags of the table above that say what those
settings say, or move the document aside by hand
([one Stop settings document](#one-stop-settings-document)).
The run registers nothing and leaves the hook file untouched. Written, registered and observed to
have fired stay three separate claims.

`--socket` is recorded as `socketPath`. It is what lets the guard tell that the state directory it
resolved holds another App Server's store: a store records the socket it serves, and a Stop hook that
inherits `CODEX_SESSION_RELAY_STATE` from a second installation would otherwise read that store, find
no relationship for its assignment, and hold a child that has finished. Configure it wherever more
than one installation shares a machine.

The marker root follows the relay's own resolution, including `CODEX_SESSION_RELAY_MARKER_ROOT`. A
default that skipped it would be a disagreement: the coordinator would publish its intents under one
tree while this hook looked under another, and every managed turn would read as unmanaged.

### Trust, and the command that is fixed when a turn starts

Codex runs a declared hook only once it has been trusted, and nothing in this repository grants
trust. Trust is recorded against the declaration's content, so a plugin update that changes the hook
command's text needs the hook trusted again, and Codex asks for it; until then nothing fires. An
update of the runtime changes no declaration, because the command names the pointer, and so needs no
new trust ([plugin packaging](plugin-packaging.md#why-the-hook-resolves-the-pointer)).

A hook command is fixed when a turn starts, and every Stop of that turn reuses it. A turn that
started while the package still declared the Python bootstrap goes on running it:
`${PLUGIN_ROOT}/wiring/crw_stop_hook.py` first, and `<CODEX_HOME>/crw-stop-hook.py` once that version
directory is gone. `crw install` places no such launcher and leaves an existing copy exactly as it is;
the Python installer placed it. Either launcher reads the same settings and runs the adapter they
name, which is the Go hook once the settings are Go-era. The packaged launchers left the package in
todo 43, after the cutover commit: the version directory such a turn names is the pre-native one,
which the install that brought the native wiring removed, so the copy is what answers it. The copy
and the Python runtime stay while any live or resumable task can still run such a command, and the
operator removes them only when the retention scan reports no reference to them
([retention](port/cutover.md#retention)).

### Who registers the hook

The plugin package declares this hook, and a host holding a second registration of it would run both
on every Stop: each leaves a row, and only the one that claims the event's accepted record asks the
guard ([one accepted record per Stop event](#one-accepted-record-per-stop-event)). `crw install hook`
writes for the plugin owner only; the user-owned registration, an entry in `<CODEX_HOME>/hooks.json`,
retired with the Python installer, and one already there is refused by name. The hook repeats the
check at run time, because installing the plugin is not a command this repository runs:
`crw hook --plugin-launch` stands down in silence unless the settings name the plugin as owner.

Only a verdict that agrees with itself is acted on. A verdict whose own decision releases while its
`hook_output` holds did not come from the guard, and any disagreement reads as
`guard_verdict_incomplete` and releases. Failures keep their own names: a runtime that could not be
run carries its `errno`, and an exit of 2 carrying the relay's own error record is the relay
declining a request it understood, while an exit of 2 carrying nothing is its argument parser
refusing before any command ran. Every one of these releases the turn and is recorded.

## One accepted record per Stop event

A turn can end more than once. When any Stop hook holds, the host appends a continuation to the
same turn and fires Stop again; when a message was waiting, it appends that and does the same. Each
of those is its own Stop event, and each one gets its own decision. Two registrations answering one
Stop, or one Stop delivered twice, are a different thing: one event handled twice. The adapter keeps
exactly one accepted record per event, asks the guard once for it, and still leaves a row for every
invocation, so the two cases stay apart instead of being counted as one number per turn.

### What identifies an event

The host hands a Stop hook nine fields and no per-invocation identifier. None of them separates two
events of one turn: `turn_id` is kept across a continuation chain, `stop_hook_active` is false on a
turn's first Stop and true on every later one, and `last_assistant_message` can repeat word for
word. What differs is the transcript. Every sampling that ends in a Stop leaves one final answer,
and before running Stop hooks the host records it in the file named by `transcript_path` with the
turn id, the thread id and an item id of its own. So an event is

    (session_id, turn_id, stop_hook_active, answer item id)

where the answer item is the one Stop of the turn, as the transcript shows it, that reported the
payload's text under the payload's `stop_hook_active`. It is established only when the latest
sampling's answer is recorded, exactly one Stop of the turn matches, and the matching answer's
thread is the delivered session. The key is a SHA-256 over the four values, so no host value becomes
a path component and nothing is minted per invocation.

The transcript is read backwards from its end to the turn's start, bounded at 64 MiB and 0.75
seconds, inside the margin the launcher keeps over the guard budget. A missing or unreadable
transcript, a scan that hits either bound, an unfinished last line and any failed condition leave
the identity unestablished, with the reason on the row. An unestablished invocation is asked about
exactly as before and is never deduplicated.

### Accepted records and attempt rows

A claim is two create-once files. The first is the host's,
`<CODEX_HOME>/crw-completion-hook/stop-events/<key>.json`: every registration the host starts for
one Stop inherits that Codex home, while the settings each one reads, and so its journal root, may
differ, so this is the file two registrations of one host always meet. Only the invocation that
creates it owns the event, and only the owner asks the guard. The owner creates the accepted record,
`<journalRoot>/accepted/<key>.json`; asks the guard; writes its row; and then writes
`accepted/<key>.outcome.json` naming the session, the turn, the outcome and the row. An invocation
that finds either file already there asks nothing, prints nothing and writes a row whose
`adapterOutcome` is `duplicate_invocation`. When the host's file can be neither created nor found,
nobody can own the event, so nobody asks: the invocation writes an `arbitration_failed` row and
releases the Stop, the adapter's ordinary failure direction.

Rows are `<journalRoot>/<YYYYMMDD>/<32 hex>.json`, one JSON object on one line with sorted keys, and
every count of them counts invocations. A row carries `sessionId`, `turnId`, `adapterOutcome`,
`eventKey`, `eventIdentity`, `acceptance` (`accepted`, `duplicate`, `unestablished`, `unclaimable`,
`claim_failed` or `unarbitrated`), `acceptedAs` and `guardInvoked`. `faults_only` leaves out the rows
of answered and duplicate invocations of identified events; `no_journal` keeps no rows at all.

The outcome record says what the adapter answered, not what the host received: it is written before
the answer is printed, exactly as the row is. A claimant killed between its claim and its outcome
leaves a claim with no outcome, and the event still has its owner, so a later delivery of it is a
duplicate and the Stop was released.

### Reading it back

`crw-dev stop-events --journal-root <root>`, in the repository's development binary, reads the rows,
the accepted records and the host ledgers the claims name, and answers one verdict (the same reading
as `scripts/stop_events.py` while that script exists). `FALSE` (exit 1) means an event was
accepted more than once; `UNREADABLE` (exit 3) means the reading cannot vouch for what it read, and
names why; `TRUE` (exit 0) otherwise.
`--session` and `--turn` choose the events of one turn, `--since` and `--until` a window in the
records' own `YYYY-MM-DDTHH:MM:SSZ` format, `--journal-root` repeats for every root the host's
registrations write to, and `--codex-home` adds a host whose ledger no claim names yet. The
invocations it cannot judge, whose identity was not established or that had no owner, are counted by
reason and never read as answered.

### Limits

The guarantee holds among the registrations of one Codex home, which is every registration one host
starts for a Stop. The claim files are never removed, like the rows. That the host records the answer
before running Stop hooks was observed in every isolated run and is consistent with every record in
the live journal, but it is not a documented host contract. A turn whose Stops repeat the same text
trades deduplication for safety: those Stops are asked about by every registration, and a reading of
a window that holds them is `UNREADABLE`.

## Registration is not firing

Written settings, a declared hook, a trusted hook and a hook that fired are four different facts,
and the only evidence of the last one is a record of this turn. End a turn in an ordinary task
started after the change, take its session and turn ids, and look for them in the journal:

```sh
journal="${CODEX_HOME:-$HOME/.codex}/crw-completion-hook/journal"
grep -l '"sessionId": "<session>"' "$journal"/*/*.json | xargs -r grep -l '"turnId": "<turn>"'
```

A row naming that session and turn is a callback this procedure can attribute; its `adapterOutcome`
says what the adapter did. No row is not a smaller number of callbacks: the turn did not reach this
hook's journal, and the cause is one of these, read in order:

| Cause | How to tell |
| --- | --- |
| The hook is not trusted | Codex has not been asked to trust this declaration since the command text last changed; nothing fires until it is |
| The pointer names no runtime that reads `--plugin-launch` | `crw doctor`: `runtime.state`, `runtime.kind` (`python-venv` has no `bin/crw`), and a Go build older than decision 26 ([the native wiring](plugin-packaging.md#the-native-wiring)) |
| No usable settings | `crw doctor`: `settings.crw-completion-hook.json.state`; the hook releases in silence without settings, with settings it cannot read, and with settings another owner holds |
| Journalling is off | `journalPolicy` is `no_journal`, or `faults_only` and nothing faulted |
| A different journal | the settings name another `journalRoot` than the one you read |

A subagent's turn is no signal: on the measured host subagent turns recorded no Stop at all. The
daemon is not on this path. The guard reads the marker and a read-only database, so a stopped daemon
is not observable from a Stop and is never inferred from one. `runtime_install.py hook-status`, the
Python installer's cell-by-cell reading of the same question, has no `crw` counterpart
([the Python fence installer](#the-python-fence-installer)).

## The composed acceptance run

Installing, updating and hooking each have their own rules above. What none of them states is the
sequence a host actually lives through, with the state that has to survive it put there before the
first install and read again after the last refusal. The Go installer's tests run that sequence
against temporary homes: install, the same install again, an update that fails at a step and puts
everything back, rollback and remove (`internal/runtime/install`, `lifecycle_test.go`,
`decisions_test.go` and `restore_test.go`), and the native Stop command and the launchers through
the pointer (`wiring_test.go`; the retired Python launchers from the pre-native testdata).

### Seven questions, seven readings

The composed run asks seven questions and forbids one reading standing in for another. A reading
that could not be made is unreadable, never `false`, and never the value of the reading beside it.

| Question | Answered by | Read from | Never established by it |
| --- | --- | --- | --- |
| Skill link | `crw-dev skills link --check`, on a linked host | its report | that a linked skill is loaded or trusted by a host. A plugin host has no links; `codex plugin list` is its reading |
| Runtime reach | `crw doctor` | `runtime.state`, `runtime.kind`, `runtime.agrees` and each `components.<name>.class` | that the runtime works for a task |
| MCP tool exposure | a fresh Codex task | the bridge tools it lists, and `get_capabilities` | anything about a task started before the change |
| App Server connection | `crw doctor` | `checks.results.connected` | that a socket that accepted a connection will accept delivery |
| Real hook callback | the journal | the rows naming the turn you ended | that a firing was judged correctly |
| Model and permission preservation | a byte comparison of `config.toml` | the file before the run and after the `crw` commands | that anything a Codex process writes later was preserved |
| Delivery acceptance | a trial | `checks.results.deliveryAccepted` from a run that created and delivered work | that an accepted delivery was acted on |

### Running the combination against a real host

A real combination is an operator action, not a check. It needs a home, a Codex home, a host record
and a state directory that are yours to change, and it establishes nothing until it is recorded. The
destination is `$HOME/.local/share/crw-runtime` of the `HOME` the commands run under, and the live
half reaches this runtime only through the plugin's declared commands, so run the block under the
`HOME` the Codex process runs with. The block names the host record and the Codex home on every
command, and the state directory wherever one is read, because none of them derives from another:
the Codex home is where the settings the plugin reads are written, and the host record defaults to
`$XDG_STATE_HOME/codex-relay-workflow/host-record.json`, whatever the Codex home. `<codex-home>` is
the Codex home that process reads. Until todo 43 the Go build cannot
serve a store in that `HOME`'s default relay state directory
([the live-state guard](port/cutover.md#the-live-state-guard-until-todo-43)), so on a host whose
relay store is that one, or that still runs the Python runtime, the procedure waits for
[the cutover](port/cutover.md).

```sh
# Substitute every <...> below before running any of it. They are placeholders, not literals, and
# an unsubstituted one is a shell redirection rather than a value.

# A NEW receipt directory, so no earlier run's files are read as this one's. mkdir without -p is
# the check, and it ends the procedure rather than running the rest into a directory it refused.
mkdir <receipt> || exit 1

# Preservation is a comparison. No crw command below writes config.toml, so the whole file is
# compared rather than keys guessed out of it. A fresh Codex home has none; record the absence.
if [ -f <codex-home>/config.toml ]; then
    cp <codex-home>/config.toml <receipt>/config.before.toml
else
    printf 'no configuration existed before this run\n' > <receipt>/config.before.absent
fi

# Nothing puts crw on PATH. The install runs the copy unpacked from the release archive into
# <scratch> (see "Installing the runtime" above); every later command runs the one the pointer
# then selects.
# The exit status belongs IN the receipt: a receipt that kept the result and lost the status
# cannot say whether the install refused, or that a 3 means the change landed.
<scratch>/crw install install --release <tag> --record <record> \
    --codex-home <codex-home> --state <state> --socket <socket> > <receipt>/install.json
printf 'install exit=%s\n' "$?" > <receipt>/install.exit

crw="$HOME/.local/share/crw-runtime/current/bin/crw"

"$crw" install register-mcp --owner plugin --record <record> \
    --codex-home <codex-home> --execution-policy <policy-file> > <receipt>/register-mcp.json
printf 'register-mcp exit=%s\n' "$?" > <receipt>/register-mcp.exit

"$crw" install hook --owner plugin --record <record> \
    --codex-home <codex-home> --socket <socket> > <receipt>/hook.json
printf 'hook exit=%s\n' "$?" > <receipt>/hook.exit

"$crw" doctor --record <record> --codex-home <codex-home> \
    --state <state> --socket <socket> > <receipt>/doctor.json
printf 'doctor exit=%s\n' "$?" > <receipt>/doctor.exit

# The other half of the preservation reading, taken before anything else can write the file.
if [ -f <codex-home>/config.toml ]; then
    cp <codex-home>/config.toml <receipt>/config.after.toml
    cmp <receipt>/config.before.toml <receipt>/config.after.toml > <receipt>/config.cmp 2>&1
    printf 'cmp exit=%s\n' "$?" >> <receipt>/config.cmp
else
    printf 'no configuration existed after the crw commands\n' > <receipt>/config.after.absent
fi

#   ... then, in Codex: trust the hook if it asks, start a fresh task, read the bridge tools and
#   get_capabilities there, end a real turn, and note that turn's session and turn ids. Only then:

journal=<codex-home>/crw-completion-hook/journal
grep -l '"sessionId": "<session>"' "$journal"/*/*.json | xargs -r grep -l '"turnId": "<turn>"' \
    > <receipt>/rows-for-this-turn.txt
printf 'rows grep exit=%s\n' "$?" >> <receipt>/rows-for-this-turn.txt

# From a checkout, the exactly-once reading for that turn:
go run -tags dev ./cmd/crw-dev stop-events --journal-root "$journal" --codex-home <codex-home> \
    --session <session> --turn <turn> > <receipt>/stop-events.json
printf 'stop-events exit=%s\n' "$?" > <receipt>/stop-events.exit
```

What this block is, and what it is not. It installs, writes both records, takes the readings and
reads the results back. It does not re-run the install, it does not present a second archive, and
it does not fail an update; this page will not tell an operator to break a runtime their host is
using in order to watch it come back. The tests above exercise those stages against temporary homes.

`cmp` exiting 0 is preservation. A difference means something wrote the file between the two copies,
and the receipt holds both sides for reading which keys moved. `rows-for-this-turn.txt` naming no
file is a turn that did not reach the hook ([registration is not firing](#registration-is-not-firing)),
not a smaller count. More than one row for the turn is not a duplicate by itself: a turn whose Stop
was held ends again, and that is a second event; `stop-events.json` separates the two.

Three of the seven are readings of something live, and the block supplies none of it: an App Server
accepting connections at `<socket>`, a session that actually listed the bridge tools, and a relay
that can carry an assignment to a returned turn id. `crw doctor` reports `deliveryAccepted` as
`not_applicable` because it creates no work; a live trial ([live-trial.md](live-trial.md)) is how a
host gets that reading. If the live half is absent, the honest receipt records the absence for that
row and says the rest. It never carries a row forward as though the question had been put.

A real run records the exact release it installed, the host it ran on, the destination kind, and the
answer to each of the seven with the command that produced it and the time it was produced. Those
receipts are host facts: they belong in the private record outside this repository, not in a commit.
This page is the procedure and the shape. It is not a record that anybody ran it.

## The Python fence installer

This section is developer-only and pre-cutover. `scripts/runtime_install.py` installs the Python
runtime, the fence release that step 0 of [the cutover](port/cutover.md#step-0-deploy-and-activate-the-python-fence-release)
deploys, and it is the rollback path until the Python execution path is removed (todo 44). It
shares the destination, the owned pointer, the host record, the promotion lock and both settings
files with `crw install`; what it installs is a Python virtual environment,
`<destination>/env-1-<digest>`, built from this checkout's packages. It still takes `--dest`, and on
a host `crw install` also serves, `<destination>` has to be `~/.local/share/crw-runtime`:
`crw install` refuses a host record whose pointer names another link.

| Command | What it does |
| --- | --- |
| `python3 scripts/runtime_install.py install --dest <destination> --apply` | Build, exercise and promote a Python runtime from this checkout. It needs a Python 3.11 or newer interpreter for the runtime (`--python`), and the controller needs `tomllib` (Python 3.11 or newer) to read a Codex configuration |
| `python3 scripts/runtime_install.py diagnose --dest <destination>` | The Python diagnosis, including `--trial` |
| `python3 scripts/runtime_install.py register-mcp --owner plugin --bridge-command <destination>/current/bin/codex-thread-bridge --apply` | The bridge record, with `--execution-policy`; before a version-2 record it probes the enabled package's cached launcher |
| `python3 scripts/runtime_install.py hook --adapter completion --owner plugin --dest <destination> --apply` | The Python-era Stop settings, and the fallback launcher `<CODEX_HOME>/crw-stop-hook.py`, placed before them. Since todo 43 the package no longer ships the launcher, and the copy comes from `internal/pluginwiring/testdata/pre-native-wiring/crw_stop_hook.py`, the same bytes |
| `python3 scripts/runtime_install.py hook-status` | The cell-by-cell firing reading, including `firingRecordAbsence` |
| `python3 scripts/runtime_install.py verify-definition` | Re-derive [the one definition](#the-one-definition); CI runs it |

Two properties of a Python runtime outlive this installer, and the cutover's retention rule rests on
them. pip writes an absolute shebang into every console script, so a process started through
`current` reports and keeps its concrete `env-*` directory after the pointer moves; and a Stop
command fixed before the native wiring names `python3` and a `.py` launcher. Both keep `env-*`
directories and the `<CODEX_HOME>/crw-stop-hook.py` copy in place until `crw doctor retention-scan`
reports nothing that resolves to them. The packaged launchers did not wait for that: such a command
names the pre-native version directory, not whatever the package ships now, so todo 43 retired
them from the package after the cutover commit.

### Trial mode

`runtime_install.py diagnose --trial` is the only command that fills `deliveryAccepted` itself: it
registers one relationship, emits and delivers once, and records the returned turn id. Everything the
trial needs is checked before its first command, against what the relay itself enforces rather than
what is merely present, so an incomplete trial writes no settings and no relationship row. It has no
`crw` counterpart (it is deferred past the Python path's removal in [the inventory](port/inventory.md));
[a live trial](live-trial.md) is a different thing with a similar name.

The full reference this page carried for the Python installer, its design record and its acceptance
procedure, is this page at the parent of the commit that rewrote it for `crw`, the oldest one that
names todo 41:

```sh
rewrite="$(git log --format=%H --grep='(todo 41)' -- docs/runtime-install.md | tail -1)"
git show "$rewrite^:docs/runtime-install.md"
```

## What none of this establishes

Running these commands against a temporary destination proves what they did there. It is not
evidence about a host's real Codex home, its installed runtime, its bridge record or its operational
database. `installed`, `mcpExposed`, `connected`, `deliveryAccepted`, `verificationComplete` and
`alwaysActive` are six separate facts under OPS-6.1, and none of them is read from another.

Written settings are not a fired hook, and a fired hook is not a delivered hold. That a settings file
is there says nothing about the host having run the hook, about the runtime it names being able to
answer, or about any turn having been judged. Those claims need the host's own evidence.

A successful update is not one of them either. That the pointer moved, that the gate found the daemon
stopped and no attempt open, and that the store's schema was compatible are readings taken at one
moment, about one destination. They say a swap was permitted and performed; they do not say the new
runtime works, and the point that would say so is measured before the swap rather than after it. Nor
does a refused update establish that a store is healthy: the gate reads whether it is safe to replace
a runtime, and reads nothing about whether the data in the store is correct.
