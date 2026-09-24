"""Where a merge target's base branch points, read by the relay rather than stated to it.

CRW-229. A landing used to record whatever base sha its caller typed, and the next candidate on
the target had to restate exactly that value before it could merge. In the CRW-124 G1 trial both
parents typed the base they had CHECKED against rather than the one the branch pointed at after
the merge, and their verified successors were refused with no way back. A value every later
check depends on is not something to take on trust when the thing it describes can be read.

So the merge turn reads ONE fact from the target - the commit its base branch points at - at the
four moments it records one: the currency check before merging, a landing, the resolution of an
unknown outcome, and a restatement.
Nothing else about the target is read here. Checks and review are still restated by the caller
and cross-checked by mergeevidence; this module has no opinion about whether a candidate may
merge.

Two kinds of repository, told apart by the shape of the string the claim was made with:

An ABSOLUTE PATH is a repository on this host, read with git. The lookup is exact
(show-ref --verify on refs/heads/<branch>), so revision syntax in a branch name is never
evaluated: rev-parse would have answered main~1 with the parent commit, which is precisely the
pre-landing base this exists to stop recording. Discovery is off (--git-dir), so a path inside
some other repository cannot read that repository's branch instead. Every GIT_* variable is
removed from the child's environment, because several of them change what git sees. The path
has to be the repository the merge goes INTO; a working clone's branch can be behind or ahead of
it, and nothing here can tell. A linked worktree's .git is a file naming its git directory;
git follows that pointer itself when it is given as --git-dir, so the worktree reads the
repository's own branches.

An OWNER/NAME is a GitHub repository, read through forge.Forge: argv, GET only, validated, the
same reader merge-evidence uses.

Anything else - and any failure to read - is TargetUnreadable. It is never a guess and never a
fallback to the caller's value: the merge turn refuses rather than record something it did not
read.
"""

import os
import re
import subprocess
from urllib.parse import quote

from . import forge

LOCAL_GIT = "local_git"
GITHUB = "github"

#: A full object name. Forty for SHA-1, sixty-four for SHA-256 repositories. Abbreviations are
#: not accepted from a reader: the value is recorded and compared exactly by the next check.
_FULL_SHA = re.compile(r"\A(?:[0-9a-f]{40}|[0-9a-f]{64})\Z")
_GITHUB_SHA = re.compile(r"\A[0-9a-f]{40}\Z")
_SLUG = re.compile(r"\A[A-Za-z0-9](?:[A-Za-z0-9-]{0,38})/[A-Za-z0-9._-]{1,100}\Z")
#: Characters git reads as revision syntax or refuses in a ref name. show-ref's exact lookup
#: would not evaluate them, but a name that needs them is not a branch this can mean.
_REVISION_SYNTAX = ("~", "^", ":", "*", "[", "\\", "@{", "?")

#: The child's environment is rebuilt with these, and nothing else from the GIT_* namespace.
_GIT_ENV = {"GIT_TERMINAL_PROMPT": "0", "LC_ALL": "C"}

EXCERPT = 400


class TargetUnreadable(Exception):
    """The base branch could not be read, and nothing was assumed in its place."""

    def __init__(self, detail):
        self.detail = detail
        super().__init__(detail)


def _branch(base_ref):
    if not isinstance(base_ref, str) or base_ref != base_ref.strip():
        # The turn's target key is derived from the raw string, so reading a stripped name
        # could reach a branch the key does not name.
        raise TargetUnreadable(
            "a base ref is a branch name without surrounding whitespace, not " + repr(base_ref))
    try:
        branch = forge.branch_ref(base_ref)
    except forge.ForgeUsage as error:
        raise TargetUnreadable(str(error)) from error
    if (branch.startswith("-") or branch.endswith("/") or branch.endswith(".lock")
            or any(part in branch for part in _REVISION_SYNTAX)):
        raise TargetUnreadable(
            "a base ref is a plain branch name; " + repr(base_ref) + " carries revision syntax"
            " or a form git refuses, so it names no branch this can read exactly")
    return branch


