"""Record json.loads's JSONDecodeError text for a table of broken documents.

  uv run --no-sync python internal/relay/registry/testdata/gen_jsonerr.py \
      > internal/relay/registry/testdata/python_jsonerr.json
"""
import json
import sys

DOCS = ["", " ", "{not json", "{", "}", "[", "[1,", "[1,]", '{"a":1,}', '{"a" 1}', '{"a":}', '{"a":1 "b":2}',
        '{"a":"x', '"\\q"', '"\\u12g4"', '"a\x01b"', "tru", "nul", "01", "-", "1.", "1e", "1 2", "{}x",
        '{"a":[1,2', '\n\n  {"a": x}', '{"é": ?}', "NaN", "-Infinity", "[1,\n2,\n]", '{"a":1}\n\n?', "[]]"]
out = []
for doc in DOCS:
    try:
        json.loads(doc)
        out.append([doc, ""])
    except json.JSONDecodeError as error:
        out.append([doc, str(error)])
json.dump(out, sys.stdout, indent=1)
sys.stdout.write("\n")
