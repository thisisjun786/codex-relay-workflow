"""Write cli_cases.json: merge-turn-* command lines for the byte-identical CLI replay.

  uv run --no-sync python internal/relay/mergeturn/testdata/make_cli_cases.py
Targets are owner/repo (the check refuses before any read) or /nonexistent-crw-229/R.git (read
as unreadable without starting a process), so neither Python nor Go reaches a forge or git.
States a readable target would produce are set with SQL. Ids are derived here, as mergeturn.py
derives them.
"""
import json
import sys

sys.path.insert(0, "packages/codex-session-relay/src")
from codex_session_relay.mergeturn import grant_id, target_key, turn_id  # noqa: E402

REPO, LOCAL, BASE = "owner/repo", "/nonexistent-crw-229/R.git", "dev"
GREEN = json.dumps({"hasNextPage": False, "pagesRead": 1, "totalCount": 1,
                    "threadsSeen": ["thread-1"], "unresolved": 0})
CHECKS = json.dumps([{"runId": "run-1", "name": "dev-gate", "headSha": "head-a",
                      "conclusion": "success", "attempt": 1}])


def bind(project, task, host="host-a"):
    return {"argv": ["linkage-bind", "--role", "parent", "--scope", project, "--task", task,
                     "--host", host]}


def supervise(project, task, host="host-a"):
    return {"argv": ["linkage-supervise", "--initiative", "INIT-1", "--project", project,
                     "--supervisor-task", "task-supervisor", "--supervisor-host", "host-s",
                     "--parent-task", task, "--parent-host", host]}


def request(project, task, repo=REPO, head="head-a", ready=True, host="host-a", extra=()):
    argv = ["merge-turn-request", "--repository", repo, "--base-ref", BASE, "--project",
            project, "--task", task, "--host", host, "--head", head, *extra]
    return {"argv": argv + (["--ready"] if ready else [])}


def turn(task, repo=REPO, tenure=1):
    return turn_id(target_key(repo, BASE), task, tenure)


def grant(task, repo=REPO, sequence=1):
    return grant_id(turn(task, repo), 1, sequence)


def ack(task, repo=REPO, sequence=1, grant_text=None):
    return {"argv": ["merge-turn-acknowledge", "--turn", turn(task, repo), "--actor", task,
                     "--grant", grant_text or grant(task, repo, sequence),
                     "--evidence", "read it and re-checked the head"]}


def check(task, repo=REPO, head="head-a", checks=CHECKS, review=GREEN, required=("dev-gate",)):
    argv = ["merge-turn-check", "--turn", turn(task, repo), "--actor", task, "--head-sha", head,
            "--base-sha", "base-0", "--checks", checks, "--review", review]
    for name in required:
        argv += ["--required", name]
    return {"argv": argv}


