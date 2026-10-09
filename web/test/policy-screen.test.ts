// Original CRW test (no CXC counterpart): the execution-policy screen rendered for real.
//
// policy-state.test.ts covers the decisions; this file covers the SCREEN, because a criterion stated
// about the screen ("the first screen shows a row for each role", "the pair select keeps the model
// you picked", "the allowed input keeps the comma you typed") is only observable by rendering the
// component and firing its own events.
//
// There is no JSX test runner here and no new dependency is allowed, so the screen is loaded the way
// vite loads it: esbuild (already a devDependency through vite) transforms the .tsx into ESM with the
// automatic JSX runtime, the module is imported through a data: URL, and react-dom/server (part of
// the react-dom the package already depends on) renders it. react-dom/server's renderToStaticMarkup
// calls the component, so the hooks it uses run and the returned tree is real React markup. A test
// then walks the returned element tree and calls each control's own onChange/onClick with a
// synthetic event, which is what the browser does.
import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdir, writeFile } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";
import * as React from "react";
import { renderToStaticMarkup } from "react-dom/server";

const here = dirname(fileURLToPath(import.meta.url));

interface ScreenModule {
  PolicyScreen: (props: {
    state: Record<string, unknown>;
    handlers: Record<string, (...args: unknown[]) => void>;
    help: { open: boolean; topic: "policy"; openHelp: () => void; closeHelp: () => void };
  }) => unknown;
}

/**
 * loadScreen bundles the screen and every module it imports into one ESM file and imports it. The
 * file is written under node_modules/.cache (gitignored) rather than handed over as a data: URL,
 * because a data: URL cannot resolve a bare specifier: the bundle's own "react" import must resolve
 * to the same react instance this test renders with, or the component's hooks would run against a
 * second copy of React and fail.
 */
async function loadScreen(): Promise<ScreenModule> {
  const build = await import("esbuild");
  const root = resolve(here, "../src");
  const out = resolve(here, "../node_modules/.cache/policy-screen.test.mjs");
  await mkdir(dirname(out), { recursive: true });
  await build.build({
    stdin: { contents: 'export { PolicyScreen } from "./pages/Policy.tsx";', resolveDir: root, loader: "tsx" },
    bundle: true,
    outfile: out,
    format: "esm",
    platform: "node",
    jsx: "automatic",
    packages: "external",
  });
  return (await import(out)) as ScreenModule;
}

/* ---- a React element walker, enough to drive the screen ---- */

interface El {
  type: unknown;
  props: Record<string, unknown>;
}

function isElement(value: unknown): value is El {
  return typeof value === "object" && value !== null && "props" in (value as Record<string, unknown>) && "type" in (value as Record<string, unknown>);
}

/**
 * walk visits every element of the tree a component returned, expanding the function components it
 * meets. React itself would do that during a render; react-dom/server renders the tree but does not
 * hand the expanded tree back, so the test expands it here. The nested components are the screen's
 * own (AllowedAdder, ExceptionRow, ExceptionAdder, the kit's Modal), so calling them with their props
 * is what the renderer does.
 */
function walk(node: unknown, visit: (el: El) => void): void {
  if (Array.isArray(node)) {
    for (const child of node) walk(child, visit);
    return;
  }
  if (!isElement(node)) return;
  visit(node);
  if (typeof node.type === "function" && !(node.type as { $$typeof?: unknown }).$$typeof) {
    // A hook-using component cannot be called outside React's renderer, and its subtree is not what
    // these tests assert, so a call that fails is skipped rather than failing the walk.
    try {
      walk((node.type as (props: Record<string, unknown>) => unknown)(node.props), visit);
    } catch {
      // The component needs React's dispatcher; the elements it was handed are already recorded.
    }
    return;
  }
  walk(node.props.children, visit);
}

/** byLabel finds the element whose aria-label is exactly label. */
function byLabel(elements: El[], label: string): El | undefined {
  return elements.find((el) => el.props["aria-label"] === label);
}

/** textOf is the rendered text of an element and its children. */
function textOf(node: unknown): string {
  if (typeof node === "string") return node;
  if (typeof node === "number") return String(node);
  if (Array.isArray(node)) return node.map(textOf).join("");
  if (!isElement(node)) return "";
  return textOf(node.props.children);
}

/** fire calls a handler prop with a synthetic event, as the DOM would. */
function fire(el: El | undefined, name: string, value?: string): void {
  assert.ok(el, `no element to fire ${name} on`);
  const handler = el?.props[name];
  assert.equal(typeof handler, "function", `${name} is not a handler`);
  (handler as (event: unknown) => void)({ target: { value }, preventDefault() {}, stopPropagation() {} });
}

/* ---- the screen, mounted from one state ---- */

const POLICY_PATH = "/host/execution-policy.json";

