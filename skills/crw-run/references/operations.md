# Operations contract

Read when installing, updating, or operating the runtime that `crw-run` depends on, when assigning
a checkout to an implementation task, and when deciding how a parent waits for delegated work. This
file is the single owner of dependency identity, installation ownership, the shared relay service
and its store, daemon lifecycle, workspace assignment, assignment routing identity, parent return
and fairness, check-result reporting, and the review location of each dependency repository.

Relay command usage belongs to [codex-session-relay](relay.md), bridge launch and recovery to
[Bridge launch and recovery](bridge.md), and integration gates to
[Merge readiness](merge-readiness.md). Nothing here installs, registers, starts, moves, or
publishes anything by itself.

## OPS-0 Status words and how a clause is written

Every statement in this contract and in the records it governs carries one of five status words,
and they are not interchangeable. **Measured** means a named command produced the value on a named
host at a named time. **Decided** means the user settled it. **Proposed** means someone suggested
it and nobody has accepted it. **Unresolved** means a decision is required before dependent work
starts. **Unmeasured** means no recorded observation covers it, which is different from knowing it
is false and different from knowing it never happened; an unrecorded check is not a check anyone
can rely on.
A report that omits the status word is incomplete, because a proposal read six months later is
indistinguishable from a decision unless it says so.

Wherever a rule can be checked, it is written as the output of a named command compared with a
named value, so a later implementation issue can turn it into a checker without reinterpreting
prose. Rules that cannot be expressed that way are kept few and marked as judgment. A clause that
describes intended behaviour of something not yet built is proposed implementation, and it is never
reported as measured installed behaviour.

## OPS-1 The compatibility record

### OPS-1.1 One record per component

`codex-relay-workflow` owns one compatibility definition covering every component the workflow
depends on. There is exactly one such definition; a version repeated in a second document or script
is a copy that will go stale. Each component record carries the component name, the source checkout
path, its remote as a URL or the literal word `none`, the full commit SHA, the full `HEAD^{tree}`
SHA, the working-tree cleanliness from `git status --porcelain`, the distribution version from
`pyproject.toml`, the declared `requires-python`, the install mode as `editable` or `copied`, one
integrity digest per install location, and a list of measured points.

An install records two different paths and never conflates them. The environment is the virtual
environment whose interpreter runs the code. The location is the package directory that
interpreter actually imports, which for a copied install sits inside the environment and for an
editable install does not: an editable install leaves a path file pointing back at the source tree,
so its location is that source directory and nothing exists under the environment's site-packages.
Recording a site-packages path for an editable install describes a directory that is not there, and
the re-measurement in OPS-1.4 then cannot reproduce the record. The location is read from the
interpreter rather than assumed.

Both the commit and the tree SHA are recorded because they fail differently. A commit SHA changes
when history is rewritten, while the tree SHA survives that, and a component here has no remote at
all, so there is no published tag or branch to anchor either one. Neither can be recomputed from an
installed copy, which is why the digest exists.

### OPS-1.2 The integrity digest

The integrity value is a SHA-256 over the package directory, computed by walking every file except
anything under `__pycache__`, sorting by POSIX-style relative path, and feeding the hash the relative
path, a zero byte, then the SHA-256 digest of the file's bytes, in that order. It is defined this
way so anyone can recompute it with the standard library alone and get the same answer for a source
tree and for an installed copy of it.

The digest is recorded once per install location, because a component can exist several times on
one host. In the example the bridge exists twice: an editable install that the MCP server runs, whose
location is the source tree itself, and a copied install inside the relay's virtual environment.
Those are separate installs of the same version and they drift independently. Equality of their
digests is the only thing that detects the drift, and a matching version string proves nothing,
since both stay `0.1.0` while their contents change. An editable install's digest equals its
source digest by construction, which is a property to record rather than a coincidence to discover.

### OPS-1.3 Measured points, never ranges

A measured point is `{interpreter, codexCli, appServer, host, date, measuredBy, method}`. The record
contains only points that were actually observed. It never states a compatible range, a minimum, or
a maximum, because nobody has run the combination across a range and a range implies they did. A
second Python version is a second point. A combination that has never been measured is
`unmeasured`, which withholds a claim rather than asserting incompatibility.

A point means the combination was exercised, not that its files were counted. An install's digest is
byte identity and lives on the install; it is not a point and never becomes one by being recorded
next to an interpreter version. The distinction matters because OPS-1.4 and OPS-2.2 consume points
as evidence that a combination works, and neither reads a disclaimer attached to one. So an
inventory observation of a package sitting in an environment nobody ran it under leaves that
combination `unmeasured`, and saying so in the method field does not convert it into a point.

### OPS-1.4 Validity is event-bound

The record is re-measured whenever the shared service starts and whenever a dispatch registers a
relay assignment, and the measurement is attached to that run's receipt. A digest that is not
already in the record flips the combination to `unverified`, which blocks the host-required commands
under OPS-2.2 and OPS-4.1 until every condition `own` names holds again. A new measured point is part
of getting there and is not by itself enough, because a recorded checkout that is still dirty
classifies `fork` however many points exist; the routes that do reach `own` are S4 in
[scenarios](operations/scenarios.md). Validity is tied to these events
rather than to a review date because a calendar gives nobody a reason to reread the record, and a
record nobody rereads becomes fiction the first time somebody reinstalls a copied install and
forgets.

A filled record is in
[compatibility-record.example.json](operations/compatibility-record.example.json). Its values are
invented, because a real one is a private receipt under OPS-3.2 rather than something to commit, but
it is worth reading for what it deliberately does not contain. It states identity throughout while
leaving the compatibility side empty, modelling a host whose commands read bytes without ever
executing either component. A record can be complete and still establish nothing about whether the
combination works, and this clause exists so that gap is visible rather than papered over by a
populated-looking file.

### OPS-1.5 Identity once the components share one repository

A repository commit identifies a checkout, not a component, and the destination repository holds
three of them. So a component record carries four identities that answer different questions and do
not substitute for one another: the repository commit and tree for the checkout it came from, the
component's subdirectory path and that subdirectory's tree hash for the component itself, the
retained upstream provenance for where the code originally came from, and the OPS-1.2 digest for the
bytes actually installed and running.

The subdirectory tree hash is what makes a shared repository workable. Every package moves whenever
any of them changes, because they share a commit, so a repository commit cannot tell you whether the
relay changed. Its subdirectory tree can.

Upstream provenance is retained rather than replaced. A component that arrived from another
repository keeps the original remote, commit, tree and licence notice in its record, because
consolidating source does not erase where it came from and a reader comparing against upstream later
needs the anchor.

