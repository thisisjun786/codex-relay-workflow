// Original CRW test (no CXC counterpart): the execution-policy screen's pure state logic.
//
// The issue's red-first list lives here: the preview before/after, 409 keeping the inputs, 422
// showing the check messages, 502 showing whether the file was restored, the three applied values,
// an unavailable catalog not disabling every effort, a saved value the catalog does not list not
// being swapped, the exception-removal preview, and the supervisor row not being editable.
//
// Everything is asserted through policy-state.ts, which is the screen's own state logic: a test
// that called the API client with a hand-made value would not cover a criterion stated about the
// screen.
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  POLICY_BLAST_RADIUS,
  POLICY_CONTROL_ELEMENTS,
  SUPERVISOR_LABEL,
  allowedEffortsLabel,
  catalogNotice,
  checkNotice,
  changeFromExceptionDraft,
  decodeCheck,
  decodePolicy,
  draftForException,
  draftForNewException,
  exceptionRoleOptions,
  lostWriteNotice,
  modelOptions,
  modelLadder,
  noticeForWrite,
  pairEffortLabel,
  pairModelLabel,
  policyEfforts,
  policyView,
  previewChange,
  removeExceptionLabel,
  roleControlsLabel,
  screenAllowedAddModel,
  screenAllowedDraft,
  screenDraftIsNew,
  screenEditable,
  screenEffortUnavailable,
  screenExceptionDraft,
  screenLoaded,
  screenCatalogLoaded,
  screenReread,
  screenSaveFinished,
  screenSaveStarted,
  initialScreen,
  allowedAddChoice,
  addModelOptions,
  allowedEntriesOf,
  screenAllowedEntryAdded,
  screenAllowedEntryRemoved,
  screenAllowedEntryText,
  screenPropose,
  type ExceptionDraft,
  type ModelCatalog,
  type PolicyChange,
  type PolicyReading,
} from "../src/policy-state.ts";

/** The reading a host with a registered policy answers, in the Go route's own shape. */
function reading(changes: Partial<PolicyReading> = {}): PolicyReading {
  return {
    state: "registered",
    path: "/host/execution-policy.json",
    mode: "allowlist",
    digest: "a".repeat(64),
    registeredDigest: "a".repeat(64),
    runningDigest: "a".repeat(64),
    roles: [
      { name: "child", expectation: "", pairs: [{ model: "anthropic/opus", reasoningEffort: "xhigh" }] },
      { name: "parent", expectation: "", pairs: [{ model: "gpt-6.1-sol", reasoningEffort: "xhigh" }] },
      { name: "supervisor", expectation: "record", pairs: [] },
    ],
    allowed: [
      { model: "anthropic/opus", efforts: ["max", "xhigh"] },
      { model: "gpt-6.1-sol", efforts: ["xhigh"] },
    ],
    exceptions: [{ id: "legacy", role: "parent", model: "devin/swe-2", reasoningEffort: "max", cwd: ["/srv/project"] }],
    applied: "applied",
    actions: [],
    ...changes,
  };
}

/** A fresh catalog answer. */
function catalog(status: "fresh" | "stale" | "unavailable", entries: ModelCatalog["entries"] = []): ModelCatalog {
  return { state: status === "fresh" ? "ocx-active" : "unavailable", status, entries };
}

/* ---- C1: the first screen reads every value from the policy file ---- */

test("the view carries the supervisor, parent and child rows and their source", () => {
  const view = policyView(reading());
  // The first screen is a row for each of the three roles, in the order the issue lists them,
  // whether or not the file declares them: a child-only policy still shows the other two.
  assert.deepEqual(view.roles.map((role) => role.name), ["supervisor", "parent", "child"]);
  assert.deepEqual(view.roles.map((role) => role.declared), [true, true, true]);
  const supervisor = view.roles.find((role) => role.name === "supervisor");
  assert.equal(supervisor?.editable, false);
  assert.equal(supervisor?.label, SUPERVISOR_LABEL);
  const parent = view.roles.find((role) => role.name === "parent");
  assert.equal(parent?.editable, true);
  assert.deepEqual(parent?.pairs, [{ model: "gpt-6.1-sol", reasoningEffort: "xhigh" }]);
  // The source is the policy file the wiring record names, not a value the screen invented.
  assert.equal(view.source.path, "/host/execution-policy.json");
  assert.equal(view.source.digest, "a".repeat(64));
  assert.equal(view.editable, true);
});

test("a reading with no record shows its state and blocks editing", () => {
  const view = policyView({ ...reading(), state: "not_registered", reason: "no record", roles: [], allowed: [], exceptions: [], digest: "", registeredDigest: "" });
  assert.equal(view.state, "not_registered");
  assert.equal(view.reason, "no record");
  assert.equal(view.editable, false);
  // The three rows remain, each marked undeclared, so the operator can see the screen is complete
  // even though the file it reads declares nothing.
  assert.deepEqual(view.roles.map((role) => role.name), ["supervisor", "parent", "child"]);
  assert.deepEqual(view.roles.map((role) => role.declared), [false, false, false]);
  assert.deepEqual(view.roles.map((role) => role.pairs), [[], [], []]);
});

test("an unreadable policy blocks editing and keeps its reason", () => {
  const view = policyView({ ...reading(), state: "unreadable", reason: "the file is not an object" });
  assert.equal(view.editable, false);
  assert.equal(view.reason, "the file is not an object");
});

