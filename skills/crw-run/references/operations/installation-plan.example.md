# Installation plan example

A worked example of the canonical five-step installation flow resolved against
[the operations contract](../operations.md). It is an example of the reasoning, not a script to
run, and it installs nothing by itself. It is illustrative and is not evidence about any host,
including its author's: it reads the same fictional compatibility record as
[compatibility-record.example.json](compatibility-record.example.json), so the run it walks through
is the one that record supports, including where that record withholds. Every value it reasons from
is that example's stated assumption rather than an observation, and the outcome is what those
assumptions force rather than a report about a machine.

## Situation

A host already has the seven current skills (including `crw-define`) linked, an MCP server registered for the bridge, and a relay
console script on `PATH`. The operator wants to confirm the combination is the verified one before
starting a daemon for a new assignment.

## Step 1, find what is already installed and who owns it

Read all four OPS-2.1 signals, not the two that are easiest to reach. Resolve the relay console
script and the MCP server's registered command; here the console script's shebang points into a
virtual environment inside the relay's development checkout and the MCP command points into the
bridge's own checkout, so both resolve into recorded paths. Read each checkout's commit and tree and
confirm `git status --porcelain` is empty. Compute the OPS-1.2 digest for each install location,
including the bridge copy inside the relay's virtual environment, because the bridge exists twice
and the two installs drift independently. Confirm the configuration registers the command the
record names.

All four must agree before anything classifies `own`, and `own` is reached only because nothing
earlier in the precedence matched. Skipping cleanliness is the specific mistake to avoid: an
uncommitted change outside the package directory, a README or a test, leaves the package digest
exactly equal to the digest the record carries for that install, so entry point, digest and
configuration all agree while the
checkout is no longer the revision the record names. That case must classify `fork`, and only the
cleanliness signal catches it.

Here the identity signals settle for every install: each entry point resolves into a recorded path,
each checkout is on its recorded commit and tree, both trees are clean, and every digest matches the
record. The evidence signal settles for none of them. `own` also requires a measured point for the
combination each install runs under, and the record carries no point at all: the commands that
produced it read identity and bytes without ever executing either component, which OPS-1.3 refuses
to count. So precedence sends all three installs to `unmeasured`: nothing conflicts, nothing differs,
and the one missing element everywhere is that nobody has recorded running them. They are preserved,
none is reused, and the step reports the missing points instead of assuming them.

This is the honest reading of a host that knows exactly what it has and has not established that any
of it works together. A record can look full and still carry no compatibility evidence, which is
precisely the confusion this class exists to surface.

Had the relay resolved to a `pipx` install or another user's path, it would classify `foreign` and
be left alone, usable only if both its digest and its running combination checked out. Had the
checkout been dirty or sitting on a different commit, it would classify `fork`, be preserved
untouched, and stay unusable here until the entry point moved to a recorded clean install or the
user committed the change and that revision was recorded with its own point. A point recorded
against a still-dirty tree would not move it out of `fork`.

## Step 2, read the combination from the one definition

Versions, revisions and digests come from the single compatibility record and from nowhere else. No
value is copied into a second document or a script, because a copy is what goes stale first. A
combination that is not in the record is `unmeasured`, which withholds a claim rather than asserting
that it will not work.

Consulting it here is what surfaces the gap rather than papering over it. The record carries no
measured point for either component. What it carries instead is an inventory observation per
component, kept as such and explicitly marked not a point, because the commands behind it read git
metadata, project files, digests and a Codex version string without running the bridge or the relay.
Its `status` is `unmeasured` while its `identityStatus` is `measured`, which is the whole distinction in
two fields. That is the state the example states, and the remaining steps follow it instead of the
outcome the operator was hoping for. On a real host the same reading would come from that host's own
private record, and it would be as binding there as it is here.

## Step 3, register the MCP server and any hook through supported configuration

The MCP server is already registered, so nothing is written. If a hook were being installed, it
would be appended at the end of its hook file rather than inserted, because inserting renumbers the
identities of every later hook in that file and detaches the trusted hash recorded against the old
identity. The registration is then read back, and the identity, the trusted hash and the hook
file's own SHA-256 are recorded with the issue that installed it.

Existing CXC, other MCP servers and other hooks are preserved. An entry already held by a different
command or a different trusted hash is a `conflict`, which stops the installation with both values
reported rather than overwriting either.

