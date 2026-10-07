// New in CRW (no CXC counterpart): the GUI API client foundation.
/**
 * api.ts - the CRW GUI API client.
 *
 * The server prints its URL with the per-run token in the fragment
 * (`http://127.0.0.1:<port>/#token=<token>`), so the token never belongs in a stored
 * address, a copied link or a Referer header. `bootstrapToken()` moves it into
 * sessionStorage and rewrites the address without it. Only write requests carry the
 * token header and a JSON content type: reads are unauthenticated, writes are not.
 *
 * A tab whose storage is denied keeps the token in memory instead, so stripping it from
 * the address never loses the only copy and writes in that tab still carry it.
 *
 * Screen-specific API functions arrive with the screens that need them; this module is
 * only the foundation they build on.
 */

/* ---- wire types the shared screens compose from ---- */

/** The reasoning-effort values the store accepts, mirroring the spawn wire enum. */
export const EFFORTS = ["low", "medium", "high", "xhigh"] as const;
export type EffortName = (typeof EFFORTS)[number];

/**
 * One selectable model, as the catalog reports it. Only the fields the shared
 * components read live here; the catalog API issue adds the rest.
 */
export interface CatalogEntry {
  id: string;
  label: string;
  reasoningEfforts?: string[] | null;
}

/** Where the fragment token is kept for the life of the tab. */
export const TOKEN_STORAGE_KEY = "crw-gui-token";

/** The fragment parameter the server prints. */
const TOKEN_PARAM = "token";

/** What a non-2xx response throws: the status and the server's error text. */
export interface ApiError {
  status: number;
  error: string;
}

/**
 * The raw value of `name` in a fragment, or null. Deliberately not URLSearchParams: that
 * decodes `+` as a space, and the token's alphabet belongs to the server, so the client
 * must carry the value byte for byte.
 */
function fragmentParam(fragment: string, name: string): string | null {
  for (const part of fragment.split("&")) {
    const eq = part.indexOf("=");
    const key = eq === -1 ? part : part.slice(0, eq);
    if (key === name) return eq === -1 ? "" : part.slice(eq + 1);
  }
  return null;
}

/** The fragment with `name` removed, keeping the order of what is left. */
function removeFragmentParam(fragment: string, name: string): string {
  return fragment
    .split("&")
    .filter((part) => {
      const eq = part.indexOf("=");
      return (eq === -1 ? part : part.slice(0, eq)) !== name;
    })
    .join("&");
}

/**
 * The tab-local copy, used when sessionStorage is unavailable or refuses a write. Without
 * it, a storage-denied tab would strip the token from the address and then have none to
 * send, which would leave the GUI able to read but never to write.
 */
let memoryToken: string | null = null;

/** True once this tab has seen storage deny a read or a write. */
let storageDenied = false;

/** sessionStorage, or undefined when the context denies access to it entirely. */
function storage(): Storage | undefined {
  try {
    return typeof sessionStorage === "undefined" ? undefined : sessionStorage;
  } catch {
    // Some contexts throw on the sessionStorage getter itself, before any call.
    storageDenied = true;
    return undefined;
  }
}

/**
 * The stored token, or null when this tab has none. While storage works it is the source
 * of truth, so a tab that never received a token answers null; the in-memory copy is read
 * only once storage has refused a read or a write.
 */
export function getToken(): string | null {
  const store = storage();
  if (store) {
    try {
      const stored = store.getItem(TOKEN_STORAGE_KEY);
      if (stored !== null) return stored;
      if (!storageDenied) return null;
    } catch {
      storageDenied = true;
    }
  }
  return memoryToken;
}

/**
 * Move `token=` out of `location.hash` into sessionStorage and rewrite the address
 * without it. Idempotent: it acts only while the fragment still carries a token, so a
 * second call - or a call from every request - is a no-op.
 */
export function bootstrapToken(): void {
  if (typeof location === "undefined") return;
  const hash = location.hash ?? "";
  if (hash === "") return;
  const fragment = hash.startsWith("#") ? hash.slice(1) : hash;
  const token = fragmentParam(fragment, TOKEN_PARAM);
  if (token === null || token === "") return;
  // Hold the token in memory first: if the write below fails, this tab still has it.
  memoryToken = token;
  const store = storage();
  if (store) {
    try {
      store.setItem(TOKEN_STORAGE_KEY, token);
    } catch {
      // Storage refused the write; the in-memory copy carries this tab.
      storageDenied = true;
    }
  }
  const rest = removeFragmentParam(fragment, TOKEN_PARAM);
  const address = `${location.pathname}${location.search}${rest === "" ? "" : `#${rest}`}`;
  history.replaceState(null, "", address);
}

