// New in CRW (no CXC counterpart): the two explicit prompt-override states.
/**
 * prompt-override.ts - the per-role prompt override as two states, kept out of the .tsx component
 * so the node:test suite can import it (node cannot load .tsx).
 *
 * The store keeps two things apart (internal/role RoleConfig.PromptOverride): null means the role
 * inherits its skill prompt, and a string - the empty one included - is an override the user
 * stored. The ported editor carried a nullable string instead and rendered both as one blank
 * textarea, mapping any empty or whitespace-only input back to null. That made an explicit
 * empty-string override impossible to create and turned "delete every character and save" into
 * "inherit", which is a different write from the one the user asked for.
 */

/** The editor's two states. Only "inherit" produces a null write. */
export type PromptOverrideState =
  | { readonly kind: "inherit" }
  | { readonly kind: "override"; readonly text: string };

/** The state for a stored value: null is inherit, and every string is an override. */
export function storedToState(stored: string | null): PromptOverrideState {
  return stored === null ? { kind: "inherit" } : { kind: "override", text: stored };
}

/** The one transition that yields inherit. Nothing else produces it. */
export function inheritState(): PromptOverrideState {
  return { kind: "inherit" };
}

/**
 * The state a keystroke or the "Set an override" control produces. The text is kept verbatim -
 * no trim, no empty-to-null - so clearing every character leaves an override that stores "".
 */
export function editText(text: string): PromptOverrideState {
  return { kind: "override", text };
}

/** Whether the draft differs from the stored value. */
export function isDirty(state: PromptOverrideState, stored: string | null): boolean {
  return state.kind === "inherit" ? stored !== null : state.text !== stored;
}

/** The wire patch a save sends: inherit writes null, an override writes its text verbatim. */
export function saveBody(state: PromptOverrideState): { promptOverride: string | null } {
  return { promptOverride: state.kind === "inherit" ? null : state.text };
}
