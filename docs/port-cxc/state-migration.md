# Copy CXC state without rewriting its evidence

This design adds an explicit, repeatable copy operation from CXC v0.2.40 locations to CRW locations. It preserves source files and record bytes, refuses destination conflicts, skips derived and excluded stores, and reports old path references that need fresh CRW evidence. Copying state does not activate CRW or prove that an armed session can continue unchanged.

The reader is the migration implementer: use the inventory and rules below to build the command, then use the implementation slices to define independently verifiable changes. Jun retains the transition-window decision J2.

Contents: [inventory](#only-the-listed-stores-are-migration-inputs), [paths and hashes](#keep-embedded-paths-and-recorded-hashes), [copy safety](#preflight-the-whole-selected-scope-before-writing), [command](#make-migration-an-explicit-install-subcommand), [implementation](#implement-foundations-before-the-command), [operator decisions](#jun-chooses-the-transition-window).

## Only the listed stores are migration inputs

Evidence baseline: upstream commit `3c1459ac`, CRW release base `54f35c86`. `O/` below means upstream `plugins/codexclaw/components/`; `P/` means `O/pabcd-state/src/`. Go anchors are repository-relative. A writer anchor identifies the publishing operation, not merely a string mentioning a path. User-authored inputs have no runtime writer; their instruction/consumer anchor is given instead.

Roots: `W` is the selected workspace; `U` is `CODEXCLAW_HOME`, otherwise `~/.codexclaw`; `V` is `CRW_HOME`, otherwise `~/.crw`; `C` is `CODEX_HOME`, otherwise `~/.codex`. Project substitution is `W/.codexclaw` to `W/.crw`. `CODEX_HOME` itself stays unchanged. Explicit roots override these defaults only for this command.

The naming contract is [name-substitution.json](../../contract/schema/cxc/name-substitution.json): R13/R16 choose user roots, R26 the project root, R18 the marker, R19 install-owner absorption, R20 backup basenames, and R28 the project policy filename. These are fixture replay/name decisions, not permission to run a regex through persisted records.

`copy` means unchanged bytes at the mapped location. `transform` means only the explicitly named basename/root changes; the payload remains unchanged. `skip` means no destination write and a report entry. Every directory mentioned below includes only its listed children; unknown children are reported and skipped. No recursive catch-all copy is permitted.

### Project records retained under `W/.crw`

| Source relative to `W/.codexclaw` | CXC writer/instruction | Go reader/writer on baseline | Disposition and reason |
| --- | --- | --- | --- |
| Root directory | `P/codexclaw-dir.ts:11-25` (same helper in bg-wake, cxc-ops, subagent-config, messenger-bridge) | `internal/pabcd/crwdir/crwdir.go:23-43` | transform container to `.crw`; create only after preflight |
| `.gitignore` | `P/codexclaw-dir.ts:4,15-25` | `internal/pabcd/crwdir/crwdir.go:18,33` | skip CXC text; publish CRW's canonical text no-replace whenever destination `.gitignore` is absent, including rerun after root creation; keep an existing regular destination |
| `sessions/<id>.json` | `P/state.ts:375-417,620-640` | `internal/pabcd/state/state.go:198`, `write.go:34,155`, `restore.go:80-95` | copy; preserve phase, guards, IDs and unknown fields |
| `ledger.jsonl` | `P/state.ts:695-701` | `internal/pabcd/state/ledger.go:88` | copy; append-only transition history |
| `interviews/<id>.jsonl` | `P/state.ts:752-759`, `P/interview-ledger.ts:222-223` | `internal/pabcd/state/ledger.go:150,158`, `internal/pabcd/interview/ledger/ledger.go:179` | copy; scan and Q/A share this ledger |
| `interview/freeze.json` | `P/freeze-cli.ts:120-121` | `internal/pabcd/interview/freezecli.go:300-345`, `freeze.go:41-49` | copy; preserve frozen hashes and evidence |
| `plan/<slug>/**` | user/agent-authored; `P/freeze-cli.ts:92-103` consumes it | `internal/pabcd/interview/freezecli.go:119-160,307` | copy regular files/directories; retain exact plan bytes and names |
| `goalplans/<slug>/goalplan.json` | `P/goalplan.ts:925-940` | `internal/pabcd/goalplan/read.go:38`, `write.go:141`, `types.go:285-300` | copy; never normalize through a Go read/write round trip |
| `goalplans/<slug>/ledger.jsonl` | `P/goalplan.ts:944-965` | `internal/pabcd/goalplan/write.go:322` | copy; keep recorded event evidence |
| `goalplans/<slug>/schema-v2.marker` | no writer in v0.2.40; reader/path `P/goalplan.ts:1759-1784` | `internal/pabcd/goalplan/integrity.go:327` | copy if present; it prevents schema downgrade, including externally authored markers |
| `evidence/<session>/test-receipt.json` | `P/receipt-cli.ts:183-184` | `internal/pabcd/cli/receipt.go:114,215`, `internal/pabcd/gate/receipt.go:109-164` | copy; historical receipt, not fresh check proof |
| Other `evidence/**` receipts, QA manifests, verdicts, identities, screenshots | agent-authored; demand/root guard `P/subagent-evidence.ts:151-174`, `P/source-receipt.ts:99-173` | `internal/pabcd/evidence/evidence.go:115-127`, `internal/pabcd/gate/manifest.go:89-200`, `internal/pabcd/hook/renderobs.go:269-324` | copy regular files; keep the complete referenced artifact group and hashes |
| `evidence-attempts/<session>-<agent>[-<turn>].json` | `P/subagent-evidence.ts:220-225` | `internal/pabcd/evidence/attempts.go:54,90-104` | copy; retry caps must not reset |
| `evidence-unrecordable/<session>-<agent>-<ms>.json` | `P/subagent-evidence.ts:342-344` | `internal/pabcd/evidence/unrecordable.go:117-152` | copy; retain durable failure/tombstone state |
| `sources/<session>.json` | `P/session-source.ts:185-192` | `internal/pabcd/source/session/session.go:138-150,218,321-333` | copy; immutable native/source binding |
| `metrics.jsonl` | `P/metrics.ts:134` | `internal/pabcd/metric/metrics.go:115,197,208` | copy; keep plateau evidence |
| `objective-kind/<session>.json` | `P/metrics.ts:140-147` | `internal/pabcd/metric/metrics.go:117,305,353` | copy; preserve loop objective semantics |
| `divergence/<session>.mode.json` | `P/divergence.ts:125-134` | `internal/pabcd/metric/divergence.go:138,238,296` | copy; preserve the chosen search mode |
| `divergence/candidates.jsonl` | `P/divergence.ts:197-198` | `internal/pabcd/metric/divergence.go:316,391` | copy; historical candidate evidence |
| `render-observations.jsonl` | `P/render-observations.ts:74,129` | `internal/pabcd/hook/renderobs.go:50,116-131,173` | copy; old observations remain historical, not a new observation |
| `dispatches/<session>/<dispatch>.json` | `O/subagent-config/src/fallback-dispatch.ts:109-115,236-267` | `internal/role/dispatch_ledger.go:275-312`, `internal/role/managed_spawn.go:25-43` | copy terminal/reconciled records; refuse the scope for unresolved in-flight attempts |
| `bg/<id>.json` | `O/bg-wake/src/registry.ts:55`, `store.ts:22-57` | `internal/relay/job/registry.go:50-120`, `store.go:55-65` | copy terminal job records; refuse running/unknown process state |
| `bg/<id>.out`, `<id>.exit` | `O/bg-wake/src/spawn.ts:50-59,104-114` | `internal/relay/job/spawn.go:160-206`, `registry.go:214-245` | copy closed output and exit records; no worker is launched or adopted |
| `bg/<id>.out.helper.cjs` | `O/bg-wake/src/spawn.ts:78-83` (Windows branch) | nominal `.crw/bg/`; `internal/relay/job/spawn.go:123-138` retains build helper but Windows activation is excluded | skip; generated Node execution helper, not portable state |
| `bg/disabled`, `bg/enabled-at`, `bg/ledger.jsonl` | `O/bg-wake/src/store.ts:80-118` | `internal/relay/job/store.go:64-65,238`, `registry.go:289,325` | copy; preserve explicit wake configuration and history |
| `attest.json` | user writes the suggested file; `P/hook.ts:495-496,1547-1548` | `internal/pabcd/cli/orchestrate_args.go:164-222` (`--attest-file` parser), `internal/pabcd/attest/attest.go:41-64` | copy; convenience input, not a completed transition |

### Project stores intentionally skipped

| Source relative to `W/.codexclaw` | CXC writer/instruction | CRW name/owner or exclusion source | Disposition and reason |
| --- | --- | --- | --- |
| `subagents.json` | `O/subagent-config/src/store.ts:252-262` | nominal `.crw/subagents.json`; `internal/role/role.go:7-12`; name table env entry `CODEXCLAW_TRUST_PROJECT_SUBAGENTS` | skip; global-layer-only decision removes the project role layer |
| `worktree-guard/<session>.json` | `P/worktree-guard.ts:513-535` | `.crw/worktree-guard`; `internal/pabcd/hook/worktree.go:24-25,286-315` | skip; rebuildable injection marker, so CRW can inject its own command advice |
| `affordance-recovery/<session-sha>.pending` | `O/cxc-ops/src/map-affordance.ts:235-276` | `.crw/affordance-recovery`; `internal/affordance/hook.go:80-109` | skip; once-only queued advisory is regenerated, not resumed as evidence |
| `friction.jsonl` | `P/friction.ts:121-132` | nominal `.crw/friction.jsonl`; Wave 0 inventory §3, dormant `P/cli.ts:450-486`, decision 9 | skip; dormant hook layer was not ported, no Go store reader |
| `edit-shapes.jsonl` | `P/edit-shape.ts:165-170` | nominal `.crw/edit-shapes.jsonl`; `internal/pabcd/hook/editshape.go:12-13` | skip; the dormant ledger is expressly excluded from the port |
| `rules/*.md` | user-authored; `P/rules.ts:9-15` | nominal `.crw/rules`; dormant `session-start-rules`, `P/cli.ts:450-486`; decision 9 | skip; rule injection was not ported; preserve user files in source |
| Caller-selected rule-impact ledger path | `P/rule-impact-ledger.ts:101-111` | no fixed basename/Go owner; Wave 0 inventory §1/§8, decision 9 | skip; dead module has no runtime importer |
| `release/**` including `<candidate>.tmp` | `P/release-cli.ts:56,134-140` | name table CLI entry `release`: out of scope | skip; upstream release tooling has no CRW state consumer |
| `traces/activations.jsonl` | `O/cxc-ops/src/activation-trace.ts:133-149` | nominal `.crw/traces`; name table env entry `CODEXCLAW_TRACE_ACTIVATIONS`: dead module | skip; unported trace layer |
| `cache/repomap/tags.v1/**` | upstream `skills/repo-map/scripts/repomap_class.py:31-35,90-112` | nominal `.crw/cache/repomap`; `cmd/crw/repomap.go:75-137` launches the skill | skip; derived diskcache data; custom cache locations are not recursively discovered |
| `bridge.db`, `bridge.db-wal`, `bridge.db-shm` | `O/messenger-bridge/src/db.ts:1037-1045` | no Go counterpart; name table `serve`/`service`: messenger bridge excluded | skip; excluded SQLite store, never attempt a live DB copy |
| `bridge-events.jsonl` and its rotated files | `O/messenger-bridge/src/event-log.ts:18-65,95-140`, `bridge-controller.ts:89-94` | no counterpart; same messenger exclusion | skip; excluded event-log layer |

### User stores under `U` mapped to `V`

| Source | CXC writer/instruction | Go reader/writer | Disposition and reason |
| --- | --- | --- | --- |
| Root and `subagents.json` | `O/subagent-config/src/store.ts:129-144,252-262` | `internal/role/store.go:16-26,91-113` | copy; the global role setting payload/schema is unchanged |
| `config.json` | user-authored; `P/agent-thread-permissions.ts:85-115` | `internal/pabcd/hook/agentthread.go:156` | copy; retain explicit permission opt-in; trust checks still apply at its new location |
| `model-catalog.json` | `O/subagent-config/src/live-catalog.ts:55-88` | `internal/role/livecatalog.go:235` | skip; environment-keyed catalog cache is rebuilt |
| `recall/index.sqlite`, `-wal`, `-shm`, `recall/` container | `O/recall/src/index-db.ts:20-27,90-120,201` | `internal/recall/indexdb.go:70-109`, `ingest.go:169-205` | skip; derived search index, including hit statistics; cold rebuild is deliberate |
| `skill-cache/<key>.cache`, `skill-cache/` | `O/skill-search/src/cache.ts:14-17,49-60` | `internal/skill/search/cache.go:31-37,62-92` | skip; TTL cache can be fetched again |
| `serve.out.log`, `serve.err.log`, `serve.cmd` under literal `~/.codexclaw` (ignores U override) | `O/messenger-bridge/src/service.ts:42-49,122-133,197-200,249-257` and service manager log appends | no counterpart; name table `service`/`serve` excluded | skip; excluded messenger service state; enumerate literal fallback home too when U differs |
| `venvs/repomap/**` | upstream `bin/codexclaw.mjs:370-402` (opt-in bootstrap) | `cmd/crw/repomap.go:44-46,98-119` | skip; rebuildable interpreter/dependency environment, not durable state |
| `runtime/ast-grep/<os>-<arch>/{sg,sg.exe}` | lookup only: upstream `skills/ast-grep/scripts/ast_grep_helper.py:232-245,598-609`; referenced installers absent from this snapshot | renamed skill; no Go installer for this cache | skip; executable installation/cache outside state copy; no writer is falsely attributed to bin |

### Codex-owned files under `C`

| Source | CXC writer | CRW name/Go reader or writer | Disposition and reason |
| --- | --- | --- | --- |
| `.codexclaw-install.json` | `O/config-guard/src/activate.ts:195-201,292-300`, `config-set.ts:44`, `self-heal.ts:110` | R19 owner absorption: actual `configguard.InstallManifestName` is `.crw-install.json`, `internal/runtime/install/configguard/manifest.go:14,31-40,57-75,191-196` | transform basename through the existing owner constant; copy payload, ownership bits and restore paths exactly |
| `codexclaw-self-heal.json` | `O/config-guard/src/self-heal.ts:272-277,308-313` | R18 `crw-self-heal.json`; `internal/runtime/install/configguard/selfheal_marker.go:14-26,128-142` | transform basename only; preserve opt-out/consent and cache fields, never trigger healing |
| `config.toml.codexclaw-<timestamp>.bak` | `O/config-guard/src/activate.ts:209-218`, `config-set.ts:99-103,126-135` | R20 `config.toml.crw-<timestamp>.bak`; `internal/runtime/install/configguard/activate.go:149-159`, `configset.go:88-103` | transform basename only; retain original backup for unchanged `backupPath`; a new copy is archival, not a rebinding |
| `config.toml` | `O/config-guard/src/activate.ts:278`, `deactivate.ts:155`, `config-set.ts:103,135`, `multi-agent-v2.ts:109`, `O/cxc-ops/src/hook-trust.ts:362-366` | same host file; `internal/runtime/install/configguard/activate.go:145-165`, `deactivate.go:72-83`, `configset.go:88-103`; retrust has no Go writer at baseline | skip; migration neither rewrites configuration nor grants hook trust |
| `config.toml.bak-<timestamp>` | `O/cxc-ops/src/hook-trust.ts:463-464` | same host backup name; no migration reader (doctor trust port covers identity only at baseline) | skip; retrust backup already remains in the unchanged Codex home; no trust mutation |
| `agents/{architect,executor}.toml`, `agents/<role>.toml.backup-<hash>` | `O/subagent-config/src/role-registration.ts:42-58,65-95` | same files, `internal/role/registration.go:42-59,200-215,271-295`; R17 treats old managed headers as foreign | skip; registration owns these shared host files; a copy must not claim CRW registration |
| `codexclaw/hook-observations/<session-hash>/<actor-hash>/<slot-hash>.json` and UUID `.tmp` | upstream `plugins/codexclaw/scripts/hook-observation.mjs:17-18,56-77` | `crw/hook-observations`; `internal/harness/observation.go:96-193` | skip; rebuildable invocation diagnostics describe the old plugin/digests, not authenticated CRW hook proof |
| `memories_<N>.sqlite` and SQLite sidecars | `O/recall/src/memory-requeue.ts:128-140` explicitly updates retry counters | same host DB; `internal/recall/memoryrequeue.go:141-173` | skip; host-owned DB stays in place, no migration or requeue operation |
| `codexclaw/subagents.json` | legacy read fallback only, `O/subagent-config/src/store.ts:130-141` | `internal/role/store.go:16-26` has canonical global layer | skip; no v0.2.40 writer; report manual legacy-only setting, do not import a removed layer |
| `runtime/ast-grep/**` | lookup-only helper anchor above | host location unchanged; no Go state writer | skip; executable installation is outside migration |

R19 is not a textual rename rule. Its reserved-prefix note concerns components inside runtime archive/staging trees (`internal/runtime/install/archive.go:231-249`); the Codex-home owner uses `.crw-install.json` outside those trees. This design names that implemented owner rather than inventing another manifest or renaming CRW's runtime installation record. Copying this consent/restore record is not proof of installed runtime, enabled hooks or successful activation.

### Locks and intermediate files are not durable records

| Source pattern | CXC writer | Go equivalent | Disposition |
| --- | --- | --- | --- |
| `sessions/<id>.json.lock` | `P/state.ts:671-689` | `internal/pabcd/state/lock.go:33` | skip transfer; presence refuses selected scope as possibly active |
| `goalplans/<slug>/.goalplan.lock/owner.json` and lock directory | `P/goalplan.ts:823-846` | `internal/pabcd/goalplan/lock.go:107-127,177` | skip transfer; lock-directory presence refuses even without owner.json |
| `dispatches/<session>/<dispatch>.json.lock/` | `O/subagent-config/src/fallback-dispatch.ts:127-167` | `internal/role/dispatch_ledger.go:275-312` | skip transfer; presence refuses, never break it |
| `agents/.<role>-update.lock` | `O/subagent-config/src/role-registration.ts:65-83` | `internal/role/registration.go:200-215` | skip transfer; presence refuses Codex scope |
| State/source/goalplan/evidence/user catalog/store intermediates: `*.tmp` with PID/time/UUID suffixes; `.tmp-*`; `evidence-unrecordable/.probe-*`; role `.ROLE-UUID.tmp`; marker `.json.tmp`; `.config.toml.tmp-*` | `P/state.ts:387,625`, `goalplan.ts:932`, `session-source.ts:188`, `subagent-evidence.ts:222,359`; `O/subagent-config/src/{store.ts:252-262,live-catalog.ts:86-87,role-registration.ts:86-95}`; `O/bg-wake/src/store.ts:47-57,spawn.ts:50-59`; `O/config-guard/src/self-heal.ts:275,310`; `O/cxc-ops/src/hook-trust.ts:363-366` | state/write.go:271; goalplan/write.go:285; source/session/session.go:365-388; evidence/attempts.go:98; role/store.go:91-113; job/store.go:133; configguard/selfheal_marker.go:128-142 | skip known producer-specific intermediates and report them; do not apply a generic `*.tmp` exclusion inside user plan/artifact trees |

Outside these roots, root `codexclaw.json` maps to `crw.json` (R28; `P/interview-policy.ts:27,105`, `internal/pabcd/projectcfg/policy.go:20-40`), and `devlog/_plan/**` is authored by `P/plan-cli.ts:178-190` and read by `internal/pabcd/attest/plangate.go:32-108`. Report their existence but do not edit committed project files in this state-copy operation. Messenger service files outside the roots (`Library/LaunchAgents/com.codexclaw.serve.plist`, `.config/systemd/user/codexclaw-serve.service`) are written by `O/messenger-bridge/src/service.ts:62-64,133,159-160,200`; skip under the messenger exclusion. Temporary subspawn grants (R21) live under TMPDIR, not these roots; do not transfer live execution capabilities.

## Keep embedded paths and recorded hashes

Rule: preserve every copied payload byte, including old `.codexclaw`, `~/.codexclaw`, commands, free text, unknown keys and hashes. Change only the inventory's destination paths. Read a separate bounded view for attention reporting; never serialize that view back over a record. Go revivers intentionally drop/default fields (`internal/pabcd/goalplan/revive.go:13-15`), so they are unsafe migration encoders.

The following covers defined path-bearing fields; arbitrary strings can also contain paths and stay opaque. No filesystem walk follows a record-supplied path outside the selected inventory.

| Record field | Baseline Go consumption of a copied old value | Rule / probe |
| --- | --- | --- |
| Session `planUnit`; attestation `planUnit`, `planPaths[]` | A-phase state restoration keeps it; plan gate resolves relative to cwd and requires real, confined numbered docs (`state/restore.go:88-90`, `attest/plangate.go:32-108`) | keep; retained old plan may still pass, no aliasing (P1) |
| Attestation `testReceiptPath`; goalplan `finalGate.testReceiptPath`, `qaReceiptPath` | goalplan revival keeps strings; receipt parser requires the file inside `.crw/evidence` (`goalplan/revive.go:184-187`, `gate/receipt.go:109-116`) | keep; old-root references refuse; fresh CRW receipt/QA required (P1/P2) |
| Session `unverifiedSubagents[].receiptClaimed` | descriptive failed claim, not a valid receipt; reconstruct/truncate and evidence resolution (`state/state.go:76-84`, `evidence/evidence.go:115-127`) | keep; retry/tombstone stays unresolved until actual evidence is supplied |
| Session `boundSourceRoot`, `phaseEntrySource.sourceRoot` | binding and comparison use canonical workspace identity; source root mismatch fails (`source/session/session.go:204-218`, `source/identity.go:120-137`) | keep; never relocate the workspace by substring substitution (P1) |
| Source binding `nativeCwd`, `sourceRoot`, `commonDir`, `gitDir` | `Resolve` canonicalizes and checks owner/worktree identity (`source/session/session.go:191-255`) | keep; workspace/git metadata do not move with the state directory |
| Goalplan `reviewRounds[].planPath`, `planUnit`, `planFiles[].path` | revival keeps local paths; recomputation reads the exact spellings (`goalplan/revive.go:96-150`, `cli/review_round_args.go:302-330`) | keep; old-root files still exist; changed paths would change aggregate hash (P1) |
| Goalplan `reviewRounds[].lane.workspaceRoot`; every lane/finalGate `sourceIdentity.sourceRoot` | retained identity/root values; source comparison, not automatic migration translation (`goalplan/revive.go:78,153-175`, `source/identity.go:120-137`) | keep; root relocation requires new evidence, not rewriting (P1) |
| Test/QA receipt `sourceIdentity.sourceRoot`, `generatedPaths[]` | parser keeps generated-path exclusions, which check capture uses verbatim (`gate/receipt.go:85-100,159-164`, `gate/check.go:51`) | keep; old exclusions can become inappropriate; regenerate receipt (P1) |
| QA receipt `artifactManifest[].path` | must be relative without `..`; resolves against receipt directory, then confines/hashes artifacts (`gate/manifest.go:89-143`) | keep; relative graph survives copy; absolute old paths refuse rather than get repaired (P1) |
| QA verdict `artifactRefs[]` (referenced from QA manifest) | absolute/relative refs resolve from the identity directory and must name that exact identity (`gate/manifest.go:157-169`) | keep; relative manifest with an old absolute verdict ref can still refuse; fresh artifact graph required (P1) |
| Freeze `planFiles[].path`; `evidenceBundle.researchReportRef` | plan paths are relative to the slug directory; evidence reference is descriptive (`interview/freeze.go:26-49`, `freezecli.go:119-160`) | keep; same plan bytes preserve hash; reference is not opened by freeze (P1) |
| Divergence candidate `worktree`, `sourceUrls[]`; render row `screenshotPath` | text is retained; render ledger reader does not revalidate the old screenshot (`metric/divergence.go:95-106`, `hook/renderobs.go:173-198`) | keep; report old-root locations, not a new view-image event (P1) |
| BG record `cwd`, `command[]`, `note` | cwd/command are retained; output/exit files are derived from selected workspace plus job ID (`relay/job/registry.go:50-120`, `store.go:55-65`) | keep; there are no persisted outputPath/exitPath fields; running jobs refuse |
| Configguard manifest `configPath`, `backupPath`, `tableKeys.*.priorValue/appliedValue` | exact values survive parsing; deactivation can read original backup (`configguard/manifest.go:57-75`, `deactivate.go:124-144`) | keep; original backup remains; no config mutation or path rebinding (P2) |
| Dispatch attempts' `taskFailure.evidence`, `reconciliation`; ledgers' `detail`/`evidence`, criteria evidence/outcomes, interview text | diagnostic/free text, never an implicit alias (dispatch_ledger.go:46-59; goalplan/types.go:272-279) | keep opaque; report defined operational references only |

`ledgerPath` in CXC is a derived Stop-work display field (`P/hook.ts:1574,1658,1682`), not a persisted goalplan field. Go derives the new ledger location from its store. Do not invent a migration rewrite for it. Self-heal marker, retry-attempt files and job records have no additional typed state-directory path fields.

Hash consequences:

- Freeze `planFiles[].sha256` hashes decoded file text; `planHash` hashes the sorted file hashes alone (`interview/freeze.go:93-100`). Preserve plan content and slug-relative names; P3 proves a one-file ASCII copy stays fresh, not universal compatibility. Go has known non-ASCII ordering differences (`interview/freeze.go:62-99`); identical multi-file copies can be stale. Keep the recorded aggregate, report attention, and require explicit revalidation rather than rehashing historical approval.
- Review-round `planSha256` hashes path plus NUL plus file hash (`cli/review_round_args.go:127-134`). Preserve both paths and hashes. A renamed path changes the aggregate even with identical bytes; never recalculate it to retain an old PASS (P1).
- Receipt/artifact hashes and `lane.artifactSha256` remain valid for identical copied bytes; confinement and source freshness remain separate checks. Do not edit plan prose or screenshot/identity metadata to fix a path.
- Stored source `commitSha`, `treeHash`, `sourceRoot` remain historical. Go excludes `.crw/`, not `.codexclaw/` (`source/identity.go:50-60,89-100`). Unignored/tracked retained old entries can change freshness; do not manufacture a new identity for an old review (P1).

Attention status is independent of copy success. IDLE/I/P sessions can be reconsidered at their normal next capture. A needs its original plan and valid audit binding. B needs a source comparison; C needs fresh CRW receipt paths/check epoch and current source; an approved final gate still needs its real consumer's current evidence. This command neither advances a phase nor bypasses a failed gate. Draining or explicitly revalidating B/C sessions is safer than promising transparent continuation.

## Preflight the whole selected scope before writing

Keep the default conflict proposal, strengthened to whole-scope preflight: equal destination bytes are skipped, any differing destination refuses the selected scope before any write. Existing directories must be real directories. Existing `.gitignore` is owner-managed and retained. Equality never changes destination mode/mtime. A difference is named in the report; there is no overwrite, merge, force or delete option.

1. Pin roots: distinct project/user source and destination trees cannot overlap or be ancestors. Codex scope intentionally shares one C directory and maps distinct allowlisted leaf names in place; only identical leaf mappings or links are refused. A C root located inside a selected project/user tree is rejected; project-inside-home uses only fixed `.codexclaw`/`.crw` source/destination subtrees. Reject links, non-directory roots, unsafe inventory entries and hard-linked input files. Inspect retained paths with no-follow handles; directories must be ordinary directories, files regular with one link and no set-ID bits; existing destination hard links are refused too. Skip subtrees are not followed; a symlink at a listed subtree root is refused. No archive extraction or record field can expand the copy boundary.
2. Enumerate/classify the entire selected scope deterministically, record size/hash/mode, and inspect every mapped destination. Active locks, unresolved dispatches, running/unknown BG jobs, unreadable retained records, unsupported names or conflicts refuse preflight. Lock absence alone is not proof of quiescence; Jun arranges the quiet window.
3. Before copying any state, create/pin `.crw` if absent and no-replace publish canonical `.gitignore` whenever it is missing, even in an existing root. Validate an existing ignore file as regular and retain it. Interruption after root creation simply repeats this absent-file publication on rerun; do not use EnsureDir's existence short-circuit as recovery. Failure or a conflicting initialization race stops before copying records. Copy dependencies before their referencing records: artifacts/plans/backups first, then bindings/ledgers/control records. Use a private exclusive current-run temp in the destination directory, write and fsync it, set the original permission bits (including read-only/executable modes), close it, atomically rename it to the absent final name with a no-replace syscall and fsync the directory; no two-link final/temp interval exists. Creation modes must respect privacy before widening; set-ID sources are refused in preflight, not silently chmodded. New directory modes are applied after children finish so read-only source directories remain reproducible; a directory this migration created, or one an interrupted run left at the private mode it creates directories with, has its source mode finished on a rerun, while a directory this migration did not create retains its mode and is reported.
4. Publication must never replace an existing file. If a racer creates the destination, reopen safely: equal bytes count as skipped, differing bytes or a link stop apply and report partial progress. `crwdir.Publish` is unsuitable: it replaces and follows final links (`crwdir/atomic.go:33-46`). Use Linux `renameat2(RENAME_NOREPLACE)` / Darwin `renameatx_np(RENAME_EXCL)`, following `internal/pabcd/state/rename_noreplace_{linux,darwin}.go:11-20`. Add small migration-owned wrappers; do not extract or modify the PABCD owner. Existing `golang.org/x/sys/unix` suffices. Other platforms refuse before applying. An unsupported kernel/filesystem discovered at publication stops apply and reports the writes already completed; never fall back to rename-over or in-place writes. Pass pinned parent-directory descriptors and leaf names to the rename syscall, so a replaced ancestor cannot redirect publication.
5. Recheck source/destination bytes and modes against the preflight inventory; source movement stops the run. An interruption or I/O error may leave complete copied files and isolated temps, never a truncated final file. A rerun re-preflights; equal completed files skip, incomplete temps from older runs are reported/ignored, never adopted or automatically removed. Successful completion has the same data result as uninterrupted copy.

A pinned root is compared with the other selected trees by directory identity, not by mount boundary. For every tree
that exists (each pair's source and destination) the identities (device and inode) of its root and of every directory
reachable below it are collected by walking from the pinned handle through `Dir.Child` only, so a link entry is skipped
and never opened, and every spelling of a directory is walked because two names of one directory can show different
submounts. A tree, or the Codex home, whose chain of directory identities holds one of another tree's identities is
refused with `overlap` and the detail `reaches <other path> through another spelling (a bind mount)`, because a bind
mount is one directory reached by a second name and its identity is unchanged; two trees that hold a directory of each
other below their own roots are refused the same way, because the same directory is reachable under both. The tree's own
identity is in its set, so every refusal the by-identity check already made stays. A directory the walk cannot open or
read, a walk that enters more than 200,000 directories or nests deeper than that, refuses instead of passing an unproven
comparison; the count is of directories entered, not of distinct identities recorded, so a tree of many spellings of one
directory cannot slip past the bound. Two limits: a FUSE mirror such as bindfs that reports its own inode numbers is not
recognised, and a mount changed during a run is out of scope, because changing one needs a privilege the copy does not use.

Source files, permissions, links and directories are never changed, removed or renamed. The command writes no config, activation, registration or source metadata. Multi-file publication is not one atomic transaction; the quiet-window requirement and references-last ordering prevent consumers from reading an unfinished graph. No rollback deletes copied records. Failure output records exactly which writes completed.

The report includes before/after hashes/counts, skipped items/reasons, conflicts, mode differences, partial progress and attention fields. A final `--report <absent-file>` is itself published no-replace after data verification; a retained report never authorizes reuse without rechecking current files. No extra manifest is inserted into the runtime's state directories.

## Make migration an explicit install subcommand

Proposed surface:

```text
crw install migrate-state [--scope project|user|codex|all] [--cwd <workspace>]
  [--from-home <old-user-root>] [--to-home <new-user-root>]
  [--codex-home <codex-root>] [--dry-run] [--json] [--report <absent-file>]
```

Default scope is project; other scopes are explicit. Defaults use the roots above. `--from-home`/`--to-home` are rejected unless user/all is selected; `--codex-home` is rejected unless codex/all is selected. Help and invalid arguments write nothing; report destination must be outside source/destination trees. Dry-run performs the same reads/classification/conflict checks and writes nothing, including no report file, temp or `.gitignore`.

Add the command to `internal/runtime/install/cli.go:31` and route it before generic install options, like `features`/`config` (`:86-92`). Give it its own help; exclude it from the legacy frozen usage rendering. `cmd/crw/main.go:171-178` already gives the install mode signal cancellation. Do not call install/update/activate/self-heal or hook registration. No hook, automatic installer path, activation path or startup task runs it.

Text output begins with result/scope, then root mappings, copied/equal-skipped/excluded counts, refusals and attention. JSON uses versioned `crw-state-migration/1`: `result`, `dryRun`, `scope`, `roots`, ordered `items` (`source`, `destination`, `disposition`, `result`, `reason`, byte count, digest, mode), `attention`, `writesCompleted`, `sourceVerified`. Results distinguish dry-run, copied, already-equal, refused and interrupted/failed.

Exit 0 means successful verified copy/already-equal/dry-run (attention may remain); exit 1 means conflict, unsafe input, source change, interruption or I/O/unsupported-filesystem failure, with actual partial writes reported; exit 2 means usage. Cancellation follows the install signal wrapper and returns a failed/partial report, not success. Exit 0 is not activation or resumed-session proof.

## Implement foundations before the command

Each slice targets about 600 changed lines or fewer including tests. Exact sizing is re-estimated before release; split any slice that exceeds the coordinator's allowance. New package `internal/runtime/install/migrate` depends on existing path/name constants and pure inspectors; install CLI depends on it. It must not import activation flows or make PABCD owners depend on install.

| Order / dependency | Files to change | Red-first cases | End condition / estimated changed lines |
| --- | --- | --- | --- |
| M1 foundations; none | NEW `internal/runtime/install/migrate/{types,roots,publish,publish_linux,publish_darwin,publish_other}.go`, matching `_test.go` | overlapping roots; source/destination/ancestor link, FIFO/directory-as-file, hard-linked input; existing differing file and publication racer; read-only mode; interruption between root mkdir and ignore publication, rerun repairs absent ignore before any state copy; unsupported no-replace rename; crash after rename before directory fsync; interrupted temp | confined no-replace primitive preserves bytes/modes; source untouched and finals whole; 450-580 |
| M2 inventory; M1 | NEW `internal/runtime/install/migrate/{inventory,classify}.go`, matching `_test.go` | every table row; removed project role layer; caches/excluded stores; unknown files; lock directory without owner; user plan named `.tmp`; unsafe skipped-root link; malformed retained record | dry planner emits exhaustive deterministic disposition, all preflight refusals cause zero writes; 450-580 |
| M3 application/attention; M2 | NEW `internal/runtime/install/migrate/{apply,attention}.go`, matching `_test.go` | source changes during copy; interruption/rerun; group references-last; old receipt/QA refs and hash-path attention; multi-file non-ASCII freeze ordering stale attention with recorded hash unchanged; raw unknown JSON fields/non-UTF-8 bytes retained; terminal versus running BG/dispatch; manifest consent and backupPath preserved | byte-identical, repeatable copy with no fresh approval; inventory/report prove completed and partial writes; 480-600 |
| M4 explicit surface; M3 | NEW `internal/runtime/install/migrate/{cli,report}.go`, matching `_test.go`; MODIFY `internal/runtime/install/cli.go`, `cli_test.go`; focused installer command documentation | help/invalid flags no writes; dry-run no root/report creation; scope/default/env resolution; all exits and cancellation; legacy install help unchanged; hooks/activation never call migrate | one explicit command covers all scopes, stable output and own help, isolated end-to-end synthetic home checks; 350-550 |

The implementation test runs use synthetic roots and fresh `-count=1` evidence. Include Linux/Darwin no-replace publication where supported; do not add dependencies or platform fallbacks that overwrite. Migration fixtures are synthetic assertions of this new contract, not edited CXC goldens. Probe programs from this design are not committed or used as production code.

## Jun chooses the transition window

J2 includes quieting existing writers, whether to drain or revalidate armed sessions, installation/trust timing and eventual CXC removal. Recommended sequence: dry-run all selected roots during a quiet window, inspect conflicts/attention, drain B/C or arrange explicit fresh checks, apply copy, verify the report, then separately authorize activation. Keeping source allows operational recovery; this design never schedules removal.

Alternatives rejected: blanket R26 replacement damages historical evidence; Go typed reserialization loses unknown fields; overwrite/merge breaks rerun safety; live copying of running jobs risks split registries; hook-triggered migration couples state writes to activation; a PABCD-only verb does not cover user/Codex ownership records. Cost: cold recall/cache rebuild, retained old backups/state, and fresh evidence for some continuing sessions.

The coordinator should review the R19 historical-note/current-owner distinction and define synthetic implementation cases from this document. Jun must decide J2. No migration or activation has run as part of this design.

## Evidence is source-backed and isolated

P1: throwaway Go program on the baseline imports state, goalplan, attest, gate, freeze, source and render consumers. It observes old receipt-root refusal/new-root acceptance, relative QA-manifest acceptance/absolute-path refusal, unchanged copied path strings, freeze freshness, path-sensitive review hash and conditional source-root/exclusion effects.

P3: CXC `runFreeze` writes an actual synthetic manifest and plan under an isolated old root; their bytes are copied to the CRW root. Go `RunFreeze --dry-run` reports fresh and leaves the copied manifest unchanged. This replaces the earlier self-comparison as evidence for that one ASCII case; it does not cover known non-ASCII ordering differences.

P2: throwaway configguard package probe, `go test -count=1 -run '^Test619CopiedPathProbe$' -v`, observes exact install/backup paths and self-heal opt-out preservation. Both run outside the branch with temporary HOME, CODEX_HOME and CRW_HOME. Commands, outputs and read ranges are recorded in the PR body; no probe, fixture or runtime state enters the branch.
