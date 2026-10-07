// Ported from CXC v0.2.40 plugins/codexclaw/gui/test/subagent-client.test.ts (1-80), modified:
// the store's scope is global only, so the read names no scope and the write body carries none;
// the scope-metadata assertion becomes the whole-answer shape check; and the effort list is
// asserted to keep the names the execution policy uses.
import { test } from "node:test";
import assert from "node:assert/strict";
import {
  defaultHelperRoleSettings,
  effortSelectable,
  getHelperRoleSettings,
  getModelCatalog,
  helperRoleEfforts,
  setHelperRole,
  type HelperRoleSettings,
} from "../src/api.ts";

/** A whole settings answer, with the parts a case changes. */
function settings(changes: Partial<HelperRoleSettings> = {}): HelperRoleSettings {
  return { ...defaultHelperRoleSettings(), ...changes };
}

/** One captured fetch call. */
interface Captured {
  url: string;
  init?: RequestInit;
}

/** The body of a captured write, parsed back. */
function bodyOf(captured: Captured): Record<string, unknown> {
  return JSON.parse(captured.init?.body as string) as Record<string, unknown>;
}

test("the settings read names no scope and returns the whole answer", async (t) => {
  const captured: Captured[] = [];
  t.mock.method(globalThis, "fetch", async (url: string, init?: RequestInit) => {
    captured.push({ url, init });
    return new Response(JSON.stringify(settings()), { status: 200 });
  });
  const loaded = await getHelperRoleSettings();
  assert.equal(captured[0].url, "/api/helper-roles");
  assert.equal(loaded.scope, "global");
  assert.equal(loaded.sources.explorer, "session");
  assert.equal(loaded.overrides.architect, false);
  assert.equal(loaded.roles.reviewer.promptOverride, null);
});

test("the write body carries the role and the patch, and no scope", async (t) => {
  const captured: Captured[] = [];
  t.mock.method(globalThis, "fetch", async (url: string, init?: RequestInit) => {
    captured.push({ url, init });
    return new Response(JSON.stringify(settings()), { status: 200 });
  });
  const current = settings();
  assert.equal((await setHelperRole("explorer", { effort: null }, current)).ok, true);
  assert.equal(captured[0].url, "/api/helper-roles");
  assert.equal((captured[0].init?.method ?? "").toUpperCase(), "POST");
  assert.deepEqual(bodyOf(captured[0]), { role: "explorer", effort: null });
  await setHelperRole("explorer", { inherit: true }, current);
  assert.deepEqual(bodyOf(captured[1]), { role: "explorer", inherit: true });
  // The write carries the JSON content type, which the server's guard requires of every write.
  assert.equal((captured[0].init?.headers as Record<string, string>)["Content-Type"], "application/json");
});

test("the prompt override keeps null (inherit) and the empty string apart on the wire", async (t) => {
  const captured: Captured[] = [];
  t.mock.method(globalThis, "fetch", async (url: string, init?: RequestInit) => {
    captured.push({ url, init });
    return new Response(JSON.stringify(settings()), { status: 200 });
  });
  const current = settings();
  await setHelperRole("reviewer", { promptOverride: "" }, current);
  await setHelperRole("reviewer", { promptOverride: null }, current);
  const empty = bodyOf(captured[0]);
  const inherit = bodyOf(captured[1]);
  assert.equal(empty.promptOverride, "");
  assert.equal(inherit.promptOverride, null);
  // The two must not collapse into one another: an empty string is a stored value, null inherits.
  assert.notDeepEqual(empty, inherit);
  assert.equal(JSON.stringify(empty) === JSON.stringify(inherit), false);
});

test("a refused write reports the store's message and leaves the caller's settings alone", async (t) => {
  const current = settings();
  t.mock.method(globalThis, "fetch", async () => new Response(JSON.stringify({ error: "invalid effort" }), { status: 400 }));
  const refused = await setHelperRole("explorer", { effort: null }, current);
  assert.equal(refused.ok, false);
  assert.equal(refused.config, current);
  assert.equal(refused.error, "invalid effort");
});