/** A registered reading with three roles, an allowlist and one exception. */
function readingBody(changes: Record<string, unknown> = {}): Record<string, unknown> {
  return {
    state: "registered",
    path: POLICY_PATH,
    mode: "allowlist",
    digest: "a".repeat(64),
    registeredDigest: "a".repeat(64),
    runningDigest: "a".repeat(64),
    roles: [
      { name: "child", expectation: "", pairs: [{ model: "anthropic/opus", reasoningEffort: "xhigh" }] },
      { name: "parent", expectation: "", pairs: [{ model: "gpt-6.1-sol", reasoningEffort: "xhigh" }] },
      { name: "supervisor", expectation: "record", pairs: null },
    ],
    allowed: [{ model: "anthropic/opus", efforts: ["max", "xhigh"] }],
    exceptions: [{ id: "legacy", role: "parent", model: "devin/swe-2", reasoningEffort: "max", cwd: ["/srv/project"] }],
    applied: "applied",
    actions: null,
    ...changes,
  };
}

/** A fresh catalog answer. */
function catalogBody(entries: Array<{ id: string; label: string; reasoningEfforts?: string[] | null }>): Record<string, unknown> {
  return { state: "ocx-active", status: "fresh", source: "ocx", entries };
}

/**
 * mount renders the screen from one state and returns the tree plus the state the screen's handlers
 * produced. Handlers do not mutate: the reducer in policy-state.ts is the same one the stateful page
 * uses, so calling a handler and re-rendering with its result is exactly what the page does.
 */
async function mount(state: Record<string, unknown>) {
  const { PolicyScreen } = await loadScreen();
  const pure = await import("../src/policy-state.ts");
  const applied: Array<{ name: string; args: unknown[] }> = [];
  const handlers = {
    propose: (...args: unknown[]) => applied.push({ name: "propose", args }),
    allowedEntryText: (...args: unknown[]) => applied.push({ name: "allowedEntryText", args }),
    allowedEntryAdded: (...args: unknown[]) => applied.push({ name: "allowedEntryAdded", args }),
    allowedEntryRemoved: (...args: unknown[]) => applied.push({ name: "allowedEntryRemoved", args }),
    allowedAddModel: (...args: unknown[]) => applied.push({ name: "allowedAddModel", args }),
    exceptionDraft: (...args: unknown[]) => applied.push({ name: "exceptionDraft", args }),
    removeException: (...args: unknown[]) => applied.push({ name: "removeException", args }),
    save: () => applied.push({ name: "save", args: [] }),
    reread: () => applied.push({ name: "reread", args: [] }),
  };
  const elements: El[] = [];
  const markup = renderToStaticMarkup(
    React.createElement(PolicyScreen as unknown as React.ComponentType<Record<string, unknown>>, {
      state,
      handlers,
      help: { open: false, topic: "policy", openHelp: () => {}, closeHelp: () => {} },
    }),
  );
  walk((PolicyScreen as unknown as (props: unknown) => unknown)({ state, handlers, help: { open: false, topic: "policy", openHelp: () => {}, closeHelp: () => {} } }), (el) => elements.push(el));
  return { elements, markup, applied, pure };
}

/** loadedState is the screen state after one successful read, built by the same reducer. */
function loadedState(pure: typeof import("../src/policy-state.ts"), reading: Record<string, unknown>, catalog: Record<string, unknown> | null) {
  let state = pure.initialScreen();
  state = pure.screenLoaded(state, pure.decodePolicy(reading));
  if (catalog) state = pure.screenCatalogLoaded(state, catalog as never);
  return state as unknown as Record<string, unknown>;
}

test("the screen renders a row for each of the three roles from the file", async () => {
  const { PolicyScreen } = await loadScreen();
  const pure = await import("../src/policy-state.ts");
  const state = loadedState(pure, readingBody(), catalogBody([]));
  const { elements } = await mount(state);
  const labels = elements.map((el) => el.props["aria-label"]).filter((label): label is string => typeof label === "string");
  assert.ok(labels.includes("supervisor policy"), "the supervisor row is rendered");
  assert.ok(labels.includes("parent policy"), "the parent row is rendered");
  assert.ok(labels.includes("child policy"), "the child row is rendered");
  const supervisor = elements.find((el) => el.props["aria-label"] === "supervisor policy");
  assert.ok(textOf(supervisor?.props.children).includes("Selected by Jun; not managed here"));
  assert.equal(byLabel(elements, "supervisor pair controls"), undefined, "the supervisor is not editable");
  assert.ok(byLabel(elements, "parent pair controls"), "the parent row is editable");
  void PolicyScreen;
});

test("a role the file does not declare still gets a row and can be given a pair", async () => {
  const pure = await import("../src/policy-state.ts");
  // internal/policystore/check_test.go's oneAllowed declares only the child. The issue's first
  // screen is three rows, so the parent and supervisor rows must still appear.
  const only = readingBody({ roles: [{ name: "child", expectation: "", pairs: [{ model: "m", reasoningEffort: "high" }] }], allowed: [{ model: "m", efforts: ["high"] }], exceptions: [] });
  const state = loadedState(pure, only, catalogBody([]));
  const { elements } = await mount(state);
  const labels = elements.map((el) => el.props["aria-label"]).filter((label): label is string => typeof label === "string");
  assert.ok(labels.includes("parent policy"), "the undeclared parent still gets a row");
  assert.ok(labels.includes("supervisor policy"));
  assert.ok(byLabel(elements, "parent pair controls"), "the undeclared parent row is editable");
  const parent = elements.find((el) => el.props["aria-label"] === "parent policy");
  assert.ok(textOf(parent?.props.children).includes("not declared in this file"));
});

