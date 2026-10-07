// Original CRW module (no CXC counterpart): the execution-policy screen's pure state logic.
//
// This file holds everything the #/policy screen decides and nothing it renders: the wire shapes
// the Go routes answer with, the view model derived from one reading, the one pending change and
// its preview, the option lists the selects are built from, and the notice a write answer becomes.
// It imports no React, no DOM and no fetch, which is what lets web/test/policy-state.test.ts import
// it directly - the same reason effort-support.ts sits outside the .tsx component.
//
// Two rules run through it. A value the reading could not establish is a named state with a reason,
// never a zero and never an empty success. And the policy file is the only source of a policy value:
// the catalog contributes model and effort NAMES, never a value this screen writes.
import type { CatalogEntry, ModelCatalog } from "./api.ts";

// The catalog types are re-exported so a test of this module can build a catalog answer without
// importing the API client, which keeps this module's tests free of a fetch boundary.
export type { CatalogEntry, ModelCatalog };

/** The exact sentence the issue fixes for the blast radius of a save. */
export const POLICY_BLAST_RADIUS = "Applies to tasks created after the relay service restarts; running tasks keep their settings.";

/** The supervisor row's label: its model is Jun's own selection and this screen does not manage it. */
export const SUPERVISOR_LABEL = "Selected by Jun; not managed here";

/** The three states a reading answers with (internal/policystore/policy.go's State). */
export type PolicyStateName = "registered" | "not_registered" | "unreadable";

/** One model and reasoning effort, spelled as the policy document spells the effort. */
export interface PolicyPair {
  model: string;
  reasoningEffort: string;
}

/** One declared role (internal/policystore.RoleView). */
export interface PolicyRoleView {
  name: string;
  expectation: string;
  pairs: PolicyPair[];
}

/** One allowlist entry (internal/policystore.AllowedView). */
export interface PolicyAllowedView {
  model: string;
  efforts: string[];
}

/** One declared exception (internal/policystore.ExceptionView). */
export interface PolicyExceptionView {
  id: string;
  role?: string;
  model: string;
  reasoningEffort: string;
  cwd: string[];
}

/** GET /api/policy's answer (internal/gui/policyBody). */
export interface PolicyReading {
  state: PolicyStateName;
  reason?: string;
  path?: string;
  mode?: string;
  digest?: string;
  registeredDigest?: string;
  runningDigest: string | null;
  runningReason?: string;
  roles: PolicyRoleView[];
  allowed: PolicyAllowedView[];
  exceptions: PolicyExceptionView[];
  applied: string;
  actions: string[];
}

/**
 * One proposed edit. It is the shape POST /api/policy/check and POST /api/policy take
 * (internal/policystore.Change). Exactly one kind is applied per request, and a member carries
 * only the fields its own kind uses, so the server's singleKind check can never see a stray field.
 */
export type PolicyChange =
  | { kind: "setRolePairs"; role: string; pairs: PolicyPair[] }
  | { kind: "setAllowed"; model: string; efforts: string[] }
  | { kind: "removeAllowed"; model: string }
  | { kind: "setException"; id: string; role?: string; model: string; effort: string; cwd: string[] }
  | { kind: "removeException"; id: string };

/** POST /api/policy/check's answer (internal/gui/checkBody). */
export interface PolicyCheckResult {
  valid: boolean;
  errors: string[];
  currentDigest: string;
  stale: boolean;
  diff: string[];
}

/** POST /api/policy's 200 answer (internal/gui/policyWriteBody). */
export interface PolicyWriteSuccess {
  stored: { digest: string };
  registered?: { digest: string };
  applied: string;
  actions: string[];
  backup?: string;
  warnings?: string[];
}

/** Every refusal of POST /api/policy (internal/gui/policyWriteErrorBody). */
export interface PolicyWriteError {
  error: string;
  reason?: string;
  currentDigest?: string;
  errors?: string[];
  restored?: boolean;
  fileDigest?: string;
  registeredDigest?: string;
  backup?: string;
  recovery?: string;
  step?: string;
}

/** One row of the first screen. */
export interface PolicyRoleRow {
  name: string;
  /** False for the supervisor, whose model and effort are the user's own selection. */
  editable: boolean;
  label: string;
  expectation: string;
  pairs: PolicyPair[];
}

/** Where the effective values come from, read from the reading rather than assumed. */
export interface PolicySource {
  path: string;
  digest: string;
  registeredDigest: string;
  applied: string;
  runningDigest: string | null;
  runningReason: string;
  mode: string;
}

