// Ported from CXC v0.2.40 plugins/codexclaw/gui/src/components/PromptOverrideEditor.tsx (1-18),
// modified: disabled and aria-label are optional props, so a role row can grey the editor while a
// save is in flight and name it for a screen reader; and the editor now carries the store's two
// states explicitly. The ported component rendered null (inherit) and "" (an override that is
// empty) as one blank textarea and mapped any empty or whitespace-only input back to null, so
// clearing a stored override silently inherited and an empty override could not be stored. The
// transitions live in ../prompt-override.ts, which node:test can import (node cannot load .tsx).
import { editText, inheritState, type PromptOverrideState } from "../prompt-override.ts";

interface Props {
  value: PromptOverrideState;
  disabled?: boolean;
  label?: string;
  onChange: (v: PromptOverrideState) => void;
}

/** The two-state per-role prompt override: "inherit" writes null and "override" writes its text
 *  verbatim, the empty string included. Overrides only the role prompt segment, not unrelated
 *  system/dev-skill text. */
export function PromptOverrideEditor({ value, onChange, disabled = false, label = "prompt override" }: Props) {
  const override = value.kind === "override";
  return (
    <div className="prompt-override">
      <select
        className="select"
        style={{ maxWidth: "220px" }}
        disabled={disabled}
        value={value.kind}
        aria-label={`${label} mode`}
        onChange={(e) => onChange(e.target.value === "inherit" ? inheritState() : editText(override ? value.text : ""))}
      >
        <option value="inherit">Inherit role prompt</option>
        <option value="override">Override role prompt</option>
      </select>
      <textarea
        className="textarea"
        placeholder={override ? "Role prompt override (an empty value is still an override)" : "Inherited from the role skill prompt"}
        aria-label={label}
        disabled={disabled || !override}
        value={override ? value.text : ""}
        onChange={(e) => onChange(editText(e.target.value))}
        rows={2}
      />
    </div>
  );
}
