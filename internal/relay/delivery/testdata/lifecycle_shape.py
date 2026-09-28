import json
import sys

from codex_session_relay.lifecycle import observe


class Host:
    def __init__(self, case):
        self.case = case

    def read_thread(self, _task):
        class Facts:
            runtime_status = self.case.get("runtime")
            can_accept_input = self.case.get("accepts")
        return Facts()

    def is_archived(self, _task, cwd=None):
        return False

    def read_goal_status(self, _task):
        return self.case.get("goal")


result = []
for case in json.load(sys.stdin):
    observation = observe(Host(case), "thread", require_evidence=True)
    result.append({"deliverable": observation.deliverable, "reason": observation.withhold_reason})
json.dump(result, sys.stdout, separators=(",", ":"))
