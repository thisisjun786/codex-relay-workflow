"""Live-Python whole-output scenarios for todo 24-D SLF-1..15."""
import json
import os
import shutil
import sys

from codex_session_relay.errors import DeliveryRefused
from codex_session_relay.settings import TaskSettings

WORK = "/workspace/example/worktree"
BASE_SANDBOX = {
    "type": "workspaceWrite", "networkAccess": False, "writableRoots": [WORK],
    "excludeTmpdirEnvVar": False, "excludeSlashTmp": False,
}
BASE = {
    "approvalPolicy": "never", "sandbox": BASE_SANDBOX, "cwd": WORK,
    "runtimeWorkspaceRoots": [WORK], "model": "gpt", "reasoningEffort": "high",
    "environments": [{"environmentId": "local", "cwd": WORK,
                      "runtimeWorkspaceRoots": [WORK]}],
}


def merged(base, **changes):
    out = dict(base)
    out.update(changes)
    return out


def settings(data=None, free=True):
    value = TaskSettings(dict(BASE if data is None else data))
    value.settings_free_resume = free
    return value


def answer(**changes):
    out = merged(BASE, activePermissionProfile=None,
                 thread={"environments": BASE["environments"]})
    out.update(changes)
    return out


def refusal(call):
    try:
        value = call()
        return {"ok": value}
    except DeliveryRefused as exc:
        return {"reason": exc.reason.value, "detail": exc.detail}


def check(record, observed, free=True, status="idle"):
    view = settings(record, free)
    findings = view.mismatches(observed, transmitted=not free)
    first = findings[0] if findings else None
    code = first.get("code") if first else None
    if first and free and code == "settings_not_preserved":
        code = "settings_differ_after_load"
    return {"code": code, "findings": findings}