test("the pair select keeps the model the operator picked, and a following effort change keeps it too", async () => {
  const pure = await import("../src/policy-state.ts");
  const state = loadedState(pure, readingBody(), catalogBody([{ id: "anthropic/opus", label: "Opus" }, { id: "gpt-6.1-sol", label: "Sol" }]));
  const first = await mount(state);
  // Pick a different model on the parent row, as the operator would.
  fire(byLabel(first.elements, "parent pair 1 model"), "onChange", "anthropic/opus");
  const proposed = first.applied.find((call) => call.name === "propose");
  assert.ok(proposed, "the model choice proposes a change");
  const next = pure.screenPropose(state as never, proposed?.args[0] as never) as unknown as Record<string, unknown>;
  const second = await mount(next);
  // The select shows the picked model rather than snapping back to the saved one.
  assert.equal(byLabel(second.elements, "parent pair 1 model")?.props.value, "anthropic/opus");
  // And an effort change on the same row keeps the model: the effort edit rebuilds from the pending
  // pairs, not from the saved reading.
  fire(byLabel(second.elements, "parent pair 1 effort"), "onChange", "max");
  const effortChange = second.applied.find((call) => call.name === "propose");
  const afterEffort = pure.screenPropose(state as never, effortChange?.args[0] as never) as unknown as Record<string, unknown>;
  const third = await mount(afterEffort);
  assert.equal(byLabel(third.elements, "parent pair 1 model")?.props.value, "anthropic/opus");
  assert.equal(byLabel(third.elements, "parent pair 1 effort")?.props.value, "max");
});

test("the allowed input keeps the comma the operator is typing", async () => {
  const pure = await import("../src/policy-state.ts");
  const state = loadedState(pure, readingBody(), catalogBody([{ id: "anthropic/opus", label: "Opus" }]));
  const first = await mount(state);
  // The first keystrokes of "max, high": a split/join on every keystroke would eat the trailing comma
  // and make the second entry impossible to type.
  fire(byLabel(first.elements, "anthropic/opus allowed effort 1"), "onChange", "max, ");
  const call = first.applied.find((c) => c.name === "allowedEntryText");
  const next = pure.screenAllowedEntryText(state as never, call?.args[0] as never, call?.args[1] as never, call?.args[2] as never, call?.args[3] as never) as unknown as Record<string, unknown>;
  const second = await mount(next);
  assert.equal(byLabel(second.elements, "anthropic/opus allowed effort 1")?.props.value, "max, ");
});

test("the allowed input an operator cleared shows what they typed", async () => {
  const pure = await import("../src/policy-state.ts");
  const state = loadedState(pure, readingBody(), catalogBody([{ id: "anthropic/opus", label: "Opus" }]));
  const first = await mount(state);
  fire(byLabel(first.elements, "anthropic/opus allowed effort 1"), "onChange", "");
  const call = first.applied.find((c) => c.name === "allowedEntryText");
  const next = pure.screenAllowedEntryText(state as never, call?.args[0] as never, call?.args[1] as never, call?.args[2] as never, call?.args[3] as never) as unknown as Record<string, unknown>;
  const second = await mount(next);
  assert.equal(byLabel(second.elements, "anthropic/opus allowed effort 1")?.props.value, "");
});

test("an existing exception can be edited, not only removed", async () => {
  const pure = await import("../src/policy-state.ts");
  const state = loadedState(pure, readingBody(), catalogBody([{ id: "anthropic/opus", label: "Opus" }, { id: "gpt-6.1-sol", label: "Sol" }]));
  const first = await mount(state);
  const edit = byLabel(first.elements, "Edit exception legacy");
  assert.ok(edit, "an existing exception offers an edit control");
  fire(edit, "onClick");
  const call = first.applied.find((c) => c.name === "exceptionDraft");
  const next = pure.screenExceptionDraft(state as never, call?.args[0] as never) as unknown as Record<string, unknown>;
  const second = await mount(next);
  // The editor is open with the exception's own values, and its role select carries the role the
  // server would otherwise keep silently.
  assert.ok(byLabel(second.elements, "legacy exception role"), "the role select is rendered");
  assert.equal(byLabel(second.elements, "legacy exception role")?.props.value, "parent");
  assert.equal(byLabel(second.elements, "legacy exception model")?.props.value, "devin/swe-2");
  // A saved model the catalog does not list reads as unavailable in this editor too.
  assert.ok(byLabel(second.elements, "legacy exception model"));
});