test("a settings answer missing a role is rejected rather than rendered", async (t) => {
  const incomplete = settings();
  // Drop one role from each of the three per-role sections: a half-populated answer would let the
  // screen render a role that has no source and no override flag.
  delete (incomplete.roles as Record<string, unknown>).architect;
  t.mock.method(globalThis, "fetch", async () => new Response(JSON.stringify(incomplete), { status: 200 }));
  await assert.rejects(getHelperRoleSettings(), /Invalid settings response/);

  const wrongScope = settings();
  (wrongScope as { scope: string }).scope = "project";
  t.mock.method(globalThis, "fetch", async () => new Response(JSON.stringify(wrongScope), { status: 200 }));
  await assert.rejects(getHelperRoleSettings(), /Invalid settings response/);

  t.mock.method(globalThis, "fetch", async () => new Response(JSON.stringify({ error: "boom" }), { status: 400 }));
  await assert.rejects(getHelperRoleSettings(), /boom/);
});

test("the catalog refresh uses the explicit query and a failure never fabricates models", async (t) => {
  let requested = "";
  const mock = t.mock.method(globalThis, "fetch", async (url: string) => {
    requested = url;
    return new Response(
      JSON.stringify({
        state: "ocx-active",
        status: "fresh",
        source: "ocx",
        fetchedAt: "2026-10-07T00:00:00Z",
        entries: [{ id: "fixture/native-new", source: "ocx", label: "new", reasoningEfforts: ["low"] }],
      }),
      { status: 200 },
    );
  });
  const catalog = await getModelCatalog(true);
  assert.equal(requested, "/api/catalog?refresh=1");
  assert.deepEqual(catalog.entries[0].reasoningEfforts, ["low"]);
  assert.equal((await getModelCatalog()).status, "fresh");
  assert.equal(requested, "/api/catalog");

  mock.mock.mockImplementation(async () => new Response("failed", { status: 503 }));
  const failure = await getModelCatalog();
  assert.equal(failure.status, "unavailable");
  assert.deepEqual(failure.entries, []);
  assert.ok((failure.message ?? "").length > 0);
});

test("the effort names come from the catalog and the policy and keep none and max", () => {
  const catalog = [
    { id: "a", label: "a", reasoningEfforts: ["none", "low"] },
    { id: "b", label: "b", reasoningEfforts: ["xhigh", "max"] },
  ];
  const names = helperRoleEfforts(catalog, ["max", "ultra"]);
  // The catalog comes first, then the policy's names, then the store's own accepted names.
  assert.deepEqual(names.slice(0, 4), ["none", "low", "xhigh", "max"]);
  assert.ok(names.includes("ultra"));
  // none and max survive, and xhigh is not swapped for max.
  assert.ok(names.includes("none"));
  assert.ok(names.includes("max"));
  assert.ok(names.includes("xhigh"));
  // Deduped: max appears once even though the catalog and the policy both name it.
  assert.equal(names.filter((name) => name === "max").length, 1);

  // With neither source readable the store's own names are still offered, so the control is usable.
  const floored = helperRoleEfforts([], []);
  assert.deepEqual(floored, ["low", "medium", "high", "xhigh"]);
  // A model whose ladder the catalog does not report contributes no names rather than an empty set.
  assert.deepEqual(helperRoleEfforts([{ id: "c", label: "c", reasoningEfforts: null }], []), ["low", "medium", "high", "xhigh"]);

  // A name the store does not hold is listed but is not offered for selection: choosing it could
  // only be refused, so the screen shows it and disables it rather than promising a write.
  assert.equal(effortSelectable("none"), false);
  assert.equal(effortSelectable("max"), false);
  assert.equal(effortSelectable("xhigh"), true);
  assert.equal(effortSelectable("low"), true);
});
