// New in CRW (no CXC counterpart): the Runs page.
//
// TestRunsPageRendersTree renders the screen for real, the way policy-screen.test.ts does: esbuild
// bundles Runs.tsx into ESM, the module is imported, and react-dom/server renders the screen from a
// run document, so a criterion stated about the screen (the tree of phases and agents, the thread and
// the commits of a node, the review links, the tokens, recovery_hold, the store that could not be
// read) is observed in the markup. TestSSEReconnect drives RunEventFeed with a fake EventSource: a
// reconnect asks for the rows after the last one delivered and no row is delivered twice.
import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdir } from "node:fs/promises";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";
import * as React from "react";
import { renderToStaticMarkup } from "react-dom/server";
import {
  RunEventFeed,
  backoff,
  controlsFor,
  isOver,
  tokenText,
  type EventSourceLike,
  type RunDetail,
  type RunInfo,
  type RunRow,
} from "../src/runs-state.ts";

const here = dirname(fileURLToPath(import.meta.url));

interface ScreenModule {
  RunsScreen: (props: Record<string, unknown>) => React.ReactElement;
}

async function loadScreen(): Promise<ScreenModule> {
  const build = await import("esbuild");
  const root = resolve(here, "../src");
  const out = resolve(here, "../node_modules/.cache/runs-screen.test.mjs");
  await mkdir(dirname(out), { recursive: true });
  await build.build({
    stdin: { contents: 'export { RunsScreen } from "./pages/Runs.tsx";', resolveDir: root, loader: "tsx" },
    bundle: true,
    outfile: out,
    format: "esm",
    platform: "node",
    jsx: "automatic",
    packages: "external",
  });
  return (await import(`${out}?t=${Date.now()}`)) as ScreenModule;
}

const noop = () => undefined;
const handlers = { select: noop, pause: noop, cancel: noop, answer: noop };

function info(over: Partial<RunInfo> = {}): RunInfo {
  return {
    run_id: "run-a1", workflow: "project-lane", owner: "thread-owner", plan_id: "plan-1", plan_revision: 3, started_at: "2026-10-12T08:00:00Z",
    state: "running", live: true, pid: 4242, processes: 1, calls: 3, unsettled: 1, recovery_holds: 1, waiting: 1, tokens: 1900, phase: "build", last_seq: 21,
    ...over,
  };
}

function detail(over: Partial<RunDetail> = {}): RunDetail {
  return {
    ...info(),
    phases: [
      { title: "design", agents: [{ call_id: "c_arch", role: "architect", status: "completed", thread_id: "thr-arch", tokens: 700 }] },
      {
        title: "build",
        agents: [
          { call_id: "c_exec", role: "executor", node: "n1", attempt: 1, status: "completed", thread_id: "thr-exec", tokens: 1200 },
          { call_id: "c_lost", role: "executor", node: "n2", attempt: 1, status: "outcome_unknown", reason: "dispatch_unknown", tokens: 0, recovery_hold: true },
        ],
      },
    ],
    nodes: [
      { node: "n1", threads: ["thr-exec"], commits: ["abcdef0123456789"], reviews: [{ call: "c_rev", head: "abcdef0123456789", state: "verdict", verdict: "needs_changes" }], tokens: 1200 },
      { node: "n2", tokens: 0, recovery_hold: true },
    ],
    questions: [{ id: "esc_one", revision: 1, node: "n1", reason: "pair_refused", ask: "configuration", current: true }],
    ...over,
  };
}

test("TestRunsPageRendersTree", async () => {
  const { RunsScreen } = await loadScreen();
  const html = renderToStaticMarkup(
    React.createElement(RunsScreen, { runs: [info(), info({ run_id: "run-b2", state: "completed", live: false, recovery_holds: 0, waiting: 0 })], unknown: null, selected: "run-a1", detail: detail(), error: null, following: true, handlers }),
  );
  // the list
  assert.match(html, /run-a1/);
  assert.match(html, /run-b2/);
  // the tree: phases in order, the agents under them
  const design = html.indexOf("phase design");
  const build = html.indexOf("phase build");
  assert.ok(design >= 0 && build > design, "design comes before build");
  assert.ok(html.indexOf("architect") > design && html.indexOf("architect") < build, "the architect is under the design phase");
  assert.ok(html.indexOf("data-call=\"c_exec\"") > build, "the executor is under the build phase");
  // the thread, the tokens, the commit and the review link of a node
  assert.match(html, /thread thr-exec/);
  assert.match(html, /1\.2k tokens/);
  assert.match(html, /commits abcdef01/);
  assert.match(html, /review c_rev needs_changes@abcdef01/);
  // recovery_hold on the call, on the node and in the run's line
  assert.equal((html.match(/recovery_hold/g) ?? []).length >= 3, true, "recovery_hold is shown on the call, the node and the run");
  assert.match(html, /outcome_unknown \(dispatch_unknown\)/);
  // a question that waits for an answer offers the answer
  assert.match(html, /question esc_one r1/);
  assert.match(html, /Apply answer/);
  assert.match(html, /following the run/);
});

