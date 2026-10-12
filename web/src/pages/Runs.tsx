// New in CRW (no CXC counterpart): the Runs screen.
/**
 * Runs.tsx - the flow runs of the relay store: each run's phase, then agent tree, the thread, the
 * commits, the review links and the tokens of each node, and recovery_hold where a call's outcome is
 * unknown.
 *
 * RunsScreen is the view and takes everything as props; RunsPage reads GET /api/runs, follows the
 * selected run through its event stream (RunEventFeed: the stream goes on from the last row on a
 * reconnect) and calls the control POSTs. A run store that could not be read shows its reason, never
 * an empty list.
 */
import { useCallback, useEffect, useRef, useState } from "react";
import { answerRun, controlRun, getRun, getRuns, runEventsUrl } from "../api.ts";
import {
  RunEventFeed,
  controlsFor,
  shortId,
  tokenText,
  type AgentNode,
  type NodeInfo,
  type Question,
  type RunDetail,
  type RunInfo,
} from "../runs-state.ts";
import { Button, Card, EmptyState, Loading } from "../ui/kit.tsx";
import { toast } from "../ui/toast.tsx";
import { usePolling } from "../usePolling.ts";

/** How often the list of runs is read, and how long the tree waits for more rows before it is read again. */
const LIST_REFRESH_MS = 4_000;
const TREE_SETTLE_MS = 400;

export interface RunsHandlers {
  select: (id: string) => void;
  pause: (id: string) => void;
  cancel: (id: string) => void;
  answer: (id: string, answer: { decision: string; node: string; question: string; revision: number }) => void;
}

export interface RunsScreenProps {
  runs: RunInfo[] | null;
  /** The reason the runs could not be read, when they could not. */
  unknown: string | null;
  selected: string | null;
  detail: RunDetail | null;
  error: string | null;
  /** True while the selected run's event stream is connected. */
  following: boolean;
  handlers: RunsHandlers;
}

function stateTone(state: string): string {
  switch (state) {
    case "running":
      return "ok";
    case "paused":
    case "held":
    case "dead":
      return "accent";
    default:
      return "";
  }
}

function AgentLine({ agent }: { agent: AgentNode }) {
  return (
    <div className="list-row tree-agent" data-call={agent.call_id}>
      <div className="row-id">
        <div className="row-name mono">
          {agent.role || "agent"}
          {agent.node ? <span className="hint"> {agent.node}</span> : null}
          {agent.attempt ? <span className="hint"> attempt {agent.attempt}</span> : null}
        </div>
        <div className="row-sub" title={agent.thread_id ?? ""}>
          {agent.thread_id ? `thread ${agent.thread_id}` : "no thread yet"}
          {agent.tokens > 0 ? ` - ${tokenText(agent.tokens)} tokens` : ""}
        </div>
      </div>
      <div className="row-actions">
        {agent.recovery_hold ? <span className="badge accent">recovery_hold</span> : null}
        <span className="badge">{agent.reason ? `${agent.status} (${agent.reason})` : agent.status}</span>
      </div>
    </div>
  );
}

function NodeCard({ node }: { node: NodeInfo }) {
  return (
    <div className="list-row tree-node" data-node={node.node}>
      <div className="row-id">
        <div className="row-name mono">node {node.node}</div>
        <div className="row-sub">
          {(node.commits ?? []).length > 0 ? `commits ${(node.commits ?? []).map((c) => shortId(c)).join(", ")}` : "no commit yet"}
          {(node.reviews ?? []).map((r) => ` - review ${r.call} ${r.verdict ?? r.state}@${shortId(r.head)}`).join("")}
          {node.acceptance ? ` - accepted ${node.acceptance}` : ""}
          {node.integrated ? ` - integrated ${shortId(node.integrated)}` : ""}
          {node.tokens > 0 ? ` - ${tokenText(node.tokens)} tokens` : ""}
        </div>
        {(node.threads ?? []).length > 0 ? <div className="row-sub">threads {(node.threads ?? []).join(", ")}</div> : null}
      </div>
      <div className="row-actions">
        {node.recovery_hold ? <span className="badge accent">recovery_hold</span> : null}
        {node.held && !node.recovery_hold ? <span className="badge">held</span> : null}
      </div>
    </div>
  );
}