A real record describes the external checkouts where those components still live, because the source
has not moved. The committed example uses invented paths for that, since a live record is a private
receipt under OPS-3.2 rather than something to vendor. Neither is rewritten to describe a layout that
does not exist yet. Once the
migration has actually run, the same record is measured again against the new subdirectories, the
repository commit and tree are filled in, the subdirectory tree hashes are computed, the upstream
provenance is carried across unchanged, and the installed digest is recomputed from whatever the
host is then running. Until that measurement happens the migrated identity is `unmeasured`.

## OPS-2 Installation ownership and conflicts

### OPS-2.1 Four signals

Classification uses four measured signals and nothing else: where the entry point actually
resolves, the checkout's commit, tree and working-tree cleanliness, whether the record agrees with
what is installed, and what the Codex configuration registers. The third signal is read twice, once
for the digest and once for the point, because a digest is byte identity while a point is evidence
somebody ran the combination, and an install can match every recorded digest while no point exists
for the interpreter it runs under. The entry point matters because a console script can carry a
shebang into a virtual environment inside a live development checkout, which means the user-facing
command is bound to whatever that checkout currently contains. Cleanliness is a required input
rather than a detail, because an uncommitted change outside the package directory leaves the digest
untouched while the checkout is no longer the revision the record names.

A signal that cannot be read is not a signal that agrees. If the commit, the tree, the cleanliness,
a digest or the registration cannot be read, classification stops and reports which reading failed
and why, instead of classifying on the remainder and calling the missing one satisfied.

### OPS-2.2 Five classes and their rules

| Class | Observed | Rule |
|---|---|---|
| `own` | Entry point resolves into a recorded checkout or its environment, the commit and tree match the record, the working tree is clean, every digest matches the record, and the combination it runs under has a measured point in OPS-1.3 | Reuse it |
| `foreign` | Entry point or MCP command resolves outside every recorded path | Never overwrite. Usable only if its digest matches the record and the combination it runs under has a measured point. Otherwise report `unverified` where a recorded expectation is contradicted and `unmeasured` where none was ever established |
| `fork` | A recorded path whose commit, tree, or digest differs, including a dirty working tree | Preserve untouched, treat as `unverified`, and do not update it silently |
| `unmeasured` | A recorded path where nothing conflicts and nothing differs: the commit and tree match, the working tree is clean, and every digest matches the record, but no recorded run covers the combination it runs under, so OPS-1.3 holds no point for it | Preserve untouched and do not reuse it for a host-required command. Report `not_verified` naming the combination that has no point. Cleared only by running that combination and recording the point, or by moving the whole combination under OPS-2.4 |
| `conflict` | An MCP server name already registered with a different command or socket, a hook identity already present with a different trusted hash, or the skill installer reporting `CONFLICT` | Stop, report both values, change nothing |

The classes are evaluated in a fixed order and the first match wins: `conflict`, then `fork`, then
`foreign`, then `unmeasured`, then `own`. Order is part of the rule because the conditions
genuinely overlap. A recorded checkout carrying an uncommitted change outside the package directory
still produces a matching digest, so without precedence it would read as `own` and be reused, and the
fork handling that exists to protect the user's work would never run. `own` is therefore the residual
class, reached only when nothing else matches.

`unmeasured` sits immediately before `own` because it is the one difference between them. Everything
about the installed bytes agrees, and what is missing is evidence that anybody ran them in this
combination. It is a named class rather than a leftover, because a state with no class has no
prescribed action, and the two actions that suggest themselves at that moment are to reuse the
install anyway or to let the matching digest stand in for the missing point. OPS-1.3 already refuses
the second and the rule above refuses the first.

The evidence reading is not the property of one class. `own` carries it among its own conditions and
`unmeasured` exists for a recorded install that fails only that reading, but a `foreign` install is
subject to the same test, because a digest matching a recorded digest says the bytes are known and
not that anybody ran them where they now sit. Bytes recorded at one interpreter and running under
another are a different combination, so a foreign install with no point for the combination it
actually runs is reported `unmeasured` and left unused, exactly as a recorded one would be.

`unmeasured` and `unverified` block the same commands and are repaired differently, so a report says
which one it is. `unverified` means a recorded expectation was contradicted, and restoring or
re-recording the runtime clears it. `unmeasured` means no expectation was ever established, and only
running the combination clears it. Neither is a claim that the component is broken.

A single installed-or-not flag is not sufficient, because the question is never whether something
is present but whether reusing it is safe, and only the class answers that.

### OPS-2.3 The skill link entry point is unchanged

`scripts/install.py` remains the standard-library-only, idempotent entry point that links this
repository's skills into Codex, and it keeps refusing to replace an existing directory or a foreign
link. Runtime installation of the MCP server and the relay is a separate step that is never folded
into it. The installer's `LINKED`, `MISSING`, and `CONFLICT` outcomes are reused as the vocabulary for
describing runtime installs so that one word means one thing across both layers.

### OPS-2.4 Update and recovery

An update moves a whole verified combination, never one component. The sequence is to measure, then
install, then measure again, then add the new point to the record. The second measurement is not a
repeat of the first: the first reads identity and bytes, while the second has to include actually
exercising the new combination, because that run is the only thing that produces a point under
OPS-1.3. An update whose second measurement only recomputes digests has installed something and
established nothing. A copied install requires an actual reinstall for a source change to take
effect; an editable install does not, which is why the install mode is part of the record.

A failed update leaves the entry point `unverified` and blocks a service start under OPS-4.1.
Recovery keeps the previous runtime in place until the replacement has passed the OPS-6 checks, and
it never removes, moves, or recreates the store. Update failure and store loss are different
accidents and the recovery for one must not cause the other.

## OPS-3 One shared relay and one durable store

### OPS-3.1 The operating scope is the sharing unit

One relay service and one durable store serve an entire operating scope, which is one host, one OS
user, and one App Server. Parents belonging to different repositories and different Linear projects
share that one service and that one store. Nothing creates a daemon or a store per project, per
repository, or per parent, because the isolation a caller actually needs is between assignments and
that already exists inside the store as rows. Splitting the store would multiply services and locks
while making cross-assignment fairness and duplicate suppression impossible to reason about.

The scope has a stable identifier of its own, recorded in the packet alongside the relationship id,
and every participant carries it. A path hash is how the default directory is currently derived, so
it is the default identifier, but it is derived from a socket path and therefore changes if that
path ever changes. The identifier is treated as a recorded value that survives such a move, not as
something to recompute and hope it matches. Always use the resolved absolute socket path: the store
directory currently hashes the user-expanded path while the bridge ledger hashes the fully resolved
one, and they agree today but would diverge the moment the socket path becomes a symlink.

