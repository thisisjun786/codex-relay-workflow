"""Record the Python MCP server's raw stdout frames for malformed messages, for wire_test.go.

Run from the repository root, with HOME/XDG_*/CODEX_HOME/TMPDIR under a scratch directory:
  uv run --no-sync python internal/bridge/mcp/testdata/gen_wire_python.py \
      > internal/bridge/mcp/testdata/wire_python.json

It starts `python -m codex_thread_bridge.server` (no app server behind it), initializes, and
sends each case's line followed by a ping; the frames Python writes before that ping's answer
are the case's frames, kept as the exact lines it wrote.
"""
import json, os, subprocess, sys, tempfile

CASES = [
    # (i) an unknown notification whose params are not an object
    {"case": "unknown-notification-array-params", "send": '{"jsonrpc":"2.0","method":"no/such","params":[1]}'},
    {"case": "unknown-notification-string-params", "send": '{"jsonrpc":"2.0","method":"no/such","params":"x"}'},
    # (ii) known notifications with array params
    {"case": "initialized-array-params", "send": '{"jsonrpc":"2.0","method":"notifications/initialized","params":[1]}'},
    {"case": "cancelled-array-params", "send": '{"jsonrpc":"2.0","method":"notifications/cancelled","params":[1]}'},
    # (iii) known requests with non-object params
    {"case": "ping-array-params", "send": '{"jsonrpc":"2.0","id":5,"method":"ping","params":[1]}'},
    {"case": "tools-list-array-params", "send": '{"jsonrpc":"2.0","id":6,"method":"tools/list","params":[1]}'},
    {"case": "ping-string-params", "send": '{"jsonrpc":"2.0","id":7,"method":"ping","params":"x"}'},
    # (iv) tools/call whose name is not a string
    {"case": "tools-call-numeric-name", "send": '{"jsonrpc":"2.0","id":10,"method":"tools/call","params":{"name":1}}'},
    {"case": "tools-call-missing-name", "send": '{"jsonrpc":"2.0","id":12,"method":"tools/call","params":{}}'},
    {"case": "tools-call-array-arguments", "send": '{"jsonrpc":"2.0","id":13,"method":"tools/call","params":{"name":"get_goal","arguments":[1]}}'},
    # tools/list, whose descriptions carry '<' and '>'
    {"case": "tools-list", "send": '{"jsonrpc":"2.0","id":11,"method":"tools/list"}'},
]
INITIALIZE = '{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"wire","version":"0"}}}'


def main():
    with tempfile.TemporaryDirectory(dir=os.environ["TMPDIR"]) as tmp:
        server = subprocess.Popen(
            [sys.executable, "-m", "codex_thread_bridge.server", "--socket", tmp + "/absent.sock", "--state-dir", tmp + "/state"],
            stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.DEVNULL)

        def send(line):
            server.stdin.write(line.encode() + b"\n")
            server.stdin.flush()

        send(INITIALIZE)
        server.stdout.readline()
        send('{"jsonrpc":"2.0","method":"notifications/initialized"}')
        for number, case in enumerate(CASES, start=900):
            send(case["send"])
            send('{"jsonrpc":"2.0","id":%d,"method":"ping"}' % number)
            frames = []
            while True:
                line = server.stdout.readline().decode().rstrip("\n")
                if json.loads(line).get("id") == number:
                    break
                frames.append(line)
            case["frames"] = frames
        server.stdin.close()
        server.wait()
    json.dump(CASES, sys.stdout, indent=1, ensure_ascii=False)
    print()


main()
