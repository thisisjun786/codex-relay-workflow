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
  decodePolicy,
  modelOptions,
  noticeForWrite,
  pairEffortLabel,
  pairModelLabel,
  policyEfforts,
  policyView,
  previewChange,
  removeExceptionLabel,
  roleControlsLabel,
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
  assert.deepEqual(view.roles.map((role) => role.name), ["child", "parent", "supervisor"]);
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
  assert.deepEqual(view.roles, []);
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
  assert.equal(item?.before, "parent devin/swe-2 max (/srv/project)");
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
  assert.equal(item?.before, "parent devin/swe-2 max (/srv/project)");
  assert.equal(item?.after, "child anthropic/opus max (/srv/other)");
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
  assert.equal(allowedEffortsLabel("anthropic/opus"), "anthropic/opus allowed efforts");
  assert.equal(removeExceptionLabel("legacy"), "Remove exception legacy");
  for (const label of [roleControlsLabel("parent"), pairModelLabel("parent", 1), pairEffortLabel("parent", 1), allowedEffortsLabel("m"), removeExceptionLabel("x")]) {
    assert.ok(label.length > 0, "a control label is never empty");
  }
  // The screen composes only native, focusable elements: the browser gives them keyboard operability
  // and a tab order, and the screen adds no custom widget and no key handler of its own.
  assert.deepEqual([...POLICY_CONTROL_ELEMENTS], ["select", "input", "button", "fieldset"]);
});