A, B = turn("task-alpha"), turn("task-beta")
LA = turn("task-alpha", LOCAL)
cases = {
    # CCL-2: a refusal prints its reason and exits two.
    "ccl2_refusal_exits_two": [
        bind("PRJ-A", "task-alpha"), request("PRJ-A", "task-alpha"), ack("task-alpha"),
        check("task-alpha", head="head-moved"),
    ],
    # CCL-3: request holds, check against an unreadable target, show reports target_unreadable.
    "ccl3_unread_target_holds": [
        bind("PRJ-A", "task-alpha"), request("PRJ-A", "task-alpha", repo=LOCAL, extra=("--pr", "7")),
        ack("task-alpha", LOCAL), check("task-alpha", LOCAL),
        {"argv": ["merge-turn-show", "--repository", LOCAL, "--base-ref", BASE]},
    ],
    # CCL-4: malformed JSON is bad_invocation naming the flag; wrong shapes too.
    "ccl4_malformed_json": [
        bind("PRJ-A", "task-alpha"), request("PRJ-A", "task-alpha"),
        check("task-alpha", checks="not json"), check("task-alpha", review="{oops}"),
        check("task-alpha", checks="[1]"), check("task-alpha", checks="{}"),
        check("task-alpha", review="[]"),
        check("task-alpha", review=json.dumps({"hasNextPage": False, "pagesRead": 1,
                                               "totalCount": 1, "threadsSeen": 1,
                                               "unresolved": 0})),
    ],
    # CCL-5: a parent that came back asks with the only identifier it has.
    "ccl5_parent_task": [
        bind("PRJ-A", "task-alpha"), bind("PRJ-B", "task-beta", "host-b"),
        request("PRJ-A", "task-alpha"), request("PRJ-B", "task-beta", host="host-b"),
        {"argv": ["merge-turn-show", "--parent-task", "task-alpha"]},
        {"argv": ["merge-turn-show", "--parent-task", "task-beta"]},
        {"argv": ["merge-turn-show", "--parent-task", "task-nobody"]},
    ],
    # CCL-6: two selectors, a repository without a base ref, or none: bad_invocation.
    "ccl6_selectors": [
        bind("PRJ-A", "task-alpha"), request("PRJ-A", "task-alpha"),
        {"argv": ["merge-turn-show", "--turn", A, "--parent-task", "task-alpha"]},
        {"argv": ["merge-turn-show", "--repository", REPO]},
        {"argv": ["merge-turn-show", "--base-ref", BASE]},
        {"argv": ["merge-turn-show"]},
        {"argv": ["merge-turn-show", "--turn", A, "--repository", REPO, "--base-ref", BASE,
                  "--parent-task", "task-alpha"]},
        {"argv": ["merge-turn-show", "--turn", A]},
        {"argv": ["merge-turn-show", "--turn", "mtn-nothing"]},
        {"argv": ["merge-turn-show", "--repository", REPO, "--base-ref", BASE]},
    ],
    # CCL-7: acknowledging twice converges on one record.
    "ccl7_acknowledge_twice": [
        bind("PRJ-A", "task-alpha"), request("PRJ-A", "task-alpha"), ack("task-alpha"),
        ack("task-alpha"),
        {"argv": ["merge-turn-acknowledge", "--turn", A, "--actor", "task-alpha", "--grant",
                  grant("task-alpha"), "--evidence", " "]},
    ],
    # CCL-8: a grant that is not this tenure's is refused at the surface.
    "ccl8_foreign_grant": [
        bind("PRJ-A", "task-alpha"), request("PRJ-A", "task-alpha"),
        ack("task-alpha", grant_text="mtg-somethingelse"),
        {"argv": ["merge-turn-acknowledge", "--turn", A, "--actor", "task-beta", "--grant",
                  grant("task-alpha"), "--evidence", "not mine"]},
    ],
    # CCL-9: a stated cause travels with the readiness it withdrew.
    "ccl9_withdraw_cause": [
        bind("PRJ-A", "task-alpha"), request("PRJ-A", "task-alpha"),
        {"argv": ["merge-turn-ready", "--turn", A, "--actor", "task-alpha", "--not-ready",
                  "--cause", "the base moved to base-7"]},
        {"argv": ["merge-turn-ready", "--turn", A, "--actor", "task-alpha", "--ready",
                  "--head", "head-b"]},
        {"argv": ["merge-turn-ready", "--turn", A, "--actor", "task-alpha"]},
        {"argv": ["merge-turn-ready", "--turn", A, "--actor", "task-alpha", "--ready",
                  "--not-ready"]},
    ],
    # Every other command: claim, queue, replay, attest, return, release and promotion.
    "commands_queue_and_release": [
        bind("PRJ-A", "task-alpha"), bind("PRJ-B", "task-beta", "host-b"),
        request("PRJ-A", "task-alpha"), request("PRJ-B", "task-beta", host="host-b", head="head-b"),
        request("PRJ-A", "task-alpha"), request("PRJ-A", "task-beta", host="host-b"),
        request("PRJ-A", "task-alpha", extra=("--pr", "x")),
        {"argv": ["merge-turn-attest", "--turn", A, "--evidence-kind", "transport_accepted",
                  "--idempotency-key", "msg-1", "--actor", "task-beta", "--evidence", "accepted"]},
        {"argv": ["merge-turn-attest", "--turn", A, "--evidence-kind", "grant",
                  "--idempotency-key", "msg-2", "--actor", "task-beta", "--evidence", "{}"]},
        {"argv": ["merge-turn-request-return", "--turn", A, "--actor", "task-beta",
                  "--evidence", "my candidate is green"]},
        {"argv": ["merge-turn-show", "--repository", REPO, "--base-ref", BASE]},
        {"argv": ["merge-turn-withdraw", "--turn", B, "--actor", "task-alpha"]},
        {"argv": ["merge-turn-release", "--turn", A, "--actor", "task-alpha",
                  "--disposition", "cancelled", "--reason", "done"]},
        {"argv": ["merge-turn-release", "--turn", A, "--actor", "task-alpha",
                  "--disposition", "landed", "--reason", "done"]},
        {"argv": ["merge-turn-release", "--turn", A, "--actor", "task-alpha",
                  "--disposition", "returned", "--reason", "deferring to a ready peer"]},
        {"argv": ["merge-turn-release", "--turn", A, "--actor", "task-alpha",
                  "--disposition", "returned", "--reason", "again"]},
        {"argv": ["merge-turn-withdraw", "--turn", B, "--actor", "task-beta"]},
        {"argv": ["merge-turn-show", "--turn", "mtn-nothing"]},
    ],
    "commands_withdraw_waiting": [
        bind("PRJ-A", "task-alpha"), bind("PRJ-B", "task-beta", "host-b"),
        request("PRJ-A", "task-alpha"), request("PRJ-B", "task-beta", host="host-b", head="head-b"),
        {"argv": ["merge-turn-withdraw", "--turn", B, "--actor", "task-beta"]},
        {"argv": ["merge-turn-withdraw", "--turn", "mtn-nothing", "--actor", "task-beta"]},
    ],
    # Outcomes on an unreadable local target, with states a reading would have produced set by SQL.
    "commands_outcomes_unreadable": [
        bind("PRJ-A", "task-alpha"), supervise("PRJ-A", "task-alpha"),
        request("PRJ-A", "task-alpha", repo=LOCAL),
        {"argv": ["merge-turn-land", "--turn", LA, "--actor", "task-alpha", "--landed-sha",
                  "merge-1", "--evidence", "merged"]},
        {"sql": "UPDATE merge_turns SET state = 'merging', checked_base_sha = 'base-0'"
                " WHERE turn_id = '" + LA + "'"},
        {"argv": ["merge-turn-land", "--turn", LA, "--actor", "task-alpha", "--landed-sha",
                  "merge-1", "--evidence", "merged"]},
        {"argv": ["merge-turn-land", "--turn", LA, "--actor", "task-alpha", "--landed-sha",
                  "merge-1", "--evidence", " "]},
        {"argv": ["merge-turn-release", "--turn", LA, "--actor", "task-alpha",
                  "--disposition", "cancelled", "--reason", "stuck", "--evidence", "gone"]},
        {"argv": ["merge-turn-unknown", "--turn", LA, "--actor", "task-stranger",
                  "--reason", "I say so"]},
        {"argv": ["merge-turn-unknown", "--turn", LA, "--actor", "task-alpha",
                  "--reason", "lost the connection"]},
        {"argv": ["merge-turn-resolve", "--turn", LA, "--actor", "task-supervisor",
                  "--observed-base-sha", "base-0", "--pr-state", "mergd", "--evidence", "x"]},
        {"argv": ["merge-turn-resolve", "--turn", LA, "--actor", "task-supervisor",
                  "--observed-base-sha", "base-0", "--pr-state", "merged", "--evidence", "merged"]},
        {"argv": ["merge-turn-resolve", "--turn", LA, "--actor", "task-supervisor",
                  "--observed-base-sha", "base-0", "--pr-state", "open", "--evidence",
                  "still open"]},
        {"argv": ["merge-turn-restate-base", "--turn", LA, "--actor", "task-alpha",
                  "--evidence", "read again"]},
        {"sql": "UPDATE merge_turns SET state = 'landed', observed_base_sha = 'base-0'"
                " WHERE turn_id = '" + LA + "'"},
        {"argv": ["merge-turn-restate-base", "--turn", LA, "--actor", "task-alpha",
                  "--evidence", "read again"]},
        {"argv": ["merge-turn-restate-base", "--turn", LA, "--actor", "task-stranger",
                  "--evidence", "read again"]},
        {"argv": ["merge-turn-restate-base", "--turn", LA, "--actor", "task-alpha",
                  "--evidence", " "]},
        {"argv": ["merge-turn-show", "--turn", LA]},
    ],
}