### OPS-3.2 Default location

The default store is
`\${XDG_STATE_HOME:-~/.local/state}/codex-session-relay/<scope-id>/relay.sqlite3`. Operational state
never lives inside a repository and never defaults to a temporary directory, because a host may
clear one and a repository may be cleaned, and losing the store loses every in-flight assignment. A
store under `/tmp` is test or evidence state; using one for real work is a measured exception that
is recorded as such in the current Linear issue under OPS-12, rather than quietly becoming the default.

### OPS-3.3 Three artifacts, three selectors

A single assignment has three durable artifacts and they do not follow the same selector. This is
the clause most likely to be got wrong.

| Artifact | `--state` | `CODEX_SESSION_RELAY_STATE` | Default |
|---|---|---|---|
| `relay.sqlite3` and `daemon.lock` | first | second | state home plus scope id |
| Relay adapter's bridge ledger `operations-<scope>.sqlite3` | ignored | first | state home plus scope id |
| MCP server's own bridge ledger | not applicable, it takes `--state-dir` | not read, but `XDG_STATE_HOME` is | `${XDG_STATE_HOME:-~/.local/state}/codex-thread-bridge/` |

Two consequences follow. First, every participant sets both `CODEX_SESSION_RELAY_STATE` and
`--state` to the same resolved absolute path, because the flag alone moves the store while leaving
the adapter's ledger behind, and a store separated from the ledger that suppresses duplicate
delivery is a store whose recovery record lives somewhere else. Second, the MCP server keeps a
ledger of its own, so a request id issued through the MCP tools is invisible to service recovery
and the reverse is also true. Request-id namespaces never cross runtimes and no recovery procedure
may assume they do.

[codex-session-relay](relay.md) now states both selectors and carries its own measurement of the
split, so it is the command-level owner of this rule rather than a reference to correct. What stays
here is the consequence. A participant that cannot set both does not fall back to the default store,
because OPS-3.4 shows that opening a path constructs it, so the fallback would quietly produce an
empty store while the assignment stayed where it was. Such a participant proceeds only where the
packet's recorded store already is the default, and otherwise a process that can set both acts on
its behalf.

### OPS-3.4 Proving that everyone shares one store

Equality of path strings is not proof. Every command constructs the store on open, creating the
directory and an empty database when they are absent, so a mistyped path produces a silent empty
store instead of an error. Proof is `doctor` from each participating process reporting the packet's
`stateDirectory`, together with `assignment-find --issue` returning the expected relationship. A
`relationships` count of zero where the packet says an assignment exists means the process is
pointed somewhere else, not that no assignment exists.

### OPS-3.5 Reaching the store from a sandboxed task

A task reaches the store only if the state directory is inside its writable roots, and that is
settled when the task is created rather than widened afterwards to make a later send connect. The
coordinator passes the state directory as an explicit writable root and records it in the task's
settings record.

Write access is required for every relay command, not only the ones that reach the App Server.
Each command opens the store on construction, creating the directory and the database file
read-write, so a task without write access cannot run even a read-only-looking command such as
`doctor`. There is no store-only fallback for a task that cannot write the directory, and offering
one would send that task into a failure halfway through its first command. The offline subset is
the fallback for a task that can write the store but cannot reach the socket, which is a different
situation.

Giving a task that directory is a trust decision, and it is worth naming what it grants. OPS-3.1
puts one store behind an entire operating scope, so the file holds every parent's assignments,
attempts and receipts. A writable root over it grants direct read and write on all of them. The
recipient rules in OPS-7 constrain what the relay routes; they are not enforcement against a
participant holding the database, which can read another project's receipts or alter delivery state
without the relay being involved at all. Naming that is the point: route filtering is not filesystem
isolation, and a reader should not mistake the first for the second.

For a trusted task inside the scope that owns the store, that trust is already granted and direct
access is the normal arrangement, recorded in the settings record as the access it is. It needs no
further approval, and routing its store operations through another process by default would add a
round trip without adding a boundary, since the trust decision was made when the task was created.

The narrower route exists for the cases that actually differ: a task outside that trust scope, or
one that cannot write the directory. There an authorized host-capable process owns every store
operation on the task's behalf, including emitting on its behalf, and the report says so rather
than implying the task reported for itself. For a task that cannot write the directory it is the
only route, since the measured behaviour above leaves no store-only fallback. So a task that will
use the relay directly has its access verified before dispatch, and a task that will not runs no
relay command at all.

Whether a given creation path honours an additional writable root on the current Codex version is
`unmeasured` and is checked rather than assumed.

## OPS-4 The shared service

### OPS-4.1 Ownership is the operating scope, not a parent

The service belongs to its operating scope. Whoever operates that scope starts, stops and restarts
it, and implementation children never start one. A start is refused unless every component the
service needs classifies `own` under OPS-2.2, and it is refused again when the re-measurement
OPS-1.4 requires at that moment turns a combination `unverified`, or when an update failed under
OPS-2.4. Naming the one permitting class rather than listing failure states is deliberate, because
a list leaks: a dirty tree whose digest still matches, a foreign path, a conflict and a combination
nobody has run are all refusals without having to be enumerated, and code nobody has run is no safer
to start than code that no longer matches its record. A refusal names the class and the component it
came from.

Because several parents share one service, no parent may stop it. A parent completing, cancelling
or abandoning its work ends that assignment and nothing else, and a shutdown that would interrupt
another parent's in-flight delivery requires the scope operator, not whichever parent happens to
finish first. This is the rule that makes sharing safe, and without it the first parent to finish
silently breaks everyone else.

The MCP server process is a separate case: Codex spawns and restarts it from its registration, not
the scope operator, and it keeps its own ledger as OPS-3.3 describes.

### OPS-4.2 Duplicate-start evidence

Determine the installed version's lock scope from current implementation and execution
evidence before claiming one service per operating scope. A per-store lock alone does not
establish OPS-3.1: two callers can pass different state directories and acquire different
locks. If that is the measured behavior, require every participant to use the same recorded
state path until scope arbitration under OPS-4.6 is implemented and verified.

For a version using an exclusive non-blocking `daemon.lock` inside the state directory,
a second start against that directory must be refused. The lock belongs to a live process;
a leftover filename is neither ownership evidence nor permission to delete it to force a
start. Keep dated observations and the installed revision in the private compatibility
record rather than treating a historical probe as the current runtime's behavior.

