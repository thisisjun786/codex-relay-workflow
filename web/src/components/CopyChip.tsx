// Ported from CXC v0.2.40 plugins/codexclaw/gui/src/components/pairing.tsx (241-262),
// modified: extracted into its own file (the handshake wizard it lived beside is not
// ported) and one em dash became a hyphen in the toast text.
import { useState } from "react";
import { Icon } from "../ui/icons.tsx";
import { toast } from "../ui/toast.tsx";

/** A small chip that copies its text and confirms with a check mark. */
export function CopyChip({ text }: { text: string }) {
  const [copied, setCopied] = useState(false);
  return (
    <button
      type="button"
      className="copy-chip"
      onClick={async () => {
        try {
          await navigator.clipboard.writeText(text);
          setCopied(true);
          setTimeout(() => setCopied(false), 1500);
        } catch {
          toast("Copy failed - select the text manually", "err");
        }
      }}
      aria-label={`Copy ${text}`}
    >
      <span className="mono">{text}</span>
      <Icon name={copied ? "check" : "copy"} size={13} />
    </button>
  );
}
