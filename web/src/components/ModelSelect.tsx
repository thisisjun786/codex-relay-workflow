// Ported from CXC v0.2.40 plugins/codexclaw/gui/src/components/ModelSelect.tsx (1-36),
// verbatim except the wire type's module path.
import type { CatalogEntry } from "../api.ts";

interface Props {
  value: string | null;
  emptyLabel?: string;
  label?: string;
  disabled: boolean;
  inherited?: boolean;
  onInherit?: () => void;
  entries: CatalogEntry[];
  onChange: (model: string | null) => void;
}

/** Model dropdown. The "main model" option (value "main") is always selectable,
 *  whether or not the catalog reports any entries. A saved model the catalog no
 *  longer lists stays visible, marked unavailable. */
export function ModelSelect({ value, disabled, entries, onChange, inherited = false, onInherit, emptyLabel = "Main model", label = "model" }: Props) {
  return (
    <select
      className="select"
      style={{ maxWidth: "220px" }}
      disabled={disabled}
      value={inherited ? "global" : value === null ? "main" : `model:${value}`}
      onChange={(e) => { const selected = e.target.value; if (selected === "global") onInherit?.(); else onChange(selected === "main" ? null : selected.slice(6)); }}
      aria-label={label}
    >
      <option value="main">{emptyLabel}</option>
      {onInherit ? <option value="global">Global settings</option> : null}
      {value && !entries.some(e => e.id === value) ? <option value={`model:${value}`}>{value} (saved, unavailable)</option> : null}
      {entries.map((e) => (
        <option key={e.id} value={`model:${e.id}`}>
          {e.label}
        </option>
      ))}
    </select>
  );
}
