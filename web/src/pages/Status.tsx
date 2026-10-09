// New in CRW (no CXC counterpart): the run-state screen.
/**
 * Status.tsx - where the relay actually stands.
 *
 * Every value here is rendered from the document GET /api/status returns, and that document
 * carries the answers of named reads: crw manage relay-read for the relay relationships, the DAG
 * plan progress, the merge lanes and the relay read's own failures; crw manage capacity
 * --dry-run for the capacity judgement; crw manage dag-review --no-state for the DAG anomalies.
 * This screen re-derives none of them. A source that could not be read shows its own state and
 * the reason the command gave, and a value the document did not carry is shown as unknown rather
 * than as a zero or a blank.
 */
import type { ReactNode } from "react";
import {
  type CapacityDocument,
  type CapacityPlanView,
  type DagAnomalyView,
  type DagDocument,
  type RelayBindingView,
  type RelayDocument,
  type RelayMergeTurnView,
  type RelayPlanView,
  type RelayRelationshipView,
  type RunState,
  type StatusSource,
  relationshipPullRequestText,
  sectionReading,
} from "../api.ts";
import { Card } from "../ui/kit.tsx";
import type { RunStateView } from "../components/RunStateBar.tsx";

/** A value the document did not carry, shown rather than blanked. */
function text(value: unknown): string {
  if (value === null || value === undefined || value === "") return "unknown";
  if (typeof value === "string") return value;
  if (typeof value === "number" || typeof value === "boolean") return String(value);
  return JSON.stringify(value);
}

/** A source's read state and reason, shown above the values it carried. */
function SourceHead({ source }: { source: StatusSource<unknown> }) {
  return (
    <div className="row wrap">
      <span className="badge">{source.state}</span>
      {source.reason ? <span className="hint">{source.reason}</span> : null}
    </div>
  );
}

/** One row of a list: an identity, a detail line and a state. */
function Row({ id, sub, state, reason }: { id: string; sub: string; state?: string; reason?: string }) {
  return (
    <div className="list-row">
      <div className="row-id">
        <div className="row-name mono">{id}</div>
        <div className="row-sub" title={sub}>{sub}</div>
      </div>
      <div className="row-actions">
        {reason ? <span className="hint cell-detail" title={reason}>{reason}</span> : null}
        <span className="badge">{state ?? "ok"}</span>
      </div>
    </div>
  );
}

/**
 * A list section that keeps "could not be read" apart from "read and empty".
 *
 * A section the document did not carry, or one the read reported as failed, is unknown with its
 * reason. Only an array the document really carried and that holds nothing is reported as empty.
 * Coalescing the two would let a failed read read as a clean one, which is the failure this
 * screen exists to prevent.
 */
function Rows<T>({
  items,
  empty,
  reason,
  render,
}: {
  items: T[] | null | undefined;
  empty: string;
  reason: string;
  render: (item: T, index: number) => ReactNode;
}) {
  if (items == null) {
    return (
      <div className="list-row">
        <div className="row-id">
          <div className="row-name">unknown</div>
          <div className="row-sub" title={reason}>{reason}</div>
        </div>
        <div className="row-actions"><span className="badge">unknown</span></div>
      </div>
    );
  }
  if (items.length === 0) {
    return (
      <div className="list-row">
        <div className="row-id"><div className="row-sub">{empty}</div></div>
      </div>
    );
  }
  return <>{items.map(render)}</>;
}

/** The relay relationships, the DAG plan progress, the merge lanes and the read's failures. */
function RelaySection({ source }: { source: StatusSource<RelayDocument> }) {
  const document: RelayDocument | null = source.data;
  const failures = document?.failures ?? [];
  // A section named in failures, or one the document omitted, was not read: its reason is the
  // read's own, so the section shows unknown rather than the empty-state sentence.
  const section = <T,>(name: string, items: T[] | null | undefined) =>
    sectionReading<T>(name, items, failures, source.reason);
  return (
    <Card title="Relay" desc="Relationships, DAG progress and merge lanes, from crw manage relay-read.">
      <SourceHead source={source} />
      {failures.length > 0 ? (
        <div className="row-list">
          {failures.map((failure, index) => (
            <Row
              key={`failure-${index}`}
              id={text(failure.section)}
              sub={text(failure.item)}
              state="unknown"
              reason={text(failure.reason)}
            />
          ))}
        </div>
      ) : null}
      <h3 className="card-title">Bindings</h3>
      <div className="row-list">
        <Rows<RelayBindingView>
          items={section("bindings", document?.bindings).items}
          empty="No live binding in the store."
          reason={section("bindings", document?.bindings).reason}
          render={(binding, index) => (
            <Row
              key={`binding-${index}`}
              id={text(binding.role)}
              sub={`${text(binding.scopeKind)}=${text(binding.scopeKey)} · task ${text(binding.taskId)} · ${text(binding.status)} · cwd ${text(binding.cwd)}`}
              state={binding.read?.state ?? "unknown"}
              reason={binding.read?.reason}
            />
          )}
        />
      </div>
      <h3 className="card-title">Relationships</h3>
      <div className="row-list">
        <Rows<RelayRelationshipView>
          items={section("relationships", document?.relationships).items}
          empty="No live relationship in the store."
          reason={section("relationships", document?.relationships).reason}
          render={(relationship, index) => (
            <Row
              key={`relationship-${index}`}
              id={text(relationship.issueKey)}
              sub={`parent ${text(relationship.parentTaskId)} · child ${text(relationship.childTaskId)} · generation ${text(relationship.executionGeneration)} · ${text(relationship.relationshipStatus)} · next ${text(relationship.nextExpectedAction)} · ${relationshipPullRequestText(relationship)} · head ${text(relationship.head?.revisionHash)}`}
              state={relationship.read?.state ?? "unknown"}
              reason={relationship.read?.reason}
            />
          )}
        />
      </div>
      <h3 className="card-title">DAG progress</h3>
      <div className="row-list">
        <Rows<RelayPlanView>
          items={section("plans", document?.plans).items}
          empty="No DAG plan in the store."
          reason={section("plans", document?.plans).reason}
          render={(plan, index) => (
            <Row
              key={`plan-${index}`}
              id={text(plan.planId)}
              sub={`revision ${text(plan.revision)} · ${stagesText(plan.stages)} · blocked ${text(plan.blocked)} of ${text(plan.denominator)}`}
              state={plan.read?.state ?? "unknown"}
              reason={plan.read?.reason}
            />
          )}
        />
      </div>
      <h3 className="card-title">Merge lanes</h3>
      <div className="row-list">
        <Rows<RelayMergeTurnView>
          items={section("mergeTurns", document?.mergeTurns).items}
          empty="No open merge turn."
          reason={section("mergeTurns", document?.mergeTurns).reason}
          render={(turn, index) => (
            <Row
              key={`turn-${index}`}
              id={`#${text(turn.prNumber)}`}
              sub={`${text(turn.state)} · holder ${text(turn.holderTaskId)} · updated ${text(turn.updatedAt)}`}
              state={turn.read?.state ?? "unknown"}
              reason={turn.read?.reason}
            />
          )}
        />
      </div>
    </Card>
  );
}

