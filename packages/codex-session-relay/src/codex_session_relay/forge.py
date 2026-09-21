"""FORGE-EVIDENCE-01: the merge-readiness record, observed instead of typed in.

`mergeevidence` states the predicates a candidate has to satisfy, and `report.py` and the merge
turn both apply them. What neither of them has is a way to find out whether the numbers are true.
Every field those predicates grade arrives as something somebody wrote down, and the failure this
module exists for came from exactly there: a reviewer counted unresolved review threads with
`reviewThreads(first: 60)` on a pull request that had sixty-three of them, saw nothing unresolved
in the truncated prefix, and reported zero. Zero is the value that OPENS the merge gate, so the
mistake did not fail safe.

This module does the reading. It talks to one forge, GitHub, through the `gh` command line, and it
produces the record the predicates grade.

Three properties are structural rather than conventional, because each of them is a way the
reading could quietly become a lie.

READ-ONLY. The only REST verb issued is GET and a GraphQL document containing a mutation is
refused before it is sent. The observer cannot resolve a review thread, and a later caller that
wanted it to cannot make it.

NO SHELL. Commands are argument arrays with no shell between them and the process. Argument arrays
alone are not enough, though: `gh` reads a leading dash as an option, so a repository and a pull
request number are validated against strict shapes BEFORE they reach argv rather than trusted to
be inert there.

NO POLICY. Nothing here decides whether a candidate may merge. The collector's own refusals are
about the ACT of observing - a page it could not reach, a set that moved while it read, a
permission it did not have - and readiness itself is still decided by `mergeevidence`. If both
files knew the rule, they would drift, and the drift would be invisible because both sides stay
green.

FOUR ANSWERS, NOT TWO. Ready, not-ready, stale and unknown are separate because the next action
is separate. Not-ready is fixed by finishing work. Stale means the candidate moved underneath the
reading and it must be taken again. Unknown means the reading did not happen - a truncated page,
a cursor going in a circle, a permission error - and it is fixed by getting access or by asking
again. Collapsing stale or unknown into either neighbour is how "I could not tell" becomes "yes".

WHAT IS TRUSTED. The forge is. A GitHub that answers consistently and falsely defeats any
observer, and nothing here can detect it. Provenance records what was invoked; it does not certify
what came back.

WHAT IS NOT ATOMIC. This observation is not atomic with any merge that follows it, and it does not
pretend to be. A thread can reopen after its page was read, even during the confirming second
pass. The window is recorded so a reader can see how wide it was; it is narrowed by the second
pass, and what actually closes the risk at merge time is the exact-head guard the merge turn
already holds, plus a late finding routing back through the correction path afterwards.
"""

import hashlib
import json
import re
import subprocess
from datetime import datetime, timezone
from urllib.parse import quote, urlencode

from . import mergeevidence
from .mergeevidence import Problem

UNDECLARED = mergeevidence.UNDECLARED

#: The verdict. READY is the only one that means a caller may proceed, which is why the CLI gives
#: it exit 0 alone: a shell reading nothing but the exit status must never take UNKNOWN for yes.
READY = "ready"
NOT_READY = "not_ready"
STALE = "stale"
UNKNOWN = "unknown"

#: Problems the COLLECTOR can find, as opposed to the ones mergeevidence grades. Each of these is
#: a statement about the reading rather than about the candidate.
TRUNCATED = "enumeration_truncated"
NOT_PROGRESSING = "enumeration_not_progressing"
TOTAL_MOVED = "enumeration_total_moved"
DUPLICATED = "enumeration_duplicated"
COUNT_DISAGREES = "enumeration_count_disagrees"
REVIEW_UNSTABLE = "review_set_unstable"
CANDIDATE_MOVED = "candidate_moved"
SUPERSEDED = "superseded_run"
GATES_MOVED = "gates_moved"
BASE_REF_MISSING = "base_ref_missing"
UNREADABLE = "unreadable"
GATE_CONFLICT = "required_gate_conflict"

#: Which codes force which verdict, before mergeevidence grades anything. A code absent from both
#: sets leaves the verdict to the grader, which is the ordinary path.
_STALE_CODES = frozenset({CANDIDATE_MOVED, GATES_MOVED, BASE_REF_MISSING})
_UNKNOWN_CODES = frozenset({
    TRUNCATED, NOT_PROGRESSING, TOTAL_MOVED, DUPLICATED, COUNT_DISAGREES, REVIEW_UNSTABLE,
    UNREADABLE, mergeevidence.CANDIDATE_UNKNOWN,
})

#: The GitHub automatic code reviewer, which repository policy has disabled. Its results are
#: recorded like any other observation and never counted as a gate. Identifiers rather than one
#: name because the same reviewer appears as a check name, as a status context and as an app slug.
DISABLED_REVIEWERS = ("codex", "codex review", "chatgpt-codex-connector", "codex-review")

_OWNER = re.compile(r"\A[A-Za-z0-9][A-Za-z0-9-]{0,38}\Z")
_NAME = re.compile(r"\A[A-Za-z0-9._-]{1,100}\Z")
_SHA = re.compile(r"\A[0-9a-fA-F]{7,64}\Z")
_MUTATION = re.compile(r"\bmutation\b", re.IGNORECASE)
_HTTP_STATUS = re.compile(r"HTTP (\d{3})")

EXCERPT = 400