### OPS-4.6 Scope-level ownership and start arbitration, proposed

This clause is `proposed`, an implementation proposal. Nothing here describes installed behaviour, and recording
it starts no service and moves no store.

The service belongs to the operating scope, so arbitration has to be decided by the scope rather
than by whichever path a caller happened to type. For a runtime that selects the store before scope arbitration, the path
selects the store and the store selects the lock, and the scope is never consulted.

| Requirement | Behaviour |
|---|---|
| Authoritative binding | A scope record binds one scope id to exactly one authoritative store path, and that record is what a start consults first |
| Same scope, different store | A start whose resolved store is not the scope's authoritative one is refused, and the refusal names the scope id, the authoritative path and the path the caller gave |
| Different scope | Starts in genuinely different scopes proceed independently and never contend, because contention is per scope and not per host |
| Competing starts | Two simultaneous starts in one scope resolve atomically to a single winner, decided in the same step that would have admitted either, so neither can win by arriving first at a different path |
| Alias and path change | A socket path that is renamed, symlinked or resolved differently maps to the existing scope rather than minting a new one, because a new scope id would silently create a second service with its own store |
| Mismatch diagnostics | A refusal reports the scope id, both store paths, and which participant supplied each, since the failure this prevents is invisible unless the message names the two stores |

Existing stores are preserved. Introducing the scope record does not move, merge or rewrite any
database, and reconciling two stores that already exist in one scope is a migration under OPS-4.5
with its own decision and its own backup.

### OPS-4.3 A process limit is not an assignment lifetime

A service process runs under a bounded deadline, four hours for example, and that bound belongs to
the process, not to the work. An assignment outlives any number of process restarts: its state is
in the store, so a process reaching its deadline, being restarted, or failing outright leaves the
assignment exactly where it was. Recovery continues from the existing store and never starts from
an empty one.

It follows that a supervised process is restarted on its own schedule without consulting any
parent, and that no assignment is ever cancelled, expired or completed because a process ended.
Conflating the two is how a durable record turns into a race against a timer.

### OPS-4.4 Updating while a handover is in flight

Let the process reach its deadline or stop it through the scope operator, reconcile open attempts
with the runtime that created them, take the OPS-1 measurement, replace the runtime, confirm the new
digest matches the record and that the combination now running carries an exercised point, run
`doctor`, reconcile again, and only then start. Those are two readings and not one: a digest that
matches proves the bytes are the intended bytes, and only the point proves anybody ran them in this
combination, so a restart authorized by the digest alone is a restart into code whose combination no
recorded run covers. This is safe because
every state transition is committed before its side effect, so an interruption leaves a recoverable
state, and because recovery resends nothing on its own.

### OPS-4.5 Runtime replacement is not migration

Replacing the runtime never touches the store files. The schema is created once at version one with
no migration path, so a change that needs a different schema is its own decision, in its own issue,
with a copied backup of the whole state directory taken first. Whether the store refuses to open a
mismatched schema version is `unmeasured`; do not rely on it refusing. No installation or update
step may move, recreate, or delete a store, and an operator who wants a store elsewhere performs a
migration deliberately rather than as a side effect of an install.

## OPS-5 Workspace assignment

### OPS-5.1 Placement

The checkout, branch, Git ownership and commit/PR capability rules in OPS-5 apply
to repository-changing implementation. Non-PR research, design and verification
use a permitted task working directory and durable private evidence root under
the applicable user/project placement convention. Record its creator, editing owner,
retention owner and cleanup authority; Git metadata ownership is inapplicable.
Do not create a repository, branch or PR to satisfy this contract. Grant the access
needed for the actual sources and result; explicit narrower scope and existing
permission limits still win, with returned settings verified under OPS-5.5.

A non-PR assignment using the relay must freeze its delivered result and evidence
in at least one file under an authorized artifact root. Emit that artifact so the
ready receipt has a nonempty manifest; a document URL alone cannot be relayed.
For a mutable linked result, include its stable ID/link, delivered revision or
updated-at evidence and verified output snapshot. Keep the input baseline separate.

An implementation task works in `/home/jun/code-worktrees/<original-project>/<task>`, where the
project segment comes from the original repository's project name rather than the directory name of
whatever checkout is currently open, and the task segment is short kebab-case. The branch is
`codex/<task>`. An existing checkout belonging to the same task is reused rather than duplicated,
and dirty work and local forks in it are preserved.

### OPS-5.2 Ownership is recorded in columns, separately from the path

A path says where the work is, not who is answerable for it, so ownership is recorded as separate
fields and never inferred from the location.

| Field | Meaning |
|---|---|
| Checkout path | Absolute path of the working tree |
| Created by | Which mechanism created it, such as `bridge-managed-retained` or the coordinator |
| Editing owner | The task id that edits source in it |
| Git metadata owner | Who may create branches and commits for it |
| Retention owner | Who decides how long it stays |
| Cleanup authorization | Which cleanup is authorized, and `none automatic` when none is |

A `bridge-managed-retained` checkout is created detached and locked. It is not Desktop-managed, it
is not bound to a Desktop project, and nothing deletes or archives it automatically. Retention
belongs to the parent that asked for it.

### OPS-5.3 Who commits, and the fallback when a task cannot

By default the child commits its own work. It owns the implementation, the tests, the commits on
its branch, the push, and the pull request, because the task that made a change is the one that can
explain it in a commit message and answer a reviewer about it. Delivery ownership is OPS-9.

That default assumes the child was created able to do it, which OPS-5.5 covers. Permission does not
follow the checkout: a worktree's git metadata lives under the original repository rather than
inside the checkout, so a task whose writable roots cover only the checkout and its evidence
directory cannot create a branch or commit there, because the reference and index writes land
outside those roots. This is measured rather than hypothetical.

Where that restriction is already in force, and only there, the fallback applies. The coordinator
prepares the git metadata, creating the branch before dispatch without widening the running task's
permissions, and the coordinator makes the commits. The child owns source edits and returns a
frozen diff against the recorded baseline with the SHA-256 of that diff and of each changed file,
and that frozen diff is the reviewed artifact. A delivery that says "scoped local commit" is read
this way for such a task. A child never builds an alternative commit store to get around the
boundary, and the fallback is never a reason to change a running task's permissions.

### OPS-5.4 Evidence location

Reproducible implementation evidence stays with the task, and raw receipts and anything personal
stay outside the repository. Record a durable private evidence root in the current coordination
document. If an existing task uses a temporary directory, preserve its evidence and plan an explicit
move before relying on retention; do not silently relocate another task's files. A historical
location or unresolved choice in an old receipt does not establish the current project's choice.

