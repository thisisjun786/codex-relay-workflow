// Original CRW module (no CXC counterpart): the execution-policy screen's state logic.
//
// This file holds everything the #/policy screen decides and nothing it renders: the wire shapes the
// Go routes answer with, the view model derived from one reading, the one pending change and its
// preview, the raw text the operator is typing into each list input, the option lists the selects
// are built from, the notice a write answer becomes, and the reducer every transition goes through.
// It imports no React, no DOM and no fetch, which is what lets web/test/policy-state.test.ts import
// it directly - the same reason effort-support.ts sits outside the .tsx component.
//
// Two rules run through it. A value the reading could not establish is a named state with a reason,
// never a zero and never an empty success. And the policy file is the only source of a policy value:
// the catalog contributes model and effort NAMES, never a value this screen writes.
import type { CatalogEntry, ModelCatalog } from "./api.ts";
import { effortExcluded } from "./effort-support.ts";

// The catalog types are re-exported so a test of this module can build a catalog answer without
// importing the API client, which keeps this module's tests free of a fetch boundary.
export type { CatalogEntry, ModelCatalog };

/** The exact sentence the issue fixes for the blast radius of a save. */
export const POLICY_BLAST_RADIUS = "Applies to tasks created after the relay service restarts; running tasks keep their settings.";

/** The supervisor row's label: its model is Jun's own selection and this screen does not manage it. */
export const SUPERVISOR_LABEL = "Selected by Jun; not managed here";

/**
 * The three roles the first screen always shows a row for, in the order it shows them. A role the
 * file does not declare still gets a row: the issue's first screen is three rows, and a policy that
 * declares only the child must still show the supervisor and the parent rather than hiding them.
 */
export const POLICY_ROLES = ["supervisor", "parent", "child"] as const;
export type PolicyRoleName = (typeof POLICY_ROLES)[number];

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
 *
 * An omitted role on setException is NOT "any role": the server keeps the role an existing
 * exception already records (internal/policystore/check.go applySetException), and a brand-new
 * exception with no role is only ever covered by a request that itself cites no role, which no task
 * does. The screen therefore requires a role and never offers an empty one as a choice.
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
  /** False when the file does not declare this role: the row then says so. */
  declared: boolean;
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
  /** The repair the applied state names, straight from the read. */
  actions: string[];
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
  /** What the server warned about on a write that still succeeded. */
  warnings: string[];
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

/** The raw text the operator is typing into one exception's editor. */
export interface ExceptionDraft {
  /** The exception being edited, or "" when a new one is being added. */
  id: string;
  /**
   * True when this draft is a NEW exception. The editor's open condition is this flag, not the id
   * being empty: the first character the operator types into the id would otherwise close the editor
   * and make a new exception impossible to finish.
   */
  isNew: boolean;
  role: string;
  model: string;
  effort: string;
  /**
   * The cwd roots, one string per entry. It is a LIST rather than one comma-separated field because
   * a cwd is a path, and a path may itself contain a comma: joining the roots and splitting them
   * again would turn one authorized path into two different authorized paths, silently widening the
   * scope of the exception.
   */
  cwd: string[];
  /** The text of the "add a root" input, which is empty until the operator types into it. */
  cwdNew: string;
}

/**
 * The screen's whole state. Every transition goes through one of the screen* functions below, which
 * is what makes the screen's behaviour testable without a DOM: the component renders this state and
 * calls these functions, so a test that drives them drives the screen.
 */
export interface PolicyScreenState {
  reading: PolicyReading | null;
  catalog: ModelCatalog | null;
  error: string | null;
  /** The one pending change, or null. */
  change: PolicyChange | null;
  /** The raw text of each allowed-list input, keyed by model. */
  allowedText: Record<string, string>;
  /** The model the "add a model to the allowed list" select is on, or "" for its first free one. */
  allowedAddModel: string;
  /** The exception editor's draft, or null when it is closed. */
  exceptionDraft: ExceptionDraft | null;
  /** The change a save in flight is writing, or null when no save is in flight. */
  saving: PolicyChange | null;
  /**
   * The drafts as they were when the save in flight started. A draft that has not moved since is the
   * one the write spent and is dropped when it lands; a draft the operator changed while the write
   * was in flight is their next edit and stays, even when it is for the same model or exception.
   */
  savingDrafts: { allowedText: Record<string, string>; exceptionDraft: ExceptionDraft | null } | null;
  notice: PolicyNotice | null;
  /**
   * The repair a person must make before this screen may edit again, or null. It is separate from
   * the notice so an explicit re-read cannot clear it: the server keeps refusing every write while
   * the file and the wiring record disagree, so the block must outlive the notice that reported it.
   */
  repair: string | null;
}