class ForgeUsage(ValueError):
    """An argument that would have been sent to the forge and should not be.

    Raised before any process starts. A repository or number that does not have the shape of one
    is a caller mistake, not a forge answer, and starting a process to find that out would put an
    unvalidated string in argv - where a leading dash is an option, not a value.
    """


class Unreadable(Exception):
    """The forge did not answer, or answered something this cannot use.

    Carries the HTTP status when the transport reported one, because 404 on a branch reference is
    a different fact from 403 on a ruleset: the first says the destination is gone and the second
    says this token cannot see it.
    """

    def __init__(self, where, detail, status=None):
        super().__init__(detail)
        self.where = where
        self.detail = detail
        self.status = status


def split_repository(value):
    """owner/name, or a refusal. Validated here so argv never carries an unexamined string."""
    text = str(value or "").strip()
    if text.count("/") != 1:
        raise ForgeUsage("a repository is written owner/name, not " + repr(value))
    owner, _, name = text.partition("/")
    if not _OWNER.match(owner) or not _NAME.match(name):
        raise ForgeUsage("a repository is written owner/name over the characters GitHub allows,"
                         " not " + repr(value))
    return owner, name


def pull_request_number(value):
    """A positive integer. A string that merely looks like one is converted, not trusted."""
    try:
        number = int(str(value).strip())
    except (TypeError, ValueError):
        raise ForgeUsage("a pull request number is a positive whole number, not " + repr(value))
    if number < 1:
        raise ForgeUsage("a pull request number is counted from one, not " + repr(value))
    return number


def commit_sha(value):
    if not _SHA.match(str(value or "")):
        raise ForgeUsage("a commit is a hexadecimal sha, not " + repr(value))
    return str(value)


def _subprocess_runner(argv, timeout):
    """The default runner: an argument array, no shell, a bounded wait."""
    completed = subprocess.run(  # noqa: S603 - argv, shell=False, values validated by the caller
        argv, capture_output=True, text=True, timeout=timeout, shell=False,
    )
    return completed.returncode, completed.stdout, completed.stderr


class Forge:
    """One forge, reached by argument array, with a budget it cannot exceed.

    The budget is not a performance concern. An enumeration that never ends looks exactly like an
    enumeration that is taking a while, and without a ceiling the difference is decided by whoever
    gets bored first. With one, running out is an event with a name, and the name maps to UNKNOWN.
    """

    def __init__(self, *, run=None, command=("gh",), page_size=100, page_budget=50,
                 call_budget=300, timeout=60):
        self._run = run or _subprocess_runner
        self.command = tuple(command)
        self.page_size = int(page_size)
        self.page_budget = int(page_budget)
        self.call_budget = int(call_budget)
        self.timeout = int(timeout)
        self.calls = []

    def _execute(self, argv, where):
        if len(self.calls) >= self.call_budget:
            raise Unreadable(where, "the collection reached its budget of "
                             + str(self.call_budget) + " forge calls before it finished, so what"
                             " it has is a prefix rather than an answer")
        try:
            code, out, err = self._run([*self.command, *argv], self.timeout)
        except subprocess.TimeoutExpired as error:
            # A timeout is an observation that did not happen, which is exactly what Unreadable
            # means. Letting it escape turned the whole command into a host failure and threw
            # away every connection already read, when the honest answer is a snapshot naming
            # the one part nobody could see.
            self.calls.append({"argv": [*self.command, *argv], "exitCode": None})
            raise Unreadable(where, "reading " + where + " exceeded the timeout of "
                             + str(self.timeout) + " seconds, so no answer was observed") from error
        self.calls.append({"argv": [*self.command, *argv], "exitCode": code})
        if code != 0:
            found = _HTTP_STATUS.search(err or "")
            status = int(found.group(1)) if found else None
            raise Unreadable(where, "reading " + where + " failed: "
                             + (err or "").strip()[:EXCERPT], status)
        try:
            return json.loads(out or "null")
        except ValueError:
            raise Unreadable(where, "reading " + where + " returned something that is not JSON")

    def rest(self, path, where, **params):
        """A GET, always. The method is stated rather than left to the default."""
        target = path + ("?" + urlencode(params) if params else "")
        return self._execute(
            ["api", "--method", "GET", "-H", "Accept: application/vnd.github+json", target], where)

    def graphql(self, document, where, **variables):
        if _MUTATION.search(document):
            raise ForgeUsage("this collector issues queries only; the document names a mutation")
        argv = ["api", "graphql", "-f", "query=" + document]
        for key in sorted(variables):
            value = variables[key]
            if value is None:
                continue
            if isinstance(value, bool):
                argv += ["-F", key + "=" + ("true" if value else "false")]
            elif isinstance(value, int):
                argv += ["-F", key + "=" + str(value)]
            else:
                argv += ["-f", key + "=" + str(value)]
        payload = self._execute(argv, where) or {}
        if payload.get("errors"):
            raise Unreadable(where, "the forge refused the query for " + where + ": "
                             + json.dumps(payload["errors"])[:EXCERPT])
        return payload.get("data") or {}


class Enumeration:
    """Items, and the evidence that they are all of the items.

    The second half is the point. A list on its own cannot distinguish "these are all of them"
    from "these are the ones I reached", and the merge gate reads the difference as the same
    number.
    """

    def __init__(self, name):
        self.name = name
        self.items = []
        self.identifiers = []
        self.pages = []
        self.total = None
        self.complete = False
        self.problems = []

    @property
    def distinct(self):
        return {one for one in self.identifiers if one}

    def record(self):
        return {
            "connection": self.name,
            "pagesRead": len(self.pages),
            "totalCount": self.total,
            "distinct": len(self.distinct),
            "complete": self.complete,
            "pages": self.pages,
        }