export interface RequestOptions {
  method?: string;
  body?: string;
  signal?: AbortSignal;
}

/** GET and HEAD read; every other method writes and needs the token. */
export function isWriteMethod(method: string): boolean {
  const upper = method.toUpperCase();
  return upper !== "GET" && upper !== "HEAD";
}

/**
 * One API call. Writes carry `Content-Type: application/json` and `X-CRW-Token`; reads
 * carry neither. A non-2xx response throws `{status, error}`.
 */
export async function request<T>(path: string, options: RequestOptions = {}): Promise<T> {
  bootstrapToken();
  const method = options.method ?? "GET";
  const headers: Record<string, string> = { Accept: "application/json" };
  if (isWriteMethod(method)) {
    headers["Content-Type"] = "application/json";
    const token = getToken();
    if (token !== null) headers["X-CRW-Token"] = token;
  }
  const init: RequestInit = { method, headers };
  if (options.body !== undefined) init.body = options.body;
  if (options.signal !== undefined) init.signal = options.signal;
  const response = await fetch(path, init);
  if (!response.ok) {
    const apiError: ApiError = { status: response.status, error: await errorText(response) };
    throw apiError;
  }
  // A HEAD request and a 204 carry no body; parsing one would reject a successful call.
  if (method.toUpperCase() === "HEAD" || response.status === 204) return undefined as T;
  return (await response.json()) as T;
}

async function errorText(response: Response): Promise<string> {
  try {
    const body = (await response.json()) as { error?: unknown };
    if (body && typeof body.error === "string" && body.error !== "") return body.error;
  } catch {
    // A non-JSON error body leaves only the status to report.
  }
  return `request failed (${response.status})`;
}

/* ---- the run-state screen (GET /api/status) ---- */

// The endpoint is read-only, so it carries no token and no body: every value below comes from a
// named read the server already has. Nothing here re-derives a judgement the server made - a
// source that could not be read arrives as unknown with the reason the command itself gave.

/** Whether a source answered. A source that could not be read is unknown, never ok. */
export type SourceState = "ok" | "unknown";

/** One status-bar reading. */
export interface StatusMark {
  state: SourceState;
  reason?: string;
}

/**
 * The execution policy's reading. The file, registered and running digests are separate values,
 * each with its own reason, so the bar never conflates the bytes on disk with the digest the
 * wiring record names and with the digest the running service published.
 */
export interface StatusPolicyReading extends StatusMark {
  policyState: string;
  path?: string;
  fileDigest: string | null;
  fileReason?: string;
  registeredDigest: string | null;
  registeredReason?: string;
  runningDigest: string | null;
  runningReason?: string;
  applied: string;
}

/** One manage source's own document, passed through from the command unchanged. */
export interface StatusSource<T = unknown> {
  state: SourceState;
  reason?: string;
  data: T | null;
}

/** One relay relationship, as the relay's own projection spells it. */
export interface RelayRelationshipView {
  relationshipId?: unknown;
  issueKey?: unknown;
  relationshipStatus?: unknown;
  executionGeneration?: unknown;
  nextExpectedAction?: unknown;
  head?: { eventId?: unknown; revisionHash?: unknown; detail?: unknown } | null;
  read?: { state?: string; reason?: string } | null;
}

/** One DAG plan's progress, under the scheduler's own stage names. */
export interface RelayPlanView {
  planId?: unknown;
  projectKey?: unknown;
  revision?: unknown;
  stages?: Record<string, number> | null;
  blocked?: unknown;
  denominator?: unknown;
  read?: { state?: string; reason?: string } | null;
}

/** One merge turn. */
export interface RelayMergeTurnView {
  turnId?: unknown;
  repository?: unknown;
  prNumber?: unknown;
  holderTaskId?: unknown;
  state?: unknown;
  updatedAt?: unknown;
  read?: { state?: string; reason?: string } | null;
}

