"""Behavioural mutation proofs for the independent part of todo 23A.

Never modifies tests. Restores each product file by byte copy and external cmp,
even on failure. Run serially: the worktree is shared with disjoint packages.
"""
import pathlib
import subprocess
import tempfile

root = pathlib.Path(__file__).resolve().parents[4]
package = root / "internal/relay/routing"
evidence = pathlib.Path(os.environ.get('CRW_EVIDENCE_PATH', root / '.omo/evidence/task-23-crw-go-port.txt'))
mutations = [
    (1, "products.go", "len(values) > 16", "len(values) > 17", "error/refused evidence count"),
    (2, "products.go", "!ok || n < 2", "!ok || n < 3", "minIndependentFixes refusal"),
    (3, "products.go", "!ok || len(checks) == 0", "!ok || len(checks) == 1", "followUpOf[].checks refusal"),
    (4, "products.go", '"declared and switched off"', '"declared but switched off"', "user_report.reason"),
    (5, "placement.go", "len(owners) == 1", "len(owners) == 2", "resolved product"),
    (6, "placement.go", 'return "unassigned", nil', 'return "unresolved", nil', "workspace"),
    (7, "placement.go", 'return owned("accumulate", opens[0]', 'return owned("new_issue", opens[0]', "disposition"),
    (8, "placement.go", 'if b["test"] == simulated {', 'if b["test"] != simulated {', "disposition/project"),
    (9, "obligations.go", 'makeOwed("add_label", nil, nil, label)', 'makeOwed("add_relation", nil, nil, label)', "obligations[0].kind"),
    (11, "completion.go", 'return "unverified", "the result could not be observed"', 'return "mismatch", "the result could not be observed"', "checks[].verdict"),
    (12, "completion.go", 'overall = "closure_pending"', 'overall = "consistent"', "verdict"),
    (17, "products.go", 'by != "operator" && !strings.HasPrefix(text(by), "llm:")', 'by != "operator" && !strings.HasPrefix(text(by), "model:")', "classification/refusal"),
    (18, "routes.go", 'Goal: nullText(value["goal"])', 'Goal: nullText(value["origin"])', "goal"),
    (19, "obligations.go", 'return "link_incomplete"', 'return "cause_unverified"', "attention"),
    (20, "projects.go", 'int64(len(members)) < minimum', 'int64(len(members)) <= minimum', "eligibility reasons"),
    (21, "routes.go", 'incidentID(fault, key))', 'incidentID(fault, key+"changed"))', "replayed fault"),
    (22, "routes.go", 'ORDER BY checked_seq, rowid LIMIT ?', 'ORDER BY rowid, checked_seq LIMIT ?', "proposal goal/order"),
]
with tempfile.TemporaryDirectory(prefix="routing-mutations-") as scratch:
    with evidence.open("a") as log:
        log.write("\n### part A mutation proofs\nCommand: python3 internal/relay/routing/testdata/mutations.py\nRevision: e5b10415\n")
        for number, name, before, after, field in mutations:
            path = package / name
            original = path.read_bytes()
            assert original.count(before.encode()) == 1, (name, before)
            backup = pathlib.Path(scratch) / name
            backup.write_bytes(original)
            line = original[:original.index(before.encode())].count(b"\n") + 1
            try:
                path.write_bytes(original.replace(before.encode(), after.encode(), 1))
                command = ["go", "test", "./internal/relay/routing", "-run", f"^Test23_PRD_{number}_", "-count=1"]
                result = subprocess.run(command, cwd=root, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=120)
                output = result.stdout.decode()
                if result.returncode != 1 or "differs" not in output or "panic:" in output or "build failed" in output:
                    raise RuntimeError(f"invalid mutation proof {number}: {result.returncode}\n{output}")
                failure = next(line for line in output.splitlines() if "differs" in line)
                # Preserve full first result diff (before the next failed subtest).
                start = output.index(failure)
                end = output.find("    --- FAIL:", start)
                diff = output[start:end if end != -1 else len(output)]
                log.write(f"PRD-{number}: internal/relay/routing/{name}:{line}; {before!r} -> {after!r}; first differing field: {field}; test exit 1.\n{diff}\n")
                log.flush()
                print(f"PRD-{number}: killed ({field})", flush=True)
            finally:
                path.write_bytes(backup.read_bytes())
                subprocess.run(["cmp", str(path), str(backup)], check=True)
        log.write("All 17 mutations restored by byte copy + cmp.\n")