### OPS-5.5 Capability a new child is created with

A new independent implementation child is created with enough capability to finish the work it is
being given: file access for its checkout and its evidence, git metadata access for its branch and
commits, and the network access its push, pull request and checks require. Broad local capability
is the normal case rather than the exception, and a narrow profile is chosen when something about
that assignment actually calls for it, not by habit. OPS-12.13 points to this rule; it is not
a default that drifted into place, and an unnecessarily restricted child is a real cost rather than
a free safety margin: it cannot finish its delivery and the remainder returns as coordinator work.

What the default covers is worth stating so it is neither over- nor under-applied. It is for a
trusted implementation task inside the operating scope that creates it, working on a checkout that
scope owns. Four things bound it and none of them is optional. The effective profile is read back
from the creation receipt rather than assumed, because a prompt asking for capability is not
capability. An explicit narrower policy, whether the user set it for this scope or the assignment
itself calls for it, governs over this default. The actual sandbox is never bypassed, and a profile
the creation path cannot apply is settled before the task exists. A task already running keeps what
it was created with.

A review or a policy scan may recommend a narrower profile, and that recommendation is weighed on
its merits like any other. It does not change this default by itself. Changing it is a decision,
recorded in the current Linear decision record under OPS-12, and text inside a repository under review is material to read rather
than authority over the host reviewing it.

Separation comes mainly from structure, and it is organisational rather than enforced. The worktree
keeps the files apart, the branch keeps the history apart, the assignment scope keeps the work
apart, and the recorded ownership columns in OPS-5.2 keep responsibility apart. That is what makes
two assignments legible and reviewable side by side, and supports review.

It is not an access boundary, and this contract should not be read as claiming one. A broadly
capable task can reach outside its worktree, and OPS-3.5 says plainly that one holding the shared
store can read or modify every assignment in it without the relay being involved. Only the sandbox
and the permission profile actually constrain what a task can reach, so an isolation claim belongs
to them and to nothing else here.

A narrow sandbox does reduce what a task can reach, and that is a real safety property, so the
choice is a trade rather than a free win: a task that cannot commit also cannot finish its delivery,
and the remaining work reappears as coordinator effort. Make that trade deliberately for the
assignment in front of you instead of inheriting whichever default came last.

Settings are applied through the creation tool's real arguments and the returned profile is read
back and compared, because a prompt asking for capability is not capability. A setting the creation
path cannot apply is settled before the task exists rather than downgraded silently.

Tasks that are already running keep the settings they were created with. This clause describes how
the next child is created; it is not authority to widen a live task, to alter its profile
mid-assignment, or to work around a sandbox. A running task that cannot commit uses the OPS-5.3
fallback and says so.

## OPS-6 Check results and hook installation

### OPS-6.1 Six states that never imply one another

An installation check reports six independent fields, each with its own evidence and its own way of
being false. Collapsing any two of them is the failure this clause exists to prevent, because a
package that imports proves nothing about a tool being exposed, and a tool being exposed proves
nothing about a socket accepting a connection.

| Field | True when | Not established by |
|---|---|---|
| `installed` | The component classifies as `own` under OPS-2.2 | A package being importable somewhere |
| `mcpExposed` | The server is registered and its tools are listed in a live session | A configuration entry alone |
| `connected` | `doctor` from the acting process reports `actorReachability.socketConnect` equal to `ok` | A socket file existing on disk |
| `deliveryAccepted` | An attempt recorded a returned turn id | A dispatch, a staged receipt, or an absent error |
| `verificationComplete` | Every condition in OPS-6.4 holds at once | A completed turn, a green check, a verdict that merely exists, or an integration |
| `alwaysActive` | A supervised runtime survives a host restart | Any of the five above |

### OPS-6.2 Record shape

Each field carries a value of `verified`, `not_verified`, `unknown`, or `not_applicable`, together
with the evidence, the exact command, the acting process, and the measurement time. The result is
JSON, matching what every relay command already emits. A filled example is in
[check-result.example.json](operations/check-result.example.json).

A measurement time is the time an observation was actually made. When no timed observation stands
behind a field, the time is `unknown`, and `unknown` is the honest answer rather than a defect to be
tidied away. Never supply a plausible time to satisfy the shape, and never copy another field's
time: both produce a record that reads as a host measurement nobody performed, which is worse than
an admitted gap because a later reader cannot tell the difference.

An example record is labelled as one. A worked example carries illustrative values, so it says so at
the top and a consumer can tell it apart from a record measured on a host. An unlabelled example is
indistinguishable from evidence the moment it is copied out of context.

### OPS-6.3 Installing a Linear hook

A hook's identity is `<source>:<event>:<matcher-index>:<hook-index>`, and Codex records a trusted
hash against that identity. Both halves matter: installation appends at the end, never inserts,
because inserting renumbers every later hook in the same file and detaches the trusted hash that
was recorded against the old identity. Removing a hook has the same effect, so a hook is disabled
rather than deleted. Changing a hook's content changes its hash, which means no installer silently
updates a trusted hook; re-trust is a separate, visible act.

An installation records the identity, the trusted hash, the hook file path with its SHA-256, and
the issue that installed it, then reads the registration back. Installed, enabled, and observed to
have fired are three separate claims and the check reports them separately. The surface is the user
hook file, since this repository ships skills only and does not package a plugin manifest; a
plugin-owned hook file is the alternative and is `proposed` rather than chosen. What the hook
decides is owned by the managed-marking contract and is deliberately not defined here.

### OPS-6.4 What makes a verification complete

A verdict existing at the head is not enough, because a verdict records a judgement and the
judgement may have been that nothing was verified. Five conditions hold together, and failing any
one leaves the field `not_verified` rather than partially true.

The disposition is `verified`, not `unverified` and not `aborted`. Every required criterion is
recorded `verified`, so one criterion left needing changes withholds the whole field. The event,
revision and generation the verdict was issued against are the current head, so a newer revision
arriving mid-review means the older result certifies something that is no longer current. The
criteria digest the review bound at claim time equals the digest in force now, because a criteria
set edited after the review means the verdict certified wording nobody is judging by. And the
parent's acknowledgement is one a host actually verified, not one recorded offline as authored
intent, since an unverified acknowledgement is the parent's stated intention rather than a
completed exchange.

Integration is a separate claim with separate evidence. Work can be verified and never integrated,
and something can be merged that this field never covered, so neither is read from the other.
## OPS-7 Assignment routing identity

### OPS-7.1 What an assignment binds

