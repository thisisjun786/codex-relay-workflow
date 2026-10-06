// Ported from CXC v0.2.40 plugins/codexclaw/gui/test/router.test.ts (1-28), modified:
// the expected paths are the CRW route set (#/status, #/policy, #/helper-roles) with
// #/status as the default, and an unknown hash is asserted to normalize as well.
import { test } from "node:test";
import assert from "node:assert/strict";
import { currentRoute, DEFAULT_ROUTE, ROUTES, routeFor } from "../src/router.ts";

function withHash(hash: string, fn: () => void): void {
  const g = globalThis as { location?: { hash: string } };
  const prev = g.location;
  g.location = { hash };
  try {
    fn();
  } finally {
    if (prev === undefined) delete g.location;
    else g.location = prev;
  }
}

test("empty hash defaults to the CRW default route", () => {
  assert.equal(DEFAULT_ROUTE, "/status");
  withHash("", () => assert.equal(currentRoute(), "/status"));
  withHash("#", () => assert.equal(currentRoute(), "/status"));
  withHash("#/", () => assert.equal(currentRoute(), "/status"));
});

test("explicit hash routes resolve to their path", () => {
  withHash("#/status", () => assert.equal(currentRoute(), "/status"));
  withHash("#/policy", () => assert.equal(currentRoute(), "/policy"));
  withHash("#/helper-roles", () => assert.equal(currentRoute(), "/helper-roles"));
});

test("the route table is the three CRW routes and nothing else", () => {
  assert.deepEqual(ROUTES.map((r) => r.path), ["/status", "/policy", "/helper-roles"]);
});

test("every route carries what a screen renders, so no route can render blank", () => {
  for (const route of ROUTES) {
    assert.equal(routeFor(route.path).path, route.path);
    assert.ok(route.label.length > 0);
    assert.ok(route.subtitle.length > 0);
  }
  // An unknown path falls back to a complete route, not to a partial one.
  assert.equal(routeFor("/nope").subtitle.length > 0, true);
});

test("a hash that is not a known route normalizes to the default", () => {
  withHash("#/channels", () => assert.equal(currentRoute(), "/status"));
  withHash("#/dashboard", () => assert.equal(currentRoute(), "/status"));
  // The server prints the token in the fragment; before bootstrapToken() strips it,
  // the fragment is not a route and must not paint an empty screen.
  withHash("#token=deadbeef", () => assert.equal(currentRoute(), "/status"));
});
