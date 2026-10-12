// New in CRW (no CXC counterpart): the Runs page's state, with no React in it.
/**
 * runs-state.ts - what the Runs page reads and how it follows a run.
 *
 * The types are the documents GET /api/runs and GET /api/runs/<id> answer (the Go types of
 * internal/flow/cli, in their JSON spelling). RunEventFeed follows the server-sent events of a run
 * (GET /api/runs/<id>/events): the row's seq is the event id, so a feed that has to reconnect asks
 * for the rows after the last seq it delivered and never delivers a row twice, whatever the server
 * repeats. The feed takes the EventSource constructor as an argument, so node:test can drive it with
 * a fake and the page drives it with the browser's.
 */

/** One run's line. */
export interface RunInfo {
  run_id: string;
  workflow: string;
  owner: string;
  plan_id: string;
  plan_revision: number;
  started_at: string;
  state: string;
  live: boolean;
  pid?: number;
  processes: number;
  calls: number;
  unsettled: number;
  recovery_holds: number;
  waiting: number;
  tokens: number;
  phase?: string;
  status?: string;
  error_kind?: string;
  paused?: string;
  held_reasons?: string[];
  last_seq: number;
}

/** One agent call in the tree. */
export interface AgentNode {
  call_id: string;
  label?: string;
  role: string;
  node?: string;
  attempt?: number;
  status: string;
  reason?: string;
  thread_id?: string;
  tokens: number;
  recovery_hold?: boolean;
}

export interface PhaseNode {
  title: string;
  agents: AgentNode[];
}

export interface ReviewLink {
  call: string;
  head: string;
  state: string;
  verdict?: string;
}

/** What a plan node of the run has so far. */
export interface NodeInfo {
  node: string;
  threads?: string[];
  commits?: string[];
  reviews?: ReviewLink[];
  acceptance?: string;
  integrated?: string;
  tokens: number;
  recovery_hold?: boolean;
  held?: boolean;
}

export interface Question {
  id: string;
  revision: number;
  node: string;
  reason: string;
  ask: string;
  current: boolean;
  applied?: string;
}

/** GET /api/runs/<id>. */
export interface RunDetail extends RunInfo {
  phases: PhaseNode[];
  nodes: NodeInfo[];
  questions: Question[];
}

/** GET /api/runs: a list, or the reason it could not be read. */
export interface RunsDocument {
  runs: RunInfo[];
  unknown?: string;
}

/** One journal row, as the event stream gives it. */
export interface RunRow {
  seq: number;
  kind: string;
  event: string;
  call_id?: string;
  op?: string;
  label?: string;
  data?: Record<string, unknown>;
}

/** The states a run is over in; the page offers no control for these. */
export function isOver(state: string): boolean {
  return state === "completed" || state === "cancelled" || state === "failed";
}

/** What the page can ask of a run in this state. A run no process holds cannot be paused. */
export function controlsFor(run: Pick<RunInfo, "state" | "live">): { pause: boolean; cancel: boolean } {
  if (isOver(run.state)) return { pause: false, cancel: false };
  return { pause: run.live && run.state !== "paused", cancel: true };
}

/** A short tag for a node or call, for the tree: the 8 leading characters of a hash. */
export function shortId(id: string | undefined, n = 8): string {
  if (!id) return "";
  return id.length > n ? id.slice(0, n) : id;
}

/** A token count as the page shows it. */
export function tokenText(n: number): string {
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`;
  if (n >= 1_000) return `${(n / 1_000).toFixed(1)}k`;
  return String(n);
}

/* ---- the event feed ---- */

/** The part of EventSource the feed uses. */
export interface EventSourceLike {
  onerror: ((event: unknown) => void) | null;
  onopen: ((event: unknown) => void) | null;
  addEventListener(type: string, listener: (event: { data?: string; lastEventId?: string }) => void): void;
  close(): void;
}

export type EventSourceFactory = (url: string) => EventSourceLike;

export interface FeedOptions {
  /** The EventSource constructor, wrapped as a factory. */
  open: EventSourceFactory;
  /** The URL of the run's events, without a cursor. */
  url: string;
  /** Called once for each row, in seq order. */
  onRow: (row: RunRow) => void;
  /** Called with the run's state whenever the stream reports a change. */
  onState?: (state: string) => void;
  /** Called when the stream ended because the run is over, or a reconnect gave up. */
  onEnd?: (state: string) => void;
  /** The pause before a reconnect, in ms, for the nth try (from 0). */
  delay?: (attempt: number) => number;
  /** Schedules a reconnect; the browser's setTimeout. */
  later?: (fn: () => void, ms: number) => unknown;
  /** The first cursor. */
  after?: number;
  /** Reconnects before the feed gives up (default 8). */
  maxRetries?: number;
}

/** The pause before a reconnect: 0.5 s, doubling, up to 15 s. */
export function backoff(attempt: number): number {
  return Math.min(15_000, 500 * 2 ** attempt);
}

/**
 * RunEventFeed follows one run. It delivers each row once and in order: a row at or below the last
 * delivered seq is dropped, so a reconnect that repeats rows (or a server that does) shows none
 * twice. On an error it closes the source and opens a new one at `?after=<last seq>`; a stream that
 * delivers a row again resets the retry count. An `end` event closes the feed for good.
 */
export class RunEventFeed {
  private last: number;
  private source: EventSourceLike | null = null;
  private closed = false;
  private attempt = 0;
  private timer: unknown = null;

  private readonly o: FeedOptions;

  constructor(options: FeedOptions) {
    this.o = options;
    this.last = options.after ?? 0;
  }

  /** The seq of the last row delivered. */
  get cursor(): number {
    return this.last;
  }

  start(): void {
    if (this.closed || this.source) return;
    const url = `${this.o.url}${this.o.url.includes("?") ? "&" : "?"}after=${this.last}`;
    const source = this.o.open(url);
    this.source = source;
    source.addEventListener("row", (event) => {
      if (this.source !== source) return;
      let row: RunRow;
      try {
        row = JSON.parse(event.data ?? "") as RunRow;
      } catch {
        return;
      }
      if (typeof row.seq !== "number" || row.seq <= this.last) return;
      this.last = row.seq;
      this.attempt = 0;
      this.o.onRow(row);
    });
    source.addEventListener("state", (event) => {
      if (this.source !== source) return;
      try {
        this.o.onState?.((JSON.parse(event.data ?? "") as { state: string }).state);
      } catch {
        /* a state event that is not JSON changes nothing */
      }
    });
    source.addEventListener("end", (event) => {
      if (this.source !== source) return;
      let state = "";
      try {
        state = (JSON.parse(event.data ?? "") as { state: string }).state;
      } catch {
        /* the end is the end whatever it says */
      }
      this.stop();
      this.o.onEnd?.(state);
    });
    source.onerror = () => {
      if (this.source !== source || this.closed) return;
      source.close();
      this.source = null;
      const max = this.o.maxRetries ?? 8;
      if (this.attempt >= max) {
        this.closed = true;
        this.o.onEnd?.("unreachable");
        return;
      }
      const ms = (this.o.delay ?? backoff)(this.attempt++);
      const later = this.o.later ?? ((fn: () => void, wait: number) => setTimeout(fn, wait));
      this.timer = later(() => {
        this.timer = null;
        this.start();
      }, ms);
    };
  }

  /** Closes the feed: no more rows, no reconnect. */
  stop(): void {
    this.closed = true;
    this.source?.close();
    this.source = null;
    if (this.timer !== null) {
      clearTimeout(this.timer as ReturnType<typeof setTimeout>);
      this.timer = null;
    }
  }
}