test("decodePolicy refuses an answer that is not the route's shape", () => {
  assert.throws(() => decodePolicy(null));
  assert.throws(() => decodePolicy({ state: "registered" }));
  assert.throws(() => decodePolicy({ ...reading(), roles: "nope" }));
  assert.equal(decodePolicy(reading()).state, "registered");
});

// The two nulls the Go route actually sends. Devin found both: policyHandler assigns
// AppliedActions(...) without the emptyIfNil wrapper, and projectRoles leaves Pairs nil for a role
// that declares only an expectation. A strict array check on either made the whole screen throw on
// every normally applied host, so each is pinned with the exact bytes the route produces.
test("a null actions list is an empty list, not a malformed answer", () => {
  const raw = { ...reading(), actions: null };
  const decoded = decodePolicy(raw);
  assert.deepEqual(decoded.actions, []);
  assert.equal(policyView(decoded).editable, true);
});

test("a null pairs list is a role with no pairs, not a malformed answer", () => {
  const raw = {
    ...reading(),
    roles: [
      { name: "supervisor", expectation: "record", pairs: null },
      { name: "child", expectation: "", pairs: [{ model: "anthropic/opus", reasoningEffort: "xhigh" }] },
    ],
  };
  const view = policyView(decodePolicy(raw));
  const supervisor = view.roles.find((role) => role.name === "supervisor");
  assert.deepEqual(supervisor?.pairs, []);
  assert.equal(supervisor?.editable, false);
  assert.equal(view.editable, true);
});

/* ---- C2: edits, and the exception-removal fallback ---- */

test("the role-pair preview shows the before and after of the changing row", () => {
  const change: PolicyChange = { kind: "setRolePairs", role: "parent", pairs: [{ model: "anthropic/opus", reasoningEffort: "max" }] };
  const preview = previewChange(reading(), change);
  const item = preview.items.find((entry) => entry.label.includes("parent"));
  assert.ok(item, "the preview names the parent row");
  assert.equal(item?.before, "gpt-6.1-sol xhigh");
  assert.equal(item?.after, "anthropic/opus max");
  assert.equal(preview.blastRadius, POLICY_BLAST_RADIUS);
});

test("removing an exception previews that scope returning to the role default", () => {
  const preview = previewChange(reading(), { kind: "removeException", id: "legacy" });
  const item = preview.items.find((entry) => entry.label.includes("legacy"));
  assert.ok(item, "the preview names the exception");
  assert.equal(item?.before, 'parent devin/swe-2 max ("/srv/project")');
  assert.ok(item?.after.includes("parent"), "the after names the role whose default returns");
  assert.ok(preview.fallback, "the preview carries the fallback sentence");
  assert.ok(preview.fallback?.includes("/srv/project"));
  assert.equal(preview.blastRadius, POLICY_BLAST_RADIUS);
});

test("the allowed-list preview shows the efforts before and after", () => {
  const preview = previewChange(reading(), { kind: "setAllowed", model: "gpt-6.1-sol", efforts: ["max"] });
  const item = preview.items.find((entry) => entry.label.includes("gpt-6.1-sol"));
  assert.equal(item?.before, "xhigh");
  assert.equal(item?.after, "max");
});

test("removing an allowed model previews the row leaving the list", () => {
  // The trigger a reviewer would try: remove the row, not edit its efforts. The backend refuses
  // removing the last entry (check.go:309-313), so the preview names which row leaves.
  const preview = previewChange(reading(), { kind: "removeAllowed", model: "gpt-6.1-sol" });
  const item = preview.items.find((entry) => entry.label.includes("gpt-6.1-sol"));
  assert.ok(item, "the preview names the allowed row");
  assert.equal(item?.before, "xhigh");
  assert.equal(item?.after, "removed");
  assert.equal(preview.blastRadius, POLICY_BLAST_RADIUS);
});

test("setting an exception previews the exception before and after", () => {
  const preview = previewChange(reading(), {
    kind: "setException",
    id: "legacy",
    role: "child",
    model: "anthropic/opus",
    effort: "max",
    cwd: ["/srv/other"],
  });
  const item = preview.items.find((entry) => entry.label.includes("legacy"));
  assert.ok(item, "the preview names the exception");
  assert.equal(item?.before, 'parent devin/swe-2 max ("/srv/project")');
  assert.equal(item?.after, 'child anthropic/opus max ("/srv/other")');
});

test("setting an exception that is not declared previews it as new", () => {
  const preview = previewChange(reading(), {
    kind: "setException",
    id: "fresh",
    role: "parent",
    model: "gpt-6.1-sol",
    effort: "xhigh",
    cwd: [],
  });
  const item = preview.items.find((entry) => entry.label.includes("fresh"));
  assert.equal(item?.before, "not declared");
  assert.equal(item?.after, "parent gpt-6.1-sol xhigh (no cwd scope)");
});

test("removing an exception with no role says what that removal actually does", () => {
  // A role-less exception is not inert: the bridge matches an exception's role against the request's
  // role exactly, so it covers the requests that cite no role. The removal sentence must describe
  // that, and must not invent a role or claim a fallback to a role default.
  const unscoped = reading({ exceptions: [{ id: "any", model: "m", reasoningEffort: "max", cwd: ["/srv/all"] }] });
  const preview = previewChange(unscoped, { kind: "removeException", id: "any" });
  const item = preview.items.find((entry) => entry.label.includes("any"));
  assert.equal(item?.after, "no role");
  assert.ok(preview.fallback?.includes("cite no role"));
  assert.ok(!preview.fallback?.includes("role default"));
  assert.ok(!preview.fallback?.includes("the cited role"));
});