# CARRIED FROM TODO 20: show --message on an unsent merge-turn grant renders the grant notice
# (delivery._render_grant with report.required_for_candidate), for each required reading.
def hexid(name):
    return (name.encode().hex() + "0" * 32)[:32]


def grant_event(event, relationship, head, repository=REPO, base=BASE):
    event = hexid(event)
    envelope = json.dumps({
        "baseRef": base, "candidateHead": head, "grantId": "mtg-" + event, "grantedFrom":
        "promotion", "kind": "merge_turn_grant", "recipientTaskId": "task-alpha",
        "repository": repository, "sequence": 1, "targetKey": "tgt-x", "tenure": 1,
        "turnId": "mtn-" + event, "wake": {"eventId": event}},
        sort_keys=True, separators=(",", ":"), ensure_ascii=False)
    return (
        "INSERT INTO events (event_id, relationship_id, execution_generation, revision_hash,"
        " outcome, producer, attempt, turn_thread_id, turn_id, turn_status, receipt, stage,"
        " first_seen_at, last_seen_at, observation_count) VALUES ('" + event + "', '"
        + relationship + "', 1, 'no-deliverable', 'merge_turn_grant', 'relay', NULL,"
        " 'task-alpha', 'mtg-" + event + "', 'completed', '" + envelope.replace("'", "''")
        + "', 'final', '2023-11-14T22:13:20.000000+00:00', '2023-11-14T22:13:20.000000+00:00', 1);"
        " INSERT INTO deliveries (event_id, relationship_id, kind, recipient_task_id,"
        " recipient_thread_id, state, attempt_count, created_at, updated_at) VALUES ('" + event
        + "', '" + relationship + "', 'merge_turn_grant', 'task-alpha', 'thread-a', 'queued', 0,"
        " '2023-11-14T22:13:20.000000+00:00', '2023-11-14T22:13:20.000000+00:00');")