def enumerate_connection(name, step, identify, *, budget):
    """Read one paginated connection to the end, or say why that did not happen.

    One mechanism for every connection on purpose. The originating defect was a truncated read of
    ONE list; writing a careful paginator for that list and ordinary loops for the others
    reproduces the bug in the next list along, where a second required check sitting on page two
    is exactly as invisible as the sixty-first review thread was.

    `step(token)` returns the page's items, the total the forge reported, and the token for the
    next page or None when there is no next page. `identify(item)` returns the item's identifier,
    because a count of entries is not a count of distinct things: a repeated page keeps the length
    and substitutes an item nobody read.
    """
    found = Enumeration(name)
    token = None
    used = []
    while True:
        if len(found.pages) >= budget:
            found.problems.append(Problem(
                TRUNCATED, "the " + name + " connection was still unfinished after "
                + str(budget) + " pages, so what was read is a prefix; a prefix of a list is not"
                " a count of the list"))
            return found
        items, total, following = step(token)
        found.pages.append({"token": token, "returned": len(items), "totalCount": total})
        if total is not None:
            if found.total is None:
                found.total = total
            elif total != found.total:
                found.problems.append(Problem(
                    TOTAL_MOVED, "the " + name + " connection reported " + str(found.total)
                    + " items and then " + str(total) + " while it was being read, so no single"
                    " set of them was ever observed"))
        for item in items:
            found.items.append(item)
            found.identifiers.append(identify(item))
        if following is None:
            found.complete = True
            break
        if following == token or following in used:
            found.problems.append(Problem(
                NOT_PROGRESSING, "the " + name + " connection handed back a page token it had"
                " already used, so the read was going in a circle and its page count is not"
                " evidence of distinct pages"))
            return found
        used.append(following)
        token = following
    identifiers = [one for one in found.identifiers]
    if any(not one for one in identifiers):
        found.problems.append(Problem(
            DUPLICATED, "an item of the " + name + " connection came back with no identifier, so"
            " it cannot be told apart from another"))
    if len(found.distinct) != len(identifiers):
        found.problems.append(Problem(
            DUPLICATED, "the " + name + " connection returned the same identifier more than once,"
            " so the number of entries is not the number of items"))
    if found.total is not None and len(found.distinct) != found.total:
        found.problems.append(Problem(
            COUNT_DISAGREES, "the " + name + " connection says it holds " + str(found.total)
            + " items and " + str(len(found.distinct)) + " distinct ones were read"))
    return found


def rest_step(forge, path, key, where, **params):
    """A page of a REST collection, numbered from one."""

    def step(token):
        page = int(token or 1)
        payload = forge.rest(path, where, page=page, per_page=forge.page_size, **params) or {}
        items = payload.get(key) or []
        total = payload.get("total_count")
        # A full page may have a successor and a short one never does. The totals comparison
        # afterwards is what catches a forge whose pages and whose count disagree.
        following = str(page + 1) if len(items) >= forge.page_size else None
        return items, total, following

    return step


def cursor_step(forge, document, where, path, **variables):
    """A page of a GraphQL connection, reached by cursor."""

    def step(token):
        data = forge.graphql(document, where, after=token, first=forge.page_size, **variables)
        node = data
        for step_name in path:
            node = (node or {}).get(step_name) or {}
        info = node.get("pageInfo") or {}
        nodes = node.get("nodes") or []
        if info.get("hasNextPage"):
            cursor = info.get("endCursor")
            if not cursor:
                raise Unreadable(where, "the " + where + " connection says another page exists"
                                 " and gives no cursor to reach it")
            return nodes, node.get("totalCount"), cursor
        return nodes, node.get("totalCount"), None

    return step


_THREADS = """query($owner:String!,$name:String!,$number:Int!,$first:Int!,$after:String){
  repository(owner:$owner,name:$name){pullRequest(number:$number){
    reviewThreads(first:$first,after:$after){
      totalCount pageInfo{hasNextPage endCursor}
      nodes{id isResolved isOutdated path line originalLine
        comments(first:1){nodes{url author{login} body createdAt}}}}}}}"""

_REVIEWS = """query($owner:String!,$name:String!,$number:Int!,$first:Int!,$after:String){
  repository(owner:$owner,name:$name){pullRequest(number:$number){
    reviews(first:$first,after:$after){
      totalCount pageInfo{hasNextPage endCursor}
      nodes{id state url body submittedAt author{login}}}}}}"""

_COMMENTS = """query($owner:String!,$name:String!,$number:Int!,$first:Int!,$after:String){
  repository(owner:$owner,name:$name){pullRequest(number:$number){
    comments(first:$first,after:$after){
      totalCount pageInfo{hasNextPage endCursor}
      nodes{id url body createdAt author{login}}}}}}"""


def _excerpt(value):
    text = " ".join(str(value or "").split())
    return text[:EXCERPT]


def _login(node):
    return ((node or {}).get("author") or {}).get("login") or ""


def _thread_finding(node):
    comment = ((node.get("comments") or {}).get("nodes") or [{}])[0]
    return {
        "kind": "reviewThread",
        "id": node.get("id"),
        "resolved": bool(node.get("isResolved")),
        "outdated": bool(node.get("isOutdated")),
        "path": node.get("path"),
        "line": node.get("line") if node.get("line") is not None else node.get("originalLine"),
        "author": _login(comment),
        "url": comment.get("url"),
        "excerpt": _excerpt(comment.get("body")),
    }


