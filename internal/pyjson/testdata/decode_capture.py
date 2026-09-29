"""Capture, once, what json.loads does to bytes before and while it scans them.

Not run by any test: its output, decode.json beside it, is the oracle the Go tests read, so
default CI never starts Python for it. Regenerate after a Python upgrade with

    PYTHONDONTWRITEBYTECODE=1 .venv/bin/python internal/pyjson/testdata/decode_capture.py

run from the repository root. Each case is a byte string; "text" is what it decodes to (a lone
surrogate written as U+FFFD, as a Go string holds it) or "decodeError" is str(UnicodeDecodeError);
"hookedError" is what json.loads(raw, object_pairs_hook=<refuse a repeated key>) raises on the
decoded text: the ValueError text, "duplicate" for the hook's own refusal, or null.
"""
import json
from pathlib import Path

OUT = Path(__file__).with_name("decode.json")
DOC = '{"roles":{"parent":{"model":"m","reasoningEffort":"high"}}}'

CASES = {
    "utf8": DOC.encode(),
    "utf8-invalid-start": b'{"a":"m\xff"}',
    "utf8-invalid-continuation": b'{"a":"\xe2\x28\xa1"}',
    "utf8-end-of-data": b'{"a":"\xe2\x82',
    "utf8-overlong": b'{"a":"\xc0\xaf"}',
    "utf8-surrogate-passes": b'{"a":"\xed\xa0\x80"}',
    "utf8-surrogate-truncated": b'\xed\xa0',
    "utf8-surrogate-bad-continuation": b'\xed\xbf\x00',
    "utf8-past-max": b'\xf4\x90\x80\x80',
    "utf8-sig": b"\xef\xbb\xbf" + DOC.encode(),
    "utf8-sig-invalid": b'\xef\xbb\xbf{"a":"\xff"}',
    "utf8-sig-twice": b"\xef\xbb\xbf\xef\xbb\xbf" + DOC.encode(),
    "utf8-sig-only": b"\xef\xbb\xbf",
    "empty": b"",
    "utf16": DOC.encode("utf-16"),
    "utf16-le": DOC.encode("utf-16-le"),
    "utf16-be": DOC.encode("utf-16-be"),
    "utf16-be-mark": b"\xfe\xff" + DOC.encode("utf-16-be"),
    "utf16-mark-only": b"\xff\xfe",
    "utf16-odd": DOC.encode("utf-16") + b"x",
    "utf16-le-odd": DOC.encode("utf-16-le") + b"x",
    "utf16-be-odd": DOC.encode("utf-16-be") + b"x",
    "utf16-high-then-odd": b'{\x00' + b'\x00\xd8' + b'x',
    "utf16-lone-low": b'{\x00"\x00a\x00"\x00:\x00"\x00\x00\xdc"\x00}\x00',
    "utf16-pair": '{"a":"\U0001f600"}'.encode("utf-16-le"),
    "utf16-lone-high-at-end": '{"a":1}'.encode("utf-16-le") + b"\x00\xd8",
    "utf32": DOC.encode("utf-32"),
    "utf32-le": DOC.encode("utf-32-le"),
    "utf32-be": DOC.encode("utf-32-be"),
    "utf32-mark-only": b"\xff\xfe\x00\x00",
    "utf32-truncated-one": b"{\x00\x00\x00x",
    "utf32-truncated-two": '{}'.encode("utf-32") + b"xy",
    "utf32-truncated-three": '{"a":1}'.encode("utf-32-be") + b"xyz",
    "utf32-out-of-range": '{"a":"'.encode("utf-32-le") + b"\x00\x00\x11\x00" + '"}'.encode("utf-32-le"),
    "utf32-surrogate-passes": '{"a":"'.encode("utf-32-le") + b"\x00\xd8\x00\x00" + '"}'.encode("utf-32-le"),
    "two-bytes-nul-first": b"\x00{",
    "two-bytes-nul-second": b"{\x00",
    "three-bytes-nul": b"{}\x00",
    "nan": b'{"a": NaN, "b": Infinity, "c": -Infinity}',
    "big-integer": b'{"a": ' + b"1" * 4301 + b"}",
    "big-negative-integer": b'{"a": -' + b"1" * 4301 + b"}",
    "integer-at-limit": b'{"a": ' + b"1" * 4300 + b"}",
    "duplicate": b'{"a": 1, "a": 2}',
    "duplicate-inner-then-syntax": b'{"x": {"a": 1, "a": 2}, !}',
    "syntax-then-duplicate": b'{"x": !, "a": 1, "a": 2}',
    "duplicate-then-extra": b'{"a": 1, "a": 2} x',
    "big-integer-then-duplicate": b'{"a": 1, "a": ' + b"2" * 4301 + b"}",
    "invalid-json": b"{not json",
    "extra": b"{} x",
}


class Duplicate(Exception):
    pass


def refuse_repeats(pairs):
    seen = {}
    for key, value in pairs:
        if key in seen:
            raise Duplicate(key)
        seen[key] = value
    return seen


def main():
    captured = {}
    for name, raw in CASES.items():
        case = {"hex": raw.hex()}
        try:
            text = raw.decode(json.detect_encoding(raw), "surrogatepass")
        except UnicodeDecodeError as error:
            case["decodeError"] = str(error)
        else:
            case["text"] = text.encode("utf-16", "surrogatepass").decode("utf-16", "replace")
            try:
                json.loads(raw, object_pairs_hook=refuse_repeats)
                case["hookedError"] = None
            except Duplicate:
                case["hookedError"] = "duplicate"
            except ValueError as error:
                case["hookedError"] = str(error)
        captured[name] = case
    OUT.write_text(json.dumps(captured, indent=1, sort_keys=True) + "\n")
    print(OUT, len(captured))


if __name__ == "__main__":
    main()
