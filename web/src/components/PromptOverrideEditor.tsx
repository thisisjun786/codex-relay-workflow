// Ported from CXC v0.2.40 plugins/codexclaw/gui/src/components/PromptOverrideEditor.tsx (1-18),
// modified: disabled and aria-label are optional props, so a role row can grey the editor while a
// save is in flight and name it for a screen reader.
interface Props {
  value: string | null;
  disabled?: boolean;
  label?: string;
  onChange: (v: string | null) => void;
}

/** Nullable per-role prompt override. Empty text -> null (never fabricated).
 *  Overrides only the role prompt segment, not unrelated system/dev-skill text. */
export function PromptOverrideEditor({ value, onChange, disabled = false, label = "prompt override" }: Props) {
  return (
    <textarea
      className="textarea"
      placeholder="Role prompt override (blank = inherit role skill prompt)"
      aria-label={label}
      disabled={disabled}
      value={value ?? ""}
      onChange={(e) => onChange(e.target.value.trim() === "" ? null : e.target.value)}
      rows={2}
    />
  );
}