/** The stage counts the scheduler reported, in its own names. */
function stagesText(stages: Record<string, number> | null | undefined): string {
  const entries = Object.entries(stages ?? {});
  if (entries.length === 0) return "no stage count";
  return entries.map(([stage, nodes]) => `${stage} ${nodes}`).join(" · ");
}

/**
 * The capacity judgement. A hold carries the command's own reasons, which are the authoritative
 * explanation of the verdict and cannot be inferred from the other fields.
 */
function CapacitySection({ source }: { source: StatusSource<CapacityDocument> }) {
  const document: CapacityDocument | null = source.data;
  return (
    <Card title="Capacity" desc="Room to add a parent, from crw manage capacity --dry-run.">
      <SourceHead source={source} />
      <div className="row wrap">
        <span className="hint">merges last hour {text(document?.lane?.merges_last_hour)}</span>
        <span className="hint">actions {text(document?.actions?.state)}</span>
        <span className="hint">child 429 {text(document?.child_429?.state)}</span>
      </div>
      <div className="row-list">
        <Rows<CapacityPlanView>
          items={document?.plans}
          empty="No plan is configured for a capacity judgement."
          reason={source.reason ?? "the capacity judgement was not read"}
          render={(plan, index) => (
            <Row
              key={`capacity-${index}`}
              id={text(plan.plan)}
              sub={`${text(plan.verdict)} · waiting ${text(plan.waiting?.length)} · slots ${text(plan.held)}/${text(plan.ceiling)} · host ${text(plan.host_memory)}${
                (plan.reasons?.length ?? 0) > 0 ? ` · ${(plan.reasons ?? []).join(", ")}` : ""
              }`}
              state={text(plan.verdict)}
            />
          )}
        />
      </div>
    </Card>
  );
}

/** The DAG anomalies the review found, and the checks it could not take. */
function DagSection({ source }: { source: StatusSource<DagDocument> }) {
  const document: DagDocument | null = source.data;
  const checks = document?.checks ?? [];
  return (
    <Card title="DAG anomalies" desc="What crw manage dag-review --no-state found.">
      <SourceHead source={source} />
      <div className="row-list">
        <Rows<DagAnomalyView>
          items={document?.anomalies}
          empty="No anomaly reported."
          reason={source.reason ?? "the DAG review was not read"}
          render={(anomaly, index) => (
            <Row
              key={`anomaly-${index}`}
              id={text(anomaly.kind)}
              sub={`${text(anomaly.plan)} · ${text(anomaly.node)} · ${text(anomaly.issue)} · ${text(anomaly.detail)}`}
              state="unknown"
            />
          )}
        />
      </div>
      {checks.length > 0 ? (
        <div className="row wrap">
          {checks.map((check, index) => (
            <span className="hint" key={`check-${index}`} title={check.detail ?? undefined}>
              {`${text(check.name)}: ${text(check.state)}`}
              {check.detail ? ` · ${check.detail}` : ""}
            </span>
          ))}
        </div>
      ) : null}
    </Card>
  );
}

/** The run-state screen. */
export function Status({ view }: { view: RunStateView }) {
  const state: RunState | null = view.state;
  if (!state) {
    return (
      <Card title="Run state" desc="The relay store, the App Server and the execution policy.">
        <SourceHead source={{ state: "unknown", reason: view.error ?? "the status request did not answer", data: null }} />
      </Card>
    );
  }
  return (
    <>
      <RelaySection source={state.relay} />
      <CapacitySection source={state.capacity} />
      <DagSection source={state.dag} />
    </>
  );
}