test("TestRunsPageRendersTree: controls follow the run's state, and a store that cannot be read shows its reason", async () => {
  const { RunsScreen } = await loadScreen();
  const live = renderToStaticMarkup(React.createElement(RunsScreen, { runs: [info()], unknown: null, selected: "run-a1", detail: detail(), error: null, following: false, handlers }));
  assert.doesNotMatch(live, /<button[^>]*disabled=""[^>]*>Pause/);
  assert.match(live, /the stream is not connected/);
  const over = detail({ state: "completed", live: false, recovery_holds: 0, waiting: 0 });
  const done = renderToStaticMarkup(React.createElement(RunsScreen, { runs: [over], unknown: null, selected: "run-a1", detail: over, error: null, following: false, handlers }));
  assert.match(done, /<button[^>]*disabled=""[^>]*>Pause/);
  assert.match(done, /<button[^>]*disabled=""[^>]*>Cancel/);
  const dead = detail({ state: "dead", live: false });
  assert.deepEqual(controlsFor(dead), { pause: false, cancel: true });
  assert.deepEqual(controlsFor(detail()), { pause: true, cancel: true });
  assert.deepEqual(controlsFor(detail({ state: "paused" })), { pause: false, cancel: true });
  assert.equal(isOver("held"), false);
  assert.equal(isOver("failed"), true);
  // not read is not empty
  const unread = renderToStaticMarkup(React.createElement(RunsScreen, { runs: null, unknown: "there is no relay store at /x", selected: null, detail: null, error: null, following: false, handlers }));
  assert.match(unread, /there is no relay store at \/x/);
  assert.doesNotMatch(unread, /No run yet/);
  const empty = renderToStaticMarkup(React.createElement(RunsScreen, { runs: [], unknown: null, selected: null, detail: null, error: null, following: false, handlers }));
  assert.match(empty, /No run yet/);
  assert.equal(tokenText(999), "999");
  assert.equal(tokenText(12_300), "12.3k");
});

/* ---- the event feed ---- */

class FakeSource implements EventSourceLike {
  onerror: ((event: unknown) => void) | null = null;
  onopen: ((event: unknown) => void) | null = null;
  closed = false;
  private listeners = new Map<string, Array<(e: { data?: string; lastEventId?: string }) => void>>();
  readonly url: string;
  constructor(url: string) {
    this.url = url;
  }
  addEventListener(type: string, listener: (e: { data?: string; lastEventId?: string }) => void): void {
    this.listeners.set(type, [...(this.listeners.get(type) ?? []), listener]);
  }
  close(): void {
    this.closed = true;
  }
  emit(type: string, data: unknown): void {
    for (const l of this.listeners.get(type) ?? []) l({ data: JSON.stringify(data) });
  }
  row(seq: number): void {
    this.emit("row", { seq, kind: "call", event: "completed", call_id: `c${seq}` } satisfies RunRow);
  }
}

