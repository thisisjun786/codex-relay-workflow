// Ported from CXC v0.2.40 plugins/codexclaw/gui/src/ui/help.tsx (1-238), modified: the
// topics are CRW's (status, execution policy, helper roles), the body text is CRW content
// in English, the /readme footer link is removed with the route it pointed at, and the
// drawer traps focus while open and follows the route when it changes underneath.
/**
 * help.tsx - per-page help drawer plus a topic button.
 *
 * Right-side drawer with overlay, Escape/overlay-click/X close, focus trap and focus
 * restore. The visual style comes from the shared kit tokens in styles.css.
 */
import { useCallback, useEffect, useRef, useState, type ReactNode } from "react";
import { Icon } from "./icons.tsx";

/* ---- topic content ---- */

export type HelpTopicId = "status" | "policy" | "helper-roles";

export interface HelpEntry {
  title: string;
  subtitle: string;
  body: ReactNode;
}

export const HELP_CONTENT: Record<HelpTopicId, HelpEntry> = {
  status: {
    title: "Status",
    subtitle: "Relay, parents, children and merge lanes",
    body: (
      <>
        <p className="help-lead">
          Shows where the relay actually stands: the store it reads, the App Server it
          reaches, and the execution policy in force.
        </p>
        <ul className="help-bullets">
          <li>The status bar keeps its three readings separate. A single green dot would hide which of them could not be read.</li>
          <li>A value whose source could not be read shows as unknown. It is never turned into zero, ok or an empty list.</li>
          <li>Relationship, generation, head and merge-lane rows arrive with the issue that implements this screen.</li>
        </ul>
      </>
    ),
  },
  policy: {
    title: "Execution policy",
    subtitle: "Supervisor, parent and child pairs",
    body: (
      <>
        <p className="help-lead">
          The supervisor, parent and child model and effort pairs, the allowed list and
          the per-scope exceptions the bridge enforces.
        </p>
        <ul className="help-bullets">
          <li>There is one policy store: the file the plugin wiring record names. There is no GUI-only settings file.</li>
          <li>A write is stored, then registered, then applied. The screen shows those three separately and never restarts the relay service.</li>
          <li>With no wiring record the screen is read-only and says so.</li>
        </ul>
      </>
    ),
  },
  "helper-roles": {
    title: "Helper roles",
    subtitle: "Explorer, reviewer, executor and architect",
    body: (
      <>
        <p className="help-lead">
          The global helper-role settings: the model, effort and prompt each role uses.
        </p>
        <ul className="help-bullets">
          <li>Main model keeps the original session's model. Choosing a model overrides that role only.</li>
          <li>Session effort follows the parent session's effort; pick a level to override it.</li>
          <li>The model list comes from the catalog. When it cannot be read the previous list or an error state is shown, never a fabricated one.</li>
          <li>An effort is greyed out for two reasons: the selected model's catalog ladder does not offer it, or the helper-role store does not hold it. The store keeps low, medium, high and xhigh, so a name the catalog or the execution policy uses (none, max) is listed but never selectable. An unreported ladder is not evidence that the model refuses it.</li>
          <li>When the selected model offers no effort the helper-role store accepts, no level is selectable and the role uses the session effort; a saved effort is kept, not silently changed.</li>
        </ul>
      </>
    ),
  },
};

/* ---- HelpTopicButton ---- */

export function HelpTopicButton({
  topic,
  onOpen,
}: {
  topic: HelpTopicId;
  onOpen: (topic: HelpTopicId) => void;
}) {
  return (
    <button
      type="button"
      className="help-topic-btn"
      aria-label={`${HELP_CONTENT[topic].title} help`}
      title={`${HELP_CONTENT[topic].title} help`}
      onClick={() => onOpen(topic)}
    >
      ?
    </button>
  );
}

/* ---- HelpDrawer ---- */

export function HelpDrawer({
  open,
  topic,
  onClose,
}: {
  open: boolean;
  topic: HelpTopicId;
  onClose: () => void;
}) {
  const ref = useRef<HTMLDivElement | null>(null);
  const closeRef = useRef<HTMLButtonElement | null>(null);

  useEffect(() => {
    if (!open) return undefined;
    const opener = document.activeElement as HTMLElement | null;
    const node = ref.current;
    closeRef.current?.focus();

    const onKey = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        onClose();
        return;
      }
      if (e.key !== "Tab" || !node) return;
      // The drawer is aria-modal, so Tab must not reach the page behind its overlay.
      const focusables = Array.from(
        node.querySelectorAll<HTMLElement>("button, a[href], input, select, textarea, [tabindex]:not([tabindex='-1'])"),
      ).filter((el) => !el.hasAttribute("disabled"));
      if (focusables.length === 0) return;
      const first = focusables[0];
      const last = focusables[focusables.length - 1];
      if (e.shiftKey && document.activeElement === first) {
        e.preventDefault();
        last.focus();
      } else if (!e.shiftKey && document.activeElement === last) {
        e.preventDefault();
        first.focus();
      }
    };
    document.addEventListener("keydown", onKey, true);
    return () => {
      document.removeEventListener("keydown", onKey, true);
      opener?.focus();
    };
  }, [open, onClose]);

  if (!open) return null;

  const entry = HELP_CONTENT[topic];

  return (
    <>
      <div className="help-overlay" onClick={onClose} aria-hidden="true" />
      <aside
        ref={ref}
        className="help-drawer"
        role="dialog"
        aria-modal="true"
        aria-label={`${entry.title} help`}
      >
        <header className="help-drawer-head">
          <div>
            <span className="help-drawer-eyebrow">{entry.subtitle}</span>
            <h3 className="help-drawer-title">{entry.title}</h3>
          </div>
          <button
            ref={closeRef}
            type="button"
            className="icon-btn"
            onClick={onClose}
            aria-label="Close"
          >
            <Icon name="x" size={16} />
          </button>
        </header>
        <div className="help-drawer-body">{entry.body}</div>
      </aside>
    </>
  );
}

/* ---- useHelp hook, shared by every screen ---- */

export function useHelp(defaultTopic: HelpTopicId) {
  const [helpOpen, setHelpOpen] = useState(false);
  const [helpTopic, setHelpTopic] = useState<HelpTopicId>(defaultTopic);

  // The route can change under an open drawer (Back/Forward, a nav click). The drawer
  // must then describe the page actually shown, not the one it was opened on.
  useEffect(() => {
    setHelpTopic(defaultTopic);
  }, [defaultTopic]);

  const openHelp = useCallback((topic: HelpTopicId) => {
    setHelpTopic(topic);
    setHelpOpen(true);
  }, []);

  const closeHelp = useCallback(() => {
    setHelpOpen(false);
  }, []);

  return { helpOpen, helpTopic, openHelp, closeHelp } as const;
}