// The unsupported catalog is a fourth state, not the generic stale sentence: this host's OCX does
// not read a live catalog at all, and the reader says so in its message.
test("the unsupported catalog is told apart from a failed refresh", () => {
  const unsupported: ModelCatalog = {
    state: "unsupported-ocx-catalog",
    status: "stale",
    entries: [{ id: "m", label: "m" }],
    message: "This OCX does not support reading the live model catalog (ocx models live --json).",
  };
  const notice = catalogNotice(unsupported);
  assert.ok(notice.includes("does not support"));
  assert.notEqual(notice, catalogNotice({ ...unsupported, state: "unavailable", status: "stale", message: "" }));
});

// A lost write response is indeterminate, not a failure: the server finishes registration after it
// has replaced the file, so the screen must re-read rather than claim nothing changed.
test("a lost write response is reported as unknown and triggers a re-read", () => {
  const notice = lostWriteNotice();
  assert.equal(notice.tone, "err");
  assert.equal(notice.reread, true);
  assert.equal(notice.keepInputs, true);
  assert.equal(notice.stored, null);
  assert.ok(notice.text.toLowerCase().includes("unknown"));
  assert.ok(!notice.text.includes("was not changed"));
});

test("decodeCheck reads the check route's answer and refuses a body without its verdict", () => {
  const refused = decodeCheck({ valid: false, errors: ["role 'supervisor' cannot declare a model"], currentDigest: "a".repeat(64), stale: false, diff: [] });
  assert.equal(refused.valid, false);
  assert.equal(refused.errors.length, 1);
  const accepted = decodeCheck({ valid: true, errors: [], currentDigest: "b".repeat(64), stale: true, diff: ["roles.parent"] });
  assert.equal(accepted.valid, true);
  assert.equal(accepted.stale, true);
  assert.deepEqual(accepted.diff, ["roles.parent"]);
  assert.throws(() => decodeCheck({ errors: [] }));
  assert.throws(() => decodeCheck(null));
});

test("a check refusal is an error notice and never a success", () => {
  const refused = checkNotice(decodeCheck({ valid: false, errors: ["the change moved allowed"], currentDigest: "a", stale: false, diff: [] }));
  assert.equal(refused.tone, "err");
  assert.ok(refused.text.includes("the change moved allowed"));
  const accepted = checkNotice(decodeCheck({ valid: true, errors: [], currentDigest: "a", stale: false, diff: ["roles.child"] }));
  assert.equal(accepted.tone, "info");
  assert.ok(accepted.text.includes("roles.child"));
});

/* ---- C3: the catalog states, and never swapping a saved value ---- */

test("the four catalog states are told apart from success", () => {
  assert.ok(catalogNotice(catalog("fresh")).includes("Model list"));
  assert.ok(catalogNotice(catalog("stale")).toLowerCase().includes("stale"));
  assert.ok(catalogNotice(catalog("unavailable")).toLowerCase().includes("unavailable"));
  assert.ok(catalogNotice(null).toLowerCase().includes("loading"));
});

test("an unavailable catalog does not remove the effort names the policy declares", () => {
  // The policy file is the authority for this screen: a catalog that could not be read must not
  // empty the effort list, and it must not disable every option.
  const names = policyEfforts(reading(), catalog("unavailable"));
  assert.deepEqual(names, ["max", "xhigh"]);
  assert.ok(names.includes("max"));
});

test("effort names keep none and max and never come from a fixed list", () => {
  const withNone = reading({
    allowed: [{ model: "m", efforts: ["none", "max"] }],
  });
  const names = policyEfforts(withNone, catalog("fresh", [{ id: "m", label: "m", reasoningEfforts: ["xhigh"] }]));
  assert.deepEqual(names, ["none", "max", "xhigh"]);
});

test("a saved model the catalog does not list stays selected and is marked unavailable", () => {
  const options = modelOptions(reading(), catalog("fresh", [{ id: "gpt-6.1-sol", label: "Sol" }]));
  const saved = options.find((option) => option.id === "anthropic/opus");
  assert.ok(saved, "the saved model is still offered");
  assert.equal(saved?.unavailable, true);
  assert.equal(options.find((option) => option.id === "gpt-6.1-sol")?.unavailable, false);
});

test("a saved model is kept when the catalog could not be read at all", () => {
  const options = modelOptions(reading(), catalog("unavailable"));
  assert.ok(options.some((option) => option.id === "anthropic/opus"));
  assert.ok(options.some((option) => option.id === "gpt-6.1-sol"));
});

