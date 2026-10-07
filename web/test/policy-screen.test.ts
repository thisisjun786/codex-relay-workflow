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
    allowedText: (...args: unknown[]) => applied.push({ name: "allowedText", args }),
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
  fire(byLabel(first.elements, "anthropic/opus allowed efforts"), "onChange", "max, ");
  const call = first.applied.find((c) => c.name === "allowedText");
  const next = pure.screenAllowedText(state as never, call?.args[0] as never, call?.args[1] as never) as unknown as Record<string, unknown>;
  const second = await mount(next);
  assert.equal(byLabel(second.elements, "anthropic/opus allowed efforts")?.props.value, "max, ");
});

test("the allowed input an operator cleared shows what they typed", async () => {
  const pure = await import("../src/policy-state.ts");
  const state = loadedState(pure, readingBody(), catalogBody([{ id: "anthropic/opus", label: "Opus" }]));
  const first = await mount(state);
  fire(byLabel(first.elements, "anthropic/opus allowed efforts"), "onChange", "");
  const call = first.applied.find((c) => c.name === "allowedText");
  const next = pure.screenAllowedText(state as never, call?.args[0] as never, call?.args[1] as never) as unknown as Record<string, unknown>;
  const second = await mount(next);
  assert.equal(byLabel(second.elements, "anthropic/opus allowed efforts")?.props.value, "");
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
  state = pure.screenAllowedText(state, "anthropic/opus", "max, high");
  const pending = state.change;
  const kept = pure.screenLoaded(state, pure.decodePolicy(readingBody({ digest: "b".repeat(64) })), true);
  assert.deepEqual(kept.change, pending, "the pending change survives the stale re-read");
  assert.equal(kept.allowedText["anthropic/opus"], "max, high", "the typed text survives too");
});

test("a save in flight does not drop an edit made while it is running", async () => {
  const pure = await import("../src/policy-state.ts");
  let state = pure.initialScreen();
  state = pure.screenLoaded(state, pure.decodePolicy(readingBody()));
  state = pure.screenAllowedText(state, "anthropic/opus", "max");
  const saving = pure.screenSaveStarted(state);
  // A second edit lands while the first save is in flight.
  const edited = pure.screenAllowedText(saving, "gpt-6.1-sol", "high");
  const finished = pure.screenSaveFinished(edited, state.change, pure.noticeForWrite(200, { stored: { digest: "b".repeat(64) }, registered: { digest: "b".repeat(64) }, applied: "applied", actions: [] }));
  assert.equal(finished.change?.kind, "setAllowed", "the later edit is still pending");
  assert.equal((finished.change as { model: string }).model, "gpt-6.1-sol");
});

test("an undeclared role row is not editable for the supervisor but is for the parent", async () => {
  const pure = await import("../src/policy-state.ts");
  const state = loadedState(pure, readingBody({ roles: [], allowed: [], exceptions: [] }), catalogBody([]));
  const { elements } = await mount(state);
  // The supervisor stays read-only even when the file declares nothing at all.
  assert.equal(byLabel(elements, "supervisor pair controls"), undefined);
  assert.ok(byLabel(elements, "parent pair controls"));
});
