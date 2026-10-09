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
  warnings?: string[];
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
  /** Set on a lost write response: its starting digest and what the re-read found. */
  lost: LostWrite | null;
}

/**
 * A lost write: a request whose result the screen does not know, because its response never arrived
 * or arrived unreadable. The server may still have replaced the file and registered it, so the result
 * is settled only by comparing the change this request proposed with the file a later read returns
 * (judgeLostWrite), never by the digest alone: a digest another write moved says nothing about this
 * change.
 *
 * The outcome is "unknown" until a registered reading settles it, "stored" when that reading holds the
 * proposed change and the wiring record names it, and "not_stored" only on evidence that the write
 * ended without storing the change: the file moved to bytes the wiring record names that do not hold
 * the change (another write finished), or the file was put back after a reading showed it holding the
 * change (the registration failed and its restore ran). A reading at the starting digest settles
 * nothing, however many of them there are: the request may still be waiting for the policy lock, or be
 * past its last cancellation check inside a file exchange that has no deadline. The screen reads on a
 * timer for LOST_SETTLE_MS and then stops reading and leaves the result unknown, because the elapsed
 * time is not evidence that the write ended. A verdict is not final while the screen is reading: every
 * later registered reading judges again, so a registration that fails after the file was read (and
 * puts the file back) turns a "stored" or an awaiting verdict into "not_stored".
 */