/** The first screen's whole model. */
export interface PolicyView {
  state: PolicyStateName;
  reason: string;
  /** False while the policy has no record or could not be read: the screen then shows why. */
  editable: boolean;
  roles: PolicyRoleRow[];
  allowed: PolicyAllowedView[];
  exceptions: PolicyExceptionView[];
  source: PolicySource;
}

/** One before/after row of the preview. */
export interface PolicyPreviewItem {
  label: string;
  before: string;
  after: string;
}

/** What saving the pending change would do, before it is sent. */
export interface PolicyPreview {
  items: PolicyPreviewItem[];
  blastRadius: string;
  /** The exception-removal sentence, when the change removes an exception. */
  fallback?: string;
}

/** What a write answer becomes on the screen. */
export interface PolicyNotice {
  tone: "ok" | "err" | "info";
  text: string;
  stored: string | null;
  registered: string | null;
  applied: string | null;
  actions: string[];
  errors: string[];
  restored: boolean | null;
  /** True when the caller's own inputs must survive (a stale digest). */
  keepInputs: boolean;
  /** True when the reading must be fetched again. */
  reread: boolean;
  /** True when a person must repair the host before another write can be attempted. */
  blockEditing: boolean;
}

/** One selectable model, and whether the catalog still lists it. */
export interface PolicyModelOption {
  id: string;
  label: string;
  unavailable: boolean;
}