test("a new exception can be added, and its role is never silently empty", async () => {
  const pure = await import("../src/policy-state.ts");
  const state = loadedState(pure, readingBody({ exceptions: [] }), catalogBody([{ id: "anthropic/opus", label: "Opus" }, { id: "gpt-6.1-sol", label: "Sol" }]));
  const first = await mount(state);
  const add = first.elements.find((el) => el.type === "button" && textOf(el.props.children) === "Add exception");
  assert.ok(add, "the screen offers adding an exception");
  fire(add, "onClick");
  const call = first.applied.find((c) => c.name === "exceptionDraft");
  assert.ok(call, "adding an exception opens the editor");
  const draft = call?.args[0] as { role: string; isNew: boolean; id: string };
  // The new exception starts on a real role, because an empty one would be covered by no request.
  assert.notEqual(draft.role, "");
  assert.equal(draft.isNew, true);
  // Typing an id keeps the editor open: the open condition is the isNew flag, not an empty id.
  const typing = pure.screenExceptionDraft(state as never, { ...draft, id: "f" } as never) as unknown as Record<string, unknown>;
  const second = await mount(typing);
  const idInput = byLabel(second.elements, "new exception id");
  assert.ok(idInput, "the new-exception editor is still rendered after a character is typed");
  assert.equal(idInput?.props.value, "f");
});

test("the exception editor judges an effort against the exception's own model", async () => {
  const pure = await import("../src/policy-state.ts");
  // The catalog advertises max for the exception's model and high for another one, so high is not
  // offered for this model even though the union of every model's efforts contains it.
  const fresh = catalogBody([{ id: "devin/swe-2", label: "SWE", reasoningEfforts: ["max"] }, { id: "gpt-6.1-sol", label: "Sol", reasoningEfforts: ["high"] }]);
  const state = loadedState(pure, readingBody(), fresh);
  const first = await mount(state);
  fire(byLabel(first.elements, "Edit exception legacy"), "onClick");
  const call = first.applied.find((c) => c.name === "exceptionDraft");
  const opened = pure.screenExceptionDraft(state as never, call?.args[0] as never) as unknown as Record<string, unknown>;
  const second = await mount(opened);
  const effort = byLabel(second.elements, "legacy exception effort");
  assert.ok(effort, "the exception's effort select is rendered");
  // The saved effort is max, which this model does advertise, so it is not marked unavailable.
  assert.equal(effort?.props.value, "max");
});

test("a stale check keeps the inputs and asks for a re-read", async () => {
  const pure = await import("../src/policy-state.ts");
  // The check route answers stale AND invalid at once when the file moved and the change no longer
  // applies. The conflict path must win: the operator has not seen the current file.
  const notice = pure.checkNotice(pure.decodeCheck({ valid: false, errors: ["exception \"legacy\" is not declared in this file"], currentDigest: "b".repeat(64), stale: true, diff: [] }));
  assert.equal(notice.reread, true);
  assert.equal(notice.keepInputs, true);
  assert.ok(notice.text.toLowerCase().includes("changed elsewhere"));
  // And the reducer keeps the pending change across the re-read it triggers.
  let state = pure.initialScreen();
  state = pure.screenLoaded(state, pure.decodePolicy(readingBody()));
  state = pure.screenAllowedDraft(state, "anthropic/opus", ["max", "high"]);
  const pending = state.change;
  const kept = pure.screenLoaded(state, pure.decodePolicy(readingBody({ digest: "b".repeat(64) })), true);
  assert.deepEqual(kept.change, pending, "the pending change survives the stale re-read");
  assert.deepEqual(kept.allowedDraft.get("anthropic/opus"), ["max", "high"], "the typed text survives too");
});

test("the preview the screen renders for a removal under presence_only promises no allowlist", async () => {
  const pure = await import("../src/policy-state.ts");
  // d1 observed on the screen itself: the file declares roles but no allowed list, so the bridge runs
  // no allowlist check (internal/bridge/execution/execution.go Authorize guards it with p.allowed != nil).
  // The rendered preview must not tell the operator the scope returns to a list this policy lacks.
  const presenceOnly = readingBody({
    mode: "presence_only",
    allowed: [],
    roles: [
      { name: "child", expectation: "pair", pairs: [{ model: "m", reasoningEffort: "high" }] },
      { name: "supervisor", expectation: "record", pairs: [] },
    ],
    exceptions: [{ id: "legacy", role: "supervisor", model: "m", reasoningEffort: "max", cwd: ["/srv/a"] }],
  });
  let state = loadedState(pure, presenceOnly, catalogBody([]));
  state = pure.screenPropose(state as never, { kind: "removeException", id: "legacy" } as never) as unknown as Record<string, unknown>;
  const { elements, markup } = await mount(state);
  assert.ok(markup.includes("Preview before saving"), "the preview is on screen before saving");
  // The assertion is scoped to the preview's own sentence: the screen renders other text that
  // legitimately says "allowed list" (the section heading and the empty-list row), so only the
  // removal sentence can be judged here.
  const sentence = elements
    .filter((el) => el.props["role"] === "status")
    .map((el) => textOf(el.props.children))
    .find((text) => text.includes("Removing this exception"));
  assert.ok(sentence, "the removal sentence is on screen");
  assert.ok(sentence?.includes("refused as unknown"), "the sentence names the stale-id refusal");
  assert.ok(!sentence?.includes("allowed list"), "and never promises an allowlist check that does not exist");
  assert.ok(sentence?.includes("declares no allowlist"), "it says the policy has no allowlist");
  // The same change against an allowlist policy still names the allowed list, so the sentence follows
  // the policy and not the code path.
  const withList = readingBody({ exceptions: [{ id: "legacy", role: "supervisor", model: "m", reasoningEffort: "max", cwd: ["/srv/a"] }] });
  let listed = loadedState(pure, withList, catalogBody([]));
  listed = pure.screenPropose(listed as never, { kind: "removeException", id: "legacy" } as never) as unknown as Record<string, unknown>;
  const second = await mount(listed);
  const listedSentence = second.elements
    .filter((el) => el.props["role"] === "status")
    .map((el) => textOf(el.props.children))
    .find((text) => text.includes("Removing this exception"));
  assert.ok(listedSentence?.includes("allowed list"), "an allowlist policy still names it");
  assert.ok(!listedSentence?.includes("declares no allowlist"), "and does not claim there is none");
});

