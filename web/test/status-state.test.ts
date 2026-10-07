// New in CRW (no CXC counterpart): the run-state bar's own test.
//
// The bar keeps the relay store, the App Server and the execution policy as three separate
// readings. One source that could not be read must not turn the other two into a pass, and a
// reading that could not be established must show its reason rather than a blank.
//
// node:test cannot load .tsx, so the assertions run against the pure mapping function
// runStateReadings in src/api.ts, which is the same function the bar component renders.
import { test } from "node:test";
import assert from "node:assert/strict";
import { runStateReadings, type RunState } from "../src/api.ts";

/** The document GET /api/status answers, with one reading overridden per case. */
function document(overrides: {
  relayStore?: { state: string; reason?: string };
  appServer?: { state: string; reason?: string };
  executionPolicy?: Record<string, unknown>;
} = {}): RunState {
  return {
    schema: "crw-gui-status/1",
    readAt: "2026-10-07T00:00:00Z",
    bar: {
      relayStore: { state: "ok", ...overrides.relayStore },
      appServer: { state: "ok", ...overrides.appServer },
      executionPolicy: {
        state: "ok",
        policyState: "registered",
        path: "/tmp/execution-policy.json",
        fileDigest: "a".repeat(64),
        registeredDigest: "a".repeat(64),
        runningDigest: null,
        runningReason: "worker_policy_unconfigured",
        applied: "unverifiable",
        ...overrides.executionPolicy,
      },
    },
    relay: { state: "ok", data: null },
    capacity: { state: "ok", data: null },
    dag: { state: "ok", data: null },
  } as unknown as RunState;
}

test("the bar shows the relay store, the App Server and the execution policy separately", () => {
  const readings = runStateReadings(document());
  assert.deepEqual(readings.map((r) => r.key), ["relayStore", "appServer", "executionPolicy"]);
  for (const reading of readings) {
    assert.ok(reading.label.length > 0, "every reading is labelled");
  }
});

test("one source that could not be read leaves the other two ok", () => {
  const readings = runStateReadings(
    document({ relayStore: { state: "unknown", reason: "relay_read_store_absent: no relay store" } }),
  );
  assert.deepEqual(readings.map((r) => r.state), ["unknown", "ok", "ok"]);
  assert.match(readings[0].reason, /relay_read_store_absent/);
  assert.equal(readings[1].reason, "");
  assert.equal(readings[2].reason, "");
});

test("an unread App Server leaves the relay store and the policy ok", () => {
  const readings = runStateReadings(
    document({ appServer: { state: "unknown", reason: "host_unreachable" } }),
  );
  assert.deepEqual(readings.map((r) => r.state), ["ok", "unknown", "ok"]);
  assert.equal(readings[1].reason, "host_unreachable");
});

test("an unregistered execution policy is unknown with its own reason", () => {
  const readings = runStateReadings(
    document({
      executionPolicy: { state: "unknown", reason: "no record at /tmp/codex/crw-bridge-mcp.json", policyState: "not_registered" },
    }),
  );
  assert.deepEqual(readings.map((r) => r.state), ["ok", "ok", "unknown"]);
  assert.match(readings[2].reason, /no record at/);
});

test("the execution policy keeps the file, registered and running digests apart", () => {
  const readings = runStateReadings(document());
  const policy = readings[2];
  assert.deepEqual(policy.parts.map((p) => p.label), ["File digest", "Registered digest", "Running digest"]);
  assert.equal(policy.parts[0].value, "a".repeat(64));
  assert.equal(policy.parts[0].state, "ok");
  assert.equal(policy.parts[2].value, "");
  assert.equal(policy.parts[2].state, "unknown");
  assert.equal(policy.parts[2].reason, "worker_policy_unconfigured");
});

test("a request that never answered shows all three as unknown with the reason", () => {
  const readings = runStateReadings(null, "request failed (500)");
  assert.deepEqual(readings.map((r) => r.state), ["unknown", "unknown", "unknown"]);
  for (const reading of readings) {
    assert.equal(reading.reason, "request failed (500)");
  }
});

test("no reading is folded into one shared answer", () => {
  const readings = runStateReadings(
    document({ relayStore: { state: "unknown", reason: "a" }, appServer: { state: "unknown", reason: "b" } }),
  );
  // Three readings, three reasons: a single status dot would have hidden which one failed.
  assert.equal(new Set(readings.map((r) => r.reason)).size, 3);
});