test("an effort the selected model's catalog ladder omits is marked unavailable, not offered", () => {
  // d5: the effort list is the names that EXIST; whether one is offered for a particular model is a
  // separate question. A name only another model advertises must not read as a normal option here.
  const fresh = catalog("fresh", [{ id: "gpt-6.1-sol", label: "Sol", reasoningEfforts: ["xhigh"] }]);
  const names = policyEfforts(reading(), fresh);
  assert.ok(names.includes("max"), "max is still a name the policy declares");
  // The parent runs gpt-6.1-sol, whose ladder is xhigh only, so max is not selectable for it.
  assert.equal(screenEffortUnavailable({ ...initialScreen(), reading: reading(), catalog: fresh }, "gpt-6.1-sol", "max"), true);
  assert.equal(screenEffortUnavailable({ ...initialScreen(), reading: reading(), catalog: fresh }, "gpt-6.1-sol", "xhigh"), false);
  // An unreported ladder is not evidence that the model refuses anything.
  assert.equal(screenEffortUnavailable({ ...initialScreen(), reading: reading(), catalog: catalog("fresh", [{ id: "gpt-6.1-sol", label: "Sol" }]) }, "gpt-6.1-sol", "max"), false);
  assert.equal(modelLadder(fresh, "gpt-6.1-sol")?.join(","), "xhigh");
  assert.equal(modelLadder(fresh, "nope"), null);
});

test("the add-model control follows a catalog that arrives after the policy", () => {
  // d3: the policy answers first and the catalog second, which is the ordinary order. The control
  // must end up on a model that is actually offered, never on an empty string.
  let state = initialScreen();
  state = screenLoaded(state, reading({ allowed: [{ model: "m", efforts: ["high"] }], exceptions: [], roles: [{ name: "child", expectation: "", pairs: [{ model: "m", reasoningEffort: "high" }] }] }));
  // Before the catalog, the only model is the policy's own, and it is already in the allowed list.
  assert.equal(allowedAddChoice(state, []), "");
  // The catalog then lists a new model.
  state = screenCatalogLoaded(state, catalog("fresh", [{ id: "fresh/model", label: "Fresh" }]) as never);
  const free = ["fresh/model"];
  assert.equal(allowedAddChoice(state, free), "fresh/model");
  // A choice the operator made is kept while it is still free, and dropped when it is not.
  state = screenAllowedAddModel(state, "fresh/model");
  assert.equal(allowedAddChoice(state, free), "fresh/model");
  // d3: a chosen model that is no longer free is KEPT rather than substituted with the first free
  // one, so the control can never display one model while Add proposes another.
  assert.equal(allowedAddChoice(state, []), "fresh/model");
  assert.deepEqual(addModelOptions(state, []), ["fresh/model"]);
  assert.deepEqual(addModelOptions(state, ["other"]), ["fresh/model", "other"]);
  // With nothing chosen, the first free model is the default.
  assert.equal(allowedAddChoice(screenAllowedAddModel(state, ""), ["other"]), "other");
});

// The five defects the second pre-merge evaluation found on the fixed head.

test("a new exception's editor stays open while its id is being typed", () => {
  // d1: the editor used to be recognised by an empty id, so the first character typed closed it.
  const draft = draftForNewException("parent", "m", "high");
  assert.equal(draft.isNew, true);
  let state = initialScreen();
  state = screenLoaded(state, reading());
  state = screenExceptionDraft(state, draft);
  assert.equal(screenDraftIsNew(state), true);
  // Typing an id does not close it, and the change becomes proposable.
  state = screenExceptionDraft(state, { ...draft, id: "fresh", cwd: ["/srv/a"] });
  assert.equal(screenDraftIsNew(state), true);
  const change = changeFromExceptionDraft(state.exceptionDraft as ExceptionDraft);
  assert.equal(change?.kind, "setException");
  assert.equal((change as { id: string }).id, "fresh");
  // An existing exception's draft is not the new one.
  const existing = draftForException({ id: "legacy", role: "parent", model: "m", reasoningEffort: "high", cwd: ["/srv/a"] });
  assert.equal(existing.isNew, false);
  assert.equal(screenDraftIsNew(screenExceptionDraft(state, existing)), false);
});

test("the effort names include what the policy's pairs and exceptions declare", () => {
  // d3: a presence_only policy declares no allowlist, so reading only the allowlist and the catalog
  // left such a host with no selectable effort and no way to add a pair or an exception.
  const presenceOnly = reading({ allowed: [], roles: [{ name: "child", expectation: "", pairs: [{ model: "m", reasoningEffort: "none" }] }], exceptions: [{ id: "e", role: "parent", model: "m", reasoningEffort: "max", cwd: ["/srv/a"] }] });
  const names = policyEfforts(presenceOnly, catalog("unavailable"));
  assert.deepEqual(names, ["none", "max"]);
});

test("the recovery block survives a re-read until the host is actually repaired", () => {
  // d5: the block used to live only in the notice, so an explicit re-read lifted it while the server
  // was still refusing every write.
  let state = initialScreen();
  const broken = reading({ digest: "1".repeat(64), registeredDigest: "2".repeat(64) });
  state = screenLoaded(state, broken);
  assert.equal(screenEditable(state), true, "the screen is editable before the failure");
  const recovery = noticeForWrite(500, { error: "recovery_needed", fileDigest: "1".repeat(64), registeredDigest: "2".repeat(64), recovery: "run the repair" });
  state = screenSaveFinished(state, null, recovery);
  assert.equal(screenEditable(state), false, "the block is in force");
  // An explicit re-read clears the notice but not the block, and a reading that still disagrees does
  // not lift it either.
  state = screenReread(state);
  state = screenLoaded(state, broken);
  assert.equal(state.notice, null);
  assert.equal(screenEditable(state), false, "a re-read of the same disagreement keeps the block");
  // Only a reading whose file digest matches the record lifts it.
  state = screenLoaded(state, reading({ digest: "3".repeat(64), registeredDigest: "3".repeat(64) }));
  assert.equal(screenEditable(state), true, "a repaired host is editable again");
});