def _provider(check):
    """Which app published a check, as the rule names it.

    A branch rule binds a context to an integration id, so that is the identity compared. The
    slug is kept beside it for a reader, but a name is not what the rule says.
    """
    app = check.get("app") or {}
    identifier = app.get("id")
    return str(identifier) if identifier is not None else None


def _outcome(entry):
    """The word that goes in the conclusion field, which is never allowed to be nothing.

    The predicate needs a string there, and an unfinished check has no conclusion. Leaving it null
    would fail the shape check at best and be coerced at worst, so an unfinished check reports its
    STATUS - queued, in_progress, waiting - in that field. It reads as what it is, and it cannot
    equal "success".
    """
    conclusion = entry.get("conclusion")
    if isinstance(conclusion, str) and conclusion.strip():
        return conclusion
    status = entry.get("status")
    if isinstance(status, str) and status.strip():
        return status
    return "unknown"



LATE_FINDING = "late_finding"
RECORD_INVALID = "record_invalid"

#: The candidate fields whose movement makes everything collected about the wrong thing. The
#: merge state is deliberately NOT here: the forge computes it asynchronously, so it routinely
#: settles from UNKNOWN to CLEAN mid-collection, and calling that movement would make almost
#: every reading stale. It is graded from the RE-READ instead, which is the fresher answer.
DRIFT_FIELDS = ("headSha", "baseSha", "baseRef", "state", "merged", "isDraft")

_BRANCH = re.compile(r"\A[^\s?#%:`^\\]+\Z")


def branch_ref(value):
    """A branch name safe to put in a URL path.

    Slashes stay, because `release/1.2` is one branch and encoding its separator asks the forge
    about a branch that does not exist. Everything that could leave the path or start a query is
    refused instead of escaped, so nothing here has to reason about what the forge would do with
    a clever one.
    """
    text = str(value or "").strip()
    if not text or ".." in text or text.startswith("/") or not _BRANCH.match(text):
        raise ForgeUsage("a branch name is a path segment without traversal or query characters,"
                         " not " + repr(value))
    return text


def _now():
    return datetime.now(timezone.utc).isoformat()


def _read(forge, name, step, identify, problems):
    """Enumerate, and turn a transport failure into a problem rather than an exception.

    One unreadable connection should not lose the rest of the collection. A reader learns more
    from a snapshot that says which part it could not see than from a traceback.
    """
    try:
        found = enumerate_connection(name, step, identify, budget=forge.page_budget)
    except Unreadable as error:
        found = Enumeration(name)
        found.problems.append(Problem(UNREADABLE, error.detail))
    except ForgeUsage:
        raise
    problems.extend(found.problems)
    return found


def _candidate(forge, owner, name, number):
    payload = forge.rest("repos/" + owner + "/" + name + "/pulls/" + str(number),
                         "the pull request") or {}
    head = payload.get("head") or {}
    base = payload.get("base") or {}
    return {
        "number": payload.get("number"),
        "url": payload.get("html_url"),
        "state": payload.get("state"),
        "merged": bool(payload.get("merged")),
        "isDraft": bool(payload.get("draft")),
        "headSha": head.get("sha"),
        "baseSha": base.get("sha"),
        "baseRef": base.get("ref"),
        "mergeable": payload.get("mergeable"),
        "mergeStateStatus": str(payload.get("mergeable_state") or "unknown").upper(),
    }


def _review(forge, owner, name, number, problems, connections):
    """The review threads, enumerated - and enumerated twice when the answer would open the gate.

    A stable `totalCount` across pages does not prove a stable SET. One thread deleted between the
    first page and the third while another is added leaves the total unchanged, and a thread that
    reopens after its own page was read leaves the total AND the identifiers unchanged while the
    answer silently rots.

    Reading everything twice would double the cost of every collection. Instead the second pass is
    spent exactly where being wrong is unsafe: `unresolved = 0` is the value that opens the merge
    gate, so that is the answer that has to survive being asked again. A non-zero count already
    refuses and needs no confirmation.

    This does not make the read atomic and does not claim to. A thread can reopen during the
    second pass, after its page was read. What the second pass buys is a narrower window; what
    closes the rest is the exact-head guard at merge time and the late-finding path afterwards.
    """
    def enumerate_threads(label):
        step = cursor_step(forge, _THREADS, label,
                           ("repository", "pullRequest", "reviewThreads"),
                           owner=owner, name=name, number=number)
        return _read(forge, label, step, lambda node: node.get("id"), problems)

    first = enumerate_threads("review threads")
    connections.append(first.record())
    findings = [_thread_finding(node) for node in first.items]
    unresolved = [one for one in findings if not one["resolved"]]
    if first.total is None and first.complete:
        problems.append(Problem(
            COUNT_DISAGREES, "the review thread connection did not report a total, so there is"
            " nothing to compare the threads that were read against"))
    if first.complete and not first.problems and not unresolved:
        second = enumerate_threads("review threads (confirming pass)")
        connections.append(second.record())
        if second.complete and not second.problems:
            before = {one["id"]: one["resolved"] for one in findings}
            after = {node.get("id"): bool(node.get("isResolved")) for node in second.items}
            if before != after:
                problems.append(Problem(
                    REVIEW_UNSTABLE, "the review threads were read twice and the two readings"
                    " disagree about which threads exist or which are resolved, so no stable set"
                    " of them was observed and the zero this would have reported is not a count"))
    coverage = {
        "hasNextPage": not first.complete,
        "pagesRead": len(first.pages),
        "totalCount": first.total if first.total is not None else len(first.distinct),
        "threadsSeen": sorted(str(one) for one in first.distinct),
        "unresolved": len(unresolved),
    }
    return coverage, findings