Because one service carries assignments belonging to several parents, several repositories and
several Linear projects, each assignment binds a full identity and routing uses nothing outside it.
The binding is the stable Linear workspace and issue identifiers, plus the project identifier
when one exists; the host; the native coordinator and child task identifiers; the execution
generation; the repository, worktree and branch for code work; the owner, artifact roots and
allowed recipients. A standalone issue records no project, but delegated delivery still names
its actual coordinator. Non-PR work retains its permitted artifact roots and result identity
without inventing Git metadata.

A displayed issue key is a label, not an identifier. It is unique inside one workspace and nowhere
else, so two workspaces can both hold the same key, and a registration or a lookup carrying only
that key can match the wrong assignment and refuse a legitimate second child as a duplicate. Every
registration therefore uses a globally stable issue identity and records its workspace and,
when present, project in the canonical scope reference. For a projectless issue, use a
workspace-qualified issue document or issue record as that reference. The bundled relay keeps
`--scope-ref` as descriptive context and routes on the exact `--issue` string and task IDs;
it does not validate Linear membership. Any command accepting a display key needs enough
identity to resolve it unambiguously before registration. See [relay registration](relay.md#register-the-assignment).

### OPS-7.2 Never route on a display name or a working directory

A display name, an issue title, a branch name and a working directory are all mutable, all
duplicable across projects, and none of them identifies a task. Two parents can legitimately work
in the same repository, and one parent can legitimately run several assignments from one checkout,
so a working directory neither identifies a parent nor separates two assignments. Routing that
falls back to any of them will eventually deliver one parent's work to another, and that failure is
silent because the message looks plausible on arrival.

Where a name is useful for a human, it is carried as a label beside the identifiers and never
consulted for a decision.

### OPS-7.3 Isolation between parents

Results, correction requests and permissions stay within their own assignment. A completion travels
only to that assignment's registered parent, a correction only to its registered child, and both
only when the recipient is in the allowed recipients recorded at registration. A task's authorized
execution settings belong to that task; no assignment inherits another's permissions, and no
delivery widens a recipient's permissions to make itself succeed.

An issue that already has an active or paused assignment cannot acquire a second registered child,
which is what stops two parents from claiming one issue. That guarantee covers the record rather
than the host, so a lookup by issue happens before anything is created.

## OPS-8 Parent return, fairness and isolation

### OPS-8.1 Parent continuation and waiting

Select the waiting mode from the parent's requested workflow and observed host capability.
An active parent CXC Loop follows its installed Loop lifecycle and waiting rules: keep the
authorized run active, use bounded transport waits, inspect meaningful results and continue
ready work. A child assignment or active child goal does not arm the parent's Loop. Do not
end the parent turn expecting a Stop hook or a relay to restart it; neither is guaranteed by
these instructions. If the host releases the turn, preserve recovery evidence and report the
interruption without calling the run complete.

Before creating or registering a child for an active parent Loop, check existing issue
ownership and whether the installed relay can deliver, acknowledge and settle a receipt while
that parent remains active. The current relay defers busy recipients, refuses acknowledgement
before delivery, and refuses verdicts without verified acknowledgement. Transport observation
does not satisfy those gates. For a new, unassigned issue without a supported active-parent
receipt path, use ordinary non-relay dispatch, transport waits and direct verification from
the outset, and record that mode. Do not register a relay assignment for that run. If the issue
is already registered, preserve its owner, artifacts and pending events; report the incompatible
delivery mode as a blocker and record the relationship/event IDs and required supported handoff
for recovery. Do not loop on waits expecting that blocker to clear, fake an acknowledgement,
reroute the registered work, reset CXC state or create a replacement writer. Continue unrelated
ready work only within its verified ownership and authorized scope.

A coordinator without an active parent Loop may return to idle for event-driven handoff only
when the registered assignment, live delivery service and supported parent-resume path have
been verified for its operating scope. A package installation, capability flag or staged receipt
alone does not establish that path. Otherwise use bounded observation through the transport's
own wait during the authorized run. A timeout leaves the work running: refresh observations and
wait again rather than ending the run, resending the prompt or creating another child. If no
usable wait or resume path exists, record the capability blocker and the exact manual resume step.

Two mechanisms are distinct and are never described as one. A native subagent finishes inside its
parent's own turn, and the parent observes that completion directly. When an independent task is
relay-managed, its completion travels as a relay receipt, which is staged when the emitting
process has no socket and becomes delivery only once a host-capable observer sees that turn end
normally. A staged receipt is real recorded progress and is never reported as delivery.

Use the independent task's actual task/host/turn IDs and cursors with native thread waits or
the bridge's task/turn wait; subagent handles and subagent waits are not substitutes. Respect
host wait bounds, prefer compact snapshots, and avoid repeated full transcript reads or rapid
polling. Give meaningful progress updates under the host's communication rules, not repetitive
unchanged status. Automatic relay handoff may wait while the parent is active; bounded observation
can read the child's result, but does not prove that a relay receipt was delivered or acknowledged.
Reconcile existing receipts through the relay when it holds the assignment; do not create a
second delivery route. Respect pause/cancellation and resource limits in either waiting mode.

### OPS-8.2 Busy, paused, cancelled and archived parents

A parent that is mid-turn is waited for and retried, not interrupted, since a transport that
refuses a message to an active task is protecting that task's turn. A paused, cancelled or archived
task is never automatically resumed to receive a delivery: the delivery waits and is reported as
waiting, and resuming that task is a human decision. Automatic resumption would restart work the
user deliberately stopped, which is the one outcome nobody can undo by retrying.

### OPS-8.3 Fairness, limits and error isolation, proposed

This clause specifies proposed combined delivery guarantees. Determine their implementation
status from the current issue and measured version; OPS-12.10 is a verification pointer, not
a cached project status. OPS-8.4 forbids reporting a design as installed behaviour. Do not rely
on these combined guarantees without matching evidence or infer them from narrower observations.

Proposed: delivery is fair per parent rather than first come first served across the whole store, so
one parent with many assignments cannot starve a parent with one. Each parent has a bound on how
many deliveries are in flight to it at once, and work beyond that bound queues rather than being
dropped.

Proposed: a failure is isolated to its own attempt and its own assignment. A recipient that is
unreachable, an assignment that is ambiguous at its head, and a summary write that fails are three
separate failures, each retried on its own schedule, and none of them re-runs a verification,
re-sends a correction, or blocks another parent's delivery. Retries are bounded and a repeatedly
failing attempt is surfaced as needing attention rather than retried forever.

Measured subset: summary-outbox retry is already implemented separately from delivery scheduling.
At `2026-09-15T17:54:05.870874Z`, the installed relay's `codex_session_relay.sync` module, SHA-256
`9c55226a558614e9cc1a3978d9e1e64edf5ac8a96679b9d77346da6a6486a255`, passed all six
`tests.test_sync_outbox.FailureIsolation` cases with its installed interpreter on the Linux
operating host with Python 3.14.4. Provenance is in private receipt
`JUN-101-summary-outbox-failure-isolation-20260915`, SHA-256
`412d005386b9c874e42d1771be1bbde9ca21c8c9fd6cfe11eaf7c763850417b8`, indexed in the
[JUN-101 measurement record](https://linear.app/jun786/issue/JUN-101). It records the exact command,
interpreter, stable host ID, hostname, timestamps, test/script digests and result. Consult that
receipt before reusing this measurement; inability to read it leaves compatibility for another
environment unmeasured. Each case used a temporary synthetic store and a simulated connector failure.
The checks retain the committed verdict, one retryable job and unchanged delivery count, reuse
that job on retry, bound repeated failures, and roll back a local enqueue failure atomically.
This supports the summary-only retry contract in [relay usage](relay.md#the-linear-summary-is-the-coordinators-own-write)
and the shared integrations reference. It does not measure a real Linear outage, multi-parent
fairness, per-parent delivery bounds, or isolation of every delivery failure. Private host and
command receipts stay outside the repository; other installed bytes require their own evidence.

### OPS-8.4 Stating the scale that was actually verified

Supported scale is a measured claim like any other. It is stated as the number of parents,
assignments and concurrent deliveries that were actually exercised, by what method, on what host,
with the observed fairness and latency. Anything beyond that is `unmeasured`. A design that intends
to scale further is proposed implementation and is labelled as such, never reported as installed
behaviour.

## OPS-9 Delivery, review and merge

### OPS-9.1 The child owns the pull request, and opens it for review

A capable child carries its work to a reviewable pull request and then through review. It
implements, runs the basic checks, commits on its branch, pushes, and puts the pull request into a
state a reviewer can act on: opened ready for review, or opened as a draft and marked ready before
any review is requested. Ready for review comes before the review rather than after it, because a
reviewer treats a draft as not yet asking, and a review that never ran then reads as a pass nobody
gave.

Ready for review is not a claim that the work is finished. It says the change is ready to be read.
Merge readiness is a separate judgement the parent makes later against the criteria and the current
head, and the relay outcome is a third thing again. So the pull request stays ready while reviews
are pending and while ordinary fixes land on it, and returning it to draft during normal review
traffic withdraws the request the reviewer was answering.

The child then owns every applicable review on that same pull request: collecting the findings,
triaging them, fixing what needs fixing, replying where a finding does not apply and saying why,
and rechecking.

| Step | Owner |
|---|---|
| Implement, basic checks, commit, push | child |
| Pull request open and ready for review | child |
| Review requested, hosted review runs | child |
| Findings triaged, fixed, replied and rechecked until the applicable gates are met | child |
| Report to the parent | child |
| Linear criteria plus the current diff, base, head, checks and review resolution verified | parent |
| Merge, without asking the user again | parent |
| Release or deployment | user |

Review handling stays on the pull request that produced it. A second pull request opened to escape
a review thread loses the history a reviewer needs and starts the review over.

### OPS-9.2 What normal completion means

A child reports normal completion only when the required checks and reviews on the CURRENT head
have finished and every blocking finding is resolved. Each finding carries its own evidence: the
finding, the commit that addressed it, and the recheck that confirms it. A summary saying review
was addressed, with no per-finding trail, is not that evidence.

An optional review that is unavailable or stalled is recorded as unavailable, with sufficient
independent review obtained instead under the repository's policy, and the work continues. Waiting
indefinitely for an optional reviewer is not diligence.

A missing mandatory review or a required check that has not passed is BLOCKED, and blocked is
reported as blocked. It is never reported as completion with a note, because the note is what gets
skimmed past.

Evidence is reused when it still applies, meaning the same revision, the same criteria and the same
environment, and it is re-run when any of those three moved. Reusing a result across a changed
revision is the mistake OPS-9.4 exists to prevent; re-running everything on every push is the waste
at the other end.

### OPS-9.3 The parent merges, and does not release

After the child reports, the parent checks the Linear criteria and the pull request's latest diff,
base, head, checks and review resolution. If those hold, the parent merges without asking the user
again, because that authority was already granted for this workflow, and then verifies the landing
rather than trusting an accepted merge request.

Release and deployment are separate and still require the user. A merge that is known to trigger a
release or a deployment needs that approval before the merge, since the branch name alone does not
carry it.

The child does not merge. It delivers and it answers review; the merge decision belongs to the
parent that holds the criteria.

### OPS-9.4 A new head invalidates the review it outran

A new head or a newly arrived finding invalidates the review evidence it supersedes, and only that
evidence. Re-run what the change actually touched rather than repeating the whole review, and never
carry a green result forward across a head it never saw.

## OPS-10 Which system owns which record

### OPS-10.1 Three owners

Three systems hold this work and each owns a different thing, so a fact is read from its owner
rather than from whichever surface is closest.

| System | Owns | Does not own |
|---|---|---|
| The pull request | Implementation, review and fix history, the diff, the checks, the review threads | Whether the work was wanted, and whether the parent accepted it |
| Linear | Goals, criteria, priority, the pull request link, the final summary | What the code does and how review went |
| The relay | Transport, acknowledgement and generation state, the verdict against registered criteria | Anything about code quality |

It follows that a passing internal workflow and a green pull request are not a relay verdict. They
are evidence a verdict can rely on. Verification under OPS-6.4 happens in the relay against the
registered criteria, and no amount of green elsewhere substitutes for it.

An independent task owns its own workflow state, its goal and its phases, and the relay owns
transport, acknowledgement and generation. Neither reads the other's state as its own. A workflow
reporting done is that task's conclusion about its work, not the parent's conclusion about whether
the obligations were met.

### OPS-10.2 The workflow is present, or its absence is reported

Managed execution here always runs with the CXC workflow alongside it, so a missing installation, an
absent contract, or an incompatible version is a condition to report, not something to route around
while calling the run normal. Quietly proceeding without it produces work that looks ordinary and
carries none of the evidence the workflow exists to produce.

Saying so does not require anyone to install, upgrade, or test anything in the middle of a running
assignment, and it changes no permission. Receipt reading, acknowledgement and recovery keep working
exactly as they already do; this clause only forbids the silent substitution.

### OPS-10.3 Where the packet and report formats are defined

The detailed correspondence between a CXC instruction packet and the report and review formats is
owned elsewhere and is deliberately not restated here. The installed CXC skill files are the source:
`delegation.md` for the task packet, `plan-output.md` for plan output, `loop-engineering.md` sections
11.2 and 11.3, `phase-check.md` for check evidence, `phase-control.md` for attestation evidence,
`dispatch-surfaces.md` for choosing a surface, and `waiting.md` for waiting on work.

The adapter that actually carries those formats across the relay is its own implementation issue and
is not built here. This contract states which system owns which record and where the formats live;
it does not define message shapes, and a reader who needs one goes to the owner rather than to a
paraphrase that will drift.

## OPS-11 The monorepo and its packages

### OPS-11.1 One destination

Skills, bridge and relay all deliver to one repository,
`github.com/thisisjun786/codex-relay-workflow`, on base `dev`. This is decided, so an
implementation issue for any of the three has a place to send a pull request and no longer waits on
a location question. The public repository starts from a reviewed source snapshot; the earlier
private repository retains its history. Do not push private-history branches into the public remote.

`dev` is the integration branch and ordinary pull requests target it. An explicitly recorded
dependent PR may target its prerequisite branch before integration. `main` is reached only by
promoting `dev` inside the same repository, and that promotion is a release, so it carries the
owner authorization that OPS-9.3 keeps separate from merge authority. A pull request retargeted from
`main` to `dev` keeps its head and its review history; only the base moves, so earlier receipts
naming the old base stay accurate for the moment they were written and are not rewritten.

The layout is `skills/crw-*` for the skills, `packages/codex-thread-bridge/` for the bridge and
`packages/codex-session-relay/` for the relay.

### OPS-11.2 What each package keeps

Consolidating where code is reviewed does not merge the components into one thing. Each package
keeps its own module and command names, its own `pyproject.toml` and its own tests, so it stays
separately buildable and separately installable. Each also keeps its upstream provenance and its
licence notice, including the MIT notice of a component that came from another author's repository,
because a licence travels with the code rather than with the repository it lands in.

The skills and the packages own different things. `skills/` holds workflow instructions that an agent
reads; `packages/` holds runtime code that a host executes. A rule that belongs to one does not move
into the other just because they now share a commit. CXC and Paperthin stay outside this repository
entirely and are not vendored by this decision.

### OPS-11.3 Four stages that are not one event

Collapsing these is how a decision about pull request destinations becomes an unplanned outage, so
they are tracked separately and each has its own evidence.

| Stage | What it means | Required evidence |
|---|---|---|
| Source destination | Where pull requests and review go | Current remote and repository policy, OPS-11.1 |
| Source migration | Code actually moved into `packages/` | Issue criteria, PR diff and landed commit |
| Runtime installation and activation | What a host installs and executes, per OPS-2 | Fresh installed-path and execution evidence |
| Store movement | Moving the durable store, per OPS-4.5 | Explicit migration scope, backup and readback |

A migration that lands source changes nothing about what is installed. The installed entry points
keep resolving where they resolve today until somebody deliberately reinstalls, and that reinstall is
an OPS-2.4 update with its own measurement. The store is untouched by both.

### OPS-11.4 What the migration must preserve

The migration copies source into the repository. It preserves the original checkouts, any dirty work
and local forks in them, and their `.git` history as evidence, because those are the only record of
what the code was before it moved and OPS-2.2 still needs them to classify a fork.

It does not import private or machine-local state: no virtual environments, no session data, no
databases, no receipts. Those are not source, they are the operational state that OPS-3.2 keeps
outside every repository, and importing them would publish a person's working history.

## OPS-12 Project decision authority

Project decisions, migration progress, unresolved questions, and real host observations
belong in the selected project's canonical Linear documents and issues. The maintainer's
[operations decision record](https://linear.app/jun786/document/615c40c73d55) holds the
historical register removed from this file; access to that private project is not a
prerequisite for using these rules with another project.

Read the current issue and authorized task scope before acting. A proposal does not
establish an installed capability, but an issue explicitly authorizing its implementation
may proceed. Missing measurements block only the capability claim or operation that
requires them. A checked-in example, old receipt, or historical status never grants
permission, proves current state, or creates a new approval requirement.

The following identifiers remain as durable cross-reference targets for older receipts.
They carry no project status; their owning clauses define the rule.

| Id | Durable rule and owner |
|---|---|
| OPS-12.1 | Route source review to the configured repository under OPS-11.1 and repository policy |
| OPS-12.2 | Keep the common destination and private-history boundary under OPS-11.1; verify the current remote before pushing |
| OPS-12.3 | Treat source migration as a scoped issue under OPS-11.3 and preserve OPS-11.4; read current progress in Linear |
| OPS-12.4 | Record a durable private evidence root outside Git under OPS-5.4 |
| OPS-12.5 | Share one relay/store per operating scope under OPS-3.1 and OPS-4.1 |
| OPS-12.6 | Treat temporary storage as an exception requiring explicit migration under OPS-3.2 and OPS-4.5 |
| OPS-12.7 | Verify both state selectors against OPS-3.3 and the relay command reference |
| OPS-12.8 | Measure additional writable-root support before relying on OPS-3.5 |
| OPS-12.9 | Measure schema-open behavior before relying on OPS-4.5 |
| OPS-12.10 | Verify implementation and scale evidence before claiming fairness under OPS-8.3 and OPS-8.4 |
| OPS-12.11 | Determine the installed lock's scope before claiming duplicate-start protection under OPS-4.2 and OPS-4.6 |
| OPS-12.12 | Reconcile existing stores before scope arbitration or migration under OPS-4.6 |
| OPS-12.13 | Carry child capability and delivery ownership under OPS-5.5 and OPS-9 |
| OPS-12.14 | Apply current authorized merge scope and separate release authority under OPS-9.3 |
| OPS-12.15 | Use the task packet and OPS-5.3 for delivery artifacts and restricted-task fallback |
| OPS-12.16 | Keep review readiness, merge readiness, and relay outcome separate under OPS-9.1 |
| OPS-12.17 | Follow repository policy for dev integration and separately authorized main promotion |

## Worked examples

[Installation plan example](operations/installation-plan.example.md) walks the canonical install
flow against these clauses. [Scenarios](operations/scenarios.md) applies the contract to a new
install, a rerun, a foreign install, a local fork, a failed update, an unreachable store, a
checkout assignment, two parents sharing one service, and a parent returning to idle.