test("a successful save keeps an edit the operator started while it was in flight", () => {
  // d2: the success branch always re-read without keeping the inputs, so the next edit was wiped.
  let state = initialScreen();
  state = screenLoaded(state, reading());
  state = screenAllowedDraft(state, "anthropic/opus", ["max"]);
  const first = state.change;
  const saving = screenSaveStarted(state);
  const second = screenAllowedDraft(saving, "gpt-6.1-sol", ["high"]);
  const finished = screenSaveFinished(second, first, noticeForWrite(200, { stored: { digest: "b".repeat(64) }, registered: { digest: "b".repeat(64) }, applied: "applied", actions: [] }));
  // The saved model's spent draft is gone, and the later edit is still pending.
  assert.equal(allowedEntriesOf(finished, "anthropic/opus", ["max", "xhigh"]).join(", "), "max, xhigh", "the spent draft is dropped");
  assert.equal(finished.change?.kind, "setAllowed");
  assert.equal((finished.change as { model: string }).model, "gpt-6.1-sol");
  // The screen re-reads with keepInputs, so the later edit survives the read.
  const afterSave = screenLoaded(finished, reading({ digest: "b".repeat(64) }), true);
  assert.equal((afterSave.change as { model: string })?.model, "gpt-6.1-sol", "the later edit survives the re-read");
  assert.equal(afterSave.allowedDraft["gpt-6.1-sol"]?.join(", "), "high");
});

test("a successful save with nothing pending leaves no drafts behind", () => {
  let state = initialScreen();
  state = screenLoaded(state, reading());
  state = screenAllowedDraft(state, "anthropic/opus", ["max"]);
  const saved = state.change;
  state = screenSaveFinished(screenSaveStarted(state), saved, noticeForWrite(200, { stored: { digest: "b".repeat(64) }, registered: { digest: "b".repeat(64) }, applied: "applied", actions: [] }));
  assert.equal(state.change, null);
  assert.equal(allowedEntriesOf(state, "anthropic/opus", ["max", "xhigh"]).join(", "), "max, xhigh", "the input shows the file again");
  const clean = screenLoaded(state, reading({ digest: "b".repeat(64) }), true);
  assert.equal(clean.change, null);
  assert.equal(allowedEntriesOf(clean, "anthropic/opus", ["max", "xhigh"]).join(", "), "max, xhigh");
});

test("editing an existing exception keeps its identifier byte for byte", () => {
  // d2: trimming a stored id sent setException for a different id and created a new exception.
  const exception = { id: " legacy ", role: "parent", model: "m", reasoningEffort: "high", cwd: ["/srv/a"] };
  const draft = draftForException(exception);
  const change = changeFromExceptionDraft({ ...draft, effort: "max" });
  assert.equal((change as { id: string }).id, " legacy ", "the stored id is not trimmed");
  // A new id the operator invents is trimmed.
  const fresh = changeFromExceptionDraft({ ...draftForNewException("parent", "m", "high"), id: " fresh ", cwd: ["/srv/a"] });
  assert.equal((fresh as { id: string }).id, "fresh");
});

test("a role-less exception is described as covering the requests that cite no role", () => {
  // d3: the bridge matches roles by exact equality, so a role-less exception covers the role-less
  // requests the schema allows; calling it inert understated a live authorization.
  const exception = { id: "legacy", model: "m", reasoningEffort: "high", cwd: ["/srv/a"] };
  const preview = previewChange(reading({ exceptions: [exception] }), { kind: "removeException", id: "legacy" });
  const item = preview.items.find((entry) => entry.label.includes("legacy"));
  assert.ok(item?.before.includes("cite no role"), "the row says which requests it covers");
  assert.ok(!item?.before.includes("covers no request"));
  assert.ok(preview.fallback?.includes("cite no role"));
  assert.ok(!preview.fallback?.includes("role default"));
});

// The five defects the fifth pre-merge evaluation found.

test("a supervisor-scoped exception keeps a matching option in its editor", () => {
  // d4: the role options came from the pair-editable roles, which excludes the supervisor, so a valid
  // supervisor-scoped exception's select had no option for the role its draft still carried.
  const options = exceptionRoleOptions("supervisor");
  assert.ok(options.includes("supervisor"), "the stored role is offered");
  assert.deepEqual(options, ["supervisor", "parent", "child"]);
  // A role the policy does not know is still offered, so a stored value is never dropped.
  assert.equal(exceptionRoleOptions("custom-role")[0], "custom-role");
  // The list is stable and complete for a new exception.
  assert.deepEqual(exceptionRoleOptions(""), ["supervisor", "parent", "child"]);
});