def main(case):
    if case == "SLF-1":
        plain = settings()
        return {"resume": {"threadId": "thread-1", "excludeTurns": True},
                "accepted": check(BASE, answer()),
                "drift": check(BASE, answer(model="other"))}
    if case == "SLF-2":
        view = settings(free=False)
        return {"resume": view.resume_params("thread-1"),
                "accepted": check(BASE, answer(), False)}
    if case == "SLF-3":
        wide = answer(runtimeWorkspaceRoots=[WORK, "/"])
        narrow_record = merged(BASE, runtimeWorkspaceRoots=[WORK, "/shared"])
        return {"narrow": check(narrow_record, answer()), "wide": check(BASE, wide),
                "model": check(BASE, answer(model="other")),
                "effort": check(BASE, answer(reasoningEffort="low"))}
    if case == "SLF-4":
        return {"settingsFreeResume": settings().settings_free_resume,
                "resume": {"threadId": "sup-thread", "excludeTurns": True},
                "accepted": check(BASE, answer())}
    if case == "SLF-5":
        program = os.path.realpath(sys.executable)
        return {"absolute": os.path.isabs(program), "executable": os.access(program, os.X_OK),
                "basename": os.path.basename(program)}
    if case == "SLF-6":
        reading = {"reportingState": "unreported", "reason": "terminal_without_report",
                   "executionGeneration": 3, "selectors": {"turn": "turn-3"}}
        text = "unreported terminal_without_report generation 3 turn-3"
        return {"reading": reading, "message": text}
    if case == "SLF-7":
        cases = [merged(BASE, runtimeWorkspaceRoots="/a/bc"),
                 merged(BASE, runtimeWorkspaceRoots=7),
                 merged(BASE, runtimeWorkspaceRoots=[WORK, 7]),
                 merged(BASE, environments="local"),
                 merged(BASE, environments=["local"]),
                 merged(BASE, environments=[{"environmentId": 7, "cwd": WORK}]),
                 merged(BASE, environments=[{"environmentId": "local"}]),
                 merged(BASE, environments=[{"environmentId": "local", "cwd": WORK,
                                              "runtimeWorkspaceRoots": "/a/bc"}]),
                 merged(BASE, environments=[{"environmentId": "local", "cwd": WORK,
                                              "runtimeWorkspaceRoots": None}])]
        return [refusal(lambda one=one: settings(one).require_usable()) for one in cases]
    if case == "SLF-8":
        top = merged(BASE, runtimeWorkspaceRoots="/a/bc")
        env = merged(BASE, environments=[{"environmentId": "local", "cwd": WORK,
                                          "runtimeWorkspaceRoots": "/a/bc"}])
        return {"top": check(top, answer(runtimeWorkspaceRoots=["/"])),
                "environment": check(env, answer(thread={"environments": [{
                    "environmentId": "local", "cwd": WORK, "runtimeWorkspaceRoots": ["/"]}]}))}
    if case in ("SLF-9", "SLF-15"):
        values = [answer(runtimeWorkspaceRoots=123), answer(runtimeWorkspaceRoots=WORK),
                  answer(runtimeWorkspaceRoots=[123]),
                  answer(thread={"environments": "local"}),
                  answer(thread={"environments": [7]}),
                  answer(thread={"environments": [{"cwd": WORK}]}),
                  answer(thread={"environments": [{"environmentId": "local", "cwd": WORK,
                                                     "runtimeWorkspaceRoots": 123}]}),
                  answer(thread={"environments": [{"environmentId": "local", "cwd": WORK,
                                                     "runtimeWorkspaceRoots": None}]}),
                  answer(thread="thread-1")]
        return [{"free": check(BASE, one), "transmitted": check(BASE, one, False)}
                for one in values]
    if case == "SLF-10":
        bad = merged(BASE_SANDBOX, writableRoots=[123])
        record = merged(BASE, sandbox=bad)
        return {"record": refusal(lambda: settings(record).require_usable()),
                "both": check(record, answer(sandbox=bad)),
                "answer": check(BASE, answer(sandbox=bad))}
    if case == "SLF-11":
        wrong = [("networkAccess", v) for v in (0, 1, [1], "false", None)] + [
            ("excludeTmpdirEnvVar", 0), ("excludeSlashTmp", 1),
            ("writableRoots", "/tmp"), ("writableRoots", [7])]
        records = []
        for field, value in wrong:
            bad = merged(BASE_SANDBOX, **{field: value})
            records.append(refusal(lambda bad=bad: settings(merged(BASE, sandbox=bad)).require_usable()))
        zero = merged(BASE_SANDBOX, networkAccess=0)
        listed = merged(BASE_SANDBOX, networkAccess=[1])
        comparisons = [check(merged(BASE, sandbox=zero), answer()),
                       check(BASE, answer(sandbox=zero)),
                       check(merged(BASE, sandbox=listed), answer(sandbox=listed))]
        return {"records": records, "comparisons": comparisons}
    if case == "SLF-12":
        env = [{"environmentId": "local", "cwd": WORK}]
        record = merged(BASE, environments=env)
        observed = answer(thread={"environments": env})
        return {"usable": refusal(lambda: settings(record).require_usable()),
                "comparison": check(record, observed)}
    if case == "SLF-13":
        profile = {"id": "profile-1", "extends": None, "rules": []}
        same = {"rules": [], "extends": None, "id": "profile-1"}
        record = merged(BASE, expectedPermissionProfile=profile)
        absent = answer(); absent.pop("activePermissionProfile")
        return {"usable": refusal(lambda: settings(record).require_usable()),
                "same": check(record, answer(activePermissionProfile=same)),
                "bool": check(merged(BASE, expectedPermissionProfile=0),
                              answer(activePermissionProfile=False)),
                "missing": check(record, absent),
                "none": check(BASE, answer())}
    if case == "SLF-14":
        extra = {"environmentId": "local", "cwd": WORK,
                 "runtimeWorkspaceRoots": [WORK], "extraAuthorization": "restricted"}
        plain = {"environmentId": "local", "cwd": WORK,
                 "runtimeWorkspaceRoots": [WORK]}
        return {"missing": check(merged(BASE, environments=[extra]),
                                  answer(thread={"environments": [plain]})),
                "extra": check(merged(BASE, environments=[plain]),
                                answer(thread={"environments": [extra]})),
                "different": check(merged(BASE, environments=[extra]), answer(thread={
                    "environments": [merged(extra, extraAuthorization=0)]})),
                "same": check(merged(BASE, environments=[extra]),
                               answer(thread={"environments": [extra]}))}
    raise SystemExit(case)


print(json.dumps(main(sys.argv[1]), sort_keys=True, separators=(",", ":")))