def _run(argv, timeout, env):
    completed = subprocess.run(  # noqa: S603 - argv, shell=False, values validated above
        argv, capture_output=True, text=True, timeout=timeout, shell=False, env=env,
        stdin=subprocess.DEVNULL)
    return completed.returncode, completed.stdout, completed.stderr


class TargetReader:
    """Reads where a target's base branch points. Injectable, so tests never start gh."""

    def __init__(self, *, run=None, forge_factory=None, git=("git",), timeout=30):
        self._run = run or _run
        self._forge_factory = forge_factory or (lambda: forge.Forge(timeout=timeout))
        self.git = tuple(git)
        self.timeout = int(timeout)

    def tip(self, repository, base_ref):
        """{"sha", "source", "reference"}, or TargetUnreadable. Never the caller's value."""
        branch = _branch(base_ref)
        repository = str(repository or "")
        if os.path.isabs(repository):
            return self._local(repository, branch)
        if _SLUG.match(repository):
            return self._github(repository, branch)
        raise TargetUnreadable(
            "repository " + repr(repository) + " is neither an absolute local path nor"
            " owner/name, so there is no target this can read")

    def _local(self, path, branch):
        if not os.path.isdir(path):
            raise TargetUnreadable("repository " + repr(path) + " is not a directory here")
        dotgit = os.path.join(path, ".git")
        git_dir = dotgit if os.path.exists(dotgit) else path
        reference = "refs/heads/" + branch
        env = {key: value for key, value in os.environ.items() if not key.startswith("GIT_")}
        env.update(_GIT_ENV)
        argv = [*self.git, "--git-dir=" + git_dir, "show-ref", "--verify", "--hash", reference]
        try:
            code, out, err = self._run(argv, self.timeout, env)
        except subprocess.TimeoutExpired:
            raise TargetUnreadable(
                "reading " + reference + " in " + repr(path) + " exceeded "
                + str(self.timeout) + " seconds")
        except OSError as error:
            raise TargetUnreadable("git could not be started: " + str(error))
        if code != 0:
            raise TargetUnreadable(
                "git could not read " + reference + " in " + repr(path) + ": "
                + ((err or "").strip()[:EXCERPT] or "exit " + str(code)))
        sha = (out or "").strip()
        if not _FULL_SHA.match(sha):
            raise TargetUnreadable(
                "git answered " + repr(sha[:EXCERPT]) + " for " + reference
                + ", which is not a full object name")
        return {"sha": sha, "source": LOCAL_GIT, "reference": reference, "repository": path}

    def _github(self, slug, branch):
        reference = "refs/heads/" + branch
        host = os.environ.get("GH_HOST") or "github.com"
        try:
            payload = self._forge_factory().rest(
                "repos/" + slug + "/git/ref/heads/" + quote(branch, safe="/"),
                "the base branch")
        except forge.Unreadable as error:
            if error.status == 404:
                raise TargetUnreadable(
                    "branch " + repr(branch) + " does not exist in " + slug) from error
            if error.status == 409:
                raise TargetUnreadable(slug + " is an empty repository") from error
            raise TargetUnreadable(error.detail) from error
        except (OSError, ValueError) as error:
            raise TargetUnreadable("the forge could not be read: " + str(error)) from error
        if not isinstance(payload, dict):
            raise TargetUnreadable(
                "the forge answered " + reference + " with something that is not one ref")
        target = payload.get("object") if isinstance(payload.get("object"), dict) else {}
        sha = str(target.get("sha") or "")
        if (payload.get("ref") != reference or target.get("type") != "commit"
                or not _GITHUB_SHA.match(sha)):
            raise TargetUnreadable(
                "the forge's answer for " + reference + " does not name that branch's commit")
        return {"sha": sha, "source": GITHUB + ":" + host, "reference": reference,
                "repository": slug}


def same_commit(one, other):
    """Whether two stated object names are the same commit: the same full name, any case.

    An abbreviation is not accepted. Two different commits can share a prefix, and nothing
    here can tell whether the one a caller typed is ambiguous in the target, so a match on a
    prefix could let a wrong value through a cross-check. Upper case is the same name.
    """
    left, right = str(one or "").strip().lower(), str(other or "").strip().lower()
    return bool(left) and left == right