test("a draft changed while a save is in flight is not dropped by that save's answer", () => {
  // d2: the spent draft was identified by model/id, so a NEWER edit to the same model lost its text
  // and a reopened exception editor was closed. The spent draft is the one that has not moved since
  // the save started.
  let state = initialScreen();
  state = screenLoaded(state, reading());
  state = screenAllowedDraft(state, "anthropic/opus", ["max"]);
  const saved = state.change;
  state = screenSaveStarted(state);
  // A newer edit to the SAME model while the write is in flight.
  state = screenAllowedDraft(state, "anthropic/opus", ["max", "xhigh"]);
  const finished = screenSaveFinished(state, saved, noticeForWrite(200, { stored: { digest: "b".repeat(64) }, registered: { digest: "b".repeat(64) }, applied: "applied", actions: [] }));
  assert.deepEqual(finished.allowedDraft["anthropic/opus"], ["max", "xhigh"], "the newer entries are kept");
  assert.deepEqual((finished.change as { efforts: string[] }).efforts, ["max", "xhigh"], "and matches the pending change");
  // An unchanged draft for the saved model IS the spent one and goes.
  let other = screenAllowedDraft(screenLoaded(initialScreen(), reading()), "anthropic/opus", ["max"]);
  const otherSaved = other.change;
  other = screenSaveFinished(screenSaveStarted(other), otherSaved, noticeForWrite(200, { stored: { digest: "c".repeat(64) }, registered: { digest: "c".repeat(64) }, applied: "applied", actions: [] }));
  assert.equal(other.allowedDraft["anthropic/opus"], undefined, "the spent draft is dropped");
});

test("the repair sentence stays on screen after an explicit re-read", () => {
  // d5: the re-read cleared the notice that carried the server's recovery instruction while the block
  // it explained stayed in force, leaving the operator blocked with no explanation.
  let state = initialScreen();
  state = screenLoaded(state, reading({ digest: "1".repeat(64), registeredDigest: "2".repeat(64) }));
  const recovery = noticeForWrite(500, { error: "recovery_needed", fileDigest: "1".repeat(64), registeredDigest: "2".repeat(64), backup: "/host/execution-policy.json.backup", recovery: "restore the backup, then re-register" });
  state = screenSaveFinished(state, null, recovery);
  const sentence = state.repair;
  assert.ok(sentence?.includes("restore the backup"), "the server's sentence is kept");
  state = screenReread(state);
  assert.equal(state.notice, null, "the notice is gone");
  assert.equal(state.repair, sentence, "but the repair sentence is not");
  assert.equal(screenEditable(state), false, "and editing is still blocked");
  // Only a repaired reading clears both.
  state = screenLoaded(state, reading({ digest: "3".repeat(64), registeredDigest: "3".repeat(64) }));
  assert.equal(state.repair, null);
  assert.equal(screenEditable(state), true);
});

// The three defects the sixth pre-merge evaluation found.

test("an allowlist entry is edited as its own entry, so a comma inside it is not a separator", () => {
  // d1: a comma-separated field split one stored approval into two. The Go policy parser does not
  // forbid a comma inside an effort name and compares the value exactly, so "low,high" is ONE
  // approved effort, not two, and splitting it would approve two others and drop the original.
  let state = initialScreen();
  state = screenLoaded(state, reading({ allowed: [{ model: "m", efforts: ["low,high"] }] }));
  // The row's single entry is offered as one entry, and editing only another entry keeps it.
  assert.deepEqual(allowedEntriesOf(state, "m", ["low,high"]), ["low,high"]);
  state = screenAllowedEntryAdded(state, "m", ["low,high"], "none");
  assert.deepEqual((state.change as { efforts: string[] }).efforts, ["low,high", "none"], "the comma stays inside its entry");
  // Editing the entry's text carries it unchanged.
  state = screenAllowedEntryText(state, "m", ["low,high"], 0, "low,high,ultra");
  assert.deepEqual((state.change as { efforts: string[] }).efforts, ["low,high,ultra", "none"]);
  // Removing an entry leaves the others byte for byte.
  state = screenAllowedEntryRemoved(state, "m", ["low,high"], 1);
  assert.deepEqual((state.change as { efforts: string[] }).efforts, ["low,high,ultra"]);
});

test("editing an existing exception keeps its model byte for byte", () => {
  // d2: the model was trimmed, so editing only the effort sent a different model identifier.
  const exception = { id: "legacy", role: "parent", model: " model-a ", reasoningEffort: "high", cwd: ["/srv/a"] };
  const draft = draftForException(exception);
  const change = changeFromExceptionDraft({ ...draft, effort: "max" });
  assert.equal((change as { model: string }).model, " model-a ", "the stored model is not trimmed");
  // A model the operator picks for a NEW exception is trimmed, because they are typing it.
  const fresh = changeFromExceptionDraft({ ...draftForNewException("parent", " model-b ", "high"), id: "fresh", cwd: ["/srv/a"] });
  assert.equal((fresh as { model: string }).model, "model-b");
});

test("an edit started after an explicit re-read survives the read", () => {
  // d3: the manual re-read pinned keep=false when it started, so a late response wiped an edit the
  // operator began after clicking it. The read now keeps whatever is pending when it resolves; the
  // drafts that existed when it started were already cleared by screenReread.
  let state = initialScreen();
  state = screenLoaded(state, reading());
  state = screenAllowedDraft(state, "anthropic/opus", ["max"]);
  const beforeReread = screenReread(state);
  assert.equal(beforeReread.change, null, "the drafts that existed at the click are dropped");
  // The operator starts a new edit while the read is in flight.
  const during = screenAllowedDraft(beforeReread, "gpt-6.1-sol", ["high"]);
  const afterRead = screenLoaded(during, reading({ digest: "b".repeat(64) }), true);
  assert.deepEqual(afterRead.allowedDraft["gpt-6.1-sol"], ["high"], "the later edit survives");
  assert.equal((afterRead.change as { model: string }).model, "gpt-6.1-sol");
});

// The four defects the third pre-merge evaluation found.