def report(event, relationship, head, required, base=BASE, submission=1, generation=1):
    event = hexid(event)
    row = ("INSERT INTO work_reports (event_id, submission_no, relationship_id,"
           " execution_generation, revision_hash, repository, base_ref, head_sha, cxc_status,"
           " cxc_reason, contract_version, summary, next_action, recorded_at) VALUES ('"
           + event + "', " + str(submission) + ", '" + relationship + "', " + str(generation)
           + ", 'rev', '" + REPO + "', " + ("NULL" if base is None else "'" + base + "'")
           + ", '" + head + "', 'done', 'r', '1', 's', 'n', '2023-11-14T22:13:20.000000+00:00');")
    if required is not None:
        row += (" INSERT INTO work_report_handoffs (event_id, submission_no, is_draft,"
                " required_declared, checks, review_coverage, thread_dispositions, recorded_at)"
                " VALUES ('" + event + "', " + str(submission) + ", 0, '"
                + required.replace("'", "''") + "', '[]', '{}', '[]',"
                " '2023-11-14T22:13:20.000000+00:00');")
    return row


def show(event):
    return {"argv": ["show", "--event", hexid(event), "--message"]}


cases["show_message_unsent_grant"] = [
    bind("PRJ-A", "task-alpha"),
    {"sql": grant_event("g1", "rel-none", "head-a")}, show("g1"),
    {"sql": grant_event("g2", "rel-named", "head-a")
     + report("w2", "rel-named", "head-a", json.dumps(["dev-gate", "build linux", "it's"]))},
    show("g2"),
    {"sql": grant_event("g3", "rel-empty", "head-a") + report("w3", "rel-empty", "head-a", "[]")},
    show("g3"),
    {"sql": grant_event("g4", "rel-other", "head-a") + report("w4", "rel-other", "head-z", "[]")},
    show("g4"),
    {"sql": grant_event("g5", "rel-nobase", "head-a")
     + report("w5", "rel-nobase", "head-a", "[]", base=None)},
    show("g5"),
    {"sql": grant_event("g6", "rel-nohandoff", "head-a")
     + report("w6", "rel-nohandoff", "head-a", None)},
    show("g6"),
    {"sql": grant_event("g7", "rel-split", "head-a")
     + report("w7a", "rel-split", "head-a", '["a"]') + report("w7b", "rel-split", "head-a", '["b"]')},
    show("g7"),
    {"sql": grant_event("g8", "rel-bad", "head-a") + report("w8", "rel-bad", "head-a", '[1]')},
    show("g8"),
    {"sql": grant_event("g9", "rel-repo", "head-a", repository="own/other")
     + report("w9", "rel-repo", "head-a", "[]")},
    show("g9"),
]
with open("internal/relay/mergeturn/testdata/cli_cases.json", "w", encoding="utf-8") as out:
    json.dump(cases, out, indent=1)
    out.write("\n")
