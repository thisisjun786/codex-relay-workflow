"""Sequential development-only mutation proof for the CI representative cases.

Run with --root CHECKOUT --work SCRATCH. Do not run concurrently with builds or
other writers: each mutation touches production source until its finally block.
"""
import argparse
import json
import shutil
import subprocess
from pathlib import Path

MUTANTS = [
    ('dict', 'evidence/forge_value.go', '\tpanic(&PythonError{"AttributeError", "\'" + TypeName(v) + "\' object has no attribute \'get\'"})', '\treturn map[string]any{}', 'forge/pull/head/'),
    ('text', 'evidence/pyvalue.go', '\tcase bool:\n\t\tif x {\n\t\t\treturn "True"', '\tcase bool:\n\t\tif x {\n\t\t\treturn "true"', 'show/origin/True'),
    ('truth', 'evidence/pyvalue.go', '\tcase string:\n\t\treturn x != ""', '\tcase string:\n\t\treturn false', 'forge/pull/merged/'),
    ('int', 'evidence/forge_value.go', 'if v {\n\t\t\treturn big.NewInt(1)', 'if v {\n\t\t\treturn big.NewInt(2)', 'forge/jobs/jobs.0.run_attempt/'),
    ('missing-total', 'evidence/collector.go', 'return Page{collectionItems(node["nodes"]), node["totalCount"], next}, nil', 'return Page{collectionItems(node["nodes"]), Or(node["totalCount"], 0), next}, nil', 'forge/reviewThreads/data.repository.pullRequest.reviewThreads.totalCount/'),
    ('hash', 'evidence/forge_value.go', '\tpanic(&PythonError{"TypeError", "unhashable type: \'" + TypeName(value) + "\'"})', '\treturn Text(value)', 'forge/checks/check_runs.0.id/'),
    ('ascii', 'evidence/collector.go', 'Dumps(digestInput, false, true, true)', 'Dumps(digestInput, false, true, false)', 'digest/'),
    ('len', 'evidence/forge_value.go', '\tpanic(&PythonError{"TypeError", "object of type \'" + TypeName(v) + "\' has no len()"})', '\treturn 0', 'render/handoff/checks/'),
    ('index', 'evidence/forge_value.go', 'panic(&PythonError{"KeyError", Text(index)})', 'return map[string]any{}', 'forge/reviewThreads/data.repository.pullRequest.reviewThreads.nodes.0.comments.nodes/'),
    ('item', 'evidence/forge_value.go', 'if value, present := Lookup(o, key); present {\n\t\t\treturn value', 'if _, present := Lookup(o, key); present {\n\t\t\treturn nil', 'revision/finding/'),
    ('items', 'evidence/forge_value.go', '\tpanic(&PythonError{"TypeError", "\'" + TypeName(v) + "\' object is not iterable"})', '\treturn nil', 'render/evidence/'),
    ('marker', 'delivery/omitted.go', 'if truthy(declaredWorkValue) && !isPath {', 'if false && truthy(declaredWorkValue) && !isPath {', 'marker/intent.json/workspace/'),
]


LIBRARY_MUTANTS = [
    ('strict-iteration', 'supervisor/send.go', 'evidence.Iter(field("evidence"))', 'evidence.Items(field("evidence"))', 'supervisor', 'Test24PacketAccessorPython'),
    ('envelope-detail', 'evidence/envelope.go', 'if Truthy(o["detail"]) {', 'if false {', 'evidence', 'Test24EnvelopeAccessorPython'),
    ('business-status', 'managed/engine.go', 'status = evidence.Text(value)', 'status = evidence.Text(value)[:0]', 'managed', 'Test24ManagedReceiptAccessorPython'),
    ('report-status', 'supervisor/report_subset.go', 'evidence.Repr(status), reportContractVersion', 'evidence.Repr(""), reportContractVersion', 'supervisor', 'Test24ReportAccessorPython'),
]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--root', type=Path, required=True)
    parser.add_argument('--work', type=Path, required=True)
    args = parser.parse_args()
    root, work = args.root.resolve(), args.work.resolve()
    work.mkdir(parents=True, exist_ok=True)
    driver = root / 'internal/relay/cli/testdata/json_access.py'
    python = root / '.venv/bin/python'
    # A mutant is meaningful only after the exact selected tests pass unchanged.
    subprocess.run(['go', 'build', '-o', str(work/'crw'), './cmd/crw'], cwd=root, check=True, timeout=120)
    subprocess.run([str(python), str(driver), '--root', str(root), '--binary', str(work/'crw'), '--representative'], cwd=root, check=True, timeout=120)
    results = []
    for name, file, old, new, case in MUTANTS:
        source = root / 'internal/relay' / file
        before = source.read_bytes()
        text = before.decode()
        if text.count(old) != 1:
            raise AssertionError((name, 'non-unique mutation target', text.count(old)))
        backup = work / (name+'.bak')
        backup.write_bytes(before)
        try:
            source.write_text(text.replace(old, new))
            subprocess.run(['go', 'build', '-o', str(work/'crw'), './cmd/crw'], cwd=root, check=True, timeout=120)
            command = [str(python), str(driver), '--root', str(root), '--binary', str(work/'crw'), '--representative', '--filter', case]
            result = subprocess.run(command, cwd=root, capture_output=True, timeout=120)
            (work/(name+'.log')).write_bytes(result.stdout+result.stderr)
            killed = result.returncode == 1 and b'DIFF' in result.stdout
            results.append({'mutation': name, 'case': case, 'exit': result.returncode, 'diff': killed})
            print(json.dumps(results[-1]), flush=True)
            if not killed:
                raise AssertionError(('surviving mutation or harness failure', name, result.stdout, result.stderr))
        finally:
            shutil.copyfile(backup, source)
            subprocess.run(['cmp', '--', str(backup), str(source)], check=True)
    for name, file, old, new, package, test in LIBRARY_MUTANTS:
        command = ['go', 'test', './internal/relay/'+package, '-run', '^'+test+'$', '-count=1']
        subprocess.run(command, cwd=root, check=True, timeout=120)
        source = root / 'internal/relay' / file
        before = source.read_bytes()
        text = before.decode()
        if text.count(old) != 1:
            raise AssertionError((name, 'non-unique mutation target', text.count(old)))
        backup = work / (name+'.bak')
        backup.write_bytes(before)
        try:
            source.write_text(text.replace(old, new))
            result = subprocess.run(command, cwd=root, capture_output=True, timeout=120)
            (work/(name+'.log')).write_bytes(result.stdout+result.stderr)
            killed = result.returncode == 1 and b'diff' in result.stdout
            results.append({'mutation': name, 'case': test, 'exit': result.returncode, 'diff': killed})
            print(json.dumps(results[-1]), flush=True)
            if not killed:
                raise AssertionError(('surviving mutation or harness failure', name, result.stdout, result.stderr))
        finally:
            shutil.copyfile(backup, source)
            subprocess.run(['cmp', '--', str(backup), str(source)], check=True)
    (work/'results.json').write_text(json.dumps(results, indent=2))


if __name__ == '__main__':
    main()