test("the screen shows the applied state and the server's action, never a guessed running digest", async () => {
  const pure = await import("../src/policy-state.ts");
  // d2 observed on the screen: needs_user_action covers both a service that has not loaded the bytes
  // and a file/record disagreement (internal/policystore/running.go Applied decides the record
  // mismatch first). The rendered notice repeats the server's action and asserts nothing about the
  // running relay.
  let state = pure.initialScreen();
  state = pure.screenLoaded(state, pure.decodePolicy(readingBody()));
  const notice = pure.noticeForWrite(200, {
    stored: { digest: "b".repeat(64) },
    registered: { digest: "c".repeat(64) },
    applied: "needs_user_action",
    actions: ["re-register the execution policy with crw install register-mcp --re-register-policy --execution-policy <file>"],
  });
  const finished = pure.screenSaveFinished(pure.screenSaveStarted(state), state.change, notice) as unknown as Record<string, unknown>;
  const { markup } = await mount(finished);
  assert.ok(markup.includes("needs_user_action"), "the applied value is shown as its own fact");
  assert.ok(markup.includes("re-register"), "the server's own action is repeated");
  assert.ok(!markup.includes("still holds the old bytes"), "the running state is never asserted");
});

test("the screen shows the warning a not_applied answer carries", async () => {
  // CRW-1001: the exchange undo could not be synced, so a power loss may bring the candidate back; the
  // 409 not_applied body carries that warning and the rendered notice must show it.
  const pure = await import("../src/policy-state.ts");
  let state = pure.initialScreen();
  state = pure.screenLoaded(state, pure.decodePolicy(readingBody()));
  const warning = "the undo of this write's exchange could not be synced, so a host that loses power now may find the candidate at the policy path";
  const notice = pure.noticeForWrite(409, { error: "not_applied", reason: "x", currentDigest: "c".repeat(64), fileDigest: "c".repeat(64), registeredDigest: "c".repeat(64), warnings: [warning] });
  const finished = pure.screenSaveFinished(pure.screenSaveStarted(state), state.change, notice) as unknown as Record<string, unknown>;
  const { markup } = await mount(finished);
  assert.ok(markup.includes("Not saved"));
  assert.ok(markup.includes("a host that loses power now may find the candidate"), "the durability warning is shown");
});

test("the screen headlines a lost write Result unknown, never Not saved", async () => {
  // d1 observed on the rendered screen: a lost response must not be headed as a refused save.
  const pure = await import("../src/policy-state.ts");
  let state = pure.initialScreen();
  state = pure.screenLoaded(state, pure.decodePolicy(readingBody()));
  state = pure.screenAllowedDraft(state, "anthropic/opus", ["max"]);
  const out = await pure.runSave(state as never, {
    check: async () => ({ status: 200, body: { valid: true, errors: [], currentDigest: "a".repeat(64), stale: false, diff: [] } }),
    write: async () => { throw new Error("connection lost"); },
  });
  const { markup } = await mount(out.state as unknown as Record<string, unknown>);
  assert.ok(markup.includes("Result unknown"), "the lost write is headed Result unknown");
  assert.ok(!markup.includes("Not saved"), "it is never headed Not saved");
});

// CRW-994 d1 on the rendered screen: the headline and the sentence under it follow the comparison of
// the proposed change with the file, not the digest.
test("the screen heads a lost write by what the file holds: Not saved for another write's digest, Result unknown while the registration runs, Saved when the record names the change", async () => {
  const pure = await import("../src/policy-state.ts");
  let state = pure.initialScreen();
  state = pure.screenLoaded(state, pure.decodePolicy(readingBody()));
  state = pure.screenAllowedDraft(state, "anthropic/opus", ["max"]);
  const out = await pure.runSave(state as never, {
    check: async () => ({ status: 200, body: { valid: true, errors: [], currentDigest: "a".repeat(64), stale: false, diff: [] } }),
    write: async () => { throw new Error("connection lost"); },
  });
  const heldBy = (changes: Record<string, unknown>) => pure.screenLoaded(out.state, pure.decodePolicy({ ...readingBody(), ...changes }), true) as unknown as Record<string, unknown>;
  const other = await mount(heldBy({ digest: "b".repeat(64), registeredDigest: "b".repeat(64), allowed: [{ model: "anthropic/opus", efforts: ["xhigh"] }] }));
  assert.ok(other.markup.includes("Not saved"), "another write's digest is not this change");
  assert.ok(other.markup.includes("does not hold this change"));
  assert.ok(!other.markup.includes("Result unknown") && !other.markup.includes(">Saved<"));
  const early = await mount(heldBy({ digest: "b".repeat(64), registeredDigest: "a".repeat(64), allowed: [{ model: "anthropic/opus", efforts: ["max"] }] }));
  assert.ok(early.markup.includes("Result unknown"), "the file holds the change before the record names it");
  assert.ok(early.markup.includes("registration has not finished"));
  const done = await mount(heldBy({ digest: "b".repeat(64), registeredDigest: "b".repeat(64), allowed: [{ model: "anthropic/opus", efforts: ["max"] }] }));
  assert.ok(done.markup.includes("Saved"), "the record names the file that holds the change");
  assert.ok(!done.markup.includes("Not saved") && !done.markup.includes("Result unknown"));
});