def _discussion(forge, owner, name, number, problems, connections):
    """Submitted reviews and summary comments, which are findings with no thread attached.

    Inline threads are not the only place a defect lands. A review body saying the approach is
    wrong, or a summary comment naming a regression, blocks a merge exactly as much and has no
    thread to be unresolved. Merge readiness asks for all of them, so all of them are enumerated
    under the same contract - and, like the threads, none of them is triaged here.
    """
    findings = []
    reviews = _read(
        forge, "submitted reviews",
        cursor_step(forge, _REVIEWS, "submitted reviews",
                    ("repository", "pullRequest", "reviews"),
                    owner=owner, name=name, number=number),
        lambda node: node.get("id"), problems)
    connections.append(reviews.record())
    for node in reviews.items:
        findings.append({
            "kind": "review",
            "id": node.get("id"),
            "state": node.get("state"),
            "author": _login(node),
            "url": node.get("url"),
            "submittedAt": node.get("submittedAt"),
            "excerpt": _excerpt(node.get("body")),
        })
    comments = _read(
        forge, "summary comments",
        cursor_step(forge, _COMMENTS, "summary comments",
                    ("repository", "pullRequest", "comments"),
                    owner=owner, name=name, number=number),
        lambda node: node.get("id"), problems)
    connections.append(comments.record())
    for node in comments.items:
        findings.append({
            "kind": "comment",
            "id": node.get("id"),
            "author": _login(node),
            "url": node.get("url"),
            "createdAt": node.get("createdAt"),
            "excerpt": _excerpt(node.get("body")),
        })
    return findings


def _checks(forge, owner, name, head, problems, connections):
    """Every check on this head, with an identity that does not collapse and an honest attempt.

    Identity is the workflow run AND the job name, not the name alone. On one real head of this
    repository two separate workflow runs each published a check called `dev-gate`; keyed by name,
    one of them silently replaces the other, and if the one that vanishes is the failing one the
    gate opens. Keyed by run and job, both are present and both have to be green.

    The attempt is the JOB's own `run_attempt`, not the attempt its workflow run is currently on.
    Taking it from the run relabels an already-read attempt-1 success as attempt 2 the moment a
    re-run starts, which inverts the very rule the attempt exists to serve - and a job that was
    not re-run in a later attempt keeps its own identity, so it is not read as missing either.

    Two runs of the SAME workflow and event on one head are a different relation again: the newer
    one REPLACED the older, which is what this repository's own CI does when it cancels an
    in-progress run on a new event. Graded as peers, the cancelled run's failed gate blocks a head
    whose current run is green - observed on this pull request, where a cancelled `dev-gate` sat
    beside a successful one. So the newest run of each workflow and event answers for it, and the
    ones it replaced are recorded as superseded rather than graded. Different workflows sharing a
    name are still peers and still both binding.
    """
    root = "repos/" + owner + "/" + name
    entries, detail = [], []
    runs = _read(forge, "workflow runs",
                 rest_step(forge, root + "/actions/runs", "workflow_runs", "workflow runs",
                           head_sha=head),
                 lambda run: run.get("id"), problems)
    connections.append(runs.record())
    newest = {}
    for run in runs.items:
        try:
            identifier = int(run.get("id"))
        except (TypeError, ValueError):
            continue
        lane = (run.get("workflow_id"), run.get("event"))
        if lane not in newest or identifier > newest[lane]:
            newest[lane] = identifier
    superseded = []
    job_ids = set()
    by_job = []
    for run in runs.items:
        try:
            run_id = str(int(run.get("id")))
        except (TypeError, ValueError):
            problems.append(Problem(UNREADABLE, "a workflow run came back without a usable id,"
                                    " so its jobs cannot be read"))
            continue
        replaced = newest.get((run.get("workflow_id"), run.get("event"))) != int(run_id)
        if replaced:
            superseded.append({"runId": run_id, "workflowId": run.get("workflow_id"),
                               "event": run.get("event"), "url": run.get("html_url"),
                               "conclusion": run.get("conclusion")})
        where = "jobs of workflow run " + run_id
        jobs = _read(forge, where,
                     rest_step(forge, root + "/actions/runs/" + run_id + "/jobs", "jobs", where,
                               filter="all"),
                     lambda job: job.get("id"), problems)
        connections.append(jobs.record())
        for job in jobs.items:
            job_ids.add(job.get("id"))
            job_name = str(job.get("name") or "")
            if not replaced:
                entry = {
                    "runId": "workflow-run:" + run_id + ":" + job_name,
                    "name": job_name,
                    "headSha": str(run.get("head_sha") or ""),
                    "conclusion": _outcome(job),
                    "attempt": int(job.get("run_attempt") or 1),
                    "provider": None,
                }
                entries.append(entry)
                by_job.append((entry, job.get("id")))
            detail.append({
                "source": "workflow-job",
                "runId": "workflow-run:" + run_id + ":" + job_name,
                "name": job_name,
                "superseded": replaced,
                "status": job.get("status"),
                "conclusion": job.get("conclusion"),
                "attempt": int(job.get("run_attempt") or 1),
                "startedAt": job.get("started_at"),
                "completedAt": job.get("completed_at"),
                "url": job.get("html_url"),
                "workflowRunUrl": run.get("html_url"),
                "workflowName": run.get("name"),
            })
    published = _read(forge, "check runs",
                      rest_step(forge, root + "/commits/" + head + "/check-runs", "check_runs",
                                "check runs", filter="latest"),
                      lambda check: check.get("id"), problems)
    connections.append(published.record())
    # The jobs endpoint does not carry the publishing app, and a branch rule can bind a context
    # to one. The two endpoints describe the same objects - a job's id IS its check run's id - so
    # the identity is taken from the one that states it.
    provider_of = {check.get("id"): _provider(check) for check in published.items}
    for entry, identifier in by_job:
        entry["provider"] = provider_of.get(identifier)
    for check in published.items:
        if check.get("id") in job_ids:
            # The same object under another endpoint. Adding it again would put one check in the
            # set twice under two identities, and the copy without an attempt would look newer.
            continue
        check_name = str(check.get("name") or "")
        entries.append({
            "runId": "check-run:" + str(check.get("id")),
            "name": check_name,
            "headSha": str(check.get("head_sha") or ""),
            "conclusion": _outcome(check),
            "attempt": 1,
            "provider": _provider(check),
        })
        detail.append({
            "source": "check-run",
            "runId": "check-run:" + str(check.get("id")),
            "name": check_name,
            "superseded": False,
            "status": check.get("status"),
            "conclusion": check.get("conclusion"),
            "attempt": 1,
            "startedAt": check.get("started_at"),
            "completedAt": check.get("completed_at"),
            "url": check.get("html_url"),
            "app": ((check.get("app") or {}).get("slug")),
            "provider": _provider(check),
        })
    statuses = _read(forge, "commit statuses",
                     rest_step(forge, root + "/commits/" + head + "/status", "statuses",
                               "commit statuses"),
                     lambda status: status.get("context"), problems)
    connections.append(statuses.record())
    for status in statuses.items:
        context = str(status.get("context") or "")
        entries.append({
            "runId": "status:" + context,
            "name": context,
            "headSha": head,
            "conclusion": str(status.get("state") or "unknown"),
            "attempt": 1,
            # A commit status carries no check-run app, so it can never answer for a context
            # whose rule names an integration. Left None rather than guessed.
            "provider": None,
        })
        detail.append({
            "source": "commit-status",
            "runId": "status:" + context,
            "name": context,
            "superseded": False,
            "status": status.get("state"),
            "conclusion": status.get("state"),
            "attempt": 1,
            "url": status.get("target_url"),
            "updatedAt": status.get("updated_at"),
        })
    return entries, detail, superseded


