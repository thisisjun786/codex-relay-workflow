package skill

const hookProbeRootHelp = `usage: hook_probe.py [-h] {observe,decide,replay} ...

Read-only probe for the Codex hook contract in ../references/hook-contract.md.

observe  reads the installed Codex binary and reports the hook input/output schemas it
         embeds, plus which events this host registers. It opens nothing for writing and
         starts no session.
decide   applies the contract's Stop decision to one sanitized observation and prints the
         resulting hook output.
replay   runs every fixture through decide and checks the recorded expectation.

Replay proves this parser and this decision table. It is not evidence that the host invoked
a hook or honored its output; that evidence comes from a real run and is recorded separately.
Replay also cross-checks the recorded host observations under fixtures/host against the
capability record each one names. That compares two recordings of the same host; it re-runs
nothing and starts no session.

positional arguments:
  {observe,decide,replay}
    observe             Report the host's hook schemas and registrations
    decide              Apply the Stop decision to one observation
    replay              Check every fixture against its recorded expectation

options:
  -h, --help            show this help message and exit
`

const hookProbeObserveHelp = `usage: hook_probe.py observe [-h] [--binary BINARY] [--codex-home CODEX_HOME]
                             [--sanitize]

options:
  -h, --help            show this help message and exit
  --binary BINARY       Path to the Codex binary; defaults to the one on PATH
  --codex-home CODEX_HOME
  --sanitize            Omit host paths and registrations so the output is
                        shareable
`

const hookProbeDecideHelp = `usage: hook_probe.py decide [-h] observation

positional arguments:
  observation

options:
  -h, --help   show this help message and exit
`

const hookProbeReplayHelp = `usage: hook_probe.py replay [-h] [--fixtures FIXTURES] [--contract CONTRACT]
                            [--host-fixtures HOST_FIXTURES]
                            [--allow-unreached]

options:
  -h, --help            show this help message and exit
  --fixtures FIXTURES
  --contract CONTRACT   Contract whose documented traces must each have a
                        fixture
  --host-fixtures HOST_FIXTURES
                        Recorded host observations to hold to their capability
                        record
  --allow-unreached     Report unreached return sites and incomplete
                        documented-trace coverage without failing; for
                        deliberate subset runs only. Fixture mismatches are
                        never waived.
`

const parentTitleRootHelp = `usage: parent_title.py [-h] {decide,readback,replay} ...

Compute a project parent's task title from the product family its project actually carries.

../../crw-plan/references/integrations.md#set-the-app-presentation-and-record owns the rule: a
project parent's Codex task title leads with the linked project's product-family label in
brackets, spelled exactly as Linear spells it. The label is data, so nothing here maps, expands or
case-folds it, and there is no table of known families to drift.

decide    read one request as JSON on stdin and print the decision. Exit 2 when the request
          itself is unreadable, because a malformed call must not look like a settled title.
readback  classify a rename readback: verified, mismatch or unread.
replay    run every fixture against its recorded expectation, and fail when a decision this
          module can reach has no fixture reaching it.

The caller supplies the family candidates already scoped to the product-family label group. That
is deliberate: the group is not exposed by the project read, so a helper counting labels would
prefix confidently from another group whenever a project with no family carried exactly one label.
Membership is evidence the caller has to bring; a count is not membership.

One rule governs every boundary below, because a label is unrestricted text and three separate
defects came from forgetting it: a label may contain "[" or "]", so where one label ends cannot be
read out of a title. A boundary is therefore only ever acted on when it was CONSTRUCTED from the
verified family, or NAMED verbatim by the caller. The bracket reader exists to notice that a
leading bracket is there and to say what it looked like in a refusal; nothing decides a strip or a
prefix from what it parsed. Keep it that way when extending this: a new branch that trusts a
parsed boundary will be wrong for some label somebody is entitled to use.

What this does not do. It writes no title and reads no host, so a decision here is a proposal and
never evidence that a task is named anything. It settles no binding either: every answer below
assumes the caller has already matched this task to this project by their stable IDs, and the one
thing it does about that is refuse to proceed when the caller says that check has not been made.
replay proves this module agrees with its recorded expectations, and proves nothing about a title
having been written, displayed, or read back from a real host.

positional arguments:
  {decide,readback,replay}
    decide              Decide one parent title from stdin JSON
    readback            Classify a rename readback
    replay              Check every fixture against its recorded expectation

options:
  -h, --help            show this help message and exit
`

const parentTitleDecideHelp = `usage: parent_title.py decide [-h]

options:
  -h, --help  show this help message and exit
`

const parentTitleReadbackHelp = `usage: parent_title.py readback [-h]

options:
  -h, --help  show this help message and exit
`

const parentTitleReplayHelp = `usage: parent_title.py replay [-h] [--fixtures FIXTURES] [--allow-unreached]

options:
  -h, --help           show this help message and exit
  --fixtures FIXTURES
  --allow-unreached    Report unreached decisions without failing; for
                       deliberate subset runs only. Fixture mismatches are
                       never waived.
`

const startPolicyRootHelp = `usage: start_policy.py [-h] {vocabulary,check,selftest} ...

Check a start-policy record's two closed-vocabulary fields against the
contract.

positional arguments:
  {vocabulary,check,selftest}
    vocabulary          print the declared values and the legal pairings
    check               check a record's two closed-vocabulary fields
    selftest            check the vocabulary and the recorded negative cases

options:
  -h, --help            show this help message and exit
`

const startPolicyVocabularyHelp = `usage: start_policy.py vocabulary [-h]

options:
  -h, --help  show this help message and exit
`

const startPolicyCheckHelp = `usage: start_policy.py check [-h] [record]

positional arguments:
  record      file to read; omit to read stdin

options:
  -h, --help  show this help message and exit
`

const startPolicySelftestHelp = `usage: start_policy.py selftest [-h]

options:
  -h, --help  show this help message and exit
`
