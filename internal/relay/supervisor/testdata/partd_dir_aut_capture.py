"""Capture one directive-place or autosend Python scenario under an isolated state tree."""
import importlib
import itertools
import json
import os
import shutil
import sqlite3
import sys
import tempfile
import unittest

ROOT, MODULE, TEST = sys.argv[1:4]
_real_rmtree = shutil.rmtree
shutil.rmtree = lambda path, *a, **k: None if os.path.abspath(path).startswith(os.path.join(os.path.abspath(ROOT), "tree")) else _real_rmtree(path, *a, **k)
captures = []
meta = {}
ticks = []
holds = []
projects = []

def plain(v):
    if v is None or isinstance(v, (str, int, float, bool)): return v
    if isinstance(v, dict): return {str(k): plain(x) for k, x in v.items()}
    if isinstance(v, (set, frozenset)): return sorted((plain(x) for x in v), key=lambda x: json.dumps(x, sort_keys=True))
    if isinstance(v, (list, tuple)): return [plain(x) for x in v]
    if hasattr(v, "__dict__"): return plain(vars(v))
    try: return {k: plain(v[k]) for k in v.keys()}
    except Exception: return repr(v)

def recorder(name, pick):
    original = getattr(unittest.TestCase, name)
    def method(self, *args, **kwargs):
        captures.append(plain(pick(*args)))
        return original(self, *args, **kwargs)
    return method
for name, pick in {
    "assertEqual": lambda a,b,*r:a, "assertNotEqual":lambda a,b,*r:a,
    "assertTrue":lambda a,*r:bool(a), "assertFalse":lambda a,*r:bool(a),
    "assertIsNone":lambda a,*r:a, "assertIsNotNone":lambda a,*r:a is not None,
    "assertIn":lambda a,b,*r:a in b, "assertNotIn":lambda a,b,*r:a in b,
    "assertGreaterEqual":lambda a,b,*r:a >= b,
}.items(): setattr(unittest.TestCase, name, recorder(name,pick))

def snapshot(case, name):
    src=sqlite3.connect(case.store.path); dst=sqlite3.connect(os.path.join(ROOT,name+".sqlite3")); src.backup(dst); dst.close(); src.close()
    daemon=getattr(case,"daemon",None)
    if daemon is not None:
        meta[name]={"projectAfter":plain(getattr(daemon,"_supervisor_after",None)),"sendAfter":plain(getattr(daemon,"_supervisor_send_after",None)),"now":case.clock.now()}