def _gates(forge, owner, name, base_ref, problems):
    """What this branch actually requires, and the difference between empty and unknown.

    The endpoint is the effective-rules one rather than branch protection, because ordinary read
    access can see it and branch protection needs admin - which would make "unreadable" the normal
    answer and unknown the normal verdict.

    An empty array is ambiguous in a way that matters: a branch with no rules and a branch that
    DOES NOT EXIST return the identical response. Read as "this branch requires nothing", a
    vanished base would let a single optional green check satisfy the no-required path. So the
    base reference is resolved first, and an empty array counts as a declaration only for a branch
    that demonstrably exists.
    """
    root = "repos/" + owner + "/" + name
    gates = {
        "readable": False,
        "baseRefExists": None,
        "requiredDeclared": UNDECLARED,
        "requiredProviders": {},
        "strictBase": False,
        "threadResolutionRequired": None,
        # Descriptive, and said so rather than left looking operative. This workflow refuses an
        # unresolved thread whether or not the branch demands resolution, because OPS-9.2 is
        # stricter than the forge here; reading this field could only ever loosen that, so it is
        # recorded for a reader and never consulted by a verdict.
        "threadResolutionNote": "recorded for the reader; the handoff refuses an unresolved"
                                " thread regardless, so this cannot loosen the gate",
        "digest": None,
    }
    try:
        reference = branch_ref(base_ref)
    except ForgeUsage as error:
        problems.append(Problem(UNREADABLE, str(error)))
        return gates
    try:
        forge.rest(root + "/git/ref/heads/" + quote(reference, safe="/"), "the base branch")
        gates["baseRefExists"] = True
    except Unreadable as error:
        if error.status == 404:
            gates["baseRefExists"] = False
            problems.append(Problem(
                BASE_REF_MISSING, "the base branch " + repr(reference) + " does not exist, so"
                " this candidate has no destination and its gates cannot be read from one"))
        else:
            problems.append(Problem(UNREADABLE, error.detail))
        return gates
    try:
        rules = forge.rest(root + "/rules/branches/" + quote(reference, safe="/"),
                           "the effective branch rules")
    except Unreadable as error:
        problems.append(Problem(UNREADABLE, error.detail))
        return gates
    contexts = []
    integrations = {}
    for rule in rules or []:
        if not isinstance(rule, dict):
            continue
        parameters = rule.get("parameters") or {}
        if rule.get("type") == "required_status_checks":
            if parameters.get("strict_required_status_checks_policy"):
                gates["strictBase"] = True
            for one in parameters.get("required_status_checks") or []:
                context = str((one or {}).get("context") or "").strip()
                if context:
                    contexts.append(context)
                    integration = (one or {}).get("integration_id")
                    if integration is not None:
                        # The rule binds this context to one app. Dropping it accepts a
                        # namesake from any other, which is the same collapse as grading two
                        # different runs by their shared name.
                        integrations[context] = str(integration)
        elif rule.get("type") == "pull_request":
            gates["threadResolutionRequired"] = bool(
                parameters.get("required_review_thread_resolution"))
    gates["readable"] = True
    gates["requiredDeclared"] = sorted(set(contexts))
    gates["requiredProviders"] = integrations
    gates["digest"] = hashlib.sha256(json.dumps({
        "required": gates["requiredDeclared"],
        "providers": integrations,
        "strictBase": gates["strictBase"],
        "threadResolutionRequired": gates["threadResolutionRequired"],
    }, sort_keys=True).encode("utf-8")).hexdigest()
    return gates


