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
    Object.assign(headers, writeHeaders());
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

/**
 * The headers a write carries: the JSON content type and, when this tab has one, the per-run
 * token. A read carries neither, which is what the server's guard expects of it.
 */
function writeHeaders(): Record<string, string> {
  bootstrapToken();
  const headers: Record<string, string> = { "Content-Type": "application/json" };
  const token = getToken();
  if (token !== null) headers["X-CRW-Token"] = token;
  return headers;
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

/* ---- helper-role settings ---- */

// Ported from CXC v0.2.40 plugins/codexclaw/gui/src/api.ts (12-38, 129-153, 333-359), modified:
// the store's scope is global only, so the read names no scope and the write body carries none;
// the "project" source and the trust warning have no counterpart here; and the effort names come
// from the catalog and the execution policy rather than from the fixed spawn enum alone.
//
// The helper roles are the four roles of the internal/role store (explorer, reviewer, executor,
// architect). They are not the supervisor, parent and child pairs of the execution policy: that is
// a different store with different names, and nothing here reads or writes it.

/** The four helper roles, in the order the store encodes them. */
export type HelperRole = "explorer" | "reviewer" | "executor" | "architect";

export const HELPER_ROLES: readonly HelperRole[] = ["explorer", "reviewer", "executor", "architect"] as const;

/** The first fallback after a role's primary candidate. */
export interface RoleFallback {
  model: string;
  effort: string | null;
}

/** One role's settings. A null member is the store's null, not a missing one. */
export interface HelperRoleConfig {
  mode: "default" | "model";
  model: string | null;
  effort: string | null;
  promptOverride: string | null;
  fallback: RoleFallback | null;
}

/** The effective settings, with each role's source and whether it overrides the session. */
export interface HelperRoleSettings {
  roles: Record<HelperRole, HelperRoleConfig>;
  scope: "global";
  sources: Record<HelperRole, "global" | "session">;
  overrides: Record<HelperRole, boolean>;
}

/**
 * A partial change to one role. An absent member stays, an explicit null clears the model, effort
 * or prompt override (or removes the fallback), and inherit resets the whole role.
 */
export interface HelperRolePatch {
  mode?: "default" | "model";
  model?: string | null;
  effort?: string | null;
  promptOverride?: string | null;
  fallback?: RoleFallback | null;
  inherit?: boolean;
}

/** What a role write reports: the settings after it, or the caller's own settings and the reason. */
export interface SetHelperRoleResult {
  ok: boolean;
  config: HelperRoleSettings;
  error?: string;
}

/** A role with no override: the main model, the session's effort, no prompt, no fallback. */
export function defaultHelperRole(): HelperRoleConfig {
  return { mode: "default", model: null, effort: null, promptOverride: null, fallback: null };
}

/** The settings of a store that holds nothing yet. */
export function defaultHelperRoleSettings(): HelperRoleSettings {
  return {
    roles: { explorer: defaultHelperRole(), reviewer: defaultHelperRole(), executor: defaultHelperRole(), architect: defaultHelperRole() },
    scope: "global",
    sources: { explorer: "session", reviewer: "session", executor: "session", architect: "session" },
    overrides: { explorer: false, reviewer: false, executor: false, architect: false },
  };
}

/**
 * Whether a body is a whole settings answer. A body missing any role, source or override is
 * rejected rather than rendered as a half-populated screen, which is the CRW form of the check
 * CXC's client made on its scope metadata.
 */
export function isHelperRoleSettings(body: unknown): body is HelperRoleSettings {
  if (!body || typeof body !== "object") return false;
  const candidate = body as HelperRoleSettings;
  if (candidate.scope !== "global") return false;
  return HELPER_ROLES.every(
    (role) =>
      !!candidate.roles?.[role] &&
      (candidate.sources?.[role] === "global" || candidate.sources?.[role] === "session") &&
      typeof candidate.overrides?.[role] === "boolean",
  );
}

/** The server's error member of a JSON body, or null. */
function errorMessageOf(body: unknown): string | null {
  if (body && typeof body === "object") {
    const value = (body as { error?: unknown }).error;
    if (typeof value === "string" && value !== "") return value;
  }
  return null;
}

/**
 * The effective helper-role settings. It throws rather than returning a fabricated default,
 * because a fabricated default could overwrite a saved override.
 */
export async function getHelperRoleSettings(signal?: AbortSignal): Promise<HelperRoleSettings> {
  const response = await fetch("/api/helper-roles", { headers: { Accept: "application/json" }, signal });
  const body = await response.json().catch(() => null);
  if (!response.ok) throw new Error(errorMessageOf(body) ?? `Settings load failed (${response.status})`);
  if (!isHelperRoleSettings(body)) throw new Error("Invalid settings response. Reload and try again.");
  return body;
}

/**
 * Change one role. A refusal is reported rather than thrown, so the screen can show the store's
 * own message beside the row, and the caller's settings are returned unchanged on failure.
 */
export async function setHelperRole(role: HelperRole, patch: HelperRolePatch, fallback: HelperRoleSettings): Promise<SetHelperRoleResult> {
  try {
    const response = await fetch("/api/helper-roles", {
      method: "POST",
      headers: writeHeaders(),
      body: JSON.stringify({ role, ...patch }),
    });
    const body = (await response.json().catch(() => null)) as unknown;
    if (!response.ok || !isHelperRoleSettings(body)) {
      return { ok: false, config: fallback, error: errorMessageOf(body) ?? `save failed (${response.status})` };
    }
    return { ok: true, config: body };
  } catch {
    return { ok: false, config: fallback, error: "backend unreachable" };
  }
}

/** The model catalog, as the server reports it. The three states stay distinct. */
export interface ModelCatalog {
  state: string;
  status: "fresh" | "stale" | "unavailable";
  source?: string;
  fetchedAt?: string | null;
  message?: string;
  entries: CatalogEntry[];
}

/**
 * The model catalog. A catalog that could not be read is reported as unavailable with an empty
 * list and the server's reason, never as an empty success.
 */
export async function getModelCatalog(refresh = false): Promise<ModelCatalog> {
  try {
    const response = await fetch(`/api/catalog${refresh ? "?refresh=1" : ""}`, { headers: { Accept: "application/json" } });
    const body = (await response.json().catch(() => null)) as unknown;
    if (!response.ok || !body || typeof body !== "object") throw new Error("invalid catalog response");
    const catalog = body as ModelCatalog;
    if (!Array.isArray(catalog.entries) || !["fresh", "stale", "unavailable"].includes(catalog.status)) {
      throw new Error("invalid catalog response");
    }
    return catalog;
  } catch {
    return {
      state: "unavailable",
      status: "unavailable",
      entries: [],
      message: "The model list could not be read. Retry, or check the catalog source.",
    };
  }
}

/**
 * The effort names the execution policy approves, read from the policy route. This is a read of
 * effort NAMES only: the policy's supervisor, parent and child pairs are not shown on this screen
 * and none of its values are mixed into it. A policy that is not registered or cannot be read
 * yields no names, which is not an error for this screen.
 */
export async function getPolicyEffortNames(): Promise<string[]> {
  try {
    const response = await fetch("/api/policy", { headers: { Accept: "application/json" } });
    if (!response.ok) return [];
    const body = (await response.json()) as { allowed?: Array<{ efforts?: unknown }> } | null;
    if (!body || !Array.isArray(body.allowed)) return [];
    const names: string[] = [];
    for (const entry of body.allowed) {
      if (!entry || !Array.isArray(entry.efforts)) continue;
      for (const effort of entry.efforts) if (typeof effort === "string" && effort !== "") names.push(effort);
    }
    return names;
  } catch {
    return [];
  }
}

/**
 * The effort names this screen offers, in first-seen order: what the catalog advertises for each
 * model, then what the execution policy approves, then the store's own accepted names as a floor.
 *
 * The order matters. The catalog and the policy are the sources, so a name CRW's policy uses that
 * the CXC spawn enum never had (none, max) appears here and is never renamed or swapped for
 * another. The floor keeps the control usable when neither source could be read, which is why it
 * is appended rather than used as the list.
 */
export function helperRoleEfforts(catalog: readonly CatalogEntry[], policyEfforts: readonly string[]): string[] {
  const seen = new Set<string>();
  const names: string[] = [];
  const add = (name: unknown): void => {
    if (typeof name === "string" && name !== "" && !seen.has(name)) {
      seen.add(name);
      names.push(name);
    }
  };
  for (const entry of catalog) {
    if (Array.isArray(entry?.reasoningEfforts)) for (const effort of entry.reasoningEfforts) add(effort);
  }
  for (const effort of policyEfforts) add(effort);
  for (const effort of EFFORTS) add(effort);
  return names;
}

/**
 * Whether the helper-role store accepts this effort name. The store's own set is the authority, and
 * it is narrower than what a catalog or an execution policy may advertise: a policy legitimately
 * names an effort the helper-role store does not hold. Such a name is still listed, so the screen
 * shows the sources as they are, but it is not offered for selection, because choosing it could
 * only produce a refusal. This is a statement about the store's contract, not a second list of
 * acceptable values.
 */
export function effortSelectable(name: string): boolean {
  return (EFFORTS as readonly string[]).includes(name);
}

/**
 * The store-accepted names a model's advertised ladder permits, in the store's own order.
 *
 * `supported` is the same three-state value the effort control uses: an array is the model's
 * advertised ladder, `undefined` means no model is selected, and `null` means the catalog did not
 * advertise a ladder. Only an array is evidence that the model refuses a name, so a non-array keeps
 * every store-accepted name selectable.
 */
export function selectableEfforts(supported: readonly string[] | null | undefined): string[] {
  if (!Array.isArray(supported)) return [...EFFORTS];
  return EFFORTS.filter((name) => supported.includes(name));
}

/**
 * Whether the model advertises a ladder that holds none of the names the helper-role store accepts.
 * When this is true no option is selectable and the role can only use the session effort, so the
 * screen owes the user a reason (see `effortFallbackNotice`).
 */
export function effortLadderUnsupported(supported: readonly string[] | null | undefined): boolean {
  return Array.isArray(supported) && selectableEfforts(supported).length === 0;
}

/**
 * The one-line reason no store-accepted effort is selectable, or null when some is. The sentence
 * states what the role then uses: the session effort when nothing is saved, and the saved value when
 * one is, because the screen never silently changes a stored value.
 */
export function effortFallbackNotice(supported: readonly string[] | null | undefined, savedEffort: string | null): string | null {
  if (!effortLadderUnsupported(supported)) return null;
  return savedEffort === null
    ? "This model advertises no effort the helper-role store accepts; the role uses the session effort."
    : `This model advertises no effort the helper-role store accepts; the saved effort ${savedEffort} is kept.`;
}