def main():
    from codex_session_relay import supervisorchannel
    from codex_session_relay.clock import SystemClock
    SystemClock.iso = lambda self: "2023-11-14T22:13:20.000000+00:00"
    tokens = itertools.count()
    def token_hex(n: int | None = None) -> str:
        return next(tokens).to_bytes(32 if n is None else n, "big").hex()
    supervisorchannel.secrets.token_hex = token_hex
    base=importlib.import_module("tests.test_supervisor_channel")
    original_completed=base.ChannelTestCase.completed
    def completed(self,*a,**k):
        value=original_completed(self,*a,**k); snapshot(self,"event"); return value
    base.ChannelTestCase.completed=completed
    tree=os.path.join(ROOT,"tree"); os.makedirs(tree,exist_ok=True); tempfile.mkdtemp=lambda *a,**k:tree
    # This task compares the supervisor pass itself, not unrelated daemon passes.
    # Keep the test's public tick call but route it to the exact step one daemon tick calls.
    if MODULE in ("test_supervisor_autosend", "test_directive_places"):
        from codex_session_relay.daemon import RelayDaemon, TickReport
        def supervisor_tick(self, *, now=None):
            if not ticks:
                snapshot(self, "pretick")
            now = self.clock.now() if now is None else now
            report = TickReport()
            self._report_upward(report, now)
            ticks.append({"now": now, "supervisorStaged": report.supervisorStaged,
                          "supervisorSent": report.supervisorSent,
                          "deferred": report.deferred, "skipped": report.skipped,
                          "notes": report.notes,
                          "afterProject": self._supervisor_after or "",
                          "afterSend": list(self._supervisor_send_after or ("", ""))})
            return report
        RelayDaemon.tick = supervisor_tick
    original_holds = supervisorchannel.SupervisorChannel.report_holds
    def report_holds(self, standing):
        result = original_holds(self, standing)
        holds.append(plain(result))
        return result
    supervisorchannel.SupervisorChannel.report_holds = report_holds
    module=importlib.import_module("tests."+MODULE)
    if TEST.startswith("parity_"):
        class ParityCase(module.TwoSupervisors):
            def runTest(self):
                import dataclasses
                event = self.completed()
                self.channel.stage(self.obligation(event))
                if TEST == "parity_expired_lease":
                    message = self.messages()[0]["message_id"]
                    self.channel._claim(message, now=self.clock.now(), owner="relay", recipient="01supervisor-task", resolution=self.channel.resolve(self.rid))
                    self.clock.advance(301)
                    snapshot(self, "pretick")
                    self.tick()
                elif TEST == "parity_deferred":
                    self.adapter.threads["01supervisor-task"].archived = True
                    self.tick()
                elif TEST == "parity_struggling_wrap":
                    self.clock.advance(1)
                    self.complete_other()
                    self.channel.stage_standing("PRJ-2")
                    rows = self.store.all("SELECT message_id, staged_at FROM supervisor_messages ORDER BY staged_at")
                    self.daemon._supervisor_send_after = (rows[0]["staged_at"], rows[0]["message_id"])
                    self.daemon.policy = dataclasses.replace(self.daemon.policy, max_supervisor_projects_per_tick=0)
                    self.adapter.threads[self.SECOND].archived = True
                    original_all = self.store.all
                    changed = False
                    def move_between_pages(sql, params=()):
                        nonlocal changed
                        answer = original_all(sql, params)
                        if "SELECT m.message_id" in sql and not changed:
                            changed = True
                            self.store.db.execute("UPDATE supervisor_messages SET recipient_task_id = ?, state='sending', lease_until=0 WHERE message_id = ?", (self.SECOND, rows[0]["message_id"]))
                        return answer
                    self.store.all = move_between_pages
                    self.tick()
                elif TEST == "parity_project_wrap":
                    self.daemon._supervisor_after = "PRJ-1"
                    original_all = self.store.all
                    def overlapping_page(sql, params=()):
                        rows = original_all(sql, params)
                        if "DISTINCT project_key" in sql and " <= " in sql:
                            return [{"project_key": "PRJ-2"}, *rows]
                        return rows
                    self.store.all = overlapping_page
                    stage = self.channel.stage_unsent
                    def stage_project(project):
                        projects.append(project)
                        return stage(project)
                    self.channel.stage_unsent = stage_project
                    self.tick()
                elif TEST == "parity_head_window":
                    for number in range(20):
                        self.clock.advance(1)
                        self.channel.stage(self.obligation(self.completed(text="backlog %d" % number)))
                    self.complete_other()
                    self.adapter.threads["01supervisor-task"].archived = True
                    self.daemon.policy = dataclasses.replace(self.daemon.policy, max_supervisor_sends_per_tick=1)
                    self.tick()
        suite = ParityCase()
    else:
        suite=unittest.defaultTestLoader.loadTestsFromName("tests."+MODULE+"."+TEST)
        while isinstance(suite,unittest.TestSuite): suite=next(iter(suite))
    case=suite; setup=case.setUp
    def wrapped(): setup(); snapshot(case,"setup")
    case.setUp=wrapped
    result=unittest.TestResult(); case.run(result)
    problems=[tb for _,tb in result.failures+result.errors]
    tables={}; path=os.path.join(tree,"state","relay.sqlite3")
    if os.path.exists(path):
        src=sqlite3.connect(path); dst=sqlite3.connect(os.path.join(ROOT,"final.sqlite3")); src.backup(dst); dst.close(); src.close()
        db=sqlite3.connect(path); db.row_factory=sqlite3.Row
        for (table,) in db.execute("SELECT name FROM sqlite_master WHERE type='table' AND name NOT IN ('schema_meta','sqlite_sequence') ORDER BY name"):
            rows=[dict(r) for r in db.execute(f"SELECT * FROM {table} ORDER BY rowid")]
            if rows: tables[table]=plain(rows)
        db.close()
    with open(os.path.join(ROOT,"capture.json"),"w") as f: json.dump({"captures":captures,"problems":problems,"tables":tables,"meta":meta,"ticks":ticks,"holds":holds,"projects":projects},f)
    if problems: print("\n".join(problems),file=sys.stderr); return 1
    return 0
if __name__=="__main__": raise SystemExit(main())