def _conflicting_reviewers(required, disabled):
    lowered = {str(one).strip().lower() for one in disabled or ()}
    return [str(one) for one in (required or ()) if str(one).strip().lower() in lowered]


def verdict_of(problems):
    """Four answers, in the order that keeps the least certain one from being swallowed.

    Stale first: if the candidate moved, everything read is about another commit, which explains
    any inconsistency below it. Then unknown, because a reading that did not happen must never be
    reported as a candidate that is merely not ready yet. Then the grader's own verdict.
    """
    codes = {problem.code for problem in problems}
    if codes & _STALE_CODES:
        return STALE
    if codes & _UNKNOWN_CODES:
        return UNKNOWN
    if problems:
        return NOT_READY
    return READY


def collect(forge, *, repository, number, disabled_reviewers=DISABLED_REVIEWERS):
    """Observe one pull request and return the record, the findings and the verdict.

    The head and base are pinned before anything else is read and re-read after everything, and
    the effective rules are re-read with them: pinning the head while the GATES move would leave
    every check unchanged and the answer still wrong.
    """
    owner, name = split_repository(repository)
    number = pull_request_number(number)
    started = _now()
    problems, connections = [], []

    def snapshot(pinned=None, reread=None, coverage=None, findings=None, checks=None,
                 detail=None, gates=None, superseded=None):
        gates = gates or {"readable": False, "requiredDeclared": UNDECLARED, "strictBase": False,
                          "baseRefExists": None, "threadResolutionRequired": None,
                          "requiredProviders": {}, "digest": None}
        pinned = pinned or {}
        required = gates.get("requiredDeclared", UNDECLARED)
        conflicting = _conflicting_reviewers(
            required if required is not UNDECLARED else (), disabled_reviewers)
        if conflicting:
            # Kept in the required set rather than dropped. Repository policy says the disabled
            # reviewer is not a gate AND that a rule still demanding it is a configuration
            # conflict to report - so dropping it here would silently satisfy a rule nobody fixed.
            problems.append(Problem(
                GATE_CONFLICT, "this branch declares " + repr(conflicting[0]) + " required, and"
                " that reviewer is disabled by policy; it stays in the required set because"
                " removing it here would hide a rule that needs an authorised correction"))
        handoff = {
            "isDraft": bool(pinned.get("isDraft")),
            "baseVerifiedAt": (reread or {}).get("verifiedAt") or _now(),
            # The base the checks were read against, carried so the parent's restatement can
            # notice a destination that moved. A record naming when its base was verified and
            # not WHICH base hands over half of the comparison.
            "baseSha": pinned.get("baseSha"),
            "baseRef": pinned.get("baseRef"),
            "reviewCoverage": coverage or {},
            "checks": checks or [],
            "requiredDeclared": required,
            "requiredProviders": gates.get("requiredProviders") or {},
            # Judgements, left empty on purpose. An observer that filled these would be
            # certifying its own evidence; they belong to the child and the parent.
            "threadDispositions": [],
            "criterionEvidence": [],
            "limitations": [],
        }
        return {
            "repository": owner + "/" + name,
            "number": number,
            "url": pinned.get("url"),
            "observation": {"startedAt": started, "finishedAt": _now(),
                            "atomic": False,
                            "note": "this observation is not atomic with any merge that follows"
                                    " it; the exact-head guard at merge time and the late-finding"
                                    " path afterwards are what bound the window"},
            "pinned": pinned,
            "reread": reread,
            "gates": gates,
            "supersededRuns": superseded or [],
            "connections": connections,
            "handoff": handoff,
            "findings": findings or [],
            "checkDetail": detail or [],
            "problems": [{"code": one.code, "detail": one.detail} for one in problems],
            "verdict": verdict_of(problems),
            "provenance": {"calls": forge.calls, "disabledReviewers": list(disabled_reviewers),
                           "conflictingRequiredReviewers": conflicting},
        }

    try:
        pinned = _candidate(forge, owner, name, number)
    except Unreadable as error:
        problems.append(Problem(UNREADABLE, error.detail))
        return snapshot()
    head = pinned.get("headSha")
    if not head or not _SHA.match(str(head)):
        problems.append(Problem(UNREADABLE, "the pull request did not report a head commit, so"
                                " there is nothing to collect evidence about"))
        return snapshot(pinned=pinned)

    coverage, findings = _review(forge, owner, name, number, problems, connections)
    discussion = _discussion(forge, owner, name, number, problems, connections)
    findings = findings + discussion
    reviews = [one for one in discussion if one.get("kind") == "review"]
    checks, detail, superseded = _checks(forge, owner, name, str(head), problems, connections)
    gates = _gates(forge, owner, name, pinned.get("baseRef"), problems)

    reread = None
    graded = pinned
    try:
        after = _candidate(forge, owner, name, number)
        reread = dict(after, verifiedAt=_now())
        # Every field whose movement makes the collection about a different thing, not just the
        # two shas. Closing the candidate, converting it to a draft or retargeting it to another
        # branch all leave both shas untouched and all invalidate what was read.
        drifted = [field for field in DRIFT_FIELDS
                   if after.get(field) != pinned.get(field)]
        if drifted:
            problems.append(Problem(
                CANDIDATE_MOVED, "the candidate changed while this was being collected ("
                + ", ".join(field + ": " + repr(pinned.get(field)) + " to "
                            + repr(after.get(field)) for field in drifted)
                + "), so what was read describes something that is no longer the candidate"))
        else:
            # Nothing moved, so the fresher merge state is simply the better reading of the same
            # candidate. The forge computes it asynchronously and it routinely settles from
            # unknown during a collection; grading the first answer would report unknown about a
            # candidate the forge had already made its mind up about.
            graded = after
    except Unreadable as error:
        problems.append(Problem(UNREADABLE, "the pull request could not be re-read to confirm it"
                                " had not moved: " + error.detail))
    if gates.get("digest"):
        confirming = []
        again = _gates(forge, owner, name, pinned.get("baseRef"), confirming)
        problems.extend(confirming)
        if again.get("digest") and again["digest"] != gates["digest"]:
            problems.append(Problem(
                GATES_MOVED, "the branch's effective rules changed while this was being"
                " collected, so the checks that were read were graded against gates that are no"
                " longer the ones this branch declares"))

    problems.extend(mergeevidence.candidate_problems(
        graded, strict_base=bool(gates.get("strictBase"))))
    problems.extend(mergeevidence.handoff_problems(
        str(head), coverage, checks, required=gates.get("requiredDeclared", UNDECLARED),
        providers=gates.get("requiredProviders"), reviews=reviews))
    return snapshot(pinned=pinned, reread=reread, coverage=coverage, findings=findings,
                    checks=checks, detail=detail, gates=gates, superseded=superseded)