function QuestionLine({ run, question, onAnswer }: { run: string; question: Question; onAnswer: RunsHandlers["answer"] }) {
  const [decision, setDecision] = useState("");
  const open = question.current && !question.applied;
  return (
    <div className="list-row" data-question={question.id}>
      <div className="row-id">
        <div className="row-name mono">question {question.id} r{question.revision}</div>
        <div className="row-sub">
          node {question.node || "(milestone)"} - {question.reason}
          {question.applied ? ` - answered by ${question.applied}` : question.current ? " - waiting for an answer" : " - replaced by a later question"}
        </div>
      </div>
      {open ? (
        <div className="row-actions">
          <input
            className="input mono"
            aria-label={`decision record for ${question.id}`}
            placeholder="decision id (crw manage decisions)"
            value={decision}
            onChange={(e) => setDecision(e.target.value)}
          />
          <Button
            variant="primary"
            disabled={decision.trim() === ""}
            onClick={() => onAnswer(run, { decision: decision.trim(), node: question.node, question: question.id, revision: question.revision })}
          >
            Apply answer
          </Button>
        </div>
      ) : null}
    </div>
  );
}

/** The tree of one run: phases, then the agents of each, the nodes and the questions. */
export function RunTree({ detail, handlers }: { detail: RunDetail; handlers: RunsHandlers }) {
  const controls = controlsFor(detail);
  return (
    <div className="run-tree" data-run={detail.run_id}>
      <Card title={`${detail.run_id} - ${detail.workflow}`} desc={`owner ${detail.owner || "unknown"} - plan ${detail.plan_id || "none"}@${detail.plan_revision}`}>
        <div className="row wrap">
          <span className={`badge ${stateTone(detail.state)}`}>{detail.state}</span>
          <span className="hint">{detail.calls} calls</span>
          <span className="hint">{tokenText(detail.tokens)} tokens</span>
          {detail.unsettled > 0 ? <span className="hint">{detail.unsettled} to go on with</span> : null}
          {detail.recovery_holds > 0 ? <span className="badge accent">{detail.recovery_holds} recovery_hold</span> : null}
          {detail.waiting > 0 ? <span className="badge accent">{detail.waiting} waiting for an answer</span> : null}
          {detail.paused ? <span className="hint">paused by {detail.paused}</span> : null}
          {(detail.held_reasons ?? []).length > 0 ? <span className="hint">held for {(detail.held_reasons ?? []).join(", ")}</span> : null}
          <span className="spacer" />
          <Button disabled={!controls.pause} onClick={() => handlers.pause(detail.run_id)}>Pause</Button>
          <Button variant="danger" disabled={!controls.cancel} onClick={() => handlers.cancel(detail.run_id)}>Cancel</Button>
        </div>
      </Card>
      {detail.phases.length === 0 ? <EmptyState title="No phase yet">The run has not opened a phase.</EmptyState> : null}
      {detail.phases.map((phase, index) => (
        <Card key={`${index}:${phase.title}`} title={`phase ${phase.title || "(none)"}`}>
          <div className="row-list">
            {phase.agents.length === 0 ? (
              <div className="list-row"><div className="row-id"><div className="row-sub">no agent call in this phase</div></div></div>
            ) : (
              phase.agents.map((agent) => <AgentLine key={agent.call_id} agent={agent} />)
            )}
          </div>
        </Card>
      ))}
      {detail.nodes.length > 0 ? (
        <Card title="Nodes" desc="What each plan node has so far.">
          <div className="row-list">{detail.nodes.map((n) => <NodeCard key={n.node} node={n} />)}</div>
        </Card>
      ) : null}
      {detail.questions.length > 0 ? (
        <Card title="Questions" desc="Waits of the run. An answer is a decision record the manager made (crw manage decisions).">
          <div className="row-list">{detail.questions.map((q) => <QuestionLine key={q.id} run={detail.run_id} question={q} onAnswer={handlers.answer} />)}</div>
        </Card>
      ) : null}
    </div>
  );
}