test("an exception's cwd is carried as a list, so a path containing a comma survives an edit", () => {
  // d1: joining the roots and splitting them again turned one authorized path into two authorized
  // paths. A cwd is a path, and a path may contain a comma.
  const commaPath = "/srv/a, /srv/b";
  const exception = { id: "legacy", role: "parent", model: "m", reasoningEffort: "high", cwd: [commaPath] };
  const draft = draftForException(exception);
  assert.deepEqual(draft.cwd, [commaPath], "the single path is kept as one entry");
  // Editing only the effort keeps the scope byte for byte.
  const change = changeFromExceptionDraft({ ...draft, effort: "max" });
  assert.deepEqual((change as { cwd: string[] }).cwd, [commaPath]);
  // And the preview shows the two different scopes as different text.
  const readingWith = reading({ exceptions: [exception] });
  const before = previewChange(readingWith, { kind: "setException", id: "legacy", role: "parent", model: "m", effort: "max", cwd: [commaPath] });
  const after = previewChange(readingWith, { kind: "setException", id: "legacy", role: "parent", model: "m", effort: "max", cwd: ["/srv/a", "/srv/b"] });
  assert.notEqual(before.items[0].after, after.items[0].after, "one path and two paths do not read the same");
});

test("cancelling an allowlist edit clears its draft so a later re-read shows the file's value", () => {
  // d2: Cancel cleared the change but left the raw text, and the successful-save re-read kept it.
  let state = initialScreen();
  state = screenLoaded(state, reading());
  state = screenAllowedDraft(state, "anthropic/opus", ["low"]);
  assert.equal(allowedEntriesOf(state, "anthropic/opus", ["max", "xhigh"]).join(", "), "low");
  // Cancel clears the pending change AND the draft text.
  state = screenPropose(state, null);
  assert.equal(allowedEntriesOf(state, "anthropic/opus", ["max", "xhigh"]).join(", "), "max, xhigh");
  // A later re-read of the file shows the file's value, never the cancelled draft.
  state = screenAllowedDraft(state, "anthropic/opus", ["low"]);
  state = screenPropose(state, null);
  state = screenLoaded(state, reading(), false);
  assert.equal(allowedEntriesOf(state, "anthropic/opus", ["max", "xhigh"]).join(", "), "max, xhigh");
});

test("the new-exception draft starts with no cwd and requires at least one root", () => {
  // d3/d4: the new-exception editor must build the same change shape the other paths do, with the
  // cwd as a list and the effort judged per model.
  const draft = draftForNewException("parent", "m", "high");
  assert.deepEqual(draft.cwd, []);
  assert.equal(draft.cwdNew, "");
  // A draft with no root yet does not propose a change.
  assert.equal(changeFromExceptionDraft({ ...draft, id: "fresh" }), null);
  // Once a root is added the change carries it, and the id and model are trimmed.
  const complete = { ...draft, id: " fresh ", cwd: ["/srv/a"] };
  const change = changeFromExceptionDraft(complete);
  assert.equal((change as { id: string }).id, "fresh");
  assert.deepEqual((change as { cwd: string[] }).cwd, ["/srv/a"]);
});

/* ---- C4/C5: the write answer, and never a success notice on a refusal ---- */

test("a 200 answer shows stored, registered and applied as three separate facts", () => {
  const notice = noticeForWrite(200, { stored: { digest: "b".repeat(64) }, registered: { digest: "b".repeat(64) }, applied: "applied", actions: [] });
  assert.equal(notice.tone, "ok");
  assert.ok(notice.text.includes("b".repeat(64).slice(0, 12)));
  assert.equal(notice.applied, "applied");
  assert.equal(notice.registered, "b".repeat(64));
  assert.deepEqual(notice.actions, []);
});

test("a 200 answer whose registered digest is missing says the registration was not read back", () => {
  const notice = noticeForWrite(200, { stored: { digest: "c".repeat(64) }, applied: "unverifiable", actions: [] });
  assert.equal(notice.tone, "ok");
  assert.equal(notice.registered, null);
  assert.ok(notice.text.toLowerCase().includes("registered"));
});

test("a 200 that carries warnings keeps them beside the success", () => {
  // The server can warn that the directory sync failed or the record could not be read back. The
  // write still succeeded, so the tone stays ok, but dropping the warnings would report an
  // unqualified save while the server says its durability or registration is uncertain.
  const notice = noticeForWrite(200, {
    stored: { digest: "f".repeat(64) },
    registered: { digest: "f".repeat(64) },
    applied: "applied",
    actions: [],
    warnings: ["the directory could not be synced; a power loss may recover the previous file"],
  });
  assert.equal(notice.tone, "ok");
  assert.equal(notice.warnings.length, 1);
  assert.ok(notice.warnings[0].includes("power loss"));
});

test("a 200 without warnings carries none", () => {
  const notice = noticeForWrite(200, { stored: { digest: "g".repeat(64) }, registered: { digest: "g".repeat(64) }, applied: "applied", actions: [] });
  assert.deepEqual(notice.warnings, []);
});

test("needs_user_action shows the work a person must do", () => {
  const notice = noticeForWrite(200, {
    stored: { digest: "d".repeat(64) },
    registered: { digest: "d".repeat(64) },
    applied: "needs_user_action",
    actions: ["restart the relay service so it loads the new policy"],
  });
  assert.equal(notice.applied, "needs_user_action");
  assert.deepEqual(notice.actions, ["restart the relay service so it loads the new policy"]);
  assert.ok(notice.text.toLowerCase().includes("restart"));
});

