"""Run one original Python fault unittest against its own fake clock and temporary Store.

argv: <module> <Class> <method>. Prints the asserted values and populated fault rows.
HOME/XDG_*/CODEX_HOME and TMPDIR must point to the caller's disposable tree.
"""
import json
import sys
import unittest

module, class_name, method_name = sys.argv[1:]
values = []


def plain(value):
    if isinstance(value, (str, int, float, bool)) or value is None:
        return value
    if isinstance(value, dict):
        return {str(k): plain(v) for k, v in value.items()}
    if isinstance(value, (list, tuple)):
        return [plain(v) for v in value]
    if isinstance(value, (set, frozenset)):
        return sorted((plain(v) for v in value), key=lambda v: json.dumps(v, sort_keys=True))
    try:
        return {k: plain(value[k]) for k in value.keys()}
    except (TypeError, AttributeError, KeyError):
        return repr(value)


for name, selector in {
    "assertEqual": lambda a, b, *rest: a,
    "assertNotEqual": lambda a, b, *rest: a,
    "assertTrue": lambda a, *rest: bool(a),
    "assertFalse": lambda a, *rest: bool(a),
    "assertIsNone": lambda a, *rest: a,
    "assertIsNotNone": lambda a, *rest: a is not None,
    "assertIn": lambda a, b, *rest: a in b,
    "assertNotIn": lambda a, b, *rest: a in b,
    "assertLess": lambda a, b, *rest: a < b,
}.items():
    original = getattr(unittest.TestCase, name)

    def capture(self, *args, _selector=selector, _original=original, **kwargs):
        values.append(plain(_selector(*args)))
        return _original(self, *args, **kwargs)

    setattr(unittest.TestCase, name, capture)

case = unittest.defaultTestLoader.loadTestsFromName(
    f"tests.{module}.{class_name}.{method_name}")
result = unittest.TestResult()
case.run(result)
problems = [message for _, message in result.errors + result.failures]
print(json.dumps({"assertions": values, "problems": problems}, default=repr))