def restate_problems(head_sha, record, snapshot):
    """The parent's currency check: restate the child's record against a reading of its own.

    OPS-9.3 gives the parent currency and gives the child enumeration, and this is the shape of
    that division. The parent does not paginate the review again to re-derive what the child
    already established; it takes a fresh snapshot and asks whether the record still describes the
    candidate.

    This is also the answer to the obvious objection that a child could simply write a plausible
    record without ever running the collector. It could. Nothing a child writes about itself is
    unforgeable, and a provenance field it also writes would not change that. What makes the
    record accountable is that it has to survive a reading the child did not produce.

    OPS-9.4's late finding is the case worth naming: a thread that appears on the SAME head, not
    in the record's threadsSeen, invalidates the record even though nothing moved. An invalidated
    record is not a verdict; it returns to the child that produced it.

    The record is GRADED before it is compared. Restatement that only looked for late threads
    accepted an empty record on a pull request with no threads: there was nothing to be late, so
    nothing objected, and a caller who had collected no evidence at all passed the currency
    check. Comparing an unvalidated record says only that it does not disagree with the forge.
    """
    if not isinstance(record, dict):
        return [Problem(mergeevidence.MALFORMED,
                        "a handoff record is an object, not a " + type(record).__name__)]
    problems = []
    pinned = (snapshot or {}).get("pinned") or {}
    observed = pinned.get("headSha")
    problems.extend(mergeevidence.handoff_problems(
        str(head_sha or ""), record.get("reviewCoverage"), record.get("checks") or [],
        required=record.get("requiredDeclared", mergeevidence.UNDECLARED),
        providers=record.get("requiredProviders")))
    if head_sha and observed and str(head_sha) != str(observed):
        problems.append(Problem(
            CANDIDATE_MOVED, "the record is about head " + repr(str(head_sha)) + " and the forge"
            " now reports " + repr(str(observed)) + ", so the record describes a commit that is"
            " no longer the candidate"))
    recorded_base = record.get("baseSha")
    if recorded_base and pinned.get("baseSha") and str(recorded_base) != str(pinned["baseSha"]):
        # The destination moved under an unchanged head. Every merge-result check the record
        # carries was computed against the old base, and a branch that requires currency will
        # not take them.
        problems.append(Problem(
            CANDIDATE_MOVED, "the record was verified against base " + repr(str(recorded_base))
            + " and the candidate now targets " + repr(str(pinned["baseSha"]))
            + ", so its checks cover a merge that is no longer the one being made"))
    elif not recorded_base:
        problems.append(Problem(
            RECORD_INVALID, "the record does not name the base commit it was verified against,"
            " so a destination that moved under an unchanged head cannot be noticed"))
    for one in (snapshot or {}).get("problems") or []:
        problems.append(Problem(one.get("code", UNREADABLE), one.get("detail", "")))
    coverage = record.get("reviewCoverage") or {}
    seen = {str(one) for one in coverage.get("threadsSeen") or []}
    late = [one for one in (snapshot or {}).get("findings") or []
            if one.get("kind") == "reviewThread" and str(one.get("id")) not in seen]
    if late:
        problems.append(Problem(
            LATE_FINDING, str(len(late)) + " review thread(s) on this head are not in the"
            " record's threadsSeen, so the record did not see them and no longer describes the"
            " candidate: " + ", ".join(sorted(str(one.get("url") or one.get("id"))
                                              for one in late)[:5])))
    return problems