## Step 4, check the six states separately

Run the check and fill all six fields of the OPS-6.1 record. On this host and from an operator
process with a socket, `mcpExposed` and `connected` come out `verified`: the server is registered and
its tools are listed in a live session, and `doctor` reports the socket connecting.

`installed` is `not_verified`, and this is the field the previous steps decided. No install reaches
`own`: every one of them matches the record on identity and bytes, and none of them has a measured
point for the combination it runs under, so OPS-2.2 classifies all three `unmeasured`. A field
covering several components is only as verified as its weakest component, and here none of them is
strong, so reporting `verified` would certify exactly what the compatibility record withholds.

Note what does not move. `mcpExposed` and `connected` stay `verified`, because they were observed
directly and OPS-6.1 keeps the six fields independent. Missing compatibility evidence says nothing
about whether a server is registered or a socket accepts a connection, and collapsing them would
lose the separation the clause exists to hold.

`deliveryAccepted` and `verificationComplete` stay `not_verified` because no assignment has been
delivered or judged yet. `alwaysActive` is also `not_verified`: no supervised unit exists, and an
absent supervisor is an unmet requirement rather than an inapplicable one. `verificationComplete`
is judged against all five OPS-6.4 conditions, so a verdict merely existing would not have raised it
either.

Resist the shortcut of reporting one state and letting it stand for the rest. A registered server
whose tools never appear in a session, and a socket file that exists while nothing accepts a
connection, are exactly the failures this separation catches.

## Step 5, the start is refused, and that is the contract working

Before a service starts, the digests are re-measured and attached to the run's receipt, so that a
reinstall somebody forgot to record turns the combination `unverified` and blocks the start instead
of quietly running unknown code. Nothing was forgotten here, so that is not why this run stops.
OPS-4.1 refuses the start on its other condition: a component the service needs classifies
`unmeasured`, and the refusal is the correct outcome rather than an obstacle to route around.

What would unblock it is specific and is not done here: an exercised point for every combination the
service needs, meaning somebody actually runs each component under its interpreter and records the
result. OPS-4.1 refuses on any service-required component that is not `own`, so clearing one
install would not open the start while a sibling stays unmeasured. Adding a point without that
execution, or lowering the bar so a digest counts as one, would defeat the check this step exists to
perform. So the example stops, which is the honest end of this run.

Had the combination been verified, the rest of the step would apply: both
`CODEX_SESSION_RELAY_STATE` and `--state` set to the same resolved absolute path so the store, the
lock and the adapter's ledger stay together, and the daemon taking the lock for that state
directory, where a second start is refused under OPS-4.2 and that refusal is also the answer. The
service is shared: one relay and one store serve the whole operating scope, so such a start is the
scope's start and not one parent's, and a later parent finding the lock held is finding the service
it should be using.

## What this example does not establish

It does not show a runtime update, which moves a whole verified combination and is covered by
[scenarios](scenarios.md). It does not prove any hook fires. It does not make the store durable
against a host that clears temporary directories. If the current store is temporary, record the
exception and migration in Linear under OPS-12.6; a durable evidence root is distinct under OPS-12.4.

It also does not show a successful start, because this host cannot honestly reach one. The fully
measured positive case, where every signal agrees and reuse follows, is S2 in
[scenarios](scenarios.md), and it is a hypothetical: it assumes measured points that the record used
here does not supply, so it illustrates the rule rather than this host. The contrasting unverified
case that blocks a start is S4, and S22 states on its own the unmeasured case this run actually hits,
at every install rather than only the copied one. Reading this example as proof that the combination
works is the one misreading it is written to prevent.


## Clauses exercised

This example applies OPS-1.2 when it recomputes the digest for every install location including the
second copy of the bridge, OPS-1.3 when no digest and no inventory reading is allowed to stand in for
a measured point, OPS-2.2 when every install classifies `unmeasured` rather than `own`, OPS-4.1 when
the start is refused on that class, OPS-4.2 for the lock refusal that would apply to a verified one,
OPS-6.1 when the independent fields are not dragged down with the compatibility ones, OPS-6.2 when
all six fields are written with their evidence, command, actor and measurement time, and OPS-6.3
when a hook would be appended and read back rather than inserted.