function isObject(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function stringOf(value: unknown): string {
  return typeof value === "string" ? value : "";
}

/** One pair, or null when the answer did not carry a usable one. */
function pairOf(raw: unknown): PolicyPair | null {
  if (!isObject(raw)) return null;
  const model = stringOf(raw.model);
  if (model === "") return null;
  return { model, reasoningEffort: stringOf(raw.reasoningEffort) };
}

/**
 * decodePolicy validates a GET /api/policy answer and returns it typed, or throws. A body missing
 * a member the screen reads is refused rather than rendered as a half-populated screen, which is
 * the CRW form of the check the helper-role client makes on its scope metadata.
 */
export function decodePolicy(raw: unknown): PolicyReading {
  if (!isObject(raw)) throw new Error("Invalid policy response. Reload and try again.");
  const state = raw.state;
  if (state !== "registered" && state !== "not_registered" && state !== "unreadable") {
    throw new Error("Invalid policy response. Reload and try again.");
  }
  if (!Array.isArray(raw.roles) || !Array.isArray(raw.allowed) || !Array.isArray(raw.exceptions)) {
    throw new Error("Invalid policy response. Reload and try again.");
  }
  if (!Array.isArray(raw.actions)) throw new Error("Invalid policy response. Reload and try again.");
  const roles: PolicyRoleView[] = [];
  for (const entry of raw.roles) {
    if (!isObject(entry) || stringOf(entry.name) === "") throw new Error("Invalid policy response. Reload and try again.");
    const pairs: PolicyPair[] = [];
    const rawPairs = entry.pairs;
    if (rawPairs !== undefined && !Array.isArray(rawPairs)) throw new Error("Invalid policy response. Reload and try again.");
    for (const pair of Array.isArray(rawPairs) ? rawPairs : []) {
      const decoded = pairOf(pair);
      if (!decoded) throw new Error("Invalid policy response. Reload and try again.");
      pairs.push(decoded);
    }
    roles.push({ name: stringOf(entry.name), expectation: stringOf(entry.expectation), pairs });
  }
  const allowed: PolicyAllowedView[] = [];
  for (const entry of raw.allowed) {
    if (!isObject(entry)) throw new Error("Invalid policy response. Reload and try again.");
    const efforts: string[] = [];
    for (const effort of Array.isArray(entry.efforts) ? entry.efforts : []) {
      if (typeof effort !== "string") throw new Error("Invalid policy response. Reload and try again.");
      efforts.push(effort);
    }
    allowed.push({ model: stringOf(entry.model), efforts });
  }
  const exceptions: PolicyExceptionView[] = [];
  for (const entry of raw.exceptions) {
    if (!isObject(entry) || stringOf(entry.id) === "") throw new Error("Invalid policy response. Reload and try again.");
    const cwd: string[] = [];
    for (const root of Array.isArray(entry.cwd) ? entry.cwd : []) {
      if (typeof root !== "string") throw new Error("Invalid policy response. Reload and try again.");
      cwd.push(root);
    }
    exceptions.push({
      id: stringOf(entry.id),
      role: entry.role === undefined ? undefined : stringOf(entry.role),
      model: stringOf(entry.model),
      reasoningEffort: stringOf(entry.reasoningEffort),
      cwd,
    });
  }
  const running = raw.runningDigest;
  if (running !== null && typeof running !== "string") throw new Error("Invalid policy response. Reload and try again.");
  return {
    state,
    reason: stringOf(raw.reason),
    path: stringOf(raw.path),
    mode: stringOf(raw.mode),
    digest: stringOf(raw.digest),
    registeredDigest: stringOf(raw.registeredDigest),
    runningDigest: running,
    runningReason: stringOf(raw.runningReason),
    roles,
    allowed,
    exceptions,
    applied: stringOf(raw.applied),
    actions: raw.actions.map((action) => stringOf(action)),
  };
}

/** derivePolicyView builds the first screen from one reading. It invents no value. */
export function policyView(reading: PolicyReading): PolicyView {
  const roles = reading.roles.map((role) => ({
    name: role.name,
    editable: role.name !== "supervisor",
    label: role.name === "supervisor" ? SUPERVISOR_LABEL : "",
    expectation: role.expectation,
    pairs: role.pairs,
  }));
  return {
    state: reading.state,
    reason: reading.reason ?? "",
    editable: reading.state === "registered",
    roles,
    allowed: reading.allowed,
    exceptions: reading.exceptions,
    source: {
      path: reading.path ?? "",
      digest: reading.digest ?? "",
      registeredDigest: reading.registeredDigest ?? "",
      applied: reading.applied,
      runningDigest: reading.runningDigest,
      runningReason: reading.runningReason ?? "",
      mode: reading.mode ?? "",
    },
  };
}

/** pairText is one pair as a row reads it. */
function pairText(pair: PolicyPair): string {
  return `${pair.model} ${pair.reasoningEffort}`.trim();
}

/** pairsText is a pair list as a row reads it, or a word for the empty list. */
function pairsText(pairs: readonly PolicyPair[]): string {
  return pairs.length === 0 ? "none" : pairs.map(pairText).join(", ");
}

/** exceptionText is one exception as a row reads it. */
function exceptionText(exception: PolicyExceptionView): string {
  const scope = exception.cwd.length === 0 ? "no cwd scope" : exception.cwd.join(", ");
  return `${exception.role ?? "any role"} ${exception.model} ${exception.reasoningEffort} (${scope})`;
}

/**
 * previewChange is the before/after of one pending change, plus the blast radius. It is derived
 * from the reading the caller already has, so the preview is shown before anything is sent; the
 * server's own check is what judges whether the change would be accepted.
 */
export function previewChange(reading: PolicyReading, change: PolicyChange): PolicyPreview {
  const items: PolicyPreviewItem[] = [];
  const preview: PolicyPreview = { items, blastRadius: POLICY_BLAST_RADIUS };
  switch (change.kind) {
    case "setRolePairs": {
      const role = reading.roles.find((entry) => entry.name === change.role);
      items.push({ label: `role ${change.role}`, before: pairsText(role?.pairs ?? []), after: pairsText(change.pairs) });
      break;
    }
    case "setAllowed": {
      const entry = reading.allowed.find((row) => row.model === change.model);
      items.push({
        label: `allowed ${change.model}`,
        before: entry ? entry.efforts.join(", ") : "not listed",
        after: change.efforts.join(", "),
      });
      break;
    }
    case "removeAllowed": {
      const entry = reading.allowed.find((row) => row.model === change.model);
      items.push({ label: `allowed ${change.model}`, before: entry ? entry.efforts.join(", ") : "not listed", after: "removed" });
      break;
    }
    case "setException": {
      const existing = reading.exceptions.find((row) => row.id === change.id);
      const after: PolicyExceptionView = { id: change.id, role: change.role, model: change.model, reasoningEffort: change.effort, cwd: change.cwd };
      items.push({
        label: `exception ${change.id}`,
        before: existing ? exceptionText(existing) : "not declared",
        after: exceptionText(after),
      });
      break;
    }
    case "removeException": {
      const existing = reading.exceptions.find((row) => row.id === change.id);
      const role = existing?.role ?? "the cited role";
      const scope = existing && existing.cwd.length > 0 ? existing.cwd.join(", ") : "the exception's scope";
      items.push({
        label: `exception ${change.id}`,
        before: existing ? exceptionText(existing) : "not declared",
        after: `${role} default`,
      });
      preview.fallback = `Removing this exception returns ${scope} to the ${role} role default.`;
      break;
    }
    default:
      break;
  }
  return preview;
}

/**
 * policyEfforts is the effort names this screen offers: what the policy's own allowlist approves,
 * then what the catalog advertises for the models it lists, in first-seen order.
 *
 * The policy file is the authority here, so its names come first and none of them is dropped: a
 * name CRW's policy uses that the CXC spawn enum never had (none, max) appears and is never renamed
 * or swapped for another. The catalog adds names a model advertises that the allowlist does not
 * mention, which is what lets the screen show an option the operator may still add. A catalog that
 * could not be read contributes nothing and takes nothing away.
 */
export function policyEfforts(reading: PolicyReading, catalog: ModelCatalog | null): string[] {
  const seen = new Set<string>();
  const names: string[] = [];
  const add = (name: unknown): void => {
    if (typeof name === "string" && name !== "" && !seen.has(name)) {
      seen.add(name);
      names.push(name);
    }
  };
  for (const entry of reading.allowed) for (const effort of entry.efforts) add(effort);
  for (const entry of catalog?.entries ?? []) {
    if (Array.isArray(entry.reasoningEfforts)) for (const effort of entry.reasoningEfforts) add(effort);
  }
  return names;
}

/** everyModel names every model the reading mentions, in the order the document declares them. */
function everyModel(reading: PolicyReading): string[] {
  const seen = new Set<string>();
  const models: string[] = [];
  const add = (model: string): void => {
    if (model !== "" && !seen.has(model)) {
      seen.add(model);
      models.push(model);
    }
  };
  for (const role of reading.roles) for (const pair of role.pairs) add(pair.model);
  for (const entry of reading.allowed) add(entry.model);
  for (const exception of reading.exceptions) add(exception.model);
  return models;
}

/**
 * modelOptions is the model list the selects are built from: what the catalog lists, then every
 * model the policy file names that the catalog does not. A saved model the catalog no longer lists
 * stays selectable and is marked unavailable, so opening the screen can never silently swap it.
 * When the catalog could not be read at all, the policy's own models are still offered.
 */
export function modelOptions(reading: PolicyReading, catalog: ModelCatalog | null): PolicyModelOption[] {
  const options: PolicyModelOption[] = [];
  const seen = new Set<string>();
  for (const entry of catalog?.entries ?? []) {
    if (entry.id === "" || seen.has(entry.id)) continue;
    seen.add(entry.id);
    options.push({ id: entry.id, label: entry.label === "" ? entry.id : entry.label, unavailable: false });
  }
  for (const model of everyModel(reading)) {
    if (seen.has(model)) continue;
    seen.add(model);
    options.push({ id: model, label: `${model} (saved, unavailable)`, unavailable: true });
  }
  return options;
}

/**
 * catalogNotice names the catalog's own state. A catalog still loading, one that failed, one that
 * is stale behind a cached list and one this host's OCX cannot read are four different sentences:
 * collapsing them into "no models" would hide which of them the operator is looking at.
 */
export function catalogNotice(catalog: ModelCatalog | null): string {
  if (!catalog) return "Loading the model list...";
  switch (catalog.status) {
    case "fresh":
      return `Model list from ${catalog.source === "ocx" ? "OCX" : "the Codex catalog"}`;
    case "stale":
      return "Model list is stale - the refresh failed, so a model may be missing or superseded";
    default:
      return catalog.message && catalog.message !== ""
        ? `Model list unavailable: ${catalog.message}`
        : "Model list unavailable - the policy file's own names are still offered";
  }
}

/** digest12 is a digest as a row reads it: enough to compare two of them, short enough to read. */
function digest12(digest: string): string {
  return digest.length > 12 ? digest.slice(0, 12) : digest;
}

function isWriteSuccess(body: unknown): body is PolicyWriteSuccess {
  if (!isObject(body) || !isObject(body.stored)) return false;
  return stringOf((body.stored as Record<string, unknown>).digest) !== "";
}

function isWriteError(body: unknown): body is PolicyWriteError {
  return isObject(body) && stringOf(body.error) !== "";
}

function emptyNotice(tone: PolicyNotice["tone"], text: string): PolicyNotice {
  return { tone, text, stored: null, registered: null, applied: null, actions: [], errors: [], restored: null, keepInputs: false, reread: false, blockEditing: false };
}

/**
 * noticeForWrite turns one POST /api/policy answer into what the screen shows. A success notice is
 * produced only for a 200 whose answer carries a stored digest; every refusal is an error notice,
 * whatever its status, because the one thing the screen may never do is report a failed write as a
 * success.
 *
 * The three facts a 200 carries are kept apart: stored is the file's new digest, registered is what
 * the wiring record now names (absent when it could not be read back), and applied is whether the
 * running relay holds those bytes. needs_user_action and unverifiable are separate values of
 * applied, never folded into applied.
 */
export function noticeForWrite(status: number, body: unknown): PolicyNotice {
  if (status === 200) {
    if (!isWriteSuccess(body)) {
      return emptyNotice("err", "The server answered 200 without a stored digest, so the write was not confirmed. Read the policy again before retrying.");
    }
    const registered = isObject(body.registered) ? stringOf((body.registered as Record<string, unknown>).digest) : "";
    const notice = emptyNotice("ok", "");
    notice.stored = body.stored.digest;
    notice.registered = registered === "" ? null : registered;
    notice.applied = stringOf(body.applied);
    notice.actions = Array.isArray(body.actions) ? body.actions.map((action) => stringOf(action)) : [];
    const parts = [`Stored ${digest12(notice.stored)}`];
    parts.push(notice.registered === null ? "registered: not read back" : `registered ${digest12(notice.registered)}`);
    if (notice.applied === "applied") parts.push("the running relay holds these bytes");
    else if (notice.applied === "needs_user_action") {
      parts.push("the running relay still holds the old bytes");
      if (notice.actions.length > 0) parts.push(`to apply it: ${notice.actions.join("; ")}`);
    }
    else if (notice.applied === "unverifiable") parts.push("whether the running relay holds these bytes could not be read");
    else parts.push(`applied: ${notice.applied || "unknown"}`);
    notice.text = `${parts.join("; ")}.`;
    return notice;
  }
  if (!isWriteError(body)) {
    return emptyNotice("err", `The policy write failed (${status}) and the server sent no reason.`);
  }
  const notice = emptyNotice("err", "");
  notice.errors = Array.isArray(body.errors) ? body.errors.map((error) => stringOf(error)) : [];
  notice.restored = typeof body.restored === "boolean" ? body.restored : null;
  switch (body.error) {
    case "stale_digest":
      notice.keepInputs = true;
      notice.reread = true;
      notice.text = "The policy changed elsewhere, so this write was not applied. The file has been read again; your inputs are kept. Check the new values, then save again.";
      break;
    case "invalid_policy":
      notice.text = notice.errors.length > 0 ? `The change was refused: ${notice.errors.join("; ")}` : "The change was refused by the policy check.";
      break;
    case "register_failed":
      notice.text = body.restored
        ? "Registration failed, so the file was put back and nothing changed. The running policy is the one that was there before."
        : "Registration failed and the file was NOT put back, so the file and the wiring record may disagree. Read the policy again and repair the record before another write.";
      notice.blockEditing = body.restored !== true;
      break;
    case "recovery_needed":
      notice.blockEditing = true;
      notice.text = `The file and the wiring record disagree (file ${digest12(stringOf(body.fileDigest))}, record ${digest12(stringOf(body.registeredDigest))}). Every write is refused until this is settled: ${stringOf(body.recovery)}`;
      break;
    case "cancelled":
      notice.text = `The write was cancelled during ${stringOf(body.step) || "an unknown step"}.`;
      break;
    case "failed":
      notice.text = stringOf(body.reason) || "The write failed.";
      break;
    default:
      notice.text = stringOf(body.reason) !== ""
        ? `The write could not be started: ${stringOf(body.reason)}`
        : `The write could not be started: ${body.error}`;
      break;
  }
  return notice;
}

/**
 * checkNotice turns POST /api/policy/check's answer into a notice. A change the server refuses is
 * never shown as ready to save: the same parser judges the write, so a refusal here is a refusal
 * there.
 */
export function checkNotice(result: PolicyCheckResult): PolicyNotice {
  if (result.valid) {
    const notice = emptyNotice("info", `The policy check accepts this change: ${result.diff.join(", ")}`);
    notice.reread = result.stale;
    notice.keepInputs = result.stale;
    if (result.stale) notice.text = `The policy changed elsewhere since it was read. Read it again before saving. Checked fields: ${result.diff.join(", ")}`;
    return notice;
  }
  const notice = emptyNotice("err", result.errors.length > 0 ? `The policy check refused this change: ${result.errors.join("; ")}` : "The policy check refused this change.");
  notice.errors = result.errors;
  return notice;
}