test("TestSSEReconnect", () => {
  const sources: FakeSource[] = [];
  const rows: number[] = [];
  const ends: string[] = [];
  const timers: Array<() => void> = [];
  const delays: number[] = [];
  const feed = new RunEventFeed({
    open: (url) => {
      const s = new FakeSource(url);
      sources.push(s);
      return s;
    },
    url: "/api/runs/run-a1/events",
    onRow: (r) => rows.push(r.seq),
    onEnd: (s) => ends.push(s),
    later: (fn, ms) => {
      delays.push(ms);
      timers.push(fn);
      return timers.length;
    },
  });
  feed.start();
  assert.equal(sources[0].url, "/api/runs/run-a1/events?after=0");
  sources[0].row(1);
  sources[0].row(2);
  sources[0].row(3);
  assert.deepEqual(rows, [1, 2, 3]);
  assert.equal(feed.cursor, 3);

  // the stream breaks: the source is closed and a reconnect is scheduled after a pause
  sources[0].onerror?.({});
  assert.equal(sources[0].closed, true);
  assert.equal(sources.length, 1);
  assert.deepEqual(delays, [backoff(0)]);
  timers.shift()?.();
  // the reconnect continues from the last row it delivered
  assert.equal(sources.length, 2);
  assert.equal(sources[1].url, "/api/runs/run-a1/events?after=3");
  // a server that repeats rows delivers none twice
  sources[1].row(3);
  sources[1].row(2);
  sources[1].row(4);
  sources[1].row(4);
  sources[1].row(5);
  assert.deepEqual(rows, [1, 2, 3, 4, 5]);

  // a late error of the source that was replaced changes nothing
  sources[0].onerror?.({});
  assert.equal(sources.length, 2);

  // another break, another pause (the count started again with the rows that arrived), and a third source
  sources[1].onerror?.({});
  assert.deepEqual(delays, [backoff(0), backoff(0)]);
  timers.shift()?.();
  assert.equal(sources[2].url, "/api/runs/run-a1/events?after=5");
  sources[2].row(6);
  sources[2].emit("end", { state: "completed", cursor: 6 });
  assert.deepEqual(rows, [1, 2, 3, 4, 5, 6]);
  assert.deepEqual(ends, ["completed"]);
  assert.equal(sources[2].closed, true);
  // the feed is closed for good: nothing reconnects
  sources[2].onerror?.({});
  assert.equal(timers.length, 0);
  assert.equal(sources.length, 3);
});

test("TestSSEReconnect: pauses grow, the cursor survives a stop and start, and the feed gives up", () => {
  assert.deepEqual([0, 1, 2, 5, 10].map(backoff), [500, 1000, 2000, 15000, 15000]);
  const sources: FakeSource[] = [];
  const ends: string[] = [];
  const timers: Array<() => void> = [];
  const feed = new RunEventFeed({
    open: (url) => {
      const s = new FakeSource(url);
      sources.push(s);
      return s;
    },
    url: "/api/runs/run-a1/events?x=1",
    after: 7,
    maxRetries: 2,
    onRow: () => undefined,
    onEnd: (s) => ends.push(s),
    later: (fn) => timers.push(fn),
  });
  feed.start();
  assert.equal(sources[0].url, "/api/runs/run-a1/events?x=1&after=7");
  for (let i = 0; i < 2; i++) {
    sources[i].onerror?.({});
    timers.shift()?.();
  }
  sources[2].onerror?.({});
  assert.deepEqual(ends, ["unreachable"]);
  assert.equal(timers.length, 0);
  // a stopped feed delivers nothing
  const quiet = new RunEventFeed({ open: (u) => new FakeSource(u), url: "/e", onRow: () => assert.fail("a stopped feed delivered a row") });
  quiet.stop();
  quiet.start();
});

test("the control calls carry the token and a JSON body, and a refusal is returned with the command's words", async () => {
  const g = globalThis as unknown as Record<string, unknown>;
  const calls: Array<{ url: string; init: RequestInit }> = [];
  const answers = [
    { status: 200, body: { ok: true, message: "pause requested of run run-a1 (process 7)" } },
    { status: 409, body: { ok: false, message: "run run-a1 is completed", error: "run run-a1 is completed" } },
    { status: 404, body: { error: "not_found" } },
  ];
  const prevFetch = g.fetch;
  g.fetch = async (url: string, init: RequestInit) => {
    calls.push({ url, init });
    const a = answers.shift()!;
    return new Response(JSON.stringify(a.body), { status: a.status, headers: { "Content-Type": "application/json" } });
  };
  try {
    const api = (await import(`../src/api.ts?case=${Date.now()}`)) as unknown as typeof import("../src/api.ts");
    const ok = await api.controlRun("run-a1", "pause");
    assert.deepEqual(ok, { ok: true, message: "pause requested of run run-a1 (process 7)" });
    assert.equal(calls[0].url, "/api/runs/run-a1/pause");
    assert.equal(calls[0].init.method, "POST");
    assert.equal((calls[0].init.headers as Record<string, string>)["Content-Type"], "application/json");
    const refused = await api.answerRun("run-a1", { decision: "ud-1", node: "n1", question: "esc_one", revision: 2 });
    assert.deepEqual(refused, { ok: false, message: "run run-a1 is completed" });
    assert.equal(calls[1].url, "/api/runs/run-a1/answer");
    assert.deepEqual(JSON.parse(String(calls[1].init.body)), { decision: "ud-1", node: "n1", question: "esc_one", revision: 2 });
    await assert.rejects(api.controlRun("run-none", "cancel"), (e: { status: number }) => e.status === 404);
    assert.equal(api.runEventsUrl("run a/1"), "/api/runs/run%20a%2F1/events");
  } finally {
    g.fetch = prevFetch;
  }
});