// CRW-1001 d2 / CRW-876 d1 (pre-merge evaluation of 711ab36e) on the rendered screen: a file that moved
// to bytes without the change, under a record that names something else, is not a refusal yet.
test("the screen keeps a lost write Result unknown while a moved file's record names another digest", async () => {
  const pure = await import("../src/policy-state.ts");
  let state = pure.initialScreen();
  state = pure.screenLoaded(state, pure.decodePolicy(readingBody()));
  state = pure.screenAllowedDraft(state, "anthropic/opus", ["max"]);
  const out = await pure.runSave(state as never, {
    check: async () => ({ status: 200, body: { valid: true, errors: [], currentDigest: "a".repeat(64), stale: false, diff: [] } }),
    write: async () => { throw new Error("connection lost"); },
  });
  const provisional = pure.screenLoaded(out.state, pure.decodePolicy({ ...readingBody(), digest: "c".repeat(64), registeredDigest: "a".repeat(64), allowed: [{ model: "anthropic/opus", efforts: ["xhigh"] }] }), true);
  const shown = await mount(provisional as unknown as Record<string, unknown>);
  assert.ok(shown.markup.includes("Result unknown"));
  assert.ok(shown.markup.includes("may still be running"));
  assert.ok(!shown.markup.includes("Not saved"));
  assert.equal(pure.lostRecheckDelay(provisional), pure.LOST_RECHECK_MS);
});

// CRW-994 (verification round 2) on the rendered screen: a first re-read that still finds the starting
// digest is not a refusal. The request may publish after it, so the headline stays Result unknown, the
// page keeps reading on its own, and it follows the late write to Saved.
test("the screen keeps a lost write Result unknown when the first re-read finds the starting digest, then follows a late write to Saved", async () => {
  const pure = await import("../src/policy-state.ts");
  let state = pure.initialScreen();
  state = pure.screenLoaded(state, pure.decodePolicy(readingBody()));
  state = pure.screenAllowedDraft(state, "anthropic/opus", ["max"]);
  const out = await pure.runSave(state as never, {
    check: async () => ({ status: 200, body: { valid: true, errors: [], currentDigest: "a".repeat(64), stale: false, diff: [] } }),
    write: async () => { throw new Error("connection lost"); },
  });
  const first = pure.screenLoaded(out.state, pure.decodePolicy(readingBody()), true);
  const unchanged = await mount(first as unknown as Record<string, unknown>);
  assert.ok(unchanged.markup.includes("Result unknown"), "one reading at the starting digest settles nothing");
  assert.ok(!unchanged.markup.includes("Not saved"));
  assert.equal(pure.lostRecheckDelay(first), pure.LOST_RECHECK_MS, "the page reads again on its own");
  const late = pure.screenLoaded(first, pure.decodePolicy({ ...readingBody(), digest: "b".repeat(64), registeredDigest: "b".repeat(64), allowed: [{ model: "anthropic/opus", efforts: ["max"] }] }), true);
  const saved = await mount(late as unknown as Record<string, unknown>);
  assert.ok(saved.markup.includes("Saved"));
  assert.ok(!saved.markup.includes("Not saved") && !saved.markup.includes("Result unknown"));
  assert.equal(pure.lostRecheckDelay(late), null);
});

test("a save in flight disables the screen's other edit controls", async () => {
  const pure = await import("../src/policy-state.ts");
  let state = pure.initialScreen();
  state = pure.screenLoaded(state, pure.decodePolicy(readingBody()));
  state = pure.screenAllowedDraft(state, "anthropic/opus", ["max"]);
  // The started state comes from runSave's own callback, which is exactly what PolicyPage applies, so
  // this renders the state the page is really in while the check and the write are in flight.
  let saving: Record<string, unknown> | null = null;
  const pending = pure.runSave(state as never, {
    check: async () => ({ status: 200, body: { valid: true, errors: [], currentDigest: "a".repeat(64), stale: false, diff: [] } }),
    write: async () => ({ status: 200, body: { stored: { digest: "b".repeat(64) }, applied: "applied", actions: [] } }),
  }, (started) => { saving = started as unknown as Record<string, unknown>; });
  assert.ok(saving, "runSave reports the started state synchronously");
  // While the save is in flight the screen disables every edit control, so no second edit can race
  // the answer (decided answer 4: one pending change at a time).
  const { elements } = await mount(saving as unknown as Record<string, unknown>);
  const allowedControls = elements.find((el) => el.props["aria-label"] === "allowed anthropic/opus controls");
  assert.ok(allowedControls, "the allowed row renders its controls");
  assert.equal(allowedControls?.props.disabled, true, "the allowed controls are disabled during a save");
  const parentControls = byLabel(elements, "parent pair controls");
  assert.equal(parentControls?.props.disabled, true, "the role controls are disabled during a save");
  const saveButton = elements.find((el) => el.type === "button" && textOf(el.props.children) === "Saving...");
  assert.ok(saveButton, "the save control reads Saving... while the write is in flight");
  assert.equal(saveButton?.props.disabled, true);
  await pending;
});

