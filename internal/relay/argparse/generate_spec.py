# Run with the locked workspace Python; generated data has no terminal-width renderings.
import argparse
import json
import pathlib
import sys
import textwrap
from codex_session_relay.cli import build_parser


def chunks(text):
    return textwrap.TextWrapper()._split(' '.join((text or '').split()))


def spec(parser):
    fmt = parser._get_formatter()
    optional = [a for a in parser._actions if a.option_strings]
    positional = [a for a in parser._actions if not a.option_strings]
    parts, split = fmt._get_actions_usage_parts_with_split(optional + positional, parser._mutually_exclusive_groups, len(optional))
    def action(a):
        return dict(flags=a.option_strings, dest=a.dest, kind=type(a).__name__, required=a.required,
                    choices=list(a.choices) if a.choices is not None else None,
                    type=getattr(a.type, '__name__', ''), header=fmt._format_action_invocation(a),
                    help=chunks(fmt._expand_help(a)) if a.help and a.help != argparse.SUPPRESS else [],
                    suppressed=a.help == argparse.SUPPRESS,
                    children=[action(child) for child in a._get_subactions()] if hasattr(a, '_get_subactions') else [])
    return dict(parts=parts, split=split, description=chunks(parser.description), epilog=chunks(parser.epilog),
                actions=[action(a) for a in parser._actions],
                sections=[dict(title=g.title, actions=[parser._actions.index(a) for a in g._group_actions]) for g in parser._action_groups],
                groups=[dict(required=g.required, actions=[parser._actions.index(a) for a in g._group_actions]) for g in parser._mutually_exclusive_groups])

p = build_parser()
result = {'': spec(p)}
for a in p._actions:
    if isinstance(a, argparse._SubParsersAction):
        result.update({n: spec(c) for n, c in a.choices.items()})
output = pathlib.Path(sys.argv[1]) if len(sys.argv) > 1 else pathlib.Path(__file__).with_name('specs.json')
output.write_text(json.dumps(result, ensure_ascii=True, indent=2) + '\n')