/** The crw manage relay-read document. */
export interface RelayDocument {
  stateDir?: unknown;
  readAt?: unknown;
  bindings?: unknown[] | null;
  relationships?: RelayRelationshipView[] | null;
  plans?: RelayPlanView[] | null;
  mergeTurns?: RelayMergeTurnView[] | null;
  failures?: { section?: string; item?: string; reason?: string }[] | null;
}

/** One capacity judgement. */
export interface CapacityPlanView {
  plan?: unknown;
  project?: unknown;
  family?: unknown;
  parent?: unknown;
  verdict?: unknown;
  waiting?: unknown[] | null;
  waiting_minutes?: number;
  held?: number;
  ceiling?: number;
  host_memory?: string;
  reasons?: string[] | null;
}

/** The crw manage capacity document. */
export interface CapacityDocument {
  lane?: { merges_last_hour?: number | null } | null;
  actions?: { state?: string; incident?: string | null } | null;
  child_429?: { state?: string; count?: number | null } | null;
  plans?: CapacityPlanView[] | null;
}

/** One DAG anomaly the review found. */
export interface DagAnomalyView {
  kind?: string;
  plan?: string;
  node?: string;
  issue?: string;
  detail?: string;
}

/** The crw manage dag-review document. */
export interface DagDocument {
  plans?: unknown[] | null;
  anomalies?: DagAnomalyView[] | null;
  checks?: { name?: string; state?: string; detail?: string }[] | null;
}

/** The GET /api/status document. */
export interface RunState {
  schema: string;
  readAt: string;
  bar: {
    relayStore: StatusMark;
    appServer: StatusMark;
    executionPolicy: StatusPolicyReading;
  };
  relay: StatusSource<RelayDocument>;
  capacity: StatusSource<CapacityDocument>;
  dag: StatusSource<DagDocument>;
}

/** One part of a reading: a value with its own state, so a blank is never read as a value. */
export interface ReadingPart {
  label: string;
  value: string;
  state: SourceState;
  reason: string;
}

/** One status-bar reading, as the bar renders it. */
export interface RunStateReading {
  key: "relayStore" | "appServer" | "executionPolicy";
  label: string;
  state: SourceState;
  reason: string;
  parts: ReadingPart[];
}

/** GET /api/status. A read: it carries no token and no body. */
export function fetchStatus(signal?: AbortSignal): Promise<RunState> {
  return request<RunState>("/api/status", signal ? { signal } : {});
}

/**
 * The three status-bar readings, in a fixed order. The mapping is deliberately total: a document
 * that never arrived turns all three unknown with the request's reason, and one source that could
 * not be read keeps its own reason without changing the other two.
 */
export function runStateReadings(state: RunState | null, error?: string | null): RunStateReading[] {
  const failed = typeof error === "string" && error !== "" ? error : "the status request did not answer";
  if (!state) {
    return [
      { key: "relayStore", label: "Relay store", state: "unknown", reason: failed, parts: [] },
      { key: "appServer", label: "App Server", state: "unknown", reason: failed, parts: [] },
      { key: "executionPolicy", label: "Execution policy", state: "unknown", reason: failed, parts: [] },
    ];
  }
  const policy = state.bar.executionPolicy;
  return [
    {
      key: "relayStore",
      label: "Relay store",
      state: state.bar.relayStore.state,
      reason: state.bar.relayStore.reason ?? "",
      parts: [],
    },
    {
      key: "appServer",
      label: "App Server",
      state: state.bar.appServer.state,
      reason: state.bar.appServer.reason ?? "",
      parts: [],
    },
    {
      key: "executionPolicy",
      label: "Execution policy",
      state: policy.state,
      reason: policy.reason ?? "",
      parts: [
        digestPart("File digest", policy.fileDigest, policy.fileReason),
        digestPart("Registered digest", policy.registeredDigest, policy.registeredReason),
        digestPart("Running digest", policy.runningDigest, policy.runningReason),
      ],
    },
  ];
}

/** One digest as a part: a value when it was read, otherwise unknown with its reason. */
function digestPart(label: string, digest: string | null | undefined, reason?: string): ReadingPart {
  if (typeof digest === "string" && digest !== "") {
    return { label, value: digest, state: "ok", reason: "" };
  }
  return { label, value: "", state: "unknown", reason: reason ?? "not read" };
}