function isObject(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function stringOf(value: unknown): string {
  return typeof value === "string" ? value : "";
}

/**
 * parseListInput is how a comma-separated list input becomes the list the API takes: each entry is
 * trimmed and the empty ones are dropped. It is deliberately NOT applied on every keystroke - the
 * screen keeps the raw text in its state and calls this only to derive the change, so a trailing
 * comma the operator is about to follow with another entry survives on screen.
 *
 * It is used for the allowed-efforts list, whose entries are effort NAMES and so cannot contain a
 * comma. A cwd is a path and uses one input per root instead (ExceptionDraft.cwd).
 */
export function parseListInput(text: string): string[] {
  return text
    .split(",")
    .map((entry) => entry.trim())
    .filter((entry) => entry !== "");
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
  // actions is the one member the Go route does not wrap with its emptyIfNil helper, so a host with
  // nothing to do answers "actions": null. That is the ordinary case, not a malformed answer, and a
  // strict array check here would leave the whole screen unreadable on every normally applied host.
  const rawActions = raw.actions;
  if (rawActions !== undefined && rawActions !== null && !Array.isArray(rawActions)) {
    throw new Error("Invalid policy response. Reload and try again.");
  }
  const roles: PolicyRoleView[] = [];
  for (const entry of raw.roles) {
    if (!isObject(entry) || stringOf(entry.name) === "") throw new Error("Invalid policy response. Reload and try again.");
    const pairs: PolicyPair[] = [];
    const rawPairs = entry.pairs;
    // A role that declares no pair (the supervisor's expectation-only entry above all) is encoded
    // "pairs": null by the projection, which is a role with no pairs rather than a malformed answer.
    if (rawPairs !== undefined && rawPairs !== null && !Array.isArray(rawPairs)) throw new Error("Invalid policy response. Reload and try again.");
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
      role: entry.role === undefined || entry.role === null ? undefined : stringOf(entry.role),
      model: stringOf(entry.model),
      reasoningEffort: stringOf(entry.reasoningEffort),
      cwd,
    });
  }
  const running = raw.runningDigest;
  if (running !== null && running !== undefined && typeof running !== "string") throw new Error("Invalid policy response. Reload and try again.");
  return {
    state,
    reason: stringOf(raw.reason),
    path: stringOf(raw.path),
    mode: stringOf(raw.mode),
    digest: stringOf(raw.digest),
    registeredDigest: stringOf(raw.registeredDigest),
    runningDigest: typeof running === "string" ? running : null,
    runningReason: stringOf(raw.runningReason),
    roles,
    allowed,
    exceptions,
    applied: stringOf(raw.applied),
    actions: (Array.isArray(rawActions) ? rawActions : []).map((action) => stringOf(action)),
  };
}

/**
 * decodeCheck validates a POST /api/policy/check answer. A body without the boolean the route always
 * answers with is refused rather than read as a refusal, which would show the operator a message the
 * server never sent.
 */
export function decodeCheck(raw: unknown): PolicyCheckResult {
  if (!isObject(raw) || typeof raw.valid !== "boolean") throw new Error("The policy check could not be read. Try saving again.");
  const strings = (value: unknown): string[] => (Array.isArray(value) ? value.filter((item): item is string => typeof item === "string") : []);
  return {
    valid: raw.valid,
    errors: strings(raw.errors),
    currentDigest: stringOf(raw.currentDigest),
    stale: raw.stale === true,
    diff: strings(raw.diff),
  };
}

/**
 * exceptionRoleOptions is the roles an exception may name. It is every role the execution policy
 * knows, not only the pair-editable ones: an exception is scoped to a role by exact equality
 * (internal/bridge/execution/execution.go exceptionCovers), and a supervisor-scoped exception is a
 * valid, live authorization the editor must be able to show and keep. The current draft's role is
 * always included first, so a stored value never loses its matching option.
 */