test("one live edit disables the other rows on the screen", async () => {
  const pure = await import("../src/policy-state.ts");
  // Decided answer 4, observed through the screen: with a pending change for one allowed model, the
  // other allowed row and the add control are disabled, so the single change the API applies cannot
  // be silently replaced by a second edit.
  const twoRows = readingBody({ allowed: [{ model: "anthropic/opus", efforts: ["max"] }, { model: "gpt-6.1-sol", efforts: ["high"] }] });
  const state = loadedState(pure, twoRows, catalogBody([{ id: "anthropic/opus", label: "Opus" }, { id: "gpt-6.1-sol", label: "Sol" }, { id: "fresh/model", label: "Fresh" }]));
  const owned = pure.screenAllowedDraft(state as never, "anthropic/opus", ["max", "xhigh"]) as unknown as Record<string, unknown>;
  const { elements } = await mount(owned);
  assert.equal(elements.find((el) => el.props["aria-label"] === "allowed anthropic/opus controls")?.props.disabled, false, "the owning row stays live");
  assert.equal(elements.find((el) => el.props["aria-label"] === "allowed gpt-6.1-sol controls")?.props.disabled, true, "the other allowed row is disabled");
  assert.equal(elements.find((el) => el.props["aria-label"] === "model to allow")?.props.disabled, true, "the add control is disabled");
  assert.equal(elements.find((el) => el.props["aria-label"] === "Add allowed model")?.props.disabled, true);
  // The exception rows are disabled too, since an exception is a different change.
  assert.equal(elements.find((el) => el.props["aria-label"] === "Edit exception legacy")?.props.disabled, true);
  // With nothing pending every control is live again.
  const free = await mount(loadedState(pure, twoRows, catalogBody([{ id: "anthropic/opus", label: "Opus" }, { id: "gpt-6.1-sol", label: "Sol" }, { id: "fresh/model", label: "Fresh" }])) as unknown as Record<string, unknown>);
  assert.equal(free.elements.find((el) => el.props["aria-label"] === "allowed gpt-6.1-sol controls")?.props.disabled, false);
});

test("an undeclared role row is not editable for the supervisor but is for the parent", async () => {
  const pure = await import("../src/policy-state.ts");
  const state = loadedState(pure, readingBody({ roles: [], allowed: [], exceptions: [] }), catalogBody([]));
  const { elements } = await mount(state);
  // The supervisor stays read-only even when the file declares nothing at all.
  assert.equal(byLabel(elements, "supervisor pair controls"), undefined);
  assert.ok(byLabel(elements, "parent pair controls"));
});

test("a model the Add control proposed gets a row the operator can edit before saving", async () => {
  const pure = await import("../src/policy-state.ts");
  // The file lists only A; the catalog adds B. After Add, B is pending and must have its own editor
  // so its effort set can be chosen before Save rather than after an unwanted approval is stored.
  const state = loadedState(pure, readingBody({ allowed: [{ model: "anthropic/opus", efforts: ["max"] }] }), catalogBody([{ id: "anthropic/opus", label: "Opus" }, { id: "gpt-6.1-sol", label: "Sol" }]));
  const first = await mount(state);
  const add = first.elements.find((el) => el.props["aria-label"] === "Add allowed model");
  assert.ok(add, "the add control is rendered");
  fire(add, "onClick");
  const proposed = first.applied.find((call) => call.name === "propose");
  assert.ok(proposed, "Add proposes a change");
  const pending = pure.screenPropose(state as never, proposed?.args[0] as never) as unknown as Record<string, unknown>;
  const second = await mount(pending);
  const row = second.elements.find((el) => el.props["aria-label"] === "allowed gpt-6.1-sol (pending)");
  assert.ok(row, "the pending model gets its own row");
  assert.ok(byLabel(second.elements, "gpt-6.1-sol allowed effort 1"), "and an editor for its effort");
});