export interface LostWrite {
  fromDigest: string;
  /** The change the lost request proposed, or null when the caller could not name it. */
  change: PolicyChange | null;
  outcome: "unknown" | "stored" | "not_stored";
  storedDigest: string;
  /**
   * True while the file holds the change but the wiring record has not caught up: the server's
   * registration is still running (it is detached from the request) or has failed and its restore has
   * not landed. The result stays unknown and the screen reads again.
   */
  awaitingRegistration: boolean;
  /**
   * True while the write may still change the file, so the screen keeps reading on a timer: from the
   * lost answer until a verdict, within LOST_SETTLE_MS of the lost answer.
   */
  watching: boolean;
  /** True once a reading showed the file holding the change, so a later return to the start is a restore. */
  sawChange: boolean;
  /** When the answer was lost (Date.now()); the screen's reading is bounded by the time since. */
  startedAt: number;
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
  /**
   * The allowlist rows being edited, keyed by the exact model identifier: one entry per approved
   * effort. A comma-separated field could not be lossless, because the Go policy parser does not
   * forbid a comma inside an effort name and compares the value exactly, so splitting one stored
   * entry on a comma would replace one approval with two (and dropping the commas would change the
   * name). Each entry is edited in its own field.
   *
   * It is a Map rather than a plain object because the key is a value the policy file declares. The
   * Go parser accepts any nonempty identifier as a model name, so a policy may legitimately name a
   * model "__proto__", "constructor" or "toString"; as an ordinary object's key that would read an
   * inherited property instead of the draft (or silently set the prototype on write). A Map has no
   * prototype chain to collide with, so every key is exactly the string the file declared.
   */
  allowedDraft: Map<string, string[]>;
  /** The text of each allowlist row's "add an effort" field. */
  allowedNew: Map<string, string>;
  /** The model the "add a model to the allowed list" select is on, or "" for its first free one. */
  allowedAddModel: string;
  /**
   * The model the Add control last proposed and that the file does not list yet, or "". It is held
   * apart from `change` because the operator is editing the row it created: clearing the row's only
   * effort entry empties the change (the server refuses an empty list) but must not make the row, and
   * the field the operator is typing in, disappear.
   */
  pendingAllowed: string;
  /** The exception editor's draft, or null when it is closed. */
  exceptionDraft: ExceptionDraft | null;
  /** The change a save in flight is writing, or null when no save is in flight. */
  saving: PolicyChange | null;
  /**
   * The drafts as they were when the save in flight started, or null. A draft that has not moved
   * since is the one the write spent and is dropped when it lands; a draft the operator changed
   * while the write was in flight is their next edit and stays, even when it is for the same model
   * or exception. The screen disables every other edit control while a write is in flight, so this
   * is the belt to that brace: even a programmatic edit cannot make a spent draft look unspent.
   */
  savingDrafts: { allowedDraft: Map<string, string[]>; exceptionDraft: ExceptionDraft | null } | null;
  /**
   * True while a read, a check or a save is in flight. Every edit control is disabled then, which is
   * what makes "one pending change at a time" hold without a race: an edit cannot be started while an
   * answer is on its way, so a re-read can never silently replace a draft begun after it started and
   * a save's answer can never land on top of an edit it did not write.
   */
  busy: boolean;
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
 * isBlank is the only check the screen makes on a value a person typed into a free-text box: an
 * obviously blank value is refused because the server would refuse it too. It is deliberately not a
 * re-implementation of the server's own blankness test - the server judges the exact bytes, and an
 * identifier is otherwise opaque here and is carried exactly as typed, never trimmed or re-parsed.
 */
function isBlank(value: string): boolean {
  return value.trim() === "";
}

/**
 * isBlankText is the same blankness check exposed to the screen, which needs it to disable a control
 * whose free-text box is still empty. It is the only judgement the screen makes on typed text; the
 * value itself is always carried exactly as typed.
 */
export function isBlankText(value: string): boolean {
  return isBlank(value);
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

/**
 * quoted is one identifier as a preview row reads it. Every value a preview shows is an identifier
 * the policy compares exactly and may contain a space or a comma, so it is quoted: a plain join
 * would make the pair ("a b", "c") and the pair ("a", "b c") read as the same text, and one effort
 * named "low, high" read the same as two efforts named "low" and "high".
 */
function quoted(value: string): string {
  return JSON.stringify(value);
}

/**
 * entryRows is one list as the preview shows it: one row per entry, so two different lists can never
 * render the same text and a reader can see exactly which entry is added, changed or removed. An
 * entry that is gone reads "removed", never an empty cell a reader could mistake for "no change".
 */
function entryRows(label: string, before: readonly string[], after: readonly string[]): PolicyPreviewItem[] {
  const rows: PolicyPreviewItem[] = [];
  const count = Math.max(before.length, after.length);
  for (let index = 0; index < count; index += 1) {
    rows.push({
      label: `${label} ${index + 1}`,
      before: index < before.length ? quoted(before[index]) : "none",
      after: index < after.length ? quoted(after[index]) : "removed",
    });
  }
  return rows;
}

/** pairRowText is one pair as a preview row reads it: both identifiers quoted, so the model and the
 * effort each stay exactly one value however they are spelled. */
function pairRowText(pair: PolicyPair): string {
  return `${quoted(pair.model)} ${quoted(pair.reasoningEffort)}`;
}

/** pairRows is a pair list as the preview shows it: one row per pair. */
function pairRows(label: string, before: readonly PolicyPair[], after: readonly PolicyPair[]): PolicyPreviewItem[] {
  const rows: PolicyPreviewItem[] = [];
  const count = Math.max(before.length, after.length);
  for (let index = 0; index < count; index += 1) {
    rows.push({
      label: `${label} ${index + 1}`,
      before: index < before.length ? pairRowText(before[index]) : "none",
      after: index < after.length ? pairRowText(after[index]) : "removed",
    });
  }
  return rows;
}

/**
 * exceptionRows is one exception as the preview shows it: a row for each part that can change, and
 * one row per cwd root, so a scope with two roots never reads as a scope with one root that contains
 * a comma.
 */
function exceptionRows(label: string, before: { role?: string; model: string; reasoningEffort: string; cwd: string[] } | null, after: { role?: string; model: string; reasoningEffort: string; cwd: string[] } | null): PolicyPreviewItem[] {
  // A side that is null is the exception absent: the whole side reads one word rather than a set of
  // empty cells a reader could mistake for "unchanged".
  const roleText = (exception: { role?: string } | null, absent: string): string => {
    if (exception === null) return absent;
    // The role is the one part whose absence carries a meaning: the bridge matches an exception's
    // role against the request's role with exact equality (internal/bridge/execution/execution.go
    // exceptionCovers), so a role-less exception covers exactly the requests that cite no role - a
    // live authorization the schema allows. "none" would understate it.
    return exception.role === undefined || exception.role === "" ? "no role - covers requests that cite no role" : quoted(exception.role);
  };
  const part = (value: string | undefined, absent: string): string => (value === undefined || value === "" ? absent : quoted(value));
  return [
    { label: `${label} role`, before: roleText(before, "not declared"), after: roleText(after, "removed") },
    { label: `${label} model`, before: part(before?.model, "not declared"), after: part(after?.model, "removed") },
    { label: `${label} effort`, before: part(before?.reasoningEffort, "not declared"), after: part(after?.reasoningEffort, "removed") },
    ...entryRows(`${label} cwd`, before?.cwd ?? [], after?.cwd ?? []),
  ];
}

/**
 * previewChange is the before/after of one pending change, plus the blast radius. It is derived
 * from the reading the caller already has, so the preview is shown before anything is sent; the
 * server's own check is what judges whether the change would be accepted.
 *
 * Every row names ONE entry and quotes it, so two different lists can never render the same text and
 * a reader can see exactly which entry is added, changed or removed rather than comparing two
 * comma-joined blobs that a comma inside an identifier would make ambiguous.
 */
export function previewChange(reading: PolicyReading, change: PolicyChange): PolicyPreview {
  const items: PolicyPreviewItem[] = [];
  const preview: PolicyPreview = { items, blastRadius: POLICY_BLAST_RADIUS };
  switch (change.kind) {
    case "setRolePairs": {
      const role = reading.roles.find((entry) => entry.name === change.role);
      items.push(...pairRows(`role ${change.role} pair`, role?.pairs ?? [], change.pairs));
      break;
    }
    case "setAllowed": {
      const entry = reading.allowed.find((row) => row.model === change.model);
      items.push(...entryRows(`allowed ${change.model} effort`, entry?.efforts ?? [], change.efforts));
      break;
    }
    case "removeAllowed": {
      const entry = reading.allowed.find((row) => row.model === change.model);
      const rows = entryRows(`allowed ${change.model} effort`, entry?.efforts ?? [], []);
      // A model the file does not list produces no per-entry rows; without this the preview would be
      // empty rather than saying which row would leave. (The server refuses such a removal, but the
      // screen must still show what it was about to ask for.)
      items.push(...(rows.length > 0 ? rows : [{ label: `allowed ${change.model}`, before: "not listed", after: "removed" }]));
      break;
    }
    case "setException": {
      const existing = reading.exceptions.find((row) => row.id === change.id);
      // The server keeps the role an existing exception already records when the change omits it
      // (internal/policystore/check.go applySetException), so the preview must show that preserved
      // role rather than an "any role" the write would not produce.
      const role = change.role !== undefined && change.role !== "" ? change.role : existing?.role;
      items.push(...exceptionRows(`exception ${change.id}`, existing ?? null, { role, model: change.model, reasoningEffort: change.effort, cwd: change.cwd }));
      break;
    }
    case "removeException": {
      const existing = reading.exceptions.find((row) => row.id === change.id);
      const scope = existing && existing.cwd.length > 0 ? existing.cwd.join(", ") : "the exception's scope";
      items.push(...exceptionRows(`exception ${change.id}`, existing ?? null, null));
      // What the removal actually does depends on the exception's ROLE EXPECTATION and on the file's
      // MODE, not on whether the role happens to list a pair.
      //
      // The expectation decides whether the pair check applies: the bridge skips it for a record role
      // (internal/bridge/execution/execution.go Authorize runs the pair branch only when the
      // expectation is "pair"), so a request that no longer cites the removed exception falls through
      // - for the supervisor exactly as for a role-less exception. A record role never has a pair to
      // fall back to and the server refuses it declaring one (internal/bridge/execution/roles.go
      // parseRole), so telling the operator to add one would name a repair the server will not accept.
      //
      // The mode decides what the fall-through reaches: Authorize checks the allowed list only when
      // the file declares one (p.allowed != nil, internal/policystore/policy.go Mode), so a
      // presence_only file has NO allowlist check and the fall-through is unrestricted. Claiming an
      // allowlist there would promise a narrower permission boundary than the host enforces.
      const role = existing?.role;
      const declared = role !== undefined ? reading.roles.find((entry) => entry.name === role) : undefined;
      const record = declared?.expectation === "record";
      const allowlist = reading.mode === "allowlist" || reading.allowed.length > 0;
      const fallThrough = allowlist
        ? "checked against the allowed list"
        : "not checked against any list, because this policy declares no allowlist, so it is allowed";
      if (role === undefined) {
        preview.fallback = `Removing this exception removes the scope ${scope} for requests that cite no role; such a request is then ${fallThrough}, and a request still citing this exception is refused as unknown.`;
      } else if (record) {
        preview.fallback = allowlist
          ? `Removing this exception returns ${scope} to the allowed list: a request that still cites the removed exception id is refused as unknown, and one that does not cite it is checked against the allowed list. A ${role} role is declared with a record expectation, so it has no pair default to return to.`
          : `Removing this exception leaves ${scope} with nothing to fall back to: this policy declares no allowlist, so a request that still cites the removed exception id is refused as unknown and one that does not cite it is allowed. A ${role} role is declared with a record expectation, so it has no pair default to return to.`;
      } else if (declared !== undefined) {
        preview.fallback = allowlist
          ? `Removing this exception returns ${scope} to the ${role} role default, and the request must also appear in the allowed list. A request that still cites the removed exception id is refused as unknown before that default is reached.`
          : `Removing this exception returns ${scope} to the ${role} role default. This policy declares no allowlist, so nothing else is checked. A request that still cites the removed exception id is refused as unknown before that default is reached.`;
      } else {
        preview.fallback = `This file declares no ${role} role, so ${scope} has no default to return to: a request that cites ${role} is refused as unknown, and one that does not is ${fallThrough}.`;
      }
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
 *
 * Only a FRESH answer is current evidence. The reader answers status "stale" with the last successful
 * list exactly when the live read failed or this host's OCX does not support the command
 * (internal/role/livecatalog.go), so that cached ladder describes a moment that has passed; treating
 * it as evidence would disable an effort the policy file still allows, which is the one thing the
 * issue says a catalog that could not be read must never do. A non-fresh answer therefore advertises
 * no ladder at all, and every effort name stays selectable.
 */
export function modelLadder(catalog: ModelCatalog | null, model: string | null | undefined): readonly string[] | null {
  if (!model) return null;
  if (catalog === null || catalog.status !== "fresh") return null;
  return catalog.entries.find((entry) => entry.id === model)?.reasoningEfforts ?? null;
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
  return { tone, text, stored: null, registered: null, applied: null, actions: [], errors: [], warnings: [], restored: null, keepInputs: false, reread: false, blockEditing: false, lost: null };
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
export function noticeForWrite(status: number, body: unknown, proposal: LostProposal = { fromDigest: "", change: null }): PolicyNotice {
  if (status === 200) {
    if (!isWriteSuccess(body)) {
      // A 200 that confirms nothing is an answer whose result is unknown, the same as one that never
      // arrived: the file may hold the change, so the screen reads again and judges.
      return lostWriteNotice(proposal.fromDigest, proposal.change, "The server answered 200 without a stored digest, so whether this change was written is unknown.");
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
      // The server returns needs_user_action for TWO different situations: the running service has
      // not loaded these bytes, and the file and the wiring record disagree
      // (internal/policystore/running.go Applied decides the record mismatch BEFORE it looks at the
      // running digest, and AppliedActions then names the re-registration). The screen cannot tell
      // them apart, so it says the policy is not in force and repeats the server's own action rather
      // than asserting what the running relay holds.
      parts.push("the policy is not in force yet");
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
  // A refusal can carry warnings too (an undo whose directory entry was not synced, a restore that
  // was not durable). They qualify the refusal, so dropping them would hide a durability risk the
  // server reported.
  notice.warnings = Array.isArray(body.warnings) ? body.warnings.map((warning) => stringOf(warning)) : [];
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
    case "not_applied":
      // The file moved under the write and already agrees with the wiring record, so nothing needs
      // repairing: the change was not applied, and the screen reads the document again.
      notice.keepInputs = true;
      notice.reread = true;
      notice.text = `The policy file changed while this write was running, so the change was not applied. The file and the wiring record both name digest ${digest12(stringOf(body.fileDigest))}. The file has been read again; your inputs are kept. Check the new values, then save again.`;
      break;
    case "recovery_needed":
      notice.blockEditing = true;
      notice.text = `The file and the wiring record disagree (file ${digest12(stringOf(body.fileDigest))}, record ${digest12(stringOf(body.registeredDigest))}). Every write is refused until this is settled: ${stringOf(body.recovery)}`;
      break;
    case "cancelled":
      notice.text = `The write was cancelled during ${stringOf(body.step) || "an unknown step"}${stringOf(body.reason) !== "" ? ` (${stringOf(body.reason)})` : ""}${stringOf(body.fileDigest) !== "" ? `; the policy file had digest ${digest12(stringOf(body.fileDigest))} when the write ended` : ""}.`;
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
export function lostWriteNotice(fromDigest = "", change: PolicyChange | null = null, cause = "The connection was lost before the server answered, so whether this change was written is unknown."): PolicyNotice {
  const notice = emptyNotice("err", `${cause} The policy is being read again to find out; check the digest before retrying.`);
  notice.keepInputs = true;
  notice.reread = true;
  notice.lost = { fromDigest, change, outcome: "unknown", storedDigest: "", awaitingRegistration: false, watching: true, sawChange: false, startedAt: Date.now() };
  return notice;
}

/** What a write request proposed: the digest it started from and the change it carried. */
export interface LostProposal {
  fromDigest: string;
  change: PolicyChange | null;
}

/**
 * saveHeading is the headline the screen shows for a save notice. A lost write is headed by its own
 * result, never by the error tone: an indeterminate write is "Result unknown" until a re-read settles
 * it, so the screen never says "Not saved" for a change the server may have stored.
 */
export function saveHeading(notice: PolicyNotice | null): string {
  if (notice === null) return "";
  if (notice.lost !== null) {
    if (notice.lost.outcome === "stored") return "Saved";
    if (notice.lost.outcome === "not_stored") return "Not saved";
    return "Result unknown";
  }
  return notice.tone === "ok" ? "Saved" : "Not saved";
}

/**
 * readingHoldsChange is whether a registered reading's file contains the change a write proposed. It
 * is the one comparison of the intended change with the file that read: the policy is the document
 * the write would have produced, so a section is judged by exactly the members the change sets or
 * removes.
 */
export function readingHoldsChange(reading: PolicyReading, change: PolicyChange): boolean {
  switch (change.kind) {
    case "setRolePairs": {
      const role = reading.roles.find((entry) => entry.name === change.role);
      return role !== undefined && role.pairs.length === change.pairs.length
        && role.pairs.every((pair, index) => pair.model === change.pairs[index].model && pair.reasoningEffort === change.pairs[index].reasoningEffort);
    }
    case "setAllowed": {
      const entry = reading.allowed.find((candidate) => candidate.model === change.model);
      // The server stores a row's efforts sorted (internal/policystore allowedEntry), so the order the
      // operator typed them in is not part of what the file holds.
      return entry !== undefined && sameEntries(sortedEfforts(entry.efforts), sortedEfforts(change.efforts));
    }
    case "removeAllowed":
      return !reading.allowed.some((candidate) => candidate.model === change.model);
    case "setException": {
      const exception = reading.exceptions.find((candidate) => candidate.id === change.id);
      return exception !== undefined
        && exception.model === change.model
        && exception.reasoningEffort === change.effort
        && (change.role === undefined || exception.role === change.role)
        && exception.cwd.length === change.cwd.length
        && exception.cwd.every((root, index) => root === change.cwd[index]);
    }
    case "removeException":
      return !reading.exceptions.some((candidate) => candidate.id === change.id);
  }
}

/** How long the screen waits before it reads again while a lost write's result is still open. */
export const LOST_RECHECK_MS = 2000;
/**
 * How long the screen keeps reading after a write's answer was lost: the request's header read
 * (internal/gui command.go ReadHeaderTimeout, 10 s), the wait for the policy lock (internal/policystore
 * writeLockTimeout, 10 s), the post-publication phase (writeDecisionTimeout, two minutes), and a
 * margin. It is the time the screen is willing to follow the write, not a server-enforced end of it:
 * the file exchange between the lock and the decision phase has no deadline of its own, so a result
 * that is still undecided at this time stays unknown instead of becoming "not stored".
 */
export const LOST_SETTLE_MS = 150_000;

/** Whether the screen has followed a lost write for as long as it is willing to. */
function lostExpired(lost: LostWrite, now: number): boolean {
  return now - lost.startedAt >= LOST_SETTLE_MS;
}

/**
 * judgeLostWrite is the single place a lost write's result is decided: the change it proposed against
 * the file a registered reading returned. The digest alone never says "stored": a digest another
 * write moved is not this change, and a file that holds the change before the wiring record names it
 * is a registration still running (or about to be put back), not yet a result. Time never says "not
 * stored" either: when LOST_SETTLE_MS has passed the screen stops reading and the result stays
 * unknown.
 *
 *   - the file is at the digest the change started from: unknown (the request may still be waiting
 *     for the lock, or be about to exchange the file); not stored only when an earlier reading showed
 *     the file holding the change (the registration failed and its restore put the file back);
 *   - the file moved and the request cannot name its change: unknown;
 *   - the file moved, does not hold the change and the wiring record names it: not stored, another
 *     write finished. The server replaces the file only while it holds the starting digest
 *     (internal/policystore Write compares ExpectedDigest under the lock), so this request cannot
 *     publish over bytes the record already names;
 *   - the file moved, does not hold the change and the record names something else: unknown, because
 *     the bytes may be another writer's publication whose registration has not finished (and which
 *     puts the starting bytes back if it fails, letting this request publish after all), or an edit
 *     nobody registered;
 *   - the file holds the change and the record does not yet name it: unknown, awaiting registration;
 *   - the file holds the change and the record names the file: stored.
 */
export function judgeLostWrite(lost: LostWrite, reading: PolicyReading, at: number = Date.now()): { lost: LostWrite; text: string } {
  const from = lost.fromDigest;
  const now = reading.digest ?? "";
  const expired = lostExpired(lost, at);
  const open = { ...lost, storedDigest: "", awaitingRegistration: false, watching: !expired };
  const stopped = expired ? ` ${LOST_SETTLE_MS / 1000} seconds have passed since the answer was lost, so the screen has stopped reading on its own and cannot tell whether the request has ended: read the policy again to see where it is now.` : " The policy is being read again.";
  if (now === from) {
    if (lost.sawChange) {
      return {
        lost: { ...lost, outcome: "not_stored", storedDigest: "", awaitingRegistration: false, watching: false },
        text: `The policy file is back at digest ${digest12(from)}, the digest this change started from, after a reading showed it holding this change: the registration did not finish and the server put the file back, so the change was not stored. Your inputs are kept.`,
      };
    }
    return {
      lost: { ...open, outcome: "unknown" },
      text: `The policy file still has digest ${digest12(from)}, the digest this change started from, but the request may not have reached the file yet, so the result stays unknown.${stopped} Your inputs are kept; a save from a stale digest is refused by the server.`,
    };
  }
  if (lost.change === null) {
    return {
      lost: { ...lost, outcome: "unknown", storedDigest: "", awaitingRegistration: false, watching: false },
      text: `The policy file now has digest ${digest12(now)}, not the ${digest12(from)} this change started from, but this screen cannot tell whether the file holds this change, so the result stays unknown. Check the values before saving again.`,
    };
  }
  const registered = reading.registeredDigest ?? "";
  if (!readingHoldsChange(reading, lost.change)) {
    if (registered === now) {
      return {
        lost: { ...lost, outcome: "not_stored", storedDigest: "", awaitingRegistration: false, watching: false },
        text: `The policy file now has digest ${digest12(now)}, not the ${digest12(from)} this change started from, and the wiring record names it, but it does not hold this change: another write finished, so the change was not stored. Your inputs are kept; check the new values, then save again.`,
      };
    }
    return {
      lost: { ...open, outcome: "unknown" },
      text: `The policy file now has digest ${digest12(now)}, not the ${digest12(from)} this change started from, and it does not hold this change, but the wiring record names ${registered === "" ? "no digest" : digest12(registered)}: another write's registration may still be running, and if it fails the file is put back and this request may still be applied. The result stays unknown.${stopped} Your inputs are kept.`,
    };
  }
  if (registered !== now) {
    return {
      lost: { ...open, outcome: "unknown", awaitingRegistration: true, sawChange: true },
      text: `The policy file holds this change (digest ${digest12(now)}), but the wiring record still names ${registered === "" ? "no digest" : digest12(registered)}: the registration has not finished, and if it fails the file is put back. The result stays unknown until the record names the file or the file is put back.${stopped}`,
    };
  }
  return {
    lost: { ...lost, outcome: "stored", storedDigest: now, awaitingRegistration: false, watching: false, sawChange: true },
    text: `The policy file now has digest ${digest12(now)}, not the ${digest12(from)} this change started from, and it holds this change, so the change was stored. Check the values before saving again.`,
  };
}

/**
 * spendLostReading is a reading that judged nothing (one that is not registered, or a read that
 * failed) on a lost write the screen is still watching. It changes nothing but the clock: once
 * LOST_SETTLE_MS has passed the screen stops reading and the result stays what it was, so a record
 * that went away or a backend that keeps failing ends the automatic reading without a verdict.
 */
function spendLostReading(notice: PolicyNotice | null, at: number = Date.now()): PolicyNotice | null {
  if (notice === null || notice.lost === null || !notice.lost.watching || !lostExpired(notice.lost, at)) return notice;
  return { ...notice, lost: { ...notice.lost, watching: false } };
}

/**
 * resolveLostNotice judges a lost write from every registered reading that lands after it, not only
 * the first: a verdict follows the file, so a registration that finished later, or a restore that put
 * the file back, moves the heading with it. A reading that is not registered judges nothing.
 */
function resolveLostNotice(notice: PolicyNotice | null, reading: PolicyReading): PolicyNotice | null {
  if (notice === null || notice.lost === null) return notice;
  if (reading.state !== "registered") return spendLostReading(notice);
  const judged = judgeLostWrite(notice.lost, reading);
  return { ...notice, lost: judged.lost, text: judged.text };
}

/**
 * lostRecheckDelay is how long to wait before the next automatic read, or null when none is due: a
 * lost write is re-read on a timer while its result is open (the file is still at the starting digest,
 * holds the change under a record that has not caught up, or is another writer's unregistered
 * publication), and only within LOST_SETTLE_MS of the lost answer, however the readings ended.
 */
export function lostRecheckDelay(state: PolicyScreenState, at: number = Date.now()): number | null {
  const lost = state.notice?.lost ?? null;
  if (lost === null || !lost.watching || lostExpired(lost, at)) return null;
  if (state.busy || state.saving !== null) return null;
  return LOST_RECHECK_MS;
}

/** The timer the page schedules with; a test passes its own. */
export interface LostRecheckClock {
  set: (run: () => void, ms: number) => unknown;
  clear: (handle: unknown) => void;
}

/**
 * startLostRecheck is the wiring PolicyPage runs as an effect: when a read is due it schedules
 * readAgain(true) (a read that keeps the operator's inputs) after lostRecheckDelay, and returns the
 * function that cancels it. It schedules nothing when no read is due, so an explicit re-read (which
 * drops the notice), a settled verdict and an expired wait all end the reading.
 */
export function startLostRecheck(state: PolicyScreenState, readAgain: (keepInputs: boolean) => void, clock: LostRecheckClock = { set: (run, ms) => setTimeout(run, ms), clear: (handle) => clearTimeout(handle as ReturnType<typeof setTimeout>) }): () => void {
  const delay = lostRecheckDelay(state);
  if (delay === null) return () => {};
  const handle = clock.set(() => readAgain(true), delay);
  return () => clock.clear(handle);
}

/** unreachableNotice is the sentence a request that could not be answered at all becomes. */
export function unreachableNotice(): PolicyNotice {
  const notice = emptyNotice("err", "The backend could not be reached, so nothing was sent.");
  notice.keepInputs = true;
  return notice;
}

/**
 * checkRefusedNotice is the sentence a check call that the server ANSWERED with a non-2xx status
 * becomes. The guard refuses a write whose token is missing, stale or repeated with 403 forbidden
 * (internal/gui/guard.go), and a route can answer 404 or 405; none of those is an unreachable
 * backend, and reporting them as one would hide the operator's actual repair. The status and the
 * server's own error member are kept.
 */
export function checkRefusedNotice(status: number, body: unknown): PolicyNotice {
  const code = isObject(body) ? stringOf(body.error) : "";
  const notice = emptyNotice("err", `The policy check was refused (${status}${code === "" ? "" : ` ${code}`}), so nothing was written.`);
  if (code !== "") notice.errors = [code];
  notice.keepInputs = true;
  return notice;
}

/**
 * blockedWriteNotice is the sentence a save attempt becomes while the screen cannot edit. The reading
 * kept the operator's change across a conflict or a lost response, but the host now answers
 * not_registered or unreadable (or a repair is outstanding): sending a check would ask the server to
 * judge a change against a file this screen cannot read, so nothing is sent and the state and its
 * reason are named. The inputs stay, because a later readable state can still save them.
 */
export function blockedWriteNotice(state: PolicyScreenState): PolicyNotice {
  const reading = state.reading;
  // The block has three distinct causes and the sentence names the one in force, so it never sends the
  // operator to fix something that is not wrong: a standing repair, an absent record, or a file this
  // screen could not read.
  const why = state.repair !== null
    ? `a repair is still outstanding: ${state.repair}`
    : reading === null
      ? "the policy could not be read"
      : reading.state === "not_registered"
        ? "this host has no registered execution policy"
        : `the policy could not be read${reading.reason ? `: ${reading.reason}` : ""}`;
  const notice = emptyNotice("err", `The policy cannot be edited right now because ${why}, so nothing was sent. Your inputs are kept; save again once the policy can be edited.`);
  notice.keepInputs = true;
  return notice;
}

/* ---- the screen's state transitions ---- */

/**
 * runRead is the whole read-and-apply sequence, outside React so a test can drive it with a fake
 * transport and prove what C6 promises: after a refresh the values and their provenance are the same
 * ones the file declares, and a read that follows a conflict or a lost response keeps the operator's
 * inputs. It returns the next state; the caller only has to render it.
 *
 * The body is decoded here rather than by the caller, so a malformed answer becomes the screen's own
 * failure state instead of a half-populated screen.
 *
 * onStart receives the started state before the first await, exactly as runSave does, so the caller
 * publishes it: every edit control is disabled while the read is in flight, and no edit can be made
 * that the read's answer would then replace.
 */
export function runRead(
  state: PolicyScreenState,
  read: () => Promise<unknown>,
  keepInputs: boolean,
  onStart?: (started: PolicyScreenState) => void,
): Promise<{ state: PolicyScreenState; ok: boolean }> {
  // One pending change at a time: the read marks the screen busy before it starts, so no draft can be
  // begun that this read's answer would silently replace.
  const started = screenReadStarted(state);
  onStart?.(started);
  return read().then(
    (body) => {
      // A malformed answer is refused here rather than rendered as a half-populated screen, so the
      // decode failure becomes the same failure state a transport failure does.
      try {
        return { state: screenLoaded(started, decodePolicy(body), keepInputs), ok: true };
      } catch (error) {
        return { state: screenLoadFailed(started, error instanceof Error ? error.message : "The execution policy could not be read."), ok: false };
      }
    },
    (error: unknown) => ({
      state: screenLoadFailed(started, error instanceof Error ? error.message : "The execution policy could not be read."),
      ok: false,
    }),
  );
}

/** One policy call's raw answer, as the client returns it. */
export interface PolicyRawResponse {
  status: number;
  body: unknown;
}

/**
 * The two calls one save makes, injected so the sequence can be driven without a DOM or a network.
 * The screen passes the real API client; a test passes a fake that answers with the status and body
 * under test.
 */
export interface PolicyWriteTransports {
  check: (payload: { expectedDigest: string; change: PolicyChange }) => Promise<PolicyRawResponse>;
  write: (payload: { expectedDigest: string; change: PolicyChange }) => Promise<PolicyRawResponse>;
}

/** What one save did: the next state, and whether the caller must re-read. */
export interface PolicySaveOutcome {
  state: PolicyScreenState;
  /** True when the caller must read the policy again. */
  reread: boolean;
  /** True when that read keeps the operator's inputs (the conflict and lost-response paths). */
  rereadKeepsInputs: boolean;
  /** True when the write succeeded, which is when the caller raises the success notice. */
  saved: boolean;
}

/**
 * runSave is the whole check-then-write sequence, outside React so a test can drive it with a fake
 * transport. It returns the next state and what the caller must do next rather than performing the
 * re-read itself, which keeps the two effects (state and read) separable and the sequence testable.
 *
 * The check runs first because the server judges the change with the bridge's own parser: a refusal
 * there is a refusal the write would meet, and sending only an accepted change keeps the two answers
 * from disagreeing. A refusal or a conflict never reaches the write, so nothing touches the file.
 */
export async function runSave(state: PolicyScreenState, transports: PolicyWriteTransports, onStart?: (started: PolicyScreenState) => void): Promise<PolicySaveOutcome> {
  const change = state.change;
  const reading = state.reading;
  if (!change || !reading || state.saving !== null) {
    return { state, reread: false, rereadKeepsInputs: false, saved: false };
  }
  // A change the conflict path kept is only saveable while the screen can edit. If the host now
  // answers not_registered or unreadable (or a repair is outstanding), the screen says editing is
  // blocked, and sending a check would ask the server to judge a change against a file this screen
  // cannot read. Nothing is sent; the inputs stay for a later readable state.
  if (!screenEditable(state)) {
    return { state: screenSaveFinished(state, change, blockedWriteNotice(state)), reread: false, rereadKeepsInputs: true, saved: false };
  }
  const started = screenSaveStarted(state);
  // The started state is handed to the caller BEFORE the first await, so the screen disables its
  // controls while the check and the write are in flight rather than only after they have answered.
  onStart?.(started);
  const payload = { expectedDigest: reading.digest ?? "", change };
  // The check call's own answer is kept whole: a non-2xx status is a refusal the server sent, not a
  // backend that could not be reached, so it is reported as that refusal. Only a transport failure
  // (the promise rejecting) is the unreachable case.
  let checkAnswer: PolicyRawResponse;
  try {
    checkAnswer = await transports.check(payload);
  } catch {
    return { state: screenSaveFinished(started, change, unreachableNotice()), reread: false, rereadKeepsInputs: false, saved: false };
  }
  if (checkAnswer.status < 200 || checkAnswer.status >= 300) {
    return { state: screenSaveFinished(started, change, checkRefusedNotice(checkAnswer.status, checkAnswer.body)), reread: false, rereadKeepsInputs: true, saved: false };
  }
  let checked: PolicyCheckResult;
  try {
    checked = decodeCheck(checkAnswer.body);
  } catch {
    // A 2xx whose body is not the check answer is a malformed answer, not a refusal and not an
    // unreachable backend: the server accepted the request, and the screen cannot read its verdict.
    // It says exactly that and keeps the operator's inputs rather than guessing the change was
    // refused or claiming nothing was sent.
    const malformed = emptyNotice("err", "The policy check was answered with a body this screen could not read, so nothing was written. Try saving again.");
    malformed.keepInputs = true;
    return { state: screenSaveFinished(started, change, malformed), reread: false, rereadKeepsInputs: true, saved: false };
  }
  if (!checked.valid || checked.stale) {
    const refused = checkNotice(checked);
    return { state: screenSaveFinished(started, change, refused), reread: refused.reread, rereadKeepsInputs: refused.keepInputs, saved: false };
  }
  let notice: PolicyNotice;
  try {
    const answer = await transports.write(payload);
    notice = noticeForWrite(answer.status, answer.body, { fromDigest: reading.digest ?? "", change });
  } catch {
    // A lost response is not a lost write: the server finishes registration after it has replaced the
    // file, so the screen re-reads rather than claiming nothing changed. The request is remembered
    // with the change it proposed, because the re-read is judged against that change.
    notice = lostWriteNotice(reading.digest ?? "", change);
  }
  return { state: screenSaveFinished(started, change, notice), reread: notice.tone === "ok" || notice.reread, rereadKeepsInputs: notice.tone === "ok" || notice.keepInputs, saved: notice.tone === "ok" };
}

/** initialScreen is the state before anything has been read. */
export function initialScreen(): PolicyScreenState {
  return { reading: null, catalog: null, error: null, change: null, allowedDraft: new Map(), allowedNew: new Map(), allowedAddModel: "", pendingAllowed: "", exceptionDraft: null, saving: null, savingDrafts: null, busy: false, notice: null, repair: null };
}

/**
 * screenReadStarted marks a read, a check or a save as in flight. Every edit control is disabled
 * until the answer lands, so nothing the operator types can race the answer that is coming.
 */
export function screenReadStarted(state: PolicyScreenState): PolicyScreenState {
  return { ...state, busy: true };
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
  if (!keepInputs) {
    return {
      ...state,
      reading,
      notice: resolveLostNotice(state.notice, reading),
      error: null,
      busy: false,
      repair,
      change: null,
      allowedDraft: new Map<string, string[]>(),
      allowedNew: new Map<string, string>(),
      allowedAddModel: "",
      pendingAllowed: "",
      exceptionDraft: null,
    };
  }
  // A keeping re-read keeps what the operator is working on. A draft that proposes NOTHING is not
  // work in progress: it is a field the operator emptied and left, and keeping it would show an input
  // disconnected from both the file and any pending change. It is dropped, and the row shows the
  // file's values again.
  const drafts = state.change === null
    ? { allowedDraft: new Map<string, string[]>(), allowedNew: new Map<string, string>(), allowedAddModel: "", pendingAllowed: "" }
    : {};
  return {
    ...state,
    reading,
    notice: resolveLostNotice(state.notice, reading),
    error: null,
    busy: false,
    repair,
    ...drafts,
  };
}

/** screenLoadFailed records that the policy itself could not be read. */
export function screenLoadFailed(state: PolicyScreenState, message: string): PolicyScreenState {
  // A read that fails while a lost write's result is open changes nothing but the clock, so a backend
  // that keeps failing is retried until the wait has run out and then left to the operator.
  return { ...state, reading: null, error: message, busy: false, notice: spendLostReading(state.notice) };
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
    return { ...state, change: null, notice: null, allowedDraft: new Map<string, string[]>(), allowedNew: new Map<string, string>(), pendingAllowed: "", exceptionDraft: null };
  }
  // One pending change at a time: a proposal from a control that does not own the live edit, or one
  // made while an answer is in flight, is refused, so the pending change is never silently replaced
  // by another row's edit. The controls are disabled in the same condition, so this is the state's
  // own guard rather than the only one.
  //
  // The exception editor is the one control whose token moves while it is open: a NEW exception's id
  // is still being typed, so the open draft - not the id in the change - is what owns the edit. A
  // setException proposed from that draft is therefore allowed even though the tokens differ. Every
  // other exception change still has to match the token, so a removal cannot steal the editor's turn.
  const fromOpenExceptionDraft = change.kind === "setException" && state.exceptionDraft !== null;
  if (!screenMayEdit(state, changeOwner(change)) && !(fromOpenExceptionDraft && !screenBusy(state))) return state;
  // An existing exception's editor closes on Apply: its values are now the pending change and the row
  // itself offers Edit again. A NEW exception's editor stays open, because the exception it describes
  // is not in the file yet: there is no row to reopen, and closing would leave the operator unable to
  // correct the values or to fix a check refusal without discarding the whole draft and retyping it.
  const keepNewDraft = change.kind === "setException" && state.exceptionDraft?.isNew === true;
  // A setAllowed proposed by the Add control is the model whose row must survive an empty
  // intermediate edit, so the model is remembered apart from the change.
  const pendingAllowed = change.kind === "setAllowed" ? change.model : state.pendingAllowed;
  return { ...state, change, notice: null, pendingAllowed, exceptionDraft: keepNewDraft ? state.exceptionDraft : null };
}

/** changeOwner is the edit token one proposed change belongs to. */
function changeOwner(change: PolicyChange): string {
  switch (change.kind) {
    case "setRolePairs":
      return `role:${change.role}`;
    case "setAllowed":
    case "removeAllowed":
      return `allowed:${change.model}`;
    case "setException":
    case "removeException":
      return `exception:${change.id}`;
  }
}

/**
 * allowedEntriesOf is the efforts an allowlist row is being edited as: the draft, then a pending
 * setAllowed for the same model, then the file. The pending change is consulted because a model the
 * Add control just proposed has no file row yet, and its row must show (and be able to edit) the
 * entries the pending change carries rather than an empty list.
 */
export function allowedEntriesOf(state: PolicyScreenState, model: string, saved: readonly string[]): string[] {
  // A Map lookup by the exact identifier: a model may legitimately be named "__proto__" or
  // "constructor" (the Go parser accepts any nonempty identifier), and as an ordinary object's key
  // that would read an inherited property rather than the draft.
  const draft = state.allowedDraft.get(model);
  if (draft !== undefined) return [...draft];
  const change = state.change;
  if (change?.kind === "setAllowed" && change.model === model) return [...change.efforts];
  return [...saved];
}

/** allowedNewOf is the text of a row's "add an effort" field. */
export function allowedNewOf(state: PolicyScreenState, model: string): string {
  return state.allowedNew.get(model) ?? "";
}

/**
 * allowedDraftModel is the model whose allowlist row holds the live draft, or "". The screen keeps at
 * most one row's draft at a time, so the first (and only) key is the row the operator is editing.
 * The draft can be blank while the operator replaces its only entry, and the row still owns the edit
 * then: without that, another row's edit would replace the map and silently discard the draft.
 */
export function allowedDraftModel(state: PolicyScreenState): string {
  for (const model of state.allowedDraft.keys()) return model;
  return "";
}

/** screenAllowedDraft stores one row's entries and derives the pending change from them. An empty
 * list clears the pending change rather than sending a list the server refuses. */
export function screenAllowedDraft(state: PolicyScreenState, model: string, entries: string[]): PolicyScreenState {
  // One pending change at a time: a row that does not own the live edit, or an edit made while an
  // answer is in flight, is refused rather than silently replacing the pending one. The control is
  // disabled in the same condition.
  if (!screenMayEdit(state, `allowed:${model}`)) return state;
  const efforts = entries.filter((text) => text !== "");
  return {
    ...state,
    // Only the row being edited holds a draft. The API applies exactly one change per request, so a
    // second row's edit replaces the first: keeping the first row's draft would leave it displaying a
    // value that no pending change carries and that a reload would silently revert.
    allowedDraft: new Map([[model, [...entries]]]),
    allowedNew: new Map([[model, allowedNewOf(state, model)]]),
    change: efforts.length === 0 ? null : { kind: "setAllowed", model, efforts },
    notice: null,
  };
}

/** screenAllowedEntryText records one entry's text, without interpreting it. */
export function screenAllowedEntryText(state: PolicyScreenState, model: string, saved: readonly string[], index: number, text: string): PolicyScreenState {
  return screenAllowedDraft(state, model, allowedEntriesOf(state, model, saved).map((current, at) => (at === index ? text : current)));
}

/** screenAllowedEntryAdded appends one entry the operator typed. */
export function screenAllowedEntryAdded(state: PolicyScreenState, model: string, saved: readonly string[], text: string): PolicyScreenState {
  if (text === "") return state;
  const next = { ...state, allowedNew: new Map([[model, ""]]) };
  return screenAllowedDraft(next, model, [...allowedEntriesOf(state, model, saved), text]);
}

/** screenAllowedNewText records the text of a row's "add an effort" field. */
export function screenAllowedNewText(state: PolicyScreenState, model: string, text: string): PolicyScreenState {
  if (!screenMayEdit(state, `allowed:${model}`)) return state;
  return { ...state, allowedNew: new Map(state.allowedNew).set(model, text) };
}

/** screenAllowedEntryRemoved drops one entry from a row. */
export function screenAllowedEntryRemoved(state: PolicyScreenState, model: string, saved: readonly string[], index: number): PolicyScreenState {
  return screenAllowedDraft(state, model, allowedEntriesOf(state, model, saved).filter((_, at) => at !== index));
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
 * NEW_EXCEPTION_TOKEN is the edit token of the open new-exception draft. It is deliberately NOT the
 * form a stored id produces (exceptionEditToken), because a policy may declare an exception whose id
 * is literally "new": with a shared form, that row would own the new draft's edit and could be
 * replaced or removed while the draft was still being written.
 */
export const NEW_EXCEPTION_TOKEN = "new-exception";

/** exceptionEditToken is the edit token of one stored exception, by its exact id. */
export function exceptionEditToken(id: string): string {
  return `exception:${id}`;
}

/**
 * allowedAddChoice is the model the add control is on. The operator's own choice is kept while it is
 * still a model the file does not list: that covers both a free option and a model the catalog has
 * since dropped, which the screen must not silently replace. A chosen model the file ALREADY lists
 * is never kept, because Add would then propose a setAllowed for it and replace its approved efforts
 * with the single first one; that is a permission change the operator did not ask for. "" means
 * there is nothing this control may add.
 */
export function allowedAddChoice(state: PolicyScreenState, free: readonly string[], listed: readonly string[]): string {
  if (state.allowedAddModel !== "" && !listed.includes(state.allowedAddModel)) return state.allowedAddModel;
  return free[0] ?? "";
}

/** addModelOptions is what the add-allowed select offers: every free model, plus a chosen one the
 * file does not list. A model the file already lists is not offered again. */
export function addModelOptions(state: PolicyScreenState, free: readonly string[], listed: readonly string[]): string[] {
  if (state.allowedAddModel === "" || listed.includes(state.allowedAddModel)) return [...free];
  return free.includes(state.allowedAddModel) ? [...free] : [state.allowedAddModel, ...free];
}

/**
 * allowedAddBlocked reports whether the Add control must be disabled. It is disabled when there is
 * nothing the control may add: no free model, or a selection that is already in the allowed list.
 * Adding a listed model would propose a setAllowed for it and replace its approved efforts with the
 * single first one, which removes approvals the operator never chose to change.
 */
export function allowedAddBlocked(state: PolicyScreenState, free: readonly string[], listed: readonly string[]): boolean {
  const choice = allowedAddChoice(state, free, listed);
  return choice === "" || listed.includes(choice);
}

/** screenExceptionDraft opens or updates the exception editor. */
export function screenExceptionDraft(state: PolicyScreenState, draft: ExceptionDraft | null): PolicyScreenState {
  // Closing is always allowed; opening or moving the editor while another edit owns the pending
  // change is refused, so the two edits never compete for the one change the API applies.
  if (draft !== null) {
    const token = draft.isNew ? NEW_EXCEPTION_TOKEN : exceptionEditToken(draft.id);
    if (!screenMayEdit(state, token)) return state;
  }
  return { ...state, exceptionDraft: draft };
}

/**
 * changeFromExceptionDraft is the change an exception draft proposes, or null when it is not yet
 * complete. An empty id or model would be refused by the check, so it is not proposed at all.
 */
export function changeFromExceptionDraft(draft: ExceptionDraft): PolicyChange | null {
  // Identifiers are opaque: an id, a model, an effort and a cwd taken from the policy, the catalog
  // or a select are carried byte for byte and never trimmed, split, joined or re-parsed. The Go
  // parser accepts any non-blank string as an identifier and the store compares it exactly, so
  // trimming a stored " legacy " or " model-a " would name a different exception or model and leave
  // the one the file declares untouched. Only blankness is checked, and only to refuse a change the
  // server would refuse; the value itself is stored exactly as typed.
  const id = draft.id;
  const model = draft.model;
  if (isBlank(id) || isBlank(model)) return null;
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

/**
 * draftForExceptionEdit opens the editor on an existing exception from the state the screen is
 * actually showing, not from the file alone. A change already proposed for this id is the operator's
 * latest intent, so reopening the editor must start from it: rebuilding from the file would show the
 * values they replaced and the next Apply would silently undo the pending change. When the pending
 * change is a removal there is nothing to reopen, so the file's values are used.
 */
export function draftForExceptionEdit(state: PolicyScreenState, exception: PolicyExceptionView): ExceptionDraft {
  const change = state.change;
  if (change?.kind === "setException" && change.id === exception.id) {
    // The change omits the role when it is unchanged (the server keeps the stored one), so the draft
    // falls back to the exception's own role exactly as the write would.
    return { id: change.id, isNew: false, role: change.role ?? exception.role ?? "", model: change.model, effort: change.effort, cwd: [...change.cwd], cwdNew: "" };
  }
  return draftForException(exception);
}

/**
 * pendingAllowedModel is the model a pending setAllowed is adding that the file does not list yet,
 * or null. Such a row needs its own editor: the Add control can only propose a first effort from the
 * union across the policy and the catalog, and without an editor the operator would have to save an
 * unwanted approval before they could correct it.
 */
export function pendingAllowedModel(state: PolicyScreenState, reading: PolicyReading): string | null {
  // The model a pending setAllowed targets, or the one the Add control last proposed. The remembered
  // value matters because the row must survive an empty intermediate edit, when the change is
  // momentarily null (the server refuses an empty effort list) but the row is still on screen.
  const change = state.change;
  const model = change?.kind === "setAllowed" ? change.model : state.pendingAllowed;
  if (model === "") return null;
  return reading.allowed.some((entry) => entry.model === model) ? null : model;
}

/**
 * pendingExceptionId is the exception a pending setException targets when the file no longer declares
 * it, or null. Another writer can remove the exception between the read and the save; the conflict
 * re-read then keeps the change (the operator's work) but the file has no row to show it in, so the
 * screen renders one from the change. Without it the change would be pending and uneditable.
 */
export function pendingExceptionId(state: PolicyScreenState, reading: PolicyReading): string | null {
  const change = state.change;
  if (change?.kind !== "setException") return null;
  return reading.exceptions.some((entry) => entry.id === change.id) ? null : change.id;
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
  return { ...state, saving: state.change, savingDrafts: { allowedDraft: new Map(state.allowedDraft), exceptionDraft: state.exceptionDraft }, busy: true };
}

/**
 * screenSaveFinished applies a write answer. The pending change is cleared only when it is still the
 * one that was saved: an edit made while the save was in flight is the operator's next change and is
 * never dropped by a response to the previous one.
 */
export function screenSaveFinished(state: PolicyScreenState, saved: PolicyChange | null, notice: PolicyNotice): PolicyScreenState {
  const stillPending = saved !== null && state.change === saved;
  let change = state.change;
  let allowedDraft = state.allowedDraft;
  let allowedNew = state.allowedNew;
  let pendingAllowed = state.pendingAllowed;
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
    // The spent draft is the one that PROPOSES the change the write saved, not merely the one that was
    // on screen when the save started: a new exception's editor stays open after Apply, so the operator
    // can keep editing it while the write is in flight, and the start snapshot would then be that newer
    // draft. Dropping it would discard the latest id, model, effort or cwd the operator had typed.
    if (saved.kind === "setAllowed" && sameEntries(effortsOf(allowedDraft.get(saved.model)), saved.efforts)) {
      allowedDraft = withoutKey(allowedDraft, saved.model);
      allowedNew = withoutKey(allowedNew, saved.model);
    }
    // A removal spends the draft of the model it removed as well. The removal's own change carries no
    // draft, so without this the removed model's typed text survives and a later Add for the same
    // model shows and proposes those old entries while the file no longer lists it.
    if (saved.kind === "removeAllowed") {
      allowedDraft = withoutKey(allowedDraft, saved.model);
      allowedNew = withoutKey(allowedNew, saved.model);
    }
    // The write landed, so the model is in the file now and is no longer "pending"; the row it had is
    // the file's own row from here on.
    if (saved.kind === "setAllowed" && state.pendingAllowed === saved.model) pendingAllowed = "";
    if (saved.kind === "setException" && exceptionDraft !== null && sameExceptionChange(saved, exceptionDraft)) {
      exceptionDraft = null;
    }
    // The pending change is cleared only when it is still the one that was saved; a later edit is
    // the operator's next change.
    if (stillPending) change = null;
  }
  return { ...state, saving: null, savingDrafts: null, busy: false, notice, change, allowedDraft, allowedNew, pendingAllowed, exceptionDraft, repair };
}

/** sameEntries reports whether two effort lists hold the same entries in the same order. */
function sameEntries(left: readonly string[] | undefined, right: readonly string[] | undefined): boolean {
  if (left === undefined || right === undefined) return left === right;
  return left.length === right.length && left.every((entry, index) => entry === right[index]);
}

/** sortedEfforts is an effort list in the order the server stores it. */
function sortedEfforts(efforts: readonly string[]): string[] {
  return [...efforts].sort();
}

/** effortsOf is a raw draft's entries as a change carries them: the blanks dropped. */
function effortsOf(entries: readonly string[] | undefined): string[] {
  return (entries ?? []).filter((entry) => entry !== "");
}

/**
 * sameExceptionChange reports whether a saved setException is the change one open draft proposes. It
 * is how screenSaveFinished tells the draft the write spent from a newer one the operator typed while
 * the write was in flight. Every field the change carries is compared, and the role is compared as
 * the change carries it (an empty role means "keep the stored one", which the draft records too).
 */
function sameExceptionChange(saved: Extract<PolicyChange, { kind: "setException" }>, draft: ExceptionDraft): boolean {
  const proposed = changeFromExceptionDraft(draft);
  if (proposed === null || proposed.kind !== "setException") return false;
  return proposed.id === saved.id
    && proposed.model === saved.model
    && proposed.effort === saved.effort
    && (proposed.role ?? "") === (saved.role ?? "")
    && sameEntries(proposed.cwd, saved.cwd);
}

/** withoutKey returns a draft map with one key removed, leaving every other key as it was. */
function withoutKey<T>(drafts: Map<string, T>, key: string): Map<string, T> {
  const next = new Map(drafts);
  next.delete(key);
  return next;
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
  return { ...state, change: null, allowedDraft: new Map<string, string[]>(), allowedNew: new Map<string, string>(), allowedAddModel: "", pendingAllowed: "", exceptionDraft: null, notice: null };
}

/**
 * screenRetryRead is the retry a failed read offers. It keeps the operator's inputs: the read that
 * failed was the automatic re-read a conflict or a lost response started, and the promise of that
 * path is that the inputs survive it. Clearing them here - as the explicit re-read does - would break
 * that promise at exactly the moment the operator is trying to recover.
 */
export function screenRetryRead(state: PolicyScreenState): PolicyScreenState {
  return { ...state, error: null };
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
 * screenBusy is whether an answer is on its way: a read, a check or a save. Every edit control is
 * disabled while it is true, so an edit can never race the answer that is coming.
 */
export function screenBusy(state: PolicyScreenState): boolean {
  return state.busy || state.saving !== null;
}

/**
 * screenEditOwner is the one edit control that may be live right now, or null when every control may
 * be. The API applies exactly one change per request, so a second edit would silently replace the
 * first and leave a row showing a value no pending change carries; the screen therefore lets one
 * edit own the pending change and disables the rest until it is saved or cancelled.
 *
 * The token names the edited thing by its own identifier: two different models, exceptions or roles
 * never share a token, and the same one keeps its token across its own edits.
 */
export function screenEditOwner(state: PolicyScreenState): string | null {
  // An open exception editor owns the edit before its change is proposed: its draft is the operator's
  // work in progress, and another row's edit would leave it unsubmittable.
  if (state.exceptionDraft !== null) return state.exceptionDraft.isNew ? NEW_EXCEPTION_TOKEN : exceptionEditToken(state.exceptionDraft.id);
  const change = state.change;
  if (change === null) {
    // An allowlist row whose draft is still on screen owns the edit even when the draft proposes no
    // change yet (the operator selected its only entry to replace it): another row's edit would
    // otherwise replace the draft map and silently discard what they were typing.
    const draftModel = allowedDraftModel(state);
    return draftModel === "" ? null : `allowed:${draftModel}`;
  }
  switch (change.kind) {
    case "setRolePairs":
      return `role:${change.role}`;
    case "setAllowed":
    case "removeAllowed":
      return `allowed:${change.model}`;
    case "setException":
    case "removeException":
      return exceptionEditToken(change.id);
  }
}

/** screenOwns reports whether one control's token is the live edit, or every control is live. */
export function screenOwns(state: PolicyScreenState, token: string): boolean {
  const owner = screenEditOwner(state);
  return owner === null || owner === token;
}

/**
 * screenMayEdit reports whether one control may make an edit right now: no answer may be in flight,
 * and no other edit may own the single pending change. The controls are disabled in this same
 * condition, so the state refuses an edit the screen already prevents.
 */
export function screenMayEdit(state: PolicyScreenState, token: string): boolean {
  // Editing is also impossible while the host cannot be edited at all (no registered policy, or a
  // repair a person must make first): the controls are disabled in that state, and the state refuses
  // an edit that reaches it anyway.
  return screenEditable(state) && !screenBusy(state) && screenOwns(state, token);
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

export function allowedEffortsLabel(model: string, index: number): string {
  return `${model} allowed effort ${index + 1}`;
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