export function exceptionRoleOptions(current: string): string[] {
  const options: string[] = [];
  if (current !== "") options.push(current);
  for (const role of POLICY_ROLES) if (!options.includes(role)) options.push(role);
  return options;
}

/** policyView builds the first screen from one reading. It invents no value. */
export function policyView(reading: PolicyReading): PolicyView {
  const roles = POLICY_ROLES.map((name) => {
    const declared = reading.roles.find((role) => role.name === name);
    return {
      name,
      declared: declared !== undefined,
      editable: name !== "supervisor",
      label: name === "supervisor" ? SUPERVISOR_LABEL : "",
      expectation: declared?.expectation ?? "",
      pairs: declared?.pairs ?? [],
    };
  });
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
      actions: reading.actions,
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
function exceptionText(exception: { role?: string; model: string; reasoningEffort: string; cwd: string[] }): string {
  // Each root is quoted, so a path that itself contains a comma still reads as one path and the
  // before/after rows cannot show two different scopes as the same text.
  const scope = exception.cwd.length === 0 ? "no cwd scope" : exception.cwd.map((root) => JSON.stringify(root)).join(", ");
  // An exception with no role is not inert: the bridge matches an exception's role against the
  // request's role with exact equality (internal/bridge/execution/execution.go exceptionCovers), so a
  // role-less exception covers exactly the requests that cite no role - which the schema allows and
  // authorize_test.go pins ("an exception written before roles existed still works for a caller that
  // names none"). Describing it as covering nothing would understate a live authorization.
  return `${exception.role || "no role - covers requests that cite no role"} ${exception.model} ${exception.reasoningEffort} (${scope})`;
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
      // The server keeps the role an existing exception already records when the change omits it
      // (internal/policystore/check.go applySetException), so the preview must show that preserved
      // role rather than an "any role" the write would not produce.
      const role = change.role !== undefined && change.role !== "" ? change.role : existing?.role;
      const after = { role, model: change.model, reasoningEffort: change.effort, cwd: change.cwd };
      items.push({
        label: `exception ${change.id}`,
        before: existing ? exceptionText(existing) : "not declared",
        after: exceptionText(after),
      });
      break;
    }
    case "removeException": {
      const existing = reading.exceptions.find((row) => row.id === change.id);
      const scope = existing && existing.cwd.length > 0 ? existing.cwd.join(", ") : "the exception's scope";
      items.push({
        label: `exception ${change.id}`,
        before: existing ? exceptionText(existing) : "not declared",
        after: existing?.role ? `${existing.role} default` : "no role",
      });
      // An exception that omits role applies to every role, so there is no one role whose default it
      // returns to: each task under that scope falls back to its own role's default.
      preview.fallback = existing?.role
        ? `Removing this exception returns ${scope} to the ${existing.role} role default.`
        : `Removing this exception removes the scope ${scope} for requests that cite no role; such a request is then checked against the allowed list, and a request still citing this exception is refused as unknown.`;
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
 *
 * This is the list of names that EXIST. Whether one is offered for a particular model is a separate
 * question the screen answers with modelLadder and effortExcluded, so a name another model
 * advertises is not silently presented as available for this one.
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
  // The policy file is the authority, so every effort it declares anywhere is a name this screen
  // offers: the allowlist, each role's pairs and each exception. A presence_only policy declares no
  // allowlist at all, and a catalog that could not be read contributes nothing, so reading only the
  // allowlist and the catalog would leave such a host with no selectable effort and no way to add a
  // pair or an exception.
  for (const entry of reading.allowed) for (const effort of entry.efforts) add(effort);
  for (const role of reading.roles) for (const pair of role.pairs) add(pair.reasoningEffort);
  for (const exception of reading.exceptions) add(exception.reasoningEffort);
  for (const entry of catalog?.entries ?? []) {
    if (Array.isArray(entry.reasoningEfforts)) for (const effort of entry.reasoningEfforts) add(effort);
  }
  return names;
}

/**
 * modelLadder is the effort ladder the catalog advertises for one model, with the same three-state
 * meaning the effort control uses: an array is the advertised ladder, and null means the catalog did
 * not report one (which is not evidence that the model refuses anything).
 */
export function modelLadder(catalog: ModelCatalog | null, model: string | null | undefined): readonly string[] | null {
  if (!model) return null;
  return catalog?.entries.find((entry) => entry.id === model)?.reasoningEfforts ?? null;
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
 * modelOptionLabel is what one model reads as in a select: the catalog's label when the catalog
 * lists it, and the "(saved, unavailable)" form when it does not. The exception editor uses this so
 * a saved model the catalog dropped is marked there exactly as it is in the pair rows.
 */
export function modelOptionLabel(options: readonly PolicyModelOption[], model: string): string {
  const found = options.find((option) => option.id === model);
  if (!found) return model;
  return found.unavailable ? `${model} (saved, unavailable)` : found.label;
}

/**
 * catalogNotice names the catalog's own state. A catalog still loading, one that failed, one that
 * is stale behind a cached list and one this host's OCX cannot read are four different sentences:
 * collapsing them into "no models" would hide which of them the operator is looking at.
 */
export function catalogNotice(catalog: ModelCatalog | null): string {
  if (!catalog) return "Loading the model list...";
  // This host's OCX does not read the live catalog at all. The reader reports that as a stale answer
  // behind a cache, and it is a different state from a refresh that simply failed: the message says
  // so, and folding it into the generic stale sentence would hide which one it is.
  if (catalog.state === "unsupported-ocx-catalog") {
    return catalog.message && catalog.message !== ""
      ? `Model list unavailable: ${catalog.message}`
      : "This host's OCX does not support reading the live model catalog; the policy file's own names are still offered";
  }
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
  return { tone, text, stored: null, registered: null, applied: null, actions: [], errors: [], warnings: [], restored: null, keepInputs: false, reread: false, blockEditing: false };
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
    // A 200 can still carry warnings (a directory sync that failed, a record that could not be read
    // back). They do not make the write a failure, but dropping them would report an unqualified
    // save while the server is saying its durability or its registration is uncertain.
    notice.warnings = Array.isArray(body.warnings) ? body.warnings.map((warning) => stringOf(warning)) : [];
    const parts = [`Stored ${digest12(notice.stored)}`];
    parts.push(notice.registered === null ? "registered: not read back" : `registered ${digest12(notice.registered)}`);
    if (notice.applied === "applied") parts.push("the running relay holds these bytes");
    else if (notice.applied === "needs_user_action") {
      parts.push("the running relay still holds the old bytes");
      if (notice.actions.length > 0) parts.push(`to apply it: ${notice.actions.join("; ")}`);
    } else if (notice.applied === "unverifiable") parts.push("whether the running relay holds these bytes could not be read");
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
 *
 * A stale check is a conflict first. The route answers stale whenever the caller's digest is no
 * longer the file's (internal/policystore/check.go), and it can be stale AND invalid at once - the
 * file moved and the change no longer applies to it. Reporting only the invalidity would leave the
 * operator retrying a change against a file they have not seen, so the stale case keeps the inputs
 * and asks for a re-read, exactly as the write route's 409 does.
 */
export function checkNotice(result: PolicyCheckResult): PolicyNotice {
  if (result.stale) {
    const notice = emptyNotice("err", `The policy changed elsewhere since it was read, so this change was not checked against the current file. The file has been read again; your inputs are kept. Check the new values, then save again.${result.errors.length > 0 ? ` The change was also refused: ${result.errors.join("; ")}` : ""}`);
    notice.keepInputs = true;
    notice.reread = true;
    notice.errors = result.errors;
    return notice;
  }
  if (result.valid) {
    const notice = emptyNotice("info", `The policy check accepts this change: ${result.diff.join(", ")}`);
    return notice;
  }
  const notice = emptyNotice("err", result.errors.length > 0 ? `The policy check refused this change: ${result.errors.join("; ")}` : "The policy check refused this change.");
  notice.errors = result.errors;
  return notice;
}

/**
 * lostWriteNotice is the sentence a write whose transport failed becomes. It is deliberately NOT
 * "the policy was not changed": the server detaches the registration phase from the request once it
 * has replaced the file, so a dropped connection can leave a write that completed with no answer.
 * The only honest reading is that the result is unknown, and the screen re-reads to find out.
 */
export function lostWriteNotice(): PolicyNotice {
  const notice = emptyNotice("err", "The connection was lost before the server answered, so whether this change was written is unknown. The policy is being read again to find out; check the digest before retrying.");
  notice.keepInputs = true;
  notice.reread = true;
  return notice;
}

/** unreachableNotice is the sentence a request that could not be answered at all becomes. */
export function unreachableNotice(): PolicyNotice {
  const notice = emptyNotice("err", "The backend could not be reached, so nothing was sent.");
  notice.keepInputs = true;
  return notice;
}

/* ---- the screen's state transitions ---- */

/** initialScreen is the state before anything has been read. */
export function initialScreen(): PolicyScreenState {
  return { reading: null, catalog: null, error: null, change: null, allowedText: {}, allowedAddModel: "", exceptionDraft: null, saving: null, savingDrafts: null, notice: null, repair: null };
}

/**
 * screenLoaded applies a freshly read policy. keepInputs is true when the read follows a conflict or
 * a lost response: the decided answer keeps the operator's inputs across a stale re-read, so the
 * pending change and the raw text stay; an ordinary load (the first one, or the one after a
 * successful save) starts from the file.
 */
export function screenLoaded(state: PolicyScreenState, reading: PolicyReading, keepInputs = false): PolicyScreenState {
  // A reading that shows the host repaired lifts the block; any other reading leaves it, because the
  // server still refuses writes while the file and the wiring record disagree.
  const repair = screenRepairCleared(reading) ? null : state.repair;
  return {
    ...state,
    reading,
    error: null,
    repair,
    ...(keepInputs ? {} : { change: null, allowedText: {}, allowedAddModel: "", exceptionDraft: null }),
  };
}

/** screenLoadFailed records that the policy itself could not be read. */
export function screenLoadFailed(state: PolicyScreenState, message: string): PolicyScreenState {
  return { ...state, reading: null, error: message };
}

/** screenCatalog applies a catalog answer. */
export function screenCatalogLoaded(state: PolicyScreenState, catalog: ModelCatalog): PolicyScreenState {
  return { ...state, catalog };
}

/** screenPropose sets the one pending change and clears the previous attempt's notice. */
export function screenPropose(state: PolicyScreenState, change: PolicyChange | null): PolicyScreenState {
  // A cancel (a null change) also drops the drafts that produced the change: leaving the typed text
  // behind would show a value the operator just abandoned, and would survive the re-read that a
  // later successful save triggers. A non-null change keeps the drafts so a multi-step edit can go on.
  if (change === null) {
    return { ...state, change: null, notice: null, allowedText: {}, exceptionDraft: null };
  }
  return { ...state, change, notice: null, exceptionDraft: null };
}

/**
 * screenAllowedText records the raw text of one allowed-list input and derives the pending change
 * from it. The raw text is what the input renders, so a trailing comma the operator is about to
 * follow with another entry survives; an empty parse clears the pending change rather than sending
 * a list the server refuses.
 */
export function screenAllowedText(state: PolicyScreenState, model: string, text: string): PolicyScreenState {
  const efforts = parseListInput(text);
  return {
    ...state,
    allowedText: { ...state.allowedText, [model]: text },
    change: efforts.length === 0 ? null : { kind: "setAllowed", model, efforts },
    notice: null,
  };
}

/**
 * screenAllowedAddModel records which model the "add to the allowed list" select is on. It is part
 * of the state rather than component-local because the select's value must follow the options: a
 * catalog that arrives after the policy would otherwise leave the control showing one model and
 * sending another.
 */
export function screenAllowedAddModel(state: PolicyScreenState, model: string): PolicyScreenState {
  return { ...state, allowedAddModel: model };
}

/**
 * allowedAddChoice is the model the add control is on: the one the operator chose when it is still
 * a free option, and the first free one otherwise. "" means there is nothing left to add.
 */
export function allowedAddChoice(state: PolicyScreenState, free: readonly string[]): string {
  // A chosen model that is no longer free is kept rather than substituted: the operator's selection
  // is not the screen's to replace, and Add must propose the model the control shows.
  return state.allowedAddModel !== "" ? state.allowedAddModel : (free[0] ?? "");
}

/** addModelOptions is what the add-allowed select offers: every free model, plus the chosen one. */
export function addModelOptions(state: PolicyScreenState, free: readonly string[]): string[] {
  if (state.allowedAddModel === "" || free.includes(state.allowedAddModel)) return [...free];
  return [state.allowedAddModel, ...free];
}

/** allowedTextOf is the text one allowed-list input shows: the draft first, then the saved list. */
export function allowedTextOf(state: PolicyScreenState, model: string, saved: readonly string[]): string {
  const text = state.allowedText[model];
  return text !== undefined ? text : saved.join(", ");
}

/** screenExceptionDraft opens or updates the exception editor. */
export function screenExceptionDraft(state: PolicyScreenState, draft: ExceptionDraft | null): PolicyScreenState {
  return { ...state, exceptionDraft: draft };
}

/**
 * changeFromExceptionDraft is the change an exception draft proposes, or null when it is not yet
 * complete. An empty id or model would be refused by the check, so it is not proposed at all.
 */
export function changeFromExceptionDraft(draft: ExceptionDraft): PolicyChange | null {
  // An existing exception's id is carried through byte for byte: the policy file is the authority on
  // its identifiers and the store finds the entry by exact match, so trimming a stored " legacy "
  // would create a different exception and leave the original untouched. Only a NEW id, which the
  // operator is inventing, is trimmed.
  const id = draft.isNew ? draft.id.trim() : draft.id;
  const model = draft.model.trim();
  if (id === "" || model === "") return null;
  // An exception with no cwd covers no request: the bridge requires a request to state a cwd the
  // exception lists (internal/bridge/execution/execution.go exceptionCovers), so a root-less
  // exception would be stored and then never apply. The editor requires at least one root.
  const cwd = draft.cwd.filter((root) => root !== "");
  if (cwd.length === 0) return null;
  // The roots are already a list, so a path containing a comma is carried through unchanged.
  const change: PolicyChange = { kind: "setException", id, model, effort: draft.effort, cwd };
  // An empty role is left off the request: the server then keeps the role an existing exception
  // records. The screen only produces this for an exception that already has no role.
  if (draft.role !== "") change.role = draft.role;
  return change;
}

/** draftForException opens the editor on an existing exception, with its recorded values. */
export function draftForException(exception: PolicyExceptionView): ExceptionDraft {
  return { id: exception.id, isNew: false, role: exception.role ?? "", model: exception.model, effort: exception.reasoningEffort, cwd: [...exception.cwd], cwdNew: "" };
}

/** draftForNewException opens the editor on a new exception, on a real role and a real model. */
export function draftForNewException(role: string, model: string, effort: string): ExceptionDraft {
  return { id: "", isNew: true, role, model, effort, cwd: [], cwdNew: "" };
}

/** screenDraftIsNew reports whether the open draft is a new exception rather than an edit. */
export function screenDraftIsNew(state: PolicyScreenState): boolean {
  return state.exceptionDraft?.isNew === true;
}

/** screenSaveStarted marks the change a save in flight is writing. */
export function screenSaveStarted(state: PolicyScreenState): PolicyScreenState {
  return { ...state, saving: state.change, savingDrafts: { allowedText: { ...state.allowedText }, exceptionDraft: state.exceptionDraft } };
}

/**
 * screenSaveFinished applies a write answer. The pending change is cleared only when it is still the
 * one that was saved: an edit made while the save was in flight is the operator's next change and is
 * never dropped by a response to the previous one.
 */
export function screenSaveFinished(state: PolicyScreenState, saved: PolicyChange | null, notice: PolicyNotice): PolicyScreenState {
  const stillPending = saved !== null && state.change === saved;
  let change = state.change;
  let allowedText = state.allowedText;
  let exceptionDraft = state.exceptionDraft;
  let repair = state.repair;
  if (notice.blockEditing) {
    change = null;
    // The repair a person must make is recorded separately from the notice so a later re-read cannot
    // silently lift the block: the server keeps refusing every write until it is done.
    repair = notice.text;
  }
  else if (notice.tone === "ok" && saved !== null) {
    // The write landed, so the drafts that produced it are spent. Only THOSE drafts are dropped: an
    // edit the operator made while the save was in flight is their next change and stays, even when
    // it is for the same model or exception. The drafts are compared against the snapshot taken when
    // the save started, so a draft that has not moved since is the spent one and a changed one is not.
    const before = state.savingDrafts;
    if (saved.kind === "setAllowed" && allowedText[saved.model] === before?.allowedText[saved.model]) {
      const { [saved.model]: _spent, ...rest } = allowedText;
      allowedText = rest;
    }
    if (saved.kind === "setException" && exceptionDraft !== null && exceptionDraft === before?.exceptionDraft) {
      exceptionDraft = null;
    }
    // The pending change is cleared only when it is still the one that was saved; a later edit is
    // the operator's next change.
    if (stillPending) change = null;
  }
  return { ...state, saving: null, savingDrafts: null, notice, change, allowedText, exceptionDraft, repair };
}

/**
 * screenReread is the explicit re-read: it drops the pending change and the previous notice. The
 * repair block is deliberately NOT dropped here - it is cleared only when a fresh read shows a
 * registered policy whose file digest matches the digest the wiring record names, which is what the
 * server requires before it will accept a write again.
 */
export function screenReread(state: PolicyScreenState): PolicyScreenState {
  // The notice goes, but the repair sentence does not: it is the server's own instruction for the
  // repair, and the block it explains is still in force. screenRepairCleared lifts both together.
  return { ...state, change: null, allowedText: {}, allowedAddModel: "", exceptionDraft: null, notice: null };
}

/**
 * screenRepairCleared reports whether a fresh reading shows the host repaired: a registered policy
 * whose file digest is the one the wiring record names. Anything else leaves the block in place.
 */
export function screenRepairCleared(reading: PolicyReading): boolean {
  if (reading.state !== "registered") return false;
  return reading.digest !== "" && reading.digest === reading.registeredDigest;
}

/**
 * screenEditable is whether the editing controls are live: the file must be registered and no
 * notice may be blocking on a repair a person has to make first.
 */
export function screenEditable(state: PolicyScreenState): boolean {
  if (state.reading === null || state.reading.state !== "registered") return false;
  return state.repair === null;
}

/** screenSaving is whether a save is in flight; the controls are disabled while it is. */
export function screenSaving(state: PolicyScreenState): boolean {
  return state.saving !== null;
}

/**
 * screenEffortUnavailable is whether one effort name is refused for one model. Only an advertised
 * ladder is evidence that a model refuses a name (effort-support.ts), so an unreported ladder or no
 * catalog at all leaves every name selectable.
 */
export function screenEffortUnavailable(state: PolicyScreenState, model: string, effort: string): boolean {
  return effortExcluded(modelLadder(state.catalog, model), effort);
}

/* ---- the accessible names the screen renders every control from ---- */

/**
 * The accessible name of a control on the screen. They live here rather than in the .tsx so the
 * label a control carries is built by a pure function the tests can call; the component renders
 * every aria-label from these, so a control cannot appear unlabelled.
 */
export function roleControlsLabel(role: string): string {
  return `${role} pair controls`;
}

export function pairModelLabel(role: string, index: number): string {
  return `${role} pair ${index + 1} model`;
}

export function pairEffortLabel(role: string, index: number): string {
  return `${role} pair ${index + 1} effort`;
}

export function allowedEffortsLabel(model: string): string {
  return `${model} allowed efforts`;
}

export function removeExceptionLabel(id: string): string {
  return `Remove exception ${id}`;
}

export function editExceptionLabel(id: string): string {
  return `Edit exception ${id}`;
}

/**
 * Every control the screen renders is one of these native elements, which the browser makes
 * keyboard operable and focusable by itself. The screen adds no custom widget, no tabindex of its
 * own and no key handler, so keyboard operability is a property of the element kind rather than of
 * code this screen writes. A test pins the list so a later edit that introduces a non-native control
 * has to change it deliberately.
 */
export const POLICY_CONTROL_ELEMENTS = ["select", "input", "button", "fieldset"] as const;
