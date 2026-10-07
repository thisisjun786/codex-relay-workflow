// New in CRW (no CXC counterpart): the sidebar run-state bar. It replaces the CXC sidebar-tail
// provider line (plugins/codexclaw/gui/src/components/OcxLinkBar.tsx) with the CRW readings, as
// the project's decision document specifies.
/**
 * RunStateBar.tsx - the status bar and the poller the run-state screen shares.
 *
 * The three readings - the relay store, the App Server and the execution policy - are rendered
 * separately, each with its own dot and its own reason. They are never folded into one green
 * dot, because that would hide which of them could not be read. A reading whose source failed
 * shows unknown with the reason the command gave, never a zero or a blank.
 */
import { useState } from "react";
import { fetchStatus, runStateReadings, type RunState } from "../api.ts";
import { StatusDot } from "../ui/kit.tsx";
import { usePolling } from "../usePolling.ts";

/** The poll interval the decided answer fixes: five seconds, and never two requests at once. */
export const RUN_STATE_POLL_MS = 5000;

/** The run state as a screen needs it: the document, or why it could not be read. */
export interface RunStateView {
  state: RunState | null;
  error: string | null;
}

/**
 * useRunState polls GET /api/status. usePolling schedules the next run only after the current
 * one settles, so a slow read delays the next poll instead of overlapping it.
 */
export function useRunState(): RunStateView {
  const [view, setView] = useState<RunStateView>({ state: null, error: null });
  usePolling(async (signal) => {
    try {
      const state = await fetchStatus(signal);
      if (signal.aborted) return;
      setView({ state, error: null });
    } catch (err) {
      if (signal.aborted) return;
      setView({ state: null, error: readErrorText(err) });
    }
  }, RUN_STATE_POLL_MS);
  return view;
}

/** The message of a failed request: the API's own error text when it carries one. */
function readErrorText(err: unknown): string {
  if (err && typeof err === "object" && "error" in err) {
    const text = (err as { error: unknown }).error;
    if (typeof text === "string" && text !== "") return text;
  }
  return err instanceof Error ? err.message : String(err);
}

/** The dot one reading's state renders as. Unknown is not ok. */
function dotFor(state: string): "ok" | "warn" | "off" | "err" {
  if (state === "ok") return "ok";
  if (state === "unknown") return "warn";
  return "off";
}

/** RunStateBar renders the three readings separately, each with its own dot and reason. */
export function RunStateBar({ view }: { view: RunStateView }) {
  const readings = runStateReadings(view.state, view.error);
  return (
    <div aria-label="status bar">
      {readings.map((reading) => (
        <div className="row" key={reading.key} title={reading.reason || reading.state}>
          <StatusDot status={dotFor(reading.state)} />
          <span className="grow">{reading.label}</span>
          <span className="badge">{reading.state}</span>
        </div>
      ))}
    </div>
  );
}