/** The screen: the list of runs on one side, the tree of the selected run beside it. */
export function RunsScreen(props: RunsScreenProps) {
  const { runs, unknown, selected, detail, error, following, handlers } = props;
  if (runs === null && unknown === null) return <Loading label="Reading the runs..." />;
  if (unknown !== null) {
    return (
      <Card title="Runs" desc="The runs could not be read.">
        <div className="row wrap">
          <span className="badge">unknown</span>
          <span className="hint">{unknown}</span>
        </div>
      </Card>
    );
  }
  const list = runs ?? [];
  return (
    <div className="runs-screen">
      <Card title="Runs" desc="Flow runs of the relay store, newest last.">
        {list.length === 0 ? (
          <EmptyState title="No run yet">A run starts with crw flow start.</EmptyState>
        ) : (
          <div className="row-list">
            {list.map((run) => (
              <button
                type="button"
                key={run.run_id}
                className={`list-row run-pick ${selected === run.run_id ? "active" : ""}`}
                aria-current={selected === run.run_id ? "true" : undefined}
                onClick={() => handlers.select(run.run_id)}
              >
                <div className="row-id">
                  <div className="row-name mono">{run.run_id}</div>
                  <div className="row-sub">
                    {run.workflow} - {run.calls} calls - {tokenText(run.tokens)} tokens
                    {run.recovery_holds > 0 ? ` - ${run.recovery_holds} recovery_hold` : ""}
                  </div>
                </div>
                <div className="row-actions"><span className={`badge ${stateTone(run.state)}`}>{run.state}</span></div>
              </button>
            ))}
          </div>
        )}
      </Card>
      {error ? <div className="hint" role="alert">{error}</div> : null}
      {selected && detail ? (
        <>
          <div className="hint" aria-live="polite">{following ? "following the run" : "the stream is not connected"}</div>
          <RunTree detail={detail} handlers={handlers} />
        </>
      ) : selected ? (
        <Loading label="Reading the run..." />
      ) : null}
    </div>
  );
}

/** The page: it reads the runs, follows the selected one, and sends the controls. */
export function RunsPage() {
  const [runs, setRuns] = useState<RunInfo[] | null>(null);
  const [unknown, setUnknown] = useState<string | null>(null);
  const [selected, setSelected] = useState<string | null>(null);
  const [detail, setDetail] = useState<RunDetail | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [following, setFollowing] = useState(false);
  const selectedRef = useRef<string | null>(null);
  selectedRef.current = selected;

  usePolling(async (signal) => {
    const doc = await getRuns(signal);
    if (signal.aborted) return;
    setUnknown(doc.unknown ?? null);
    setRuns(doc.runs);
    if (selectedRef.current === null && doc.runs.length > 0) {
      const live = [...doc.runs].reverse().find((r) => r.state === "running" || r.state === "paused") ?? doc.runs[doc.runs.length - 1];
      setSelected(live.run_id);
    }
  }, LIST_REFRESH_MS);

  const reload = useCallback(async (id: string) => {
    try {
      const d = await getRun(id);
      if (selectedRef.current === id) {
        setDetail(d);
        setError(null);
      }
    } catch (err) {
      if (selectedRef.current === id) setError(`the run could not be read: ${(err as { error?: string }).error ?? String(err)}`);
    }
  }, []);

  // follow the selected run: a row means the tree changed, and the tree is read once the rows stop coming
  useEffect(() => {
    if (!selected) return;
    setDetail(null);
    void reload(selected);
    let timer: ReturnType<typeof setTimeout> | null = null;
    const feed = new RunEventFeed({
      open: (url) => new EventSource(url) as unknown as ReturnType<ConstructorParameters<typeof RunEventFeed>[0]["open"]>,
      url: runEventsUrl(selected),
      onRow: () => {
        setFollowing(true);
        if (timer !== null) clearTimeout(timer);
        timer = setTimeout(() => void reload(selected), TREE_SETTLE_MS);
      },
      onState: () => setFollowing(true),
      onEnd: () => {
        setFollowing(false);
        void reload(selected);
      },
    });
    feed.start();
    return () => {
      feed.stop();
      if (timer !== null) clearTimeout(timer);
      setFollowing(false);
    };
  }, [selected, reload]);

  const handlers: RunsHandlers = {
    select: setSelected,
    pause: (id) => void control(id, "pause"),
    cancel: (id) => void control(id, "cancel"),
    answer: (id, answer) => void answer_(id, answer),
  };

  async function control(id: string, verb: "pause" | "cancel") {
    try {
      const result = await controlRun(id, verb);
      toast(result.message ?? (result.ok ? `${verb} requested` : `${verb} refused`), result.ok ? "ok" : "err");
    } catch (err) {
      toast(`${verb} failed: ${(err as { error?: string }).error ?? String(err)}`, "err");
    }
    void reload(id);
  }

  async function answer_(id: string, a: { decision: string; node: string; question: string; revision: number }) {
    try {
      const result = await answerRun(id, a);
      toast(result.message ?? (result.ok ? "answer applied" : "answer refused"), result.ok ? "ok" : "err");
    } catch (err) {
      toast(`answer failed: ${(err as { error?: string }).error ?? String(err)}`, "err");
    }
    void reload(id);
  }

  return <RunsScreen runs={runs} unknown={unknown} selected={selected} detail={detail} error={error} following={following} handlers={handlers} />;
}