test("every input the screen actually renders carries a label and is a native control", async () => {
  // C6: the label requirement is about the CONTROLS THE SCREEN RENDERS, so this renders the screen
  // with an exception, an allowlist and all three roles and walks the real tree, rather than checking
  // the label builders against themselves.
  const pure = await import("../src/policy-state.ts");
  const state = loadedState(pure, readingBody(), catalogBody([{ id: "anthropic/opus", label: "Opus" }, { id: "gpt-6.1-sol", label: "Sol" }]));
  const { elements } = await mount(state);
  const controls = elements.filter((el) => el.type === "select" || el.type === "input" || el.type === "button");
  assert.ok(controls.length > 0, "the screen renders controls");
  const native = new Set(["select", "input", "button"]);
  for (const control of controls) {
    // A button may carry visible text instead of an aria-label; an input or select must have one.
    if (control.type === "button") continue;
    assert.equal(typeof control.props["aria-label"], "string", `${String(control.type)} has an aria-label`);
    assert.ok((control.props["aria-label"] as string).length > 0, "the label is never empty");
    assert.ok(native.has(String(control.type)), "the control is a native element the browser makes keyboard operable");
  }
  // A fieldset groups the editable rows and is itself a native element.
  const fieldsets = elements.filter((el) => el.type === "fieldset");
  assert.ok(fieldsets.length > 0, "the editable rows are grouped in fieldsets");
  for (const fieldset of fieldsets) assert.equal(typeof fieldset.props["aria-label"], "string", "each fieldset is labelled");
});

test("a reopened exception editor shows the pending values, not the file's", async () => {
  // C6's other half: after a change is proposed, what the screen shows must still be what a save
  // would send. This drives the real Edit control and inspects the controls it renders, so a wrong
  // draft passed to the handler is caught here rather than only in the pure function.
  const pure = await import("../src/policy-state.ts");
  let state = loadedState(pure, readingBody(), catalogBody([{ id: "anthropic/opus", label: "Opus" }, { id: "gpt-6.1-sol", label: "Sol" }, { id: "devin/swe-2", label: "SWE" }]));
  state = pure.screenPropose(state as never, { kind: "setException", id: "legacy", role: "parent", model: "anthropic/opus", effort: "max", cwd: ["/srv/new"] }) as unknown as Record<string, unknown>;
  const first = await mount(state);
  // The row offers Edit, and pressing it hands the handler the pending draft.
  const edit = byLabel(first.elements, "Edit exception legacy");
  assert.ok(edit, "the row offers an Edit control");
  fire(edit, "onClick");
  const call = first.applied.find((entry) => entry.name === "exceptionDraft");
  assert.ok(call, "Edit opens the editor");
  const draft = call?.args[0] as { model: string; effort: string; cwd: string[] };
  assert.equal(draft.model, "anthropic/opus", "the handler was given the pending model, not the file's");
  assert.equal(draft.effort, "max");
  assert.deepEqual(draft.cwd, ["/srv/new"]);
  // Rendering the state that call produces shows those values in the editor's own controls.
  const opened = pure.screenExceptionDraft(state as never, draft as never) as unknown as Record<string, unknown>;
  const second = await mount(opened);
  assert.equal(byLabel(second.elements, "legacy exception model")?.props.value, "anthropic/opus");
  assert.equal(byLabel(second.elements, "legacy exception effort")?.props.value, "max");
  assert.equal(byLabel(second.elements, "legacy exception cwd 1")?.props.value, "/srv/new");
});

test("a pending exception the file no longer declares keeps a row the operator can act on", async () => {
  // d2: the conflict re-read keeps the change but the file lost the entry, so no row was rendered and
  // the retained change had no control at all. The screen now renders one from the change itself.
  const pure = await import("../src/policy-state.ts");
  let state = loadedState(pure, readingBody(), catalogBody([{ id: "anthropic/opus", label: "Opus" }, { id: "gpt-6.1-sol", label: "Sol" }, { id: "devin/swe-2", label: "SWE" }]));
  state = pure.screenPropose(state as never, { kind: "setException", id: "legacy", role: "parent", model: "anthropic/opus", effort: "max", cwd: ["/srv/new"] }) as unknown as Record<string, unknown>;
  // Another writer removes the exception; the keeping re-read retains the change.
  const after = pure.screenLoaded(state as never, pure.decodePolicy(readingBody({ exceptions: [], digest: "b".repeat(64) })), true) as unknown as Record<string, unknown>;
  assert.equal((after.change as { kind: string })?.kind, "setException", "the change is kept");
  const { elements } = await mount(after);
  const row = elements.find((el) => el.props["aria-label"] === "exception legacy");
  assert.ok(row, "the pending exception still has a row");
  assert.ok(byLabel(elements, "Edit exception legacy") ?? byLabel(elements, "Remove exception legacy"), "with a control to act on");
  // An allowed edit whose model left the file is the same shape.
  let allowedState = loadedState(pure, readingBody(), catalogBody([{ id: "anthropic/opus", label: "Opus" }]));
  allowedState = pure.screenAllowedDraft(allowedState as never, "anthropic/opus", ["max", "high"]) as unknown as Record<string, unknown>;
  const allowedAfter = pure.screenLoaded(allowedState as never, pure.decodePolicy(readingBody({ allowed: [{ model: "gpt-6.1-sol", efforts: ["xhigh"] }], digest: "b".repeat(64) })), true) as unknown as Record<string, unknown>;
  const allowedTree = await mount(allowedAfter);
  assert.ok(allowedTree.elements.find((el) => el.props["aria-label"] === "allowed anthropic/opus (pending)"), "the pending allowed model still has a row");
});
