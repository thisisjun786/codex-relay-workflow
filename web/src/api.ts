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