test("unverifiable is its own applied value and is not reported as applied", () => {
  const notice = noticeForWrite(200, { stored: { digest: "e".repeat(64) }, registered: { digest: "e".repeat(64) }, applied: "unverifiable", actions: [] });
  assert.equal(notice.applied, "unverifiable");
  assert.notEqual(notice.applied, "applied");
});

test("a 409 stale digest says changed elsewhere and keeps the inputs", () => {
  const notice = noticeForWrite(409, { error: "stale_digest", currentDigest: "f".repeat(64) });
  assert.equal(notice.tone, "err");
  assert.ok(notice.text.toLowerCase().includes("changed elsewhere"));
  assert.equal(notice.keepInputs, true);
  assert.equal(notice.reread, true);
});

test("a 409 that could not start the write names its own reason and is not a conflict of digests", () => {
  for (const [kind, reason] of [["busy", "another writer holds the lock"], ["not_registered", "no record"], ["unreadable", "the file is not an object"], ["symlinked", "the path is a symbolic link"]] as const) {
    const notice = noticeForWrite(409, { error: kind, reason });
    assert.equal(notice.tone, "err");
    assert.ok(notice.text.includes(reason), `${kind} keeps the server's reason`);
    assert.equal(notice.reread, false);
  }
});

test("a 422 shows every check message and never a success notice", () => {
  const notice = noticeForWrite(422, { error: "invalid_policy", errors: ["role 'supervisor' cannot declare a model", "the change moved allowed"] });
  assert.equal(notice.tone, "err");
  assert.equal(notice.errors.length, 2);
  assert.ok(notice.text.includes("supervisor"));
});

test("a 502 reports the registration failure and whether the file was put back", () => {
  const restored = noticeForWrite(502, { error: "register_failed", restored: true });
  assert.equal(restored.tone, "err");
  assert.ok(restored.text.toLowerCase().includes("registration"));
  assert.equal(restored.restored, true);
  const notRestored = noticeForWrite(502, { error: "register_failed", restored: false });
  assert.equal(notRestored.restored, false);
});

test("a recovery_needed answer shows the recovery sentence and blocks editing", () => {
  const notice = noticeForWrite(500, {
    error: "recovery_needed",
    fileDigest: "1".repeat(64),
    registeredDigest: "2".repeat(64),
    backup: "/host/execution-policy.json.backup-2026",
    recovery: "run crw install register-mcp --re-execution-policy to reconcile the record",
  });
  assert.equal(notice.tone, "err");
  assert.equal(notice.blockEditing, true);
  assert.ok(notice.text.includes("reconcile the record"));
  assert.ok(notice.text.includes("1".repeat(12)));
});

test("the cancelled and failed answers are errors with their own detail", () => {
  const cancelled = noticeForWrite(500, { error: "cancelled", step: "backup", backup: "/host/b" });
  assert.equal(cancelled.tone, "err");
  assert.ok(cancelled.text.includes("backup"));
  const failed = noticeForWrite(500, { error: "failed", reason: "the execution policy could not be written" });
  assert.equal(failed.tone, "err");
  assert.ok(failed.text.includes("could not be written"));
});

test("no refusal ever produces a success tone", () => {
  const refusals: Array<[number, unknown]> = [
    [409, { error: "stale_digest", currentDigest: "f".repeat(64) }],
    [409, { error: "busy", reason: "lock" }],
    [422, { error: "invalid_policy", errors: ["x"] }],
    [502, { error: "register_failed", restored: true }],
    [500, { error: "recovery_needed", recovery: "r" }],
    [500, { error: "cancelled", step: "publish" }],
    [500, { error: "failed", reason: "r" }],
  ];
  for (const [status, body] of refusals) {
    assert.notEqual(noticeForWrite(status, body).tone, "ok", `${status} must not read as success`);
  }
});

/* ---- C6: the same values and sources after a refresh ---- */

test("deriving the view twice from one reading gives the same values and sources", () => {
  const first = policyView(decodePolicy(reading()));
  const second = policyView(decodePolicy(reading()));
  assert.deepEqual(first, second);
});

// C6's other half. node:test cannot load the .tsx, so the label a control carries is built by a
// pure function here and the screen renders every control's aria-label from it: a control cannot be
// added without a label, and the label text is pinned where a test can read it.
test("every control on the screen has a label, and the screen uses native controls only", () => {
  assert.equal(roleControlsLabel("parent"), "parent pair controls");
  assert.equal(pairModelLabel("child", 0), "child pair 1 model");
  assert.equal(pairEffortLabel("child", 0), "child pair 1 effort");
  assert.equal(allowedEffortsLabel("anthropic/opus", 0), "anthropic/opus allowed effort 1");
  assert.equal(removeExceptionLabel("legacy"), "Remove exception legacy");
  for (const label of [roleControlsLabel("parent"), pairModelLabel("parent", 1), pairEffortLabel("parent", 1), allowedEffortsLabel("m", 0), removeExceptionLabel("x")]) {
    assert.ok(label.length > 0, "a control label is never empty");
  }
  // The screen composes only native, focusable elements: the browser gives them keyboard operability
  // and a tab order, and the screen adds no custom widget and no key handler of its own.
  assert.deepEqual([...POLICY_CONTROL_ELEMENTS], ["select", "input", "button", "fieldset"]);
});
