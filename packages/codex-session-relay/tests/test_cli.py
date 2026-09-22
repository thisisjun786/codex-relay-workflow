"""The command surface, driven end to end with no host and no socket."""

import argparse
import ast
import json
import os
import shutil
import stat
import subprocess
import sys
import tempfile
import unittest

from codex_session_relay import forge

from .support import (
    CHILD, DISPATCH_TURN, HOST, ISSUE, PARENT, DeliveryTestCase, RelayTestCase,
)
# The forge transcript helpers live beside the collector's own cases. The subprocess half
# lives HERE because this module is already declared as one that spends real wall time,
# and a second module doing it would be a fact about the suite nobody had written down.
from .test_forge_evidence import HEAD, REQUIRED_DEV_GATE, job, pull, threads

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


def option_values(command):
    """A shell round trip, with `--opt=value` tokens taken back apart.

    The generated commands attach values with '=' so that a path beginning with a dash stays
    one token and argparse reads it as a value rather than another option. That also means the
    value is no longer a word of its own after shlex.split, which is what these assertions
    have to look at: the round trip still has to hand the path back whole and unexecuted.
    """
    import shlex

    values = []
    for word in shlex.split(command):
        head, sep, tail = word.partition("=")
        values.append(tail if sep and head.startswith("--") else word)
    return values


# ------------------------------------------------------------- field extraction
#
# test_every_transformation_a_send_applies_to_the_record_is_covered derives what it mutates
# from settings.py source instead of listing it, because a list drops the next member. The
# derivation lives here rather than inside the test so that every shape it follows can be
# proved against source written for the purpose -- see FieldExtractionShapes at the end of
# this module. What it does not follow is written down in those docstrings.

_RECORD = ("record", None)
_EXTERNAL = ("external", None)
_OTHER = ("other", None)


def _definitions(tree):
    """Module functions, each method's OWN class, and the classes by name.

    Kept apart deliberately. One flat map keyed by name lets a bare call and a self.<method> call
    resolve to the same definition, and lets an unrelated class's method answer for this one's.
    """
    functions = {node.name: node for node in tree.body if isinstance(node, ast.FunctionDef)}
    owners, classes = {}, {}
    for node in tree.body:
        if not isinstance(node, ast.ClassDef):
            continue
        owned = {}
        for child in node.body:
            if isinstance(child, ast.FunctionDef):
                owned.setdefault(child.name, child)
        for child in owned.values():
            owners[id(child)] = owned
        classes.setdefault(node.name, owned)
    return functions, owners, classes


def _module_constants(tree):
    """Top-level NAME = "text" assignments, read from the tree rather than the imported module.

    Reading them here is what lets source written for a test declare its own constant.
    """
    found = {}
    for node in tree.body:
        if (isinstance(node, ast.Assign) and len(node.targets) == 1
                and isinstance(node.targets[0], ast.Name)
                and isinstance(node.value, ast.Constant) and isinstance(node.value.value, str)):
            found.setdefault(node.targets[0].id, node.value.value)
    return found


def _evaluated_parts(node):
    """The parts of a nested definition that run in the ENCLOSING scope.

    Decorators, defaults and annotations are evaluated where the definition is written, even
    though its body is a separate scope, so skipping the whole node would lose a field only they
    reach.
    """
    yield from getattr(node, "decorator_list", [])
    arguments = getattr(node, "args", None)
    if isinstance(arguments, ast.arguments):
        yield from arguments.defaults
        yield from [default for default in arguments.kw_defaults if default is not None]
        for group in (arguments.posonlyargs, arguments.args, arguments.kwonlyargs):
            yield from [a.annotation for a in group if a.annotation is not None]
        for extra in (arguments.vararg, arguments.kwarg):
            if extra is not None and extra.annotation is not None:
                yield extra.annotation
    returns = getattr(node, "returns", None)
    if returns is not None:
        yield returns
    if isinstance(node, ast.ClassDef):
        yield from node.bases
        yield from [keyword.value for keyword in node.keywords]


def _scoped(function):
    """Every node in this function's OWN scope, breadth first in source order.

    A nested definition is another scope: its body is skipped, its evaluated parts are not.
    """
    queue = list(ast.iter_child_nodes(function))
    index = 0
    while index < len(queue):
        node = queue[index]
        index += 1
        if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef, ast.Lambda, ast.ClassDef)):
            queue.extend(_evaluated_parts(node))
            continue
        yield node
        queue.extend(ast.iter_child_nodes(node))


def _bound_names(node):
    """Every name this node binds, however it binds it.

    A name bound anywhere other than one plain assignment is not an alias of anything this walk
    can follow, so the reader below types it as OTHER rather than guessing.
    """
    def names(target):
        if isinstance(target, ast.Name):
            yield target.id
        elif isinstance(target, (ast.Tuple, ast.List)):
            for element in target.elts:
                yield from names(element)
        elif isinstance(target, ast.Starred):
            yield from names(target.value)

    if isinstance(node, ast.Assign):
        for target in node.targets:
            yield from names(target)
    elif isinstance(node, (ast.AugAssign, ast.AnnAssign, ast.NamedExpr)):
        yield from names(node.target)
    elif isinstance(node, (ast.For, ast.AsyncFor, ast.comprehension)):
        yield from names(node.target)
    elif isinstance(node, ast.withitem):
        if node.optional_vars is not None:
            yield from names(node.optional_vars)
    elif isinstance(node, ast.ExceptHandler):
        if node.name:
            yield node.name


class _Reader:
    """One function body, with every name it binds typed once.

    Five types. RECORD is self.data; FIELD(k) a recorded field; EXTERNAL a non-self parameter of
    an entry method, which is the host's response; EXTFIELD(k) a field of that; OTHER everything
    else. Anything layered on top of FIELD or EXTFIELD is OTHER, and that single rule is what
    keeps sub-keys such as type, cwd and environmentId out of the derived set.
    """

    def __init__(self, function, parameters):
        self.types = dict(parameters)
        self.bindings = {}
        self.resolving = set()
        counts = {}
        for node in _scoped(function):
            for name in _bound_names(node):
                counts[name] = counts.get(name, 0) + 1
            if (isinstance(node, ast.Assign) and len(node.targets) == 1
                    and isinstance(node.targets[0], ast.Name)):
                self.bindings.setdefault(node.targets[0].id, node.value)
        for name, count in counts.items():
            if count > 1 or name not in self.bindings:
                self.types[name] = _OTHER

    def type_of(self, node):
        if (isinstance(node, ast.Attribute) and node.attr == "data"
                and isinstance(node.value, ast.Name) and node.value.id == "self"):
            return _RECORD
        if isinstance(node, ast.Name):
            if node.id in self.types:
                return self.types[node.id]
            if node.id in self.bindings and node.id not in self.resolving:
                self.resolving.add(node.id)
                self.types[node.id] = self.type_of(self.bindings[node.id])
                self.resolving.discard(node.id)
                return self.types[node.id]
            return _OTHER
        if isinstance(node, ast.Subscript):
            base = self.type_of(node.value)
            if (base in (_RECORD, _EXTERNAL) and isinstance(node.slice, ast.Constant)
                    and isinstance(node.slice.value, str)):
                return ("field" if base == _RECORD else "extfield", node.slice.value)
            return _OTHER
        if (isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute)
                and node.func.attr == "get" and node.args
                and isinstance(node.args[0], ast.Constant)
                and isinstance(node.args[0].value, str)):
            base = self.type_of(node.func.value)
            if base in (_RECORD, _EXTERNAL):
                return ("field" if base == _RECORD else "extfield", node.args[0].value)
            return _OTHER
        if isinstance(node, ast.BoolOp) and isinstance(node.op, ast.Or) and node.values:
            carried = self.type_of(node.values[0])
            return carried if carried[0] in ("field", "extfield") else _OTHER
        return _OTHER


def _handed_over(call):
    """(selector, expression) for everything a call hands over.

    The selector is the parameter position or name, or None when the call cannot say: a starred
    argument, and every positional argument after one, because how many parameters the star
    consumed is a runtime fact.
    """
    carried = []
    starred = False
    for position, argument in enumerate(call.args):
        if isinstance(argument, ast.Starred):
            starred = True
            carried.append((None, argument.value))
        else:
            carried.append((None if starred else position, argument))
    for keyword in call.keywords:
        carried.append((keyword.arg, keyword.value))
    return carried


def _hop(call, defined, current):
    functions, owners, _classes = defined
    if isinstance(call.func, ast.Name):
        return functions.get(call.func.id), False
    if (isinstance(call.func, ast.Attribute) and isinstance(call.func.value, ast.Name)
            and call.func.value.id == "self"):
        return owners.get(id(current), {}).get(call.func.attr), True
    return None, False


def _bind(function, carried, reader, method):
    """What the callee's parameters hold, or None when Python would refuse the call.

    Typed deliberately: a parameter receiving a FIELD is a field alias, not the record. Without
    that, normalise_policy(self.data["sandbox"]) would make policy a record and policy.get("type")
    would invent a field named type.
    """
    plain = [argument.arg for argument in function.args.args]
    positional = [argument.arg for argument in function.args.posonlyargs] + plain
    nameable = list(plain)
    if method:
        positional = positional[1:]
        # self is the first parameter overall, and may itself be positional-only.
        if not function.args.posonlyargs:
            nameable = nameable[1:]
    by_name = nameable + [argument.arg for argument in function.args.kwonlyargs]
    every = list(dict.fromkeys(positional + by_name))
    bound = {}
    for selector, expression in carried:
        if isinstance(selector, int):
            if selector < len(positional):
                bound[positional[selector]] = reader.type_of(expression)
        elif selector in by_name:
            if selector in bound:
                # "multiple values for argument": that body never runs, so this is not a hop.
                return None
            bound[selector] = reader.type_of(expression)
    return {name: bound.get(name, _OTHER) for name in every}


def _entry_parameters(function, method):
    arguments = function.args
    names = [argument.arg for argument
             in arguments.posonlyargs + arguments.args + arguments.kwonlyargs]
    if method and names:
        names = names[1:]
    return {name: _EXTERNAL for name in names}


def _entry(name, defined, owner):
    """The entry definition, resolved inside its OWNING class rather than by bare name."""
    functions, _owners, classes = defined
    owned = classes.get(owner, {})
    if name in owned:
        return owned[name], True
    return functions.get(name), False


def _visit(defined, function, parameters, seen, collect):
    """Walk one body and everything it calls, carrying each callee's parameter types.

    Memoised on the definition AND its binding signature: the same helper called first with the
    response and then with the record has to be walked twice.
    """
    key = (id(function), tuple(sorted(parameters.items())))
    if key in seen:
        return set()
    seen.add(key)
    reader = _Reader(function, parameters)
    found = collect(function, reader)
    for node in _scoped(function):
        if not isinstance(node, ast.Call):
            continue
        target, method = _hop(node, defined, function)
        if target is None:
            continue
        inner = _bind(target, _handed_over(node), reader, method)
        if inner is not None:
            found |= _visit(defined, target, inner, seen, collect)
    return found


def transformed_fields(tree, entries, owner="TaskSettings"):
    """Recorded fields these methods hand to a call, directly or through what they call.

    A field is handed over as an argument -- positional, keyword, starred or double-splatted --
    or as the receiver of a method call, since FIELD.method() raises on a value no transformation
    can consume. Container literals are never descended into: a field buried in a dict passed to
    a call is not itself handed to one.
    """
    defined = _definitions(tree)

    def collect(function, reader):
        found = set()
        for node in _scoped(function):
            if not isinstance(node, ast.Call):
                continue
            for _selector, expression in _handed_over(node):
                carried = reader.type_of(expression)
                if carried[0] == "field":
                    found.add(carried[1])
            if isinstance(node.func, ast.Attribute):
                carried = reader.type_of(node.func.value)
                if carried[0] == "field":
                    found.add(carried[1])
        return found

    seen, fields = set(), set()
    for name in entries:
        function, method = _entry(name, defined, owner)
        if function is not None:
            fields |= _visit(defined, function, _entry_parameters(function, method), seen, collect)
    return fields


def value_constraints(tree, entries, owner="TaskSettings"):
    """Response fields these methods compare against a fixed value.

    A constraint cannot be found the way a transformation is, because nothing raises on it and
    so there is no call to look inside; what marks it is a comparison against a literal or a
    module constant, in either order. One operator only: a chained comparison is not this shape.

    This half reads the RESPONSE. `recorded_constraints` below reads the RECORD, and the two
    are asserted equal rather than floored separately, because they are one rule seen from its
    two ends.
    """
    defined = _definitions(tree)
    constants = _module_constants(tree)

    def literal(node):
        if isinstance(node, ast.Constant):
            return node.value
        if isinstance(node, ast.Name):
            return constants.get(node.id)
        return None

    def collect(function, reader):
        found = set()
        for node in _scoped(function):
            if not (isinstance(node, ast.Compare) and len(node.ops) == 1
                    and isinstance(node.ops[0], (ast.Eq, ast.NotEq, ast.Is, ast.IsNot))):
                continue
            for one, other in ((node.left, node.comparators[0]),
                               (node.comparators[0], node.left)):
                carried = reader.type_of(one)
                if carried[0] != "extfield":
                    continue
                value = literal(other)
                if isinstance(value, str):
                    found.add((carried[1], value))
        return found

    seen, found = set(), set()
    for name in entries:
        function, method = _entry(name, defined, owner)
        if function is not None:
            found |= _visit(defined, function, _entry_parameters(function, method), seen, collect)
    return found


def recorded_constraints(tree, entries, owner="TaskSettings"):
    """Recorded fields these methods compare against a fixed value. The mirror of the above.

    One line differs -- `field` where that one reads `extfield` -- and it is the line that
    decides which end of the rule is being read. That is the whole reason this exists as its own
    extractor rather than a parameter: the recorded half was STRUCTURALLY INVISIBLE while it did
    not exist, and adding it is what makes the derivation able to see it at all. Run against the
    source as it stood before the approval policy moved, this returns nothing for that field
    while `value_constraints` returns it, which is exactly the asymmetry the caller now refuses.

    Walked through the same helper hops, so a constraint applied inside a function reached from
    one of the entries counts here as it does there.
    """
    defined = _definitions(tree)
    constants = _module_constants(tree)

    def literal(node):
        if isinstance(node, ast.Constant):
            return node.value
        if isinstance(node, ast.Name):
            return constants.get(node.id)
        return None

    def collect(function, reader):
        found = set()
        for node in _scoped(function):
            if not (isinstance(node, ast.Compare) and len(node.ops) == 1
                    and isinstance(node.ops[0], (ast.Eq, ast.NotEq, ast.Is, ast.IsNot))):
                continue
            for one, other in ((node.left, node.comparators[0]),
                               (node.comparators[0], node.left)):
                carried = reader.type_of(one)
                if carried[0] != "field":
                    continue
                value = literal(other)
                if isinstance(value, str):
                    found.add((carried[1], value))
        return found

    seen, found = set(), set()
    for name in entries:
        function, method = _entry(name, defined, owner)
        if function is not None:
            found |= _visit(defined, function, _entry_parameters(function, method), seen, collect)
    return found


def mutation_probes(fields, constraints):
    """One probe per derived member, keyed by KIND as well as by field.

    Keyed by field alone, a constraint on a field that is also transformed would overwrite that
    transformation's mutant, inherit its branch, be refused for the transformation's own reason,
    and pass -- with the probe count unmoved. Each kind carries the mutant its kind needs: a
    transformation cannot consume 7, and a value constraint needs a well-typed value that is
    simply not the authorized one.
    """
    probes = [("transformation", field, 7) for field in fields]
    probes += [("constraint", field, f"not-{literal}") for field, literal in constraints]
    return sorted(probes)


class CliBase(RelayTestCase):
    def run_cli(self, *args, expect=0):
        environment = dict(os.environ, PYTHONPATH=os.path.join(REPO, "src"))
        completed = subprocess.run(
            [sys.executable, "-m", "codex_session_relay.cli", "--state", self.tmp, *args],
            capture_output=True, text=True, env=environment, timeout=60,
        )
        self.assertEqual(
            completed.returncode, expect,
            f"exit {completed.returncode}: {completed.stdout}{completed.stderr}",
        )
        return json.loads(completed.stdout)

    def register(self, **_kwargs):
        return self.run_cli(
            "register", "--parent-task", PARENT, "--parent-host", HOST,
            "--child-task", CHILD, "--child-host", HOST, "--issue", ISSUE,
            "--artifact-root", self.root, "--allowed-recipient", PARENT,
            "--dispatch-request-id", "dispatch-1", "--dispatch-turn-id", DISPATCH_TURN,
        )


class TheIssueHalfOfTheProof(CliBase):
    """OPS-3.4 makes the proof a conjunction, and nothing used to order the two halves.

    Running the issue lookup first against a mistyped state directory CONSTRUCTS an empty
    store and then honestly reports that nothing is assigned, after which a coordinator opens
    a second writer for an issue that already has one. doctor --issue answers both halves from
    one read-only connection, so the answer can always say which file it came from.
    """

    def cli_store(self):
        return os.path.join(self.tmp, "relay.sqlite3")

    def test_it_answers_without_creating_the_database_it_was_asked_about(self):
        """The whole point. An absent store must stay absent after a diagnosis."""
        self.assertFalse(os.path.exists(self.cli_store()))
        issue = self.run_cli("doctor", "--issue", ISSUE)["issue"]
        self.assertFalse(issue["readable"])
        self.assertIsNone(issue["holds"])
        self.assertFalse(
            os.path.exists(self.cli_store()),
            "doctor --issue created the store it was only asked to look at",
        )

    def test_an_unreadable_store_reports_null_rather_than_no_assignment(self):
        """null and false are different answers and only one of them is safe to act on."""
        issue = self.run_cli("doctor", "--issue", ISSUE)["issue"]
        self.assertIsNone(issue["responsibleRelationship"])
        self.assertIsNone(issue["holds"])
        self.assertIn("not readable", issue["detail"])

    def test_it_names_the_responsible_child_once_one_is_registered(self):
        relationship = self.register()
        issue = self.run_cli("doctor", "--issue", ISSUE)["issue"]
        self.assertTrue(issue["holds"])
        self.assertEqual(issue["responsibleChild"], CHILD)
        self.assertEqual(
            issue["responsibleRelationship"], relationship["relationshipId"]
        )

    def test_a_real_store_with_no_assignment_for_this_issue_says_false_not_null(self):
        self.register()
        issue = self.run_cli("doctor", "--issue", "SOME-OTHER-ISSUE")["issue"]
        self.assertTrue(issue["readable"])
        self.assertFalse(issue["holds"])
        self.assertIsNone(issue["responsibleChild"])

    def test_the_rows_and_the_identity_come_from_the_same_read(self):
        self.register()
        report = self.run_cli("doctor", "--issue", ISSUE)
        self.assertEqual(report["issue"]["storeAgreement"], "same")
        self.assertEqual(report["issue"]["storeId"], report["store"]["storeId"])

    def test_a_paused_assignment_still_holds_its_issue(self):
        """A pause does not release the issue, so it must not read as unowned."""
        relationship = self.register()
        self.run_cli(
            "relationship-status", "--relationship", relationship["relationshipId"],
            "--status", "paused", "--actor", PARENT,
        )
        self.assertTrue(self.run_cli("doctor", "--issue", ISSUE)["issue"]["holds"])

    def test_doctor_without_the_flag_is_unchanged(self):
        self.register()
        self.assertNotIn("issue", self.run_cli("doctor"))

    def test_a_superseded_relationship_is_not_a_live_owner(self):
        """Live ownership is a live status AND no successor, everywhere else in the package.

        A repaired or interrupted store can hold an active row that already has a successor.
        Reporting it through holds would hand a caller an obsolete relationship and child.
        """
        import sqlite3

        relationship = self.register()
        connection = sqlite3.connect(self.cli_store())
        try:
            connection.execute(
                "UPDATE relationships SET superseded_by = 'rel-0000000000000001'"
                " WHERE relationship_id = ?",
                (relationship["relationshipId"],),
            )
            connection.commit()
        finally:
            connection.close()
        issue = self.run_cli("doctor", "--issue", ISSUE)["issue"]
        self.assertFalse(issue["holds"])
        self.assertIsNone(issue["responsibleRelationship"])


class CommandLine(CliBase):
    def test_register_emit_and_status_round_trip(self):
        relationship = self.register()
        self.assertTrue(relationship["relationshipId"].startswith("rel-"))
        self.assertEqual(relationship["authorizedScope"]["artifactRoots"], [self.root])

        path = self.artifact("out.txt", "the deliverable")
        emitted = self.run_cli(
            "emit", "--relationship", relationship["relationshipId"], "--generation", "1",
            "--outcome", "ready_for_review", "--turn-thread", CHILD,
            "--turn-id", DISPATCH_TURN, "--turn-status", "completed", "--artifact", path,
        )
        # With no host to confirm the turn ended, the claim is staged rather than queued.
        self.assertEqual(emitted["stage"], "staged")
        self.assertEqual(emitted["receipt"]["outcome"], "ready_for_review")
        self.assertEqual(emitted["terminalProof"], "unverified_staged")
        self.assertEqual(self.run_cli("status")["deliveries"], [])

    def test_a_refusal_exits_two_with_a_machine_readable_reason(self):
        relationship = self.register()
        outside = os.path.join(self.tmp, "outside.txt")
        with open(outside, "w", encoding="utf-8") as handle:
            handle.write("not yours")
        refused = self.run_cli(
            "emit", "--relationship", relationship["relationshipId"], "--generation", "1",
            "--outcome", "ready_for_review", "--turn-thread", CHILD,
            "--turn-id", DISPATCH_TURN, "--turn-status", "completed", "--artifact", outside,
            expect=2,
        )
        self.assertEqual(refused["error"], "refused")
        self.assertEqual(refused["reason"], "scope_escape")

    def test_emitting_from_a_live_turn_stages_without_queueing(self):
        relationship = self.register()
        path = self.artifact("out.txt", "still working")
        emitted = self.run_cli(
            "emit", "--relationship", relationship["relationshipId"], "--generation", "1",
            "--outcome", "ready_for_review", "--turn-thread", CHILD,
            "--turn-id", DISPATCH_TURN, "--turn-status", "inProgress", "--artifact", path,
        )
        self.assertEqual(emitted["stage"], "staged")
        self.assertNotIn("delivery", emitted)

    def test_a_later_turn_needs_a_continuation_and_the_emit_carries_it(self):
        relationship = self.register()
        path = self.artifact("out.txt", "finished later")
        refused = self.run_cli(
            "emit", "--relationship", relationship["relationshipId"], "--generation", "1",
            "--outcome", "ready_for_review", "--turn-thread", CHILD,
            "--turn-id", "turn-loop-5", "--turn-status", "completed", "--artifact", path,
            expect=2,
        )
        self.assertEqual(refused["reason"], "unassigned_turn")

        accepted = self.run_cli(
            "emit", "--relationship", relationship["relationshipId"], "--generation", "1",
            "--outcome", "ready_for_review", "--turn-thread", CHILD,
            "--turn-id", "turn-loop-5", "--turn-status", "completed", "--artifact", path,
            "--continues-anchor", DISPATCH_TURN, "--continuation-actor", "child-loop",
            "--continuation-reason", "cycle 5 of this execution",
        )
        self.assertEqual(accepted["receipt"]["outcome"], "ready_for_review")
        self.assertEqual(accepted["stage"], "staged")

    def test_ack_proof_is_computed_by_the_caller_not_the_relay(self):
        import hashlib

        event = "a" * 32
        proof = self.run_cli("ack-proof", "--event", event, "--turn", "parent-turn-9")
        self.assertEqual(
            proof["ackProof"],
            hashlib.sha256(f"{event}|parent-turn-9".encode()).hexdigest(),
        )

    def test_the_ack_command_requires_a_supplied_proof(self):
        environment = dict(os.environ, PYTHONPATH=os.path.join(REPO, "src"))
        completed = subprocess.run(
            [sys.executable, "-m", "codex_session_relay.cli", "--state", self.tmp,
             "ack", "--event", "a" * 32, "--ack-turn", "t"],
            capture_output=True, text=True, env=environment, timeout=60,
        )
        self.assertNotEqual(completed.returncode, 0)
        self.assertIn("--ack-proof", completed.stderr)

    def test_a_command_needing_the_host_says_so_rather_than_guessing(self):
        result = self.run_cli("deliver", expect=4)
        self.assertEqual(result["error"], "usage")
        self.assertIn("--socket", result["detail"])

    def test_doctor_reports_the_environment(self):
        report = self.run_cli("doctor")
        self.assertTrue(report["procAvailable"])
        self.assertEqual(report["adapter"], "none (read-only, no --socket)")

    def test_pause_refuses_and_resume_requires_the_restated_scope(self):
        relationship = self.register()
        rid = relationship["relationshipId"]
        self.run_cli("relationship-status", "--relationship", rid, "--status", "paused",
                     "--actor", "user")
        refused = self.run_cli(
            "relationship-resume", "--relationship", rid, "--expect-generation", "9",
            "--expect-artifact-root", self.root, "--expect-allowed-recipient", PARENT,
            "--actor", "user", expect=2,
        )
        self.assertEqual(refused["reason"], "relationship_not_active")
        resumed = self.run_cli(
            "relationship-resume", "--relationship", rid, "--expect-generation", "1",
            "--expect-artifact-root", self.root, "--expect-allowed-recipient", PARENT,
            "--actor", "user",
        )
        self.assertEqual(resumed["status"], "active")


if __name__ == "__main__":
    unittest.main()


class TerminalProof(CliBase):
    """A caller cannot manufacture the terminal proof that makes a receipt deliverable."""

    def test_an_offline_readiness_claim_is_staged_not_finalized(self):
        relationship = self.register()
        path = self.artifact("out.txt", "claimed complete with nobody to check")
        emitted = self.run_cli(
            "emit", "--relationship", relationship["relationshipId"], "--generation", "1",
            "--outcome", "ready_for_review", "--turn-thread", CHILD,
            "--turn-id", DISPATCH_TURN, "--turn-status", "completed", "--artifact", path,
        )
        self.assertEqual(emitted["terminalProof"], "unverified_staged")
        self.assertEqual(emitted["observedTurnStatus"], "inProgress")
        self.assertEqual(emitted["stage"], "staged")
        self.assertNotIn("delivery", emitted, "nothing is queued on an unverified claim")

    def test_a_staged_claim_is_visible_and_not_deliverable(self):
        relationship = self.register()
        path = self.artifact("out.txt", "work")
        self.run_cli(
            "emit", "--relationship", relationship["relationshipId"], "--generation", "1",
            "--outcome", "ready_for_review", "--turn-thread", CHILD,
            "--turn-id", DISPATCH_TURN, "--turn-status", "completed", "--artifact", path,
        )
        self.assertEqual(self.run_cli("status")["deliveries"], [])


class ScopedStatus(CliBase):
    """A filtered status must filter every block it returns, health included.

    Reporting one assignment's deliveries beside every assignment's observation backlog
    reads as that assignment being behind, which is the opposite of what a filter is for.
    """

    def staged(self, name):
        """A second assignment with its own parent, child and staged receipt."""
        parent, child = f"01parent-{name}", f"01child-{name}"
        root = os.path.join(self.root, name)
        os.makedirs(root, exist_ok=True)
        relationship = self.run_cli(
            "register", "--parent-task", parent, "--parent-host", HOST,
            "--child-task", child, "--child-host", HOST, "--issue", f"REL-{name}",
            "--artifact-root", root, "--allowed-recipient", parent,
            "--dispatch-request-id", f"dispatch-{name}", "--dispatch-turn-id", f"turn-{name}",
        )
        path = os.path.join(root, "out.txt")
        with open(path, "w", encoding="utf-8") as handle:
            handle.write(f"{name} still going")
        emitted = self.run_cli(
            "emit", "--relationship", relationship["relationshipId"], "--generation", "1",
            "--outcome", "ready_for_review", "--turn-thread", child,
            "--turn-id", f"turn-{name}", "--turn-status", "completed", "--artifact", path,
        )
        self.assertEqual(emitted["stage"], "staged")
        return relationship["relationshipId"], emitted["receipt"]["eventId"]

    def test_a_scoped_status_reports_only_the_requested_assignment(self):
        mine, my_event = self.staged("a")
        theirs, their_event = self.staged("b")

        health = self.run_cli("status", "--relationship", mine)["observation"]

        self.assertEqual([s["eventId"] for s in health["stagedEvents"]], [my_event])
        self.assertEqual(list(health["anchors"]), [mine])
        self.assertEqual(list(health["backlog"]), [mine])
        self.assertNotIn(theirs, health["backlog"])
        self.assertNotIn(their_event, [s["eventId"] for s in health["stagedEvents"]])

    def test_an_unscoped_status_still_reports_every_assignment(self):
        mine, my_event = self.staged("a")
        theirs, their_event = self.staged("b")

        health = self.run_cli("status")["observation"]

        self.assertEqual({s["eventId"] for s in health["stagedEvents"]},
                         {my_event, their_event})
        self.assertEqual(set(health["anchors"]), {mine, theirs})
        self.assertEqual(set(health["backlog"]), {mine, theirs})


class ServiceExitCodes(CliBase):
    """A refusal that exits zero is read by automation as a success."""

    def test_a_refused_enable_does_not_exit_zero(self):
        self.run_cli("service", "enable")
        state = os.path.join(self.tmp, "daemon.json")
        with open(state, "w", encoding="utf-8") as handle:
            json.dump({"pid": os.getpid(), "installationId": "someone-else",
                       "storeId": "another-store", "bootId": None,
                       "startTicks": None, "workerPid": None}, handle)
        # No lock is held here, so this must still succeed: a stopped foreign registration
        # is not a reason to make a state directory unconfigurable.
        self.assertTrue(self.run_cli("service", "enable")["ok"])

    def test_a_refused_enable_is_a_refusal_the_shell_can_see(self):
        """Returning the payload directly exits zero, and automation reads that as done."""
        from unittest import mock

        from codex_session_relay import cli

        class Refusing:
            def enable(self, *, actor):
                return {"ok": False, "reason": "not_ours", "intent": {"enabled": False}}

        args = argparse.Namespace(service_command="enable", actor=None)
        with mock.patch.object(cli, "_service_for", lambda _services: Refusing()):
            with self.assertRaises(cli.PayloadExit) as caught:
                cli.cmd_service(object(), args)
        self.assertEqual(caught.exception.code, cli.EXIT_REFUSED)
        self.assertEqual(caught.exception.payload["reason"], "not_ours")

    def test_a_refused_disable_does_not_exit_zero(self):
        self.run_cli("service", "enable")
        state = os.path.join(self.tmp, "daemon.json")
        with open(state, "w", encoding="utf-8") as handle:
            json.dump({"pid": os.getpid(), "installationId": "someone-else",
                       "storeId": "another-store", "bootId": "irrelevant",
                       "startTicks": 1, "workerPid": None}, handle)
        refused = self.run_cli("service", "disable", expect=2)
        self.assertFalse(refused["ok"])
        self.assertEqual(refused["reason"], "not_ours")


class SettingsCommands(CliBase):
    """The registration interface JUN-92 populates from Run's creation result."""

    def _settings(self, cwd="/parent"):
        return {
            "sandbox": {"type": "workspaceWrite", "writableRoots": [], "networkAccess": False,
                        "excludeTmpdirEnvVar": False, "excludeSlashTmp": False},
            "approvalPolicy": "never",
            "cwd": cwd,
            "runtimeWorkspaceRoots": [cwd],
            "model": "anthropic/claude-opus-5",
            "reasoningEffort": "xhigh",
            "environments": [{"environmentId": "local", "cwd": cwd,
                              "runtimeWorkspaceRoots": [cwd]}],
        }

    def test_register_records_settings_for_both_endpoints(self):
        payload = self.run_cli(
            "register", "--parent-task", PARENT, "--parent-host", HOST,
            "--child-task", CHILD, "--child-host", HOST, "--issue", ISSUE,
            "--artifact-root", self.root, "--allowed-recipient", PARENT,
            "--dispatch-request-id", "dispatch-1", "--dispatch-turn-id", DISPATCH_TURN,
            "--parent-settings", json.dumps(self._settings()),
            "--child-settings", json.dumps(self._settings(self.root)),
        )
        self.assertEqual(payload["authorizedSettings"], {PARENT: "recorded", CHILD: "recorded"})
        shown = self.run_cli("settings-show", "--task", PARENT)
        self.assertTrue(shown["usable"])
        self.assertEqual(shown["missing"], [])
        self.assertEqual(shown["settings"]["reasoningEffort"], "xhigh")

    def test_registering_without_settings_leaves_them_unrecorded(self):
        payload = self.register()
        self.assertIsNone(payload["authorizedSettings"])
        shown = self.run_cli("settings-show", "--task", PARENT)
        self.assertIsNone(shown["settings"])
        self.assertFalse(shown["usable"])
        self.assertIn("environments", shown["missing"])

    def test_settings_record_accepts_a_file_path(self):
        path = os.path.join(self.tmp, "settings.json")
        with open(path, "w", encoding="utf-8") as handle:
            json.dump(self._settings(), handle)
        recorded = self.run_cli("settings-record", "--task", PARENT, "--settings", f"@{path}")
        self.assertEqual(recorded["taskId"], PARENT)
        self.assertEqual(recorded["source"], "creation_result")
        self.assertTrue(self.run_cli("settings-show", "--task", PARENT)["usable"])

    def test_an_incomplete_record_is_refused_with_a_machine_readable_reason(self):
        broken = self._settings()
        del broken["environments"]
        refused = self.run_cli(
            "settings-record", "--task", PARENT, "--settings", json.dumps(broken), expect=2,
        )
        self.assertEqual(refused["reason"], "settings_incomplete")
        self.assertIn("environments", refused["detail"])

    def test_a_mistyped_record_is_refused_at_registration(self):
        """The same predicate a send runs, run where the record is written."""
        refused = self.run_cli(
            "settings-record", "--task", PARENT,
            "--settings", json.dumps(dict(self._settings(), model=7)), expect=2,
        )
        self.assertEqual(refused["reason"], "settings_mistyped")
        self.assertIn("model is int, not str", refused["detail"])

    def test_an_unsupported_approval_policy_is_refused_at_registration(self):
        """A row every send refuses is a row that should never have been written.

        Left to delivery, the refusal is rediscovered once per pass by whoever is waiting for
        the message rather than once by whoever recorded it - and this particular row used to
        not be refused at all against a host that normalised the value, which made delivery
        depend on the host for something the record already settled.
        """
        refused = self.run_cli(
            "settings-record", "--task", PARENT,
            "--settings", json.dumps(dict(self._settings(), approvalPolicy="on-request")),
            expect=2,
        )
        self.assertEqual(refused["reason"], "unsupported_approval_policy")
        self.assertIn("on-request", refused["detail"])
        self.assertIn("never", refused["detail"])
        shown = self.run_cli("settings-show", "--task", PARENT)
        self.assertFalse(shown["usable"], "the refused row reached the store")

    def test_a_complete_but_mistyped_record_is_not_reported_deliverable(self):
        """This command has to answer what delivery and doctor answer, not half of it.

        "usable" is about the record HAVING its fields, and this one has all of them. Until the
        recorded string fields were typed, `not missing()` and `require_usable()` agreed on
        every row this could be asked about, so "deliverable" could be computed from the first
        one. They no longer agree, and a row called deliverable here is one delivery withholds
        and doctor reports refused - which is why the field now runs the predicate itself and
        says which rule refused.

        Written past the recorder deliberately: registration refuses this input now, so the
        only way a store holds such a row is an older writer or a hand edit.
        """
        from pathlib import Path

        from codex_session_relay.store import Store

        self.run_cli(
            "settings-record", "--task", PARENT, "--settings", json.dumps(self._settings()),
        )
        store = Store(Path(self.tmp) / "relay.sqlite3")
        with store.transaction() as db:
            db.execute(
                "UPDATE authorized_settings SET settings = ? WHERE task_id = ?",
                (json.dumps(dict(self._settings(), cwd=7)), PARENT),
            )
        store.db.commit()
        store.close()

        shown = self.run_cli("settings-show", "--task", PARENT)
        self.assertTrue(shown["usable"], "the record did not become incomplete")
        self.assertEqual(shown["missing"], [])
        self.assertFalse(shown["deliverable"])
        self.assertEqual(shown["recordFinding"]["code"], "settings_mistyped")
        self.assertIn("cwd is int, not str", shown["recordFinding"]["detail"])


class Diagnosis(unittest.TestCase):
    """doctor has to answer ON the host it is describing, including a broken one."""

    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="relay-doctor-")
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)
        self.home = os.path.join(self.tmp, "home")
        os.makedirs(self.home)

    def cli(self, *args, state=None, socket=None, expect=0, env=None):
        environment = dict(
            os.environ, PYTHONPATH=os.path.join(REPO, "src"), HOME=self.home,
        )
        environment.pop("CODEX_SESSION_RELAY_STATE", None)
        environment.pop("XDG_STATE_HOME", None)
        environment.update(env or {})
        completed = subprocess.run(
            [sys.executable, "-m", "codex_session_relay.cli",
             *(["--state", state] if state else []),
             *(["--socket", socket] if socket else []), *args],
            capture_output=True, text=True, env=environment, timeout=60,
        )
        self.assertEqual(
            completed.returncode, expect,
            f"exit {completed.returncode}: {completed.stdout}{completed.stderr}",
        )
        return json.loads(completed.stdout)

    def test_doctor_answers_for_a_state_directory_that_does_not_exist_yet(self):
        absent = os.path.join(self.tmp, "absent")
        report = self.cli("doctor", state=absent)
        self.assertEqual(report["stateSelection"]["source"], "flag")
        self.assertFalse(report["access"]["directoryExists"])
        self.assertFalse(report["contents"]["available"])
        # The old doctor built a Store first, which created the directory it was asked about.
        self.assertFalse(os.path.exists(absent))

    def test_doctor_does_not_turn_an_unrelated_file_into_a_relay_database(self):
        """The directory existing was not the whole side effect; counting rows was too.

        A readable relay.sqlite3 sent the contents block through a real Store, and
        Store.__init__ opens O_RDWR, switches on WAL and runs the entire schema script. An
        empty, legacy or unrelated file was quietly adopted by the command that promised to
        do nothing but look.
        """
        state = os.path.join(self.tmp, "borrowed")
        os.makedirs(state)
        target = os.path.join(state, "relay.sqlite3")
        open(target, "w").close()

        report = self.cli("doctor", state=state)

        self.assertEqual(os.path.getsize(target), 0, "doctor wrote a schema into it")
        self.assertEqual(
            sorted(os.listdir(state)), ["relay.sqlite3"], "no WAL or shm sidecar either",
        )
        self.assertTrue(report["access"]["dbExists"])
        self.assertFalse(report["contents"]["available"],
                         "and it says so rather than inventing counts")
        self.assertIsNotNone(report["contents"]["detail"])

    def test_doctor_names_the_rule_that_chose_the_directory(self):
        chosen = os.path.join(self.tmp, "chosen")
        by_env = self.cli("doctor", env={"CODEX_SESSION_RELAY_STATE": chosen})
        self.assertEqual(by_env["stateSelection"]["source"], "env")
        self.assertEqual(by_env["stateSelection"]["path"], chosen)
        by_flag = self.cli("doctor", state=chosen, env={
            "CODEX_SESSION_RELAY_STATE": os.path.join(self.tmp, "ignored"),
        })
        self.assertEqual(by_flag["stateSelection"]["source"], "flag")
        self.assertEqual(by_flag["stateSelection"]["path"], chosen)

    def test_a_different_store_is_refused_rather_than_reported_healthy(self):
        a, b = os.path.join(self.tmp, "a"), os.path.join(self.tmp, "b")
        mine = self.cli("store-identity", state=a)["store"]
        theirs = self.cli("store-identity", state=b)["store"]
        self.assertNotEqual(mine["storeId"], theirs["storeId"])
        refused = self.cli(
            "doctor", "--expect-store", mine["storeId"], state=b, expect=2,
        )
        self.assertEqual(refused["sameStore"], "mismatch")
        # The whole diagnosis survives the refusal; it is not replaced by an error envelope.
        self.assertIn("stateSelection", refused)
        self.assertIn("access", refused)

    def test_a_nonce_and_the_physical_identity_prove_one_store_and_disprove_a_copy(self):
        """Proof takes both, and either alone is refused rather than reported healthy.

        A nonce says a write of the other participant's reached the file being read. A copy
        taken AFTER the challenge was written carries it with the bytes, so that alone does
        not say the two are one file now - the device and inode are what answer that. The copy
        here is made BEFORE the write, which is why it lacks the nonce and is a mismatch.
        """
        a, b = os.path.join(self.tmp, "a"), os.path.join(self.tmp, "b")
        mine = self.cli("store-identity", state=a)["store"]
        os.makedirs(b, exist_ok=True)
        for suffix in ("", "-wal", "-shm"):
            source = os.path.join(a, f"relay.sqlite3{suffix}")
            if os.path.exists(source):
                shutil.copy(source, os.path.join(b, f"relay.sqlite3{suffix}"))
        nonce = self.cli("store-challenge", "--write", "--actor", "parent", state=a)["nonce"]
        pair = f"{mine['device']}:{mine['inode']}"
        log = f"{mine['logDevice']}:{mine['logInode']}:{mine['logName']}"
        proven = self.cli(
            "doctor", "--expect-store", mine["storeId"], "--expect-inode", pair,
            "--expect-log", log, "--expect-nonce", nonce, state=a,
        )
        self.assertEqual(proven["sameStore"], "proven")
        # On evidence rather than on the word: the store this doctor measured reports the
        # same log location that was passed in, and the nonce was read through it too.
        self.assertEqual(
            f"{proven['store']['logDevice']}:{proven['store']['logInode']}"
            f":{proven['store']['logName']}",
            log,
        )
        self.assertEqual(
            (proven["nonce"]["logDevice"], proven["nonce"]["logInode"]),
            (proven["store"]["logDevice"], proven["store"]["logInode"]),
        )
        # The nonce on its own is not proof, and unproven exits non-zero like a mismatch.
        alone = self.cli(
            "doctor", "--expect-store", mine["storeId"], "--expect-nonce", nonce, state=a,
            expect=2,
        )
        self.assertEqual(alone["sameStore"], "unproven")
        self.assertIn("--expect-inode", alone["detail"])
        self.assertIn("--expect-log", alone["detail"])
        copied = self.cli(
            "doctor", "--expect-store", mine["storeId"], "--expect-inode", pair,
            "--expect-log", log, "--expect-nonce", nonce, state=b, expect=2,
        )
        self.assertEqual(copied["sameStore"], "mismatch")
        # Identifier alone cannot separate them, which is why it is graded unproven.
        weak = self.cli("doctor", "--expect-store", mine["storeId"], state=b, expect=2)
        self.assertEqual(weak["sameStore"], "unproven")

    def test_a_second_name_for_one_store_is_refused_through_the_command(self):
        """The second pathname, end to end, by the route that needs no privilege.

        The case CRW-18 is about is a file bind mount, which a test suite cannot make here -
        this host refuses an unprivileged mount namespace. A hardlink reaches the same hazard:
        two directories, one inode, and a write-ahead log each. Both refusals are asserted
        because they are different facts. The name count is this side's own measurement and
        needs nothing from the peer; the log location is the general answer and needs the peer
        to say where its log goes. A bind mount leaves the first one blind, which is the whole
        reason the second exists.
        """
        a, b = os.path.join(self.tmp, "one"), os.path.join(self.tmp, "two")
        mine = self.cli("store-identity", state=a)["store"]
        nonce = self.cli("store-challenge", "--write", "--actor", "parent", state=a)["nonce"]
        os.makedirs(b, exist_ok=True)
        os.link(os.path.join(a, "relay.sqlite3"), os.path.join(b, "relay.sqlite3"))
        pair = f"{mine['device']}:{mine['inode']}"
        log = f"{mine['logDevice']}:{mine['logInode']}:{mine['logName']}"

        # Without the peer's log location, the name count is what catches it.
        counted = self.cli(
            "doctor", "--expect-store", mine["storeId"], "--expect-inode", pair,
            "--expect-nonce", nonce, state=b, expect=2,
        )
        self.assertEqual(counted["sameStore"], "unproven")
        self.assertIn("names", counted["detail"])

        # With it, both reasons stand together rather than one outranking the other, because
        # the log location is graded unproven and not as a mismatch.
        graded = self.cli(
            "doctor", "--expect-store", mine["storeId"], "--expect-inode", pair,
            "--expect-log", log, "--expect-nonce", nonce, state=b, expect=2,
        )
        self.assertEqual(graded["sameStore"], "unproven")
        self.assertIn("names", graded["detail"])
        self.assertIn("write-ahead log", graded["detail"])

        # And the case is the one described: one inode reached in two directories.
        self.assertEqual(
            (graded["store"]["device"], graded["store"]["inode"]),
            (mine["device"], mine["inode"]),
        )
        self.assertEqual(graded["store"]["links"], 2, graded["store"])
        self.assertNotEqual(graded["store"]["logInode"], mine["logInode"])

    def test_an_expectation_with_no_usable_value_is_still_a_question(self):
        """An empty flag asked something, and its answer must not exit 0.

        The exit was decided by `any` over the VALUES, which counts only non-empty ones, so an
        empty expectation was a question nobody had asked and its unproven payload came back
        at exit 0 - which a caller reads as yes. True of the three expectations that came
        before the log location as well, so the fix is theirs too.
        """
        a = os.path.join(self.tmp, "asked")
        self.cli("store-identity", state=a)
        for flag, value in (
            ("--expect-log", ""), ("--expect-log", "1:2"), ("--expect-log", "nonsense"),
            ("--expect-store", ""), ("--expect-inode", ""), ("--expect-nonce", ""),
        ):
            with self.subTest(flag=flag, value=value):
                refused = self.cli("doctor", flag, value, state=a, expect=2)
                self.assertNotEqual(refused["sameStore"], "proven", refused)

    def test_doctor_reports_that_state_and_the_transport_ledger_have_split(self):
        state = os.path.join(self.tmp, "state")
        socket = os.path.join(self.tmp, "app.sock")
        split = self.cli("doctor", state=state, socket=socket)
        self.assertTrue(split["ledger"]["configured"])
        # --state moved the store; the adapter resolves its ledger from the environment.
        self.assertTrue(split["ledger"]["split"])
        together = self.cli(
            "doctor", state=state, socket=socket,
            env={"CODEX_SESSION_RELAY_STATE": state},
        )
        self.assertFalse(together["ledger"]["split"])


class LazyServices(unittest.TestCase):
    """Building dependencies on demand must not drop the wiring __init__ used to do."""

    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="relay-lazy-")
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)

    def services(self):
        from codex_session_relay.cli import Services

        built = Services(argparse.Namespace(state=self.tmp, socket=None))
        self.addCleanup(built.close)
        return built

    def test_the_first_ack_is_already_wired_to_the_outbox(self):
        built = self.services()
        # Touching ack FIRST is the regression: record_verdict skips its outbox obligation
        # when sync is absent, so an unwired ack would lose it silently.
        self.assertIsNotNone(built.ack.sync)
        self.assertIs(built.ack.sync, built.sync)

    def test_every_dependency_shares_one_store_and_is_built_once(self):
        built = self.services()
        self.assertIs(built.registry.store, built.store)
        self.assertIs(built.delivery.store, built.store)
        self.assertIs(built.ack.store, built.store)
        self.assertIs(built.registry, built.registry)
        self.assertIs(built.delivery, built.delivery)

    def test_closing_without_ever_using_the_store_creates_nothing(self):
        from codex_session_relay.cli import Services

        empty = os.path.join(self.tmp, "untouched")
        built = Services(argparse.Namespace(state=empty, socket=None))
        built.close()
        self.assertFalse(os.path.exists(empty))


class WorkerPolicyRequirements(CliBase):
    """doctor --require-worker-policy parses requirements and refuses a non-ready worker.

    These cover the CLI surface only: what one command accepts as requirements, what it
    refuses, and that the diagnosis survives the refusal. Whether a LIVE worker's published
    snapshot satisfies it is test_worker_policy.py's question, answered with real processes.
    """

    REQUIREMENTS = json.dumps(
        [{"role": "parent", "model": "devin/swe-2", "reasoningEffort": "max"}]
    )

    def test_malformed_requirements_are_a_usage_error(self):
        # A JSON document that cannot be parsed never reaches the readiness comparison:
        # usage 4 names the input, not the worker. Shape errors are a different failure and
        # belong to the refusal test below.
        for bad in ("{not json",):
            with self.subTest(raw=bad):
                refused = self.run_cli(
                    "doctor", "--require-worker-policy", bad, expect=4,
                )
                self.assertEqual(refused["error"], "usage")
                self.assertIn("worker policy requirements", refused["detail"])

    def test_wrong_type_requirements_are_refused_not_reported_healthy(self):
        # Valid JSON with the wrong shape is a different failure than unparseable JSON: the
        # comparison runs and answers readiness False with its own reason, exit 2, rather
        # than exit 4 on parsing. Each fixture names its own reason, so a refactor that
        # quietly folds one into the other fails here rather than passing as "some refusal".
        for wrong, reason in (
            ('{"role": "parent"}', "worker_policy_requirements_invalid"),
            ('[{"role": "supervisor", "model": "m", "reasoningEffort": "max"}]',
             "worker_policy_role_unsupported"),
            ('[{"role": "parent", "model": false, "reasoningEffort": "max"}]',
             "worker_policy_requirements_invalid"),
            ("[]", "worker_policy_requirements_invalid"),
        ):
            with self.subTest(raw=wrong, reason=reason):
                report = self.run_cli(
                    "doctor", "--require-worker-policy", wrong, expect=2,
                )
                self.assertFalse(report["workerReadiness"]["ready"])
                self.assertEqual(report["workerReadiness"]["reason"], reason)

    def test_an_unreadable_requirements_file_is_a_usage_error(self):
        missing = os.path.join(self.tmp, "absent-requirements.json")
        refused = self.run_cli(
            "doctor", "--require-worker-policy", f"@{missing}", expect=4,
        )
        self.assertEqual(refused["error"], "usage")
        self.assertIn("No such file", refused["detail"])

    def test_doctor_without_the_flag_reports_the_worker_and_gates_nothing(self):
        report = self.run_cli("doctor")
        self.assertFalse(report["workerPolicy"]["observed"])
        self.assertEqual(report["callerWorkerAgreement"], "unknown")
        self.assertNotIn("workerReadiness", report,
                         "diagnostic doctor answers a question nobody asked")

    def test_the_refusal_keeps_the_whole_diagnosis(self):
        # Like the store-mismatch refusal, the payload on exit 2 is the full report, so a
        # coordinator still sees state selection, access and the role policy beside the
        # readiness answer that refused.
        report = self.run_cli(
            "doctor", "--require-worker-policy", self.REQUIREMENTS, expect=2,
        )
        self.assertFalse(report["workerReadiness"]["ready"])
        self.assertEqual(report["workerReadiness"]["reason"], "worker_policy_unreadable")
        self.assertIn("stateSelection", report)
        self.assertIn("access", report)
        self.assertIn("rolePolicy", report)

    def test_the_readiness_refusal_retains_the_store_nonce_and_issue_answers(self):
        """One combined invocation: readiness False must not hide the requested readings.

        A coordinator asks doctor one question in one command: is the worker ready, is
        this the store the participants reported, and does it hold the issue? The
        readiness refusal used to fire before the other answers were computed, so exit 2
        carried a readiness verdict and nothing else -- the aggregation gap this
        regression closes.
        """
        mine = self.run_cli("store-identity")["store"]
        nonce = self.run_cli("store-challenge", "--write", "--actor", "parent")["nonce"]
        report = self.run_cli(
            "doctor", "--require-worker-policy", self.REQUIREMENTS,
            "--expect-store", mine["storeId"],
            "--expect-inode", f"{mine['device']}:{mine['inode']}",
            "--expect-log",
            f"{mine['logDevice']}:{mine['logInode']}:{mine['logName']}",
            "--expect-nonce", nonce, "--issue", ISSUE,
            expect=2,
        )
        self.assertFalse(report["workerReadiness"]["ready"])
        self.assertEqual(report["workerReadiness"]["reason"], "worker_policy_unreadable")
        # The refusal carries the whole answer: proven store identity, the nonce found
        # in the file it proves, and the issue reading, beside the readiness that
        # refused.
        self.assertEqual(report["sameStore"], "proven")
        self.assertTrue(report["nonce"]["found"])
        self.assertTrue(report["issue"]["readable"])
        self.assertFalse(report["issue"]["holds"])

    def test_requirements_staged_as_a_file_are_read(self):
        """The @path spelling goes through the same parser as the inline value."""
        path = os.path.join(self.tmp, "requirements.json")
        with open(path, "w", encoding="utf-8") as handle:
            handle.write(self.REQUIREMENTS)
        report = self.run_cli("doctor", "--require-worker-policy", f"@{path}", expect=2)
        self.assertEqual(report["workerReadiness"]["reason"], "worker_policy_unreadable")


class ContestedSocket(CliBase):
    """Two stores recording one socket must not quietly become three.

    These runs deliberately pass no --state: the whole question is what the environment alone
    resolves to, and an explicit directory answers it before discovery ever runs.
    """

    def contested(self, name):
        """A home holding two stores that both record one socket."""
        from pathlib import Path

        from codex_session_relay.store import Store

        home = os.path.join(self.tmp, name)
        root = os.path.join(home, ".local", "state", "codex-session-relay")
        socket = os.path.join(self.tmp, f"{name}.sock")
        for directory in ("aaaa444444444444", "bbbb444444444444"):
            os.makedirs(os.path.join(root, directory))
            Store(Path(root) / directory / "relay.sqlite3", socket_path=socket).close()
        return home, root, socket

    def run_in_home(self, home, *args, expect=0):
        environment = dict(os.environ, PYTHONPATH=os.path.join(REPO, "src"), HOME=home)
        for name in ("CODEX_SESSION_RELAY_STATE", "XDG_STATE_HOME"):
            environment.pop(name, None)
        completed = subprocess.run(
            [sys.executable, "-m", "codex_session_relay.cli", *args],
            capture_output=True, text=True, env=environment, timeout=60,
        )
        self.assertEqual(
            completed.returncode, expect,
            f"exit {completed.returncode}: {completed.stdout}{completed.stderr}",
        )
        return json.loads(completed.stdout)

    def test_an_ordinary_command_refuses_rather_than_creating_a_third_store(self):
        home, root, socket = self.contested("contested-status")

        refused = self.run_in_home(home, "--socket", socket, "status", expect=2)

        self.assertEqual(refused["reason"], "ambiguous_state_directory")
        self.assertEqual(len(refused["candidates"]), 2)
        self.assertFalse(
            os.path.exists(refused["wouldHaveCreated"]),
            "the refusal must not leave behind the store it refused to choose",
        )
        self.assertEqual(sorted(os.listdir(root)), ["aaaa444444444444", "bbbb444444444444"])

    def test_doctor_still_describes_a_contested_socket(self):
        home, _root, socket = self.contested("contested-doctor")

        report = self.run_in_home(home, "--socket", socket, "doctor")

        self.assertTrue(report["siblingStores"]["ambiguous"])
        self.assertEqual(len(report["siblingStores"]["claimingThisSocket"]), 2)

    def test_a_state_directory_recording_another_socket_is_refused(self):
        """An explicit directory reused with a different App Server.

        Choosing a directory is not choosing what is already in it: the service would claim
        and serve the new socket while the database went on attributing itself to the old
        one, so one installation's assignments could be exposed through another and later
        discovery would still match the store to the socket it no longer serves.
        """
        from pathlib import Path

        from codex_session_relay.store import Store

        state = os.path.join(self.tmp, "reused-state")
        first = os.path.join(self.tmp, "first.sock")
        second = os.path.join(self.tmp, "second.sock")
        Store(Path(state) / "relay.sqlite3", socket_path=first).close()

        environment = dict(os.environ, PYTHONPATH=os.path.join(REPO, "src"))
        completed = subprocess.run(
            [sys.executable, "-m", "codex_session_relay.cli", "--state", state,
             "--socket", second, "status"],
            capture_output=True, text=True, env=environment, timeout=60,
        )

        self.assertEqual(completed.returncode, 2, completed.stdout + completed.stderr)
        refused = json.loads(completed.stdout)
        self.assertEqual(refused["reason"], "state_directory_serves_another_socket")
        self.assertEqual(refused["recordedSocket"], first)

    def test_a_store_recording_no_socket_also_refuses_before_creating_one(self):
        """A store older than provenance cannot be matched to a socket by anything but its
        directory hash, which cannot be inverted. Reporting it through doctor was not enough,
        because an ordinary command does not run doctor and creates the store anyway."""
        from pathlib import Path

        from codex_session_relay.store import Store

        home = os.path.join(self.tmp, "unlabelled-home")
        root = os.path.join(home, ".local", "state", "codex-session-relay")
        os.makedirs(os.path.join(root, "0123456789abcdef"))
        Store(Path(root) / "0123456789abcdef" / "relay.sqlite3").close()
        socket = os.path.join(self.tmp, "unlabelled.sock")

        refused = self.run_in_home(home, "--socket", socket, "status", expect=2)

        self.assertEqual(refused["reason"], "unidentified_state_directory")
        self.assertFalse(os.path.exists(refused["wouldHaveCreated"]))
        self.assertEqual(os.listdir(root), ["0123456789abcdef"], "no store was created")

    def test_the_refusal_prints_commands_an_operator_can_actually_run(self):
        """The payload is all an operator has.

        It used to end with "--state <the directory above> once, to adopt it deliberately",
        which is not a command, does not say which directory, and drops the --socket that
        made the two stores candidates for each other in the first place. It also promised an
        adoption that does not exist: choosing one of two claiming stores leaves both still
        recording the socket, so default discovery refuses again on the next invocation.
        """
        home, root, socket = self.contested("contested-recovery")

        refused = self.run_in_home(home, "--socket", socket, "status", expect=2)

        recover = refused["recover"]
        commands = [line for line in recover if not line.startswith("  ")]
        self.assertTrue(commands, "the refusal offered no runnable command")
        for command in commands:
            self.assertIn(f"--socket={socket}", command,
                          f"a recovery command dropped the socket: {command}")
        for candidate in refused["candidates"]:
            self.assertTrue(
                any(f"--state={candidate}" in c for c in commands),
                f"no command inspects candidate {candidate}",
            )
        self.assertTrue(
            any("doctor" in c for c in commands) and any("service status" in c for c in commands),
            "recovery must both identify the store and show what it carries",
        )
        self.assertNotIn(
            "the directory above", " ".join(recover),
            "the payload still points at a directory it never names",
        )
        self.assertTrue(
            any("retired" in line for line in recover),
            "the payload must say that choosing one store does not retire the other",
        )

    def test_the_wrong_socket_refusal_offers_a_matching_pair_not_an_adoption(self):
        """This refusal has nothing to adopt: using a store does not rewrite the socket it
        recorded. It printed no recovery at all, which left the operator to infer that."""
        from pathlib import Path

        from codex_session_relay.store import Store

        state = os.path.join(self.tmp, "pair-state")
        first = os.path.join(self.tmp, "pair-first.sock")
        second = os.path.join(self.tmp, "pair-second.sock")
        Store(Path(state) / "relay.sqlite3", socket_path=first).close()

        environment = dict(os.environ, PYTHONPATH=os.path.join(REPO, "src"))
        completed = subprocess.run(
            [sys.executable, "-m", "codex_session_relay.cli", "--state", state,
             "--socket", second, "status"],
            capture_output=True, text=True, env=environment, timeout=60,
        )
        self.assertEqual(completed.returncode, 2, completed.stdout + completed.stderr)
        refused = json.loads(completed.stdout)

        joined = " ".join(refused["recover"])
        self.assertIn(first, joined, "no command reads the store under the socket it records")
        self.assertIn(second, joined, "no command looks for the socket that was asked for")
        self.assertIn("does not rewrite", refused["note"])

    def test_recovery_commands_are_safe_to_paste(self):
        """These strings exist to be pasted, so a path carrying shell syntax is executable.

        A state directory or socket path with a substitution in it would run as the operator
        did exactly what the refusal told them to do.
        """
        import shlex
        from pathlib import Path

        from codex_session_relay.store import Store

        home = os.path.join(self.tmp, "quoted-home")
        root = os.path.join(home, ".local", "state", "codex-session-relay")
        # A socket whose name would run a command if it were pasted unquoted.
        socket = os.path.join(self.tmp, "sock$(touch /tmp/pwned);x.sock")
        for directory in ("aaaa555555555555", "bbbb555555555555"):
            os.makedirs(os.path.join(root, directory))
            Store(Path(root) / directory / "relay.sqlite3", socket_path=socket).close()

        refused = self.run_in_home(home, "--socket", socket, "status", expect=2)

        commands = [line for line in refused["recover"] if not line.startswith("  ")]
        self.assertTrue(commands)
        for command in commands:
            # The real property is the round trip: the shell must hand the socket back as ONE
            # intact argument rather than splitting it or running the substitution in it.
            words = option_values(command)
            self.assertIn(
                socket, words,
                f"the socket did not survive a shell round trip intact: {command}",
            )
            self.assertFalse(
                [w for w in words if "$(" in w and w != socket],
                f"a substitution escaped quoting: {command}",
            )

    def test_the_wrong_socket_recovery_is_quoted_too(self):
        """The other refusal prints commands as well, and paths reach it the same way."""
        import shlex
        from pathlib import Path

        from codex_session_relay.store import Store

        state = os.path.join(self.tmp, "quoted state")
        first = os.path.join(self.tmp, "first$(id).sock")
        second = os.path.join(self.tmp, "second.sock")
        Store(Path(state) / "relay.sqlite3", socket_path=first).close()

        environment = dict(os.environ, PYTHONPATH=os.path.join(REPO, "src"))
        completed = subprocess.run(
            [sys.executable, "-m", "codex_session_relay.cli", "--state", state,
             "--socket", second, "status"],
            capture_output=True, text=True, env=environment, timeout=60,
        )
        self.assertEqual(completed.returncode, 2, completed.stdout + completed.stderr)
        refused = json.loads(completed.stdout)

        commands = [line for line in refused["recover"] if not line.startswith("  ")]
        self.assertTrue(commands)
        words = [word for command in commands for word in option_values(command)]
        self.assertIn(first, words, "the recorded socket did not survive a shell round trip")
        self.assertIn(state, words, "the state directory did not survive a shell round trip")

    def test_the_wrong_socket_recovery_drops_a_state_pin_that_would_reselect_it(self):
        """The socket-first line carries no --state, so an inherited pin overrides its intent.

        CODEX_SESSION_RELAY_STATE is one of the two ways to reach this refusal, and it is the
        way that turns the recovery line into a dead end: pasted with the pin still set,
        "find the store that belongs to this socket" re-selects the store that produced the
        refusal and hands back the same error. This runs the printed command rather than
        matching its text, because what matters is where pasting it actually lands.
        """
        import shlex
        from pathlib import Path

        from codex_session_relay.store import Store

        home = os.path.join(self.tmp, "pinned-home")
        os.makedirs(home)
        state = os.path.join(self.tmp, "pinned-state")
        recorded = os.path.join(self.tmp, "pinned-recorded.sock")
        wanted = os.path.join(self.tmp, "pinned-wanted.sock")
        Store(Path(state) / "relay.sqlite3", socket_path=recorded).close()

        # HOME is pinned to a temporary directory: the recovery command falls back to default
        # discovery once the pin is dropped, and that must not reach the real user state.
        environment = dict(
            os.environ, PYTHONPATH=os.path.join(REPO, "src"), HOME=home,
            CODEX_SESSION_RELAY_STATE=state,
        )
        environment.pop("XDG_STATE_HOME", None)
        completed = subprocess.run(
            [sys.executable, "-m", "codex_session_relay.cli", "--socket", wanted, "status"],
            capture_output=True, text=True, env=environment, timeout=60,
        )
        self.assertEqual(completed.returncode, 2, completed.stdout + completed.stderr)
        refused = json.loads(completed.stdout)
        self.assertEqual(refused["reason"], "state_directory_serves_another_socket")

        socket_first = [
            line for line in refused["recover"]
            if not line.startswith("  ") and wanted in line
        ]
        self.assertEqual(len(socket_first), 1, refused["recover"])

        replayed = subprocess.run(
            shlex.split(socket_first[0]),
            capture_output=True, text=True, env=environment, timeout=60,
        )

        # doctor is exempt from this guard, so it does not hand the refusal back - it does
        # something quieter and worse. With the pin still set it reports the very store that
        # produced the refusal, while its caption says it finds the store belonging to the
        # socket. That is what the assertion has to catch.
        selection = json.loads(replayed.stdout)["stateSelection"]
        self.assertNotEqual(
            selection["path"], state,
            "the recovery command re-selected the store the refusal was about",
        )
        self.assertNotEqual(
            selection["source"], "env",
            "the state pin survived into the command printed to look past it",
        )

    def wrong_socket_refusal(self, name, *, flagged_socket, wanted_socket, pin):
        """Refuse a --state store that records another socket, with the variable also set.

        Returns the payload and the environment it was produced in, so a test can replay the
        commands it printed under exactly the conditions that printed them.
        """
        from pathlib import Path

        from codex_session_relay.store import Store

        home = os.path.join(self.tmp, f"{name}-home")
        os.makedirs(home)
        flagged = os.path.join(self.tmp, f"{name}-flagged")
        Store(Path(flagged) / "relay.sqlite3", socket_path=flagged_socket).close()

        environment = dict(
            os.environ, PYTHONPATH=os.path.join(REPO, "src"), HOME=home,
            CODEX_SESSION_RELAY_STATE=(flagged if pin == "same" else pin),
        )
        environment.pop("XDG_STATE_HOME", None)
        completed = subprocess.run(
            [sys.executable, "-m", "codex_session_relay.cli", "--state", flagged,
             "--socket", wanted_socket, "status"],
            capture_output=True, text=True, env=environment, timeout=60,
        )
        self.assertEqual(completed.returncode, 2, completed.stdout + completed.stderr)
        refused = json.loads(completed.stdout)
        self.assertEqual(refused["reason"], "state_directory_serves_another_socket")
        return refused, environment, flagged

    def test_a_pin_repeating_the_flags_mistake_does_not_come_back_as_the_answer(self):
        """--state A with the variable also naming A. Two conditions failed this case.

        Keying the prefix on which rule caused the refusal proved only that --state won, not
        that the lower-precedence store was any better: here it is the SAME store. Left
        pinned, the socket-first line re-selects the directory the refusal was about, and
        because doctor is exempt from this guard it exits 0 under a caption claiming it found
        the requested socket's store. The line no longer decides - it always runs unpinned.
        """
        import shlex

        wanted = os.path.join(self.tmp, "samepin-wanted.sock")
        refused, environment, flagged = self.wrong_socket_refusal(
            "samepin",
            flagged_socket=os.path.join(self.tmp, "samepin-other.sock"),
            wanted_socket=wanted, pin="same",
        )

        discovery = [
            line for line in refused["recover"]
            if not line.startswith("  ") and wanted in line
        ]
        self.assertEqual(len(discovery), 1, refused["recover"])

        replayed = subprocess.run(
            shlex.split(discovery[0]),
            capture_output=True, text=True, env=environment, timeout=60,
        )

        selection = json.loads(replayed.stdout)["stateSelection"]
        self.assertNotEqual(
            selection["path"], flagged,
            "the recovery command handed back the store the refusal was about",
        )
        self.assertNotEqual(selection["source"], "env", selection)

    def test_a_flag_caused_refusal_offers_the_environment_store_as_its_own_candidate(self):
        """--state wins over the variable, so the variable may hold the right store.

        Guessing in either direction was wrong, so both candidates are printed: one line
        discovers by socket with no pin at all, and a second reads the directory the variable
        names. Nothing here decides which of them the operator meant.
        """
        import shlex
        from pathlib import Path

        from codex_session_relay.store import Store

        wanted = os.path.join(self.tmp, "flagenv-wanted.sock")
        pinned = os.path.join(self.tmp, "flagenv-pinned")
        # The variable's store is the one that records the socket actually being asked for.
        Store(Path(pinned) / "relay.sqlite3", socket_path=wanted).close()
        refused, environment, _flagged = self.wrong_socket_refusal(
            "flagenv",
            flagged_socket=os.path.join(self.tmp, "flagenv-other.sock"),
            wanted_socket=wanted, pin=pinned,
        )

        commands = [line for line in refused["recover"] if not line.startswith("  ")]
        unpinned = [c for c in commands if wanted in c and "env -u" in c]
        env_candidate = [c for c in commands if f"--state={pinned}" in c]
        self.assertEqual(len(unpinned), 1, refused["recover"])
        self.assertEqual(
            len(env_candidate), 1,
            f"the directory the variable names was never offered: {refused['recover']}",
        )

        replayed = subprocess.run(
            shlex.split(env_candidate[0]),
            capture_output=True, text=True, env=environment, timeout=60,
        )

        selection = json.loads(replayed.stdout)["stateSelection"]
        self.assertEqual(selection["path"], pinned, selection)
        self.assertTrue(
            os.path.exists(selection["dbPath"]),
            "the offered candidate reported a database that does not exist",
        )

    def test_a_pin_that_only_spells_the_same_directory_differently_is_not_a_candidate(self):
        """Offering a candidate has to mean offering a different store.

        Two spellings reach the same directory and neither is exotic: `~/pinned` because
        selection expands it, and `.../x/../pinned` because the filesystem does. Compared as
        written they look like separate candidates, and the line each would add resolves
        straight back to the store that caused the refusal - a dead end wearing the label of
        an alternative.
        """
        from pathlib import Path

        from codex_session_relay.store import Store

        home = os.path.join(self.tmp, "tilde-home")
        pinned = os.path.join(home, "pinned")
        os.makedirs(home)
        wanted = os.path.join(self.tmp, "tilde-wanted.sock")
        Store(
            Path(pinned) / "relay.sqlite3",
            socket_path=os.path.join(self.tmp, "tilde-other.sock"),
        ).close()

        # Each spelling names exactly the directory --state names, by a different route.
        spellings = {
            "the home shortcut": "~/pinned",
            "a dot segment": os.path.join(home, "pinned", "..", "pinned"),
        }
        for label, spelling in spellings.items():
            with self.subTest(spelling=label):
                environment = dict(
                    os.environ, PYTHONPATH=os.path.join(REPO, "src"), HOME=home,
                    CODEX_SESSION_RELAY_STATE=spelling,
                )
                environment.pop("XDG_STATE_HOME", None)
                completed = subprocess.run(
                    [sys.executable, "-m", "codex_session_relay.cli", f"--state={pinned}",
                     f"--socket={wanted}", "status"],
                    capture_output=True, text=True, env=environment, timeout=60,
                )
                self.assertEqual(completed.returncode, 2, completed.stdout + completed.stderr)
                refused = json.loads(completed.stdout)
                self.assertEqual(refused["reason"], "state_directory_serves_another_socket")

                commands = [
                    line for line in refused["recover"] if not line.startswith("  ")
                ]
                pinning = [c for c in commands if "--state=" in c]
                self.assertEqual(
                    len(pinning), 1,
                    f"the refusing store came back as its own alternative: {refused['recover']}",
                )
                self.assertNotIn(spelling, " ".join(commands))

    def test_an_unexpandable_pin_does_not_replace_the_refusal_with_a_host_error(self):
        """The refusal payload is everything the operator has, so it has to survive.

        An explicit --state overrides the variable, so nothing validates the variable's value
        at startup and building the recovery list is the first thing that touches it.
        Path.expanduser() raises RuntimeError for a ~user whose home cannot be resolved. The
        top-level handler catches it, so this is not a traceback - it is worse in a quieter
        way: exit 3 with a generic {"error": "host"} and none of the recorded socket, the
        requested socket or the recovery commands the operator needed.
        """
        from pathlib import Path

        from codex_session_relay.store import Store

        state = os.path.join(self.tmp, "badpin-state")
        wanted = os.path.join(self.tmp, "badpin-wanted.sock")
        Store(
            Path(state) / "relay.sqlite3",
            socket_path=os.path.join(self.tmp, "badpin-other.sock"),
        ).close()

        environment = dict(
            os.environ, PYTHONPATH=os.path.join(REPO, "src"),
            CODEX_SESSION_RELAY_STATE="~no-such-user-for-this-test/store",
        )
        completed = subprocess.run(
            [sys.executable, "-m", "codex_session_relay.cli", f"--state={state}",
             f"--socket={wanted}", "status"],
            capture_output=True, text=True, env=environment, timeout=60,
        )

        self.assertEqual(
            completed.returncode, 2,
            f"the refusal did not survive: {completed.stdout}{completed.stderr}",
        )
        refused = json.loads(completed.stdout)
        self.assertEqual(refused["reason"], "state_directory_serves_another_socket")
        commands = [line for line in refused["recover"] if not line.startswith("  ")]
        self.assertEqual(len(commands), 2, refused["recover"])
        # And the unusable value is reported rather than silently dropped.
        self.assertIn("does not resolve", " ".join(refused["recover"]))

    def test_a_socket_path_beginning_with_a_dash_still_produces_runnable_commands(self):
        """A relative socket path may legitimately begin with a dash.

        The original invocation can pass it as --socket=-odd.sock, but a generated line that
        separates them with a space makes argparse read the value as another option and fail
        with "expected one argument" - so every command in the payload is unusable for that
        input. The value travels attached.

        This drives the ambiguous refusal rather than the different-socket one, because that
        is where a dash can still reach the output: the different-socket payload prints
        canonical_socket() on both sides, which is absolute and therefore never leads with a
        dash, while _recovery_commands prints services.socket_path, the argument as given.
        """
        import shlex
        from pathlib import Path

        from codex_session_relay.store import Store

        # RELATIVE, so the value itself begins with the dash. An absolute path with a dashed
        # basename still starts with '/' and never reaches the parser ambiguity.
        relative = "-odd.sock"
        home = os.path.join(self.tmp, "dash-home")
        root = os.path.join(home, ".local", "state", "codex-session-relay")
        # Both stores record what that relative name resolves to under the subprocess cwd,
        # so the run is ambiguous for exactly the socket being asked about.
        for directory in ("aaaa666666666666", "bbbb666666666666"):
            os.makedirs(os.path.join(root, directory))
            Store(
                Path(root) / directory / "relay.sqlite3",
                socket_path=os.path.join(self.tmp, relative),
            ).close()

        environment = dict(os.environ, PYTHONPATH=os.path.join(REPO, "src"), HOME=home)
        for name in ("CODEX_SESSION_RELAY_STATE", "XDG_STATE_HOME"):
            environment.pop(name, None)
        completed = subprocess.run(
            [sys.executable, "-m", "codex_session_relay.cli",
             f"--socket={relative}", "status"],
            capture_output=True, text=True, env=environment, timeout=60, cwd=self.tmp,
        )
        self.assertEqual(completed.returncode, 2, completed.stdout + completed.stderr)
        refused = json.loads(completed.stdout)
        self.assertEqual(refused["reason"], "ambiguous_state_directory")

        commands = [line for line in refused["recover"] if not line.startswith("  ")]
        self.assertTrue(commands)
        for command in commands:
            self.assertIn(
                relative, option_values(command),
                f"a dashed socket path did not survive the round trip: {command}",
            )
            # Running it is the assertion that matters: the parser must accept the value
            # rather than reading it as an option it does not have.
            replayed = subprocess.run(
                shlex.split(command), capture_output=True, text=True,
                env=environment, timeout=60, cwd=self.tmp,
            )
            self.assertNotIn(
                "expected one argument", replayed.stderr,
                f"the printed command is unusable: {command}",
            )
            self.assertTrue(
                replayed.stdout.strip().startswith("{"),
                f"the printed command produced no payload: {command}\n{replayed.stderr}",
            )
    def test_the_printed_command_names_the_interpreter_that_is_running(self):
        """These lines are pasted into a shell where python3 may be absent or different.

        The relay can be running under a virtualenv or a versioned interpreter. A bare
        python3 there reaches another installation, or nothing.
        """
        import shlex
        from pathlib import Path

        from codex_session_relay.store import Store

        state = os.path.join(self.tmp, "interpreter-state")
        recorded = os.path.join(self.tmp, "interpreter-recorded.sock")
        wanted = os.path.join(self.tmp, "interpreter-wanted.sock")
        Store(Path(state) / "relay.sqlite3", socket_path=recorded).close()

        environment = dict(os.environ, PYTHONPATH=os.path.join(REPO, "src"))
        environment.pop("CODEX_SESSION_RELAY_STATE", None)
        completed = subprocess.run(
            [sys.executable, "-m", "codex_session_relay.cli", "--state", state,
             "--socket", wanted, "status"],
            capture_output=True, text=True, env=environment, timeout=60,
        )
        self.assertEqual(completed.returncode, 2, completed.stdout + completed.stderr)
        refused = json.loads(completed.stdout)

        commands = [line for line in refused["recover"] if not line.startswith("  ")]
        self.assertTrue(commands)
        for command in commands:
            self.assertEqual(
                shlex.split(command)[0], sys.executable,
                f"this line names an interpreter that may not be the running one: {command}",
            )
        self.assertNotIn(
            "env -u", " ".join(commands),
            "no pin is set here, so there is nothing to drop and nothing to explain",
        )

    def test_an_explicit_state_directory_resolves_the_contest(self):
        home, root, socket = self.contested("contested-explicit")
        chosen = os.path.join(root, "aaaa444444444444")

        answer = self.run_in_home(home, "--state", chosen, "--socket", socket, "status")

        self.assertEqual(answer["deliveries"], [])


class ParticipantAccessReceipts(CliBase):
    """Parent, child and daemon on one database, each proving its own access to it.

    The criterion asks for two things a single healthy-looking report cannot give. Whether the
    participants share a store is a question about THREE observations, not one; and whether
    each sandbox permits what that participant needs is a question about what it can actually
    do, not about what its configuration says. So every participant emits a receipt and the
    receipts are compared.

    Isolation, because this touches the same machinery a real installation uses: a temporary
    HOME and CODEX_HOME, a socket bound here and closed here, an explicit temporary --state,
    and CODEX_SESSION_RELAY_STATE pinned to that same directory. The pin matters on its own -
    the adapter reads it, and setting only --state lets the store and the ledger diverge. The
    real state directory under the user's home is never selected by any of these runs.

    HOW THAT IS MEASURED, because the obvious way is wrong here. Snapshotting the real state
    directory before and after a suite run does NOT establish isolation on a host where
    anything else touches it: on 2026-09-17 that directory grew WAL and shared-memory
    sidecars across all three of its stores while this suite was not running at all, in a
    100-second control with nothing else started. Three bisections each blamed a different
    test file, because the external writes simply landed during whichever file was running.
    A before/after snapshot therefore fails for the wrong reason, the way a vacuous
    assertion passes for the wrong one. What this class relies on instead is per-run
    attribution: every participant here is given its own HOME and its own explicit --state,
    so the real directory is never selected, and that is a property of the arguments rather
    than of what the filesystem happened to do.

    WHAT THIS DOES NOT COVER. The participants here are three processes with three
    environments, not three genuinely different sandboxes: this host runs them all under the
    same kernel policy, so the receipts prove the store is shared and that each process really
    could read and write it, not that a restrictive sandbox would have been reported
    correctly. The recorded sandbox each receipt carries is the settings the adapter would
    send with, which is the value a denial would have to be explained against.

    The device and inode pair is decisive in one direction only. A DIFFERENT pair means a
    different file and that is conclusive; an agreeing pair is not sufficient for the same
    one. It is namespace-local, so participants in separate mount namespaces or on different
    hosts can hold one pair while sharing nothing, and one inode can be reached at more than
    one pathname - a hardlink name or a file bind mount - each of which carries its own
    write-ahead log. What settles a shared store is `store-challenge` with
    `doctor --expect-nonce` AND the peer's `--expect-inode`: a copy taken after the challenge
    carries the nonce, so the live half and the physical half are each necessary.
    `compare_store` (store.py) is where all of it is graded.
    """

    def probe_socket(self):
        """A socket that really accepts, so reachability is observed rather than assumed."""
        import socket

        path = os.path.join(self.tmp, "probe-app-server.sock")
        listener = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
        listener.bind(path)
        listener.listen(1)
        self.addCleanup(listener.close)
        return path

    def participant(self, *args, state=None, pin=None, cwd=None, expect=0):
        """Run one participant's own doctor, in its own environment."""
        home = os.path.join(self.tmp, "participant-home")
        os.makedirs(home, exist_ok=True)
        environment = dict(
            os.environ,
            PYTHONPATH=os.path.join(REPO, "src"),
            HOME=home,
            CODEX_HOME=os.path.join(self.tmp, "codex-home"),
        )
        environment.pop("XDG_STATE_HOME", None)
        if pin is None:
            environment.pop("CODEX_SESSION_RELAY_STATE", None)
        else:
            environment["CODEX_SESSION_RELAY_STATE"] = pin
        argv = [sys.executable, "-m", "codex_session_relay.cli"]
        if state is not None:
            argv.append(f"--state={state}")
        argv += list(args)
        completed = subprocess.run(
            argv, capture_output=True, text=True, env=environment, timeout=60,
            cwd=cwd or self.tmp,
        )
        self.assertEqual(
            completed.returncode, expect,
            f"exit {completed.returncode}: {completed.stdout}{completed.stderr}",
        )
        return json.loads(completed.stdout)

    def settings(self, cwd):
        """Shaped like the creation result the host reports, which is what gets recorded."""
        return {
            "sandbox": {"type": "workspaceWrite", "writableRoots": [cwd],
                        "networkAccess": False, "excludeTmpdirEnvVar": False,
                        "excludeSlashTmp": False},
            "approvalPolicy": "never",
            "cwd": cwd,
            "runtimeWorkspaceRoots": [cwd],
            "model": "anthropic/claude-opus-5",
            "reasoningEffort": "xhigh",
            "environments": [{"environmentId": "local", "cwd": cwd,
                              "runtimeWorkspaceRoots": [cwd]}],
        }

    def seeded(self):
        """A store with an assignment in it, and settings recorded for both participants.

        Recorded through register's own --parent-settings/--child-settings, which is the path
        a creation result really takes, so the sandbox in the receipt is the one a send would
        carry rather than something this test wrote by hand into the table.
        """
        parent_cwd = os.path.join(self.tmp, "parent")
        os.makedirs(parent_cwd, exist_ok=True)
        recorded = self.run_cli(
            "register", "--parent-task", PARENT, "--parent-host", HOST,
            "--child-task", CHILD, "--child-host", HOST, "--issue", ISSUE,
            "--artifact-root", self.root, "--allowed-recipient", PARENT,
            "--dispatch-request-id", "dispatch-1", "--dispatch-turn-id", DISPATCH_TURN,
            "--parent-settings", json.dumps(self.settings(parent_cwd)),
            "--child-settings", json.dumps(self.settings(self.root)),
        )
        self.assertEqual(
            recorded["authorizedSettings"], {PARENT: "recorded", CHILD: "recorded"},
        )
        return self.tmp

    def test_three_participants_reach_one_store_by_three_different_routes(self):
        state = self.seeded()
        socket_path = self.probe_socket()

        # Deliberately not the same route to the same place: if only one rule were exercised
        # this would prove nothing about participants that reach the store differently.
        parent = self.participant(
            "--socket", socket_path, "doctor", state=state, pin=state,
            cwd=os.path.join(self.tmp, "parent"),
        )["accessReceipt"]
        child = self.participant(
            "--socket", socket_path, "doctor", pin=state, cwd=self.root,
        )["accessReceipt"]
        daemon = self.participant(
            "--socket", socket_path, "doctor", state=state, pin=state,
        )["accessReceipt"]
        receipts = {"parent": parent, "child": child, "daemon": daemon}

        self.assertEqual(
            {name: r["selectedBy"]["source"] for name, r in receipts.items()},
            {"parent": "flag", "child": "env", "daemon": "flag"},
            "the routes collapsed, so this no longer tests what it claims to",
        )
        # The path is not the assertion. Two spellings can be one file and one spelling can be
        # two files, so identity is settled on the store id and the device/inode pair.
        for name, receipt in receipts.items():
            with self.subTest(participant=name):
                self.assertIsNotNone(receipt["storeId"], receipt)
                self.assertEqual(receipt["storeId"], parent["storeId"])
                self.assertEqual(
                    (receipt["device"], receipt["inode"]),
                    (parent["device"], parent["inode"]),
                    "this participant is on a different file",
                )
                # Measured, not inferred from a permission bit.
                self.assertTrue(receipt["observedAccess"]["read"], receipt)
                self.assertTrue(receipt["observedAccess"]["write"], receipt)
                self.assertIsNone(receipt["observedAccess"]["detail"], receipt)

    def test_each_receipt_carries_the_sandbox_a_denial_would_be_explained_against(self):
        state = self.seeded()
        receipt = self.participant("doctor", state=state, pin=state)["accessReceipt"]

        recorded = receipt["recordedSandbox"]
        self.assertTrue(recorded["available"], recorded)
        self.assertEqual(sorted(recorded["participants"]), sorted([PARENT, CHILD]))
        for task in (PARENT, CHILD):
            with self.subTest(task=task):
                sandbox = recorded["participants"][task]
                self.assertTrue(sandbox["readable"], sandbox)
                # The value the adapter would actually send with, not a policy file.
                self.assertEqual(sandbox["mode"], "workspaceWrite")
                self.assertIsInstance(sandbox["writableRoots"], list)
                self.assertIs(sandbox["networkAccess"], False)
                self.assertEqual(sandbox["recordedFrom"], "creation_result")
        self.assertEqual(
            recorded["participants"][CHILD]["cwd"], self.root,
            "the child's recorded cwd is not the workspace it actually runs in",
        )

    def test_a_policy_that_omits_its_defaults_still_reports_what_would_be_sent(self):
        """The receipt has to show the effective sandbox, not the recorded keystrokes.

        `{"type": "workspaceWrite"}` is accepted, and the adapter fills networkAccess false,
        empty writable roots and the two temporary-directory flags from the pinned defaults
        before sending. Reported raw, networkAccess reads as null for a participant whose
        sends really do carry false - which would have an operator diagnosing a denial against
        a value the host never sees.
        """
        settings = self.settings(self.root)
        settings["sandbox"] = {"type": "workspaceWrite"}
        self.run_cli(
            "register", "--parent-task", PARENT, "--parent-host", HOST,
            "--child-task", CHILD, "--child-host", HOST, "--issue", ISSUE,
            "--artifact-root", self.root, "--allowed-recipient", PARENT,
            "--dispatch-request-id", "dispatch-1", "--dispatch-turn-id", DISPATCH_TURN,
            "--parent-settings", json.dumps(settings),
            "--child-settings", json.dumps(settings),
        )

        receipt = self.participant("doctor", state=self.tmp, pin=self.tmp)["accessReceipt"]

        sandbox = receipt["recordedSandbox"]["participants"][PARENT]
        self.assertTrue(sandbox["readable"], sandbox)
        self.assertEqual(sandbox["mode"], "workspaceWrite")
        self.assertIs(sandbox["networkAccess"], False, "the default was reported as unknown")
        self.assertEqual(sandbox["writableRoots"], [])
        self.assertIs(sandbox["excludeTmpdirEnvVar"], False)
        self.assertIs(sandbox["excludeSlashTmp"], False)

    def test_a_record_delivery_cannot_carry_is_not_reported_as_one_it_would(self):
        """Readable is not deliverable, and this field is documented as the second one.

        The preparation a send performs stops in more than one place and each place stops on
        its own: a record missing any REQUIRED field is rejected before the string fields are
        typed, those are typed before the approval policy is read, the policy is read before the
        sandbox type is looked at, and the resume-params construction in `_guarded_send` fails
        after all of them. All five records below are readable and none of them can carry its
        settings to a host - the first four are refused before anything is claimed or sent, the
        fifth fails while the params are built, after `thread/read` and before `thread/resume`
        (bridge_adapter.py). Reporting any of them as the sandbox the adapter would carry tells
        an operator access is fine for a participant whose sends are never made.

        Three of them are here because three versions of this field each stopped one step short
        of the path: the sandbox type alone, then `require_usable()` alone. A suite missing the
        params-construction case passes while the field still lies. The approval policy is here
        for the opposite reason: this field once reported that row as deliverable and named the
        problem in a SEPARATE field, which was true of a send whose outcome depended on what the
        host did with the value, and is no longer true of one that is never made.

        The first four are written past the validating recorder deliberately: registration
        refuses them, so the only way a store holds one is an older writer or a hand edit,
        which is the case this helper says it supports. The fifth needs no hand edit at all -
        `record_settings` validates with `require_usable()` (registry.py) and that accepts it,
        so this row can arrive through the ordinary recorder and still fail every send.
        """
        from pathlib import Path

        from codex_session_relay.store import Store

        self.seeded()
        unsupported = dict(self.settings(self.root), sandbox={"type": "externalSandbox"})
        # A supported sandbox, but the record around it is incomplete. This is the gate
        # require_usable() reaches FIRST, and the one a sandbox-only check walks past.
        incomplete = dict(self.settings(self.root))
        del incomplete["cwd"]
        # Complete, supported, and still not sendable: require_usable() types the three fields
        # the resume contract declares as strings and says nothing about this one, while
        # resume_params calls list() on it.
        unusable_roots = dict(self.settings(self.root), runtimeWorkspaceRoots=7)
        # Complete and supported too, and refused one gate earlier than that: present is not
        # the same as usable, and no host answer could tell us what it did with cwd: 7.
        mistyped = dict(self.settings(self.root), cwd=7)
        # Complete, well-typed, and refused for what a value MEANS rather than what it is: this
        # transport cannot service an interactive approval, so the row cannot be carried as
        # recorded whatever a host would have answered about it.
        interactive = dict(self.settings(self.root), approvalPolicy="on-request")

        cases = {
            "an unsupported sandbox type": (
                unsupported, "unsupported_sandbox_type", "externalSandbox",
            ),
            "a record missing a required field": (incomplete, "settings_incomplete", "cwd"),
            "a field recorded with a type the contract does not declare": (
                mistyped, "settings_mistyped", "cwd is int, not str",
            ),
            "an approval policy this transport cannot carry": (
                interactive, "unsupported_approval_policy", "'on-request'",
            ),
            "a field the params construction cannot use": (
                unusable_roots, "unexpected", "TypeError",
            ),
        }
        for label, (stale, expected_reason, detail_says) in cases.items():
            with self.subTest(refusal=label):
                store = Store(Path(self.tmp) / "relay.sqlite3")
                with store.transaction() as db:
                    db.execute(
                        "UPDATE authorized_settings SET settings = ? WHERE task_id = ?",
                        (json.dumps(stale), CHILD),
                    )
                store.db.commit()
                store.close()

                receipt = self.participant(
                    "doctor", state=self.tmp, pin=self.tmp,
                )["accessReceipt"]
                child = receipt["recordedSandbox"]["participants"][CHILD]
                parent = receipt["recordedSandbox"]["participants"][PARENT]

                # The defect stated as what it is: a participant delivery refuses outright
                # was reported exactly like one it can serve, with nothing in the row to
                # tell them apart. Asserted first so the failure says that, not KeyError.
                signal = ("deliverable", "refusedBy", "resumeMode")
                self.assertNotEqual(
                    {key: child.get(key) for key in signal},
                    {key: parent.get(key) for key in signal},
                    "a refused record is reported exactly like one a send can carry",
                )
                self.assertFalse(child["deliverable"], child)
                # Delivery's own vocabulary, so a receipt and a delivery journal agree.
                self.assertEqual(child["refusedBy"], expected_reason, child)
                self.assertIsNone(child["resumeMode"], "a refused record sends no sandbox")
                self.assertTrue(child["detail"], child)
                # The refusal that actually happened, not a generic one: an operator reading
                # this has to be able to tell these three apart.
                self.assertIn(detail_says, child["detail"], child)
                # The row stays readable and what it records is still shown: dropping it
                # would lose the only clue to why delivery refuses this participant.
                self.assertTrue(child["readable"], child)
                self.assertEqual(child["mode"], stale["sandbox"]["type"], child)

                # And the participant delivery can serve is still reported as one it can.
                self.assertTrue(parent["deliverable"], parent)
                self.assertIsNone(parent["refusedBy"], parent)
                self.assertEqual(parent["resumeMode"], "workspace-write")
                self.assertIsNone(parent["detail"])

    def test_every_transformation_a_send_applies_to_the_record_is_covered(self):
        """The SET, read out of the source, rather than the instances found so far.

        Three versions of `deliverable` were wrong the same way: a predicate was applied to
        the member that had been demonstrated instead of to the set that member belongs to.
        First the sandbox type, then `require_usable()`, then the params construction - and
        the fourth instance, `environments`, arrived the same way the first three did.

        So the set is derived here instead of listed. It is the constraints delivery imposes
        on the RECORDED settings before turn/start, and it has two kinds of member, both read
        out of the source. Both start from the `TaskSettings` methods the send path calls,
        taken from `delivery.py` and `bridge_adapter.py`.

        A TRANSFORMATION can fail on the row by raising: every recorded field handed to a
        call inside those methods. Today `normalise_policy(sandbox)`, the `isinstance` checks
        `require_usable` applies to `cwd`, `model` and `reasoningEffort`,
        `list(runtimeWorkspaceRoots)`, `normalise_environments(environments)`.

        A VALUE CONSTRAINT cannot. It exists only as a comparison against a fixed value -
        `mismatches` refuses any returned `approvalPolicy` that is not the authorized one -
        and nothing raises on it, which is why the transformation extraction cannot see it:
        there is no call to put the field into. That member was found by review rather than by
        this test, and the extractions below are the answer to that rather than another
        hand-added case.

        A constraint has TWO ends and the rule is only real at both. `value_constraints` reads
        what the verification refuses in the RESPONSE; `recorded_constraints` reads what the
        validator refuses in the RECORD; and they are asserted EQUAL. That equality is the
        property, not the floors: a constraint written only against the response leaves the send
        to whatever the host does with the value, which is the defect this pair exists to catch,
        and one written only against the record refuses a row the verification would have
        accepted. Either asymmetry fails here.

        Each derived field is then mutated with the mutant its kind needs - a value no
        transformation can consume, or a well-typed value that is not the authorized literal -
        and BOTH kinds must make `deliverable` false, because both are now decided on the row
        before any host is asked. The constraint probe additionally requires the refusal to name
        the field and the value: 'not deliverable' alone would be satisfied by an unrelated gate
        rejecting the mutant, and would then pass for a constraint nobody enforces. A new member
        of either kind joins the derived set and fails here until the probe reaches it, which is
        the property a written-down list cannot have.

        The derivation itself is `transformed_fields`, `value_constraints` and
        `recorded_constraints` at module scope, where each shape each one follows is proved
        against source written for the purpose (`FieldExtractionShapes`). What they do NOT
        follow is recorded in their docstrings, and the short version is: names bound anywhere
        other than one plain assignment; container literals passed as arguments; non-constant
        keys; sites other than call arguments and receivers; callables outside the two supported
        call forms; and anything inside a nested definition's body. Passing this is not proof of
        total coverage.

        `mutation_probes` keys the probes by KIND as well as by field. Keyed by field alone,
        a constraint on a field that is also transformed would overwrite that transformation's
        mutant, inherit its branch, be refused for the transformation's own reason, and pass -
        with the probe count unmoved.

        The floor assertions are not the definition either. They guard the extractors: an AST
        walk that silently matched nothing would run zero mutations and pass, which is how
        this kind of test goes green while holding nothing.
        """
        import inspect
        from pathlib import Path

        from codex_session_relay import bridge_adapter, delivery
        from codex_session_relay import settings as settings_module
        from codex_session_relay.settings import TaskSettings
        from codex_session_relay.store import Store

        api = {name for name in vars(TaskSettings) if not name.startswith("_")}
        called = set()
        for module in (delivery, bridge_adapter):
            for node in ast.walk(ast.parse(inspect.getsource(module))):
                if (isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute)
                        and node.func.attr in api):
                    called.add(node.func.attr)

        recorded = ast.parse(inspect.getsource(settings_module))
        fields = transformed_fields(recorded, called)
        constraints = value_constraints(recorded, called)
        enforced = recorded_constraints(recorded, called)

        self.assertGreaterEqual(
            called, {"require_usable", "resume_params", "mismatches"},
            "the send path's settings calls were not found, so nothing below is derived",
        )
        self.assertGreaterEqual(
            fields,
            {"sandbox", "cwd", "model", "reasoningEffort", "runtimeWorkspaceRoots",
             "environments"},
            "the extraction found fewer transformations than are known to be there",
        )
        self.assertGreaterEqual(
            constraints, {("approvalPolicy", "never")},
            "the value-constraint extraction found less than is known to be there",
        )
        self.assertGreaterEqual(
            enforced, {("approvalPolicy", "never")},
            "the recorded-constraint extraction found less than is known to be there",
        )
        self.assertEqual(
            enforced, constraints,
            "a value the verification refuses in the response and the validator does not refuse"
            " in the record leaves the send to whatever the host does with it, and a value"
            " refused only in the record refuses a row the verification would have accepted;"
            " one rule, both ends",
        )

        probes = mutation_probes(fields, constraints)
        self.assertEqual(
            {(kind, field) for kind, field, _mutant in probes},
            {("transformation", field) for field in fields}
            | {("constraint", field) for field, _literal in constraints},
            "every derived member has to be probed as its own kind; merging the two kinds by"
            " field name drops one of them and asserts the wrong receipt behaviour for the"
            " one that survives",
        )

        self.seeded()
        for kind, field, mutant in probes:
            with self.subTest(constrains=field, kind=kind):
                stale = dict(self.settings(self.root))
                stale[field] = mutant
                store = Store(Path(self.tmp) / "relay.sqlite3")
                with store.transaction() as db:
                    db.execute(
                        "UPDATE authorized_settings SET settings = ? WHERE task_id = ?",
                        (json.dumps(stale), CHILD),
                    )
                store.db.commit()
                store.close()

                child = self.participant(
                    "doctor", state=self.tmp, pin=self.tmp,
                )["accessReceipt"]["recordedSandbox"]["participants"][CHILD]

                if kind == "transformation":
                    # A transformation cannot consume it, so no host can either: the row
                    # itself settles the send.
                    self.assertIsNot(
                        child.get("deliverable"), True,
                        f"no send can carry this {field!r}, and the receipt advertised one",
                    )
                    if child["readable"]:
                        self.assertTrue(child["refusedBy"], child)
                        self.assertTrue(child["detail"], child)
                else:
                    # The record settles this one too, so no host is asked and the receipt has
                    # to deny the send rather than describe a preparation.
                    self.assertIs(
                        child.get("deliverable"), False,
                        f"the receipt advertised a send for {field!r} that delivery withholds"
                        " before any transport call",
                    )
                    self.assertTrue(child["refusedBy"], child)
                    # Refused FOR THIS VALUE, not by some other gate that happens to reject the
                    # mutant. Without this a constraint nobody enforces would pass here on the
                    # strength of an unrelated refusal, which is the whole failure this test
                    # exists to make impossible.
                    self.assertIn(field, child["detail"], child)
                    self.assertIn(repr(mutant), child["detail"], child)
                    self.assertNotIn(
                        "refusedIfPreserved", child,
                        "the receipt still reports the field that described a send made against"
                        " a host that replaces the value",
                    )

    def test_a_store_replaced_by_a_copy_under_the_read_is_reported_not_served(self):
        """The receipt's own mid-command replacement check, against the case it missed.

        The identity and the participants come out of one read so that an atomic replacement
        between two opens cannot pair one store's identity with another store's rows. That
        check compared store ids, and a COPY carries the store id: the row is minted once and
        copied with the bytes, which `test_a_copy_keeps_the_identifier_and_is_not_the_same_store`
        (test_store.py) already states. So the comparison that exists to catch a replacement
        was satisfied by a replacement made with a copy.

        The replacement here happens between the probe and the read for real - the report
        passed in is the one measured before it - which is the window the check exists for.
        """
        from types import SimpleNamespace

        from codex_session_relay.cli import Services, _access_receipt
        from codex_session_relay.store import probe, resolve_state_dir

        state = self.seeded()
        selection = resolve_state_dir(state, None)
        measured = probe(selection)
        self.assertTrue(measured["store"]["storeId"], measured)

        database = os.path.join(state, "relay.sqlite3")
        replacement = os.path.join(state, "replacement.sqlite3")
        shutil.copy(database, replacement)
        os.replace(replacement, database)

        # Asserted, not assumed: a copy that had lost the identity row, or one that landed on
        # the same inode, would make the receipt below right for a reason this is not testing.
        swapped = probe(selection)["store"]
        self.assertEqual(
            swapped["storeId"], measured["store"]["storeId"],
            "the replacement is not a real copy, so nothing here is about a copy",
        )
        self.assertNotEqual(swapped["inode"], measured["store"]["inode"])

        receipt = _access_receipt(Services(SimpleNamespace(state=state, socket=None)), measured)

        recorded = receipt["recordedSandbox"]
        self.assertFalse(
            recorded["available"],
            "rows read from a file that replaced the measured one were served as its own",
        )
        self.assertIn("inode", recorded["detail"], recorded)

    def test_one_unreadable_participant_does_not_take_the_diagnosis_with_it(self):
        """A damaged row is exactly when the rest of the report is worth most.

        These rows can hold whatever an older writer or a hand edit left, and a shape the
        parser accepts is not a shape the reader can use: json.loads returns a list for `[]`
        quite happily. Raising there would cost the store identity and the access evidence too,
        leaving a generic host error where the diagnosis should be.
        """
        from pathlib import Path

        from codex_session_relay.store import Store

        self.seeded()
        store = Store(Path(self.tmp) / "relay.sqlite3")
        self.addCleanup(store.close)
        with store.transaction() as db:
            db.execute(
                "UPDATE authorized_settings SET settings = ? WHERE task_id = ?",
                ("[]", CHILD),
            )
        store.db.commit()

        receipt = self.participant("doctor", state=self.tmp, pin=self.tmp)["accessReceipt"]

        participants = receipt["recordedSandbox"]["participants"]
        self.assertFalse(participants[CHILD]["readable"], participants[CHILD])
        self.assertIn("not an object", participants[CHILD]["detail"])
        # The damage is confined to the row that carries it.
        self.assertTrue(participants[PARENT]["readable"], participants[PARENT])
        self.assertIsNotNone(receipt["storeId"])
        self.assertTrue(receipt["observedAccess"]["read"])
    def test_a_store_swapped_mid_command_is_reported_rather_than_paired(self):
        """Identity and participants have to come from the same file, or say they did not.

        Collected by two opens, an atomic replacement between them pairs one store's identity
        with another store's participants, and nothing in the receipt would show it - a
        mismatch invisible in exactly the comparison this exists to support. One statement
        carries both now, and its store id is checked against the one the probe measured.

        Driven at the function rather than through the CLI: the window is one command against
        a database being swapped underneath it, which cannot be opened from outside the
        process. The probe result is real; only the identity it reports is moved, which is
        what a replacement between the two reads would have produced.
        """
        from codex_session_relay.cli import _access_receipt
        from codex_session_relay.store import probe, resolve_state_dir

        self.seeded()
        selection = resolve_state_dir(self.tmp)
        report = probe(selection)
        self.assertTrue(report["access"]["dbReadable"], report)

        class Services:
            pass

        services = Services()
        services.selection = selection

        honest = _access_receipt(services, report)
        self.assertTrue(honest["recordedSandbox"]["available"], honest)
        self.assertIn(PARENT, honest["recordedSandbox"]["participants"])

        # Now the identity names a file the settings did not come from.
        moved = dict(report, store=dict(report["store"], storeId="another-store-entirely"))
        receipt = _access_receipt(services, moved)

        recorded = receipt["recordedSandbox"]
        self.assertFalse(recorded["available"], recorded)
        self.assertEqual(recorded["participants"], {}, "mismatched participants were reported")
        self.assertIn("changed under this command", recorded["detail"])
        self.assertIn("another-store-entirely", recorded["detail"])

    def test_a_participant_on_another_store_is_refused_rather_than_called_healthy(self):
        """The failure this criterion is really about: agreeing while looking at two stores."""
        state = self.seeded()
        mine = self.participant("doctor", state=state, pin=state)["accessReceipt"]

        elsewhere = os.path.join(self.tmp, "elsewhere")
        # A real second database, not an empty directory. doctor constructs no Store, so
        # pointing it at a path that does not exist yet returns null identity and null
        # device/inode - which makes every inequality below pass for the wrong reason and
        # turns the refusal into a test of an ABSENT store rather than a different one.
        # store-identity opens one, which is what gives this test two stores to tell apart.
        created = self.participant("store-identity", state=elsewhere, pin=elsewhere)
        self.assertIsNotNone(created["store"]["storeId"], created)
        stray = self.participant("doctor", state=elsewhere, pin=elsewhere)["accessReceipt"]

        for name, receipt in (("mine", mine), ("stray", stray)):
            with self.subTest(receipt=name):
                self.assertIsNotNone(receipt["storeId"], receipt)
                self.assertIsNotNone(receipt["inode"], receipt)
        self.assertNotEqual(stray["storeId"], mine["storeId"])
        self.assertNotEqual(
            (stray["device"], stray["inode"]), (mine["device"], mine["inode"]),
            "the two runs landed on one file, so this proves nothing",
        )
        # And asked to prove it is the same store, a participant sitting on the other one
        # refuses instead of reporting health - which is the failure the criterion names.
        refused = self.participant(
            "doctor", f"--expect-store={mine['storeId']}",
            state=elsewhere, pin=elsewhere, expect=2,
        )
        self.assertNotEqual(refused["sameStore"], "proven", refused)
        self.assertEqual(refused["accessReceipt"]["storeId"], stray["storeId"], refused)


class RestorationFlagValidatesItsInput(DeliveryTestCase):
    """--criteria accepts arbitrary JSON, and --restoration reads it before anything checks it.

    This command's contract is a named refusal and an exit code, so a malformed array must
    reach the refusal that words it rather than raising out of a comprehension on the way.
    """

    def _verdict(self, criteria, restoration):
        from types import SimpleNamespace

        from codex_session_relay import cli

        return cli.cmd_verdict(
            SimpleNamespace(ack=self.ack),
            SimpleNamespace(
                event="e" * 32, verdict="needs_changes", verdict_turn="v1",
                criterion=None, finding=None, criteria=json.dumps(criteria),
                restoration=restoration, reason=None, expect_criteria_digest=None,
            ),
        )

    def test_a_null_entry_does_not_raise_out_of_the_restoration_marking(self):
        from codex_session_relay.errors import RelayError

        with self.assertRaises(RelayError) as caught:
            self._verdict([None], "c1")
        self.assertEqual(caught.exception.reason.value, "disposition_conflict")

    def test_a_string_entry_does_not_raise_out_of_the_restoration_marking(self):
        from codex_session_relay.errors import RelayError

        with self.assertRaises(RelayError) as caught:
            self._verdict(["c1"], "c1")
        self.assertEqual(caught.exception.reason.value, "disposition_conflict")


FAKE_GH = '''#!/usr/bin/env python3
import json, sys
TRANSCRIPT = json.load(open(__file__ + ".json"))
argv = sys.argv[1:]
if argv[:2] == ["api", "graphql"]:
    document = [one for one in argv if one.startswith("query=")][0]
    for key in ("reviewThreads", "reviews(", "comments("):
        if key in document:
            name = {"reviewThreads": "reviewThreads", "reviews(": "reviews",
                    "comments(": "comments"}[key]
            print(json.dumps({"data": {"repository": {"pullRequest": {
                name: TRANSCRIPT[name]}}}}))
            sys.exit(0)
target = argv[-1].split("?")[0]
for fragment, payload in TRANSCRIPT["rest"]:
    if target.endswith(fragment):
        print(json.dumps(payload))
        sys.exit(0)
sys.stderr.write("gh: Not Found (HTTP 404)\\n")
sys.exit(1)
'''


class TheCommandRunsAsACommand(unittest.TestCase):
    """A subprocess, with a real gh on PATH, because that seam is where argv actually lands.

    Everything above drives the collector in process. None of it would notice an argument array
    the shell mangles, a payload that is not JSON on stdout, or an exit code that says yes.
    """

    def setUp(self):
        self.tmp = tempfile.mkdtemp(prefix="forge-cli-")
        self.addCleanup(shutil.rmtree, self.tmp, ignore_errors=True)

    def install(self, transcript):
        path = os.path.join(self.tmp, "gh")
        with open(path, "w", encoding="utf-8") as handle:
            handle.write(FAKE_GH)
        os.chmod(path, os.stat(path).st_mode | stat.S_IEXEC | stat.S_IXGRP | stat.S_IXOTH)
        with open(path + ".json", "w", encoding="utf-8") as handle:
            json.dump(transcript, handle)
        return path

    def run_cli(self, transcript, *extra, expect=0):
        self.install(transcript)
        environment = dict(
            os.environ,
            PATH=self.tmp + os.pathsep + os.environ.get("PATH", ""),
            PYTHONPATH=os.path.join(REPO, "src"),
        )
        completed = subprocess.run(
            [sys.executable, "-m", "codex_session_relay.cli", "merge-evidence",
             "--repository", "owner/name", "--pull-request", "7", *extra],
            capture_output=True, text=True, env=environment, timeout=120,
        )
        self.assertEqual(completed.returncode, expect,
                         completed.stdout + completed.stderr)
        return json.loads(completed.stdout)

    @staticmethod
    def transcript(*, thread_nodes, jobs):
        def connection(nodes):
            return {"totalCount": len(nodes),
                    "pageInfo": {"hasNextPage": False, "endCursor": None}, "nodes": nodes}

        return {
            "reviewThreads": connection(thread_nodes),
            "reviews": connection([]),
            "comments": connection([]),
            "rest": [
                ["/pulls/7", pull()],
                ["/git/ref/heads/dev", {"ref": "refs/heads/dev"}],
                ["/rules/branches/dev", REQUIRED_DEV_GATE],
                ["/actions/runs/1/jobs", {"total_count": len(jobs), "jobs": jobs}],
                ["/actions/runs", {"total_count": 1, "workflow_runs": [
                    {"id": 1, "name": "CI", "head_sha": HEAD, "html_url": "https://forge/run/1"}]}],
                ["/check-runs", {"total_count": 0, "check_runs": []}],
                ["/status", {"total_count": 0, "statuses": []}],
            ],
        }

    def test_a_ready_candidate_exits_zero_with_the_record_on_stdout(self):
        payload = self.run_cli(
            self.transcript(thread_nodes=threads(3), jobs=[job("dev-gate")]))
        self.assertEqual(payload["verdict"], forge.READY)
        self.assertEqual(payload["handoff"]["requiredDeclared"], ["dev-gate"])
        self.assertEqual(payload["handoff"]["reviewCoverage"]["totalCount"], 3)
        self.assertEqual(payload["handoff"]["threadDispositions"], [])

    def test_an_unresolved_thread_exits_two_and_still_prints_everything(self):
        payload = self.run_cli(
            self.transcript(thread_nodes=threads(3, unresolved=(2,)), jobs=[job("dev-gate")]),
            expect=2)
        self.assertEqual(payload["verdict"], forge.NOT_READY)
        self.assertEqual(payload["handoff"]["reviewCoverage"]["unresolved"], 1)
        self.assertTrue(payload["findings"])

    def test_an_unknown_answer_exits_two_rather_than_zero(self):
        # The exit code is the whole point: a shell reading only $? must not take "I could not
        # tell" for "yes", which is the truncation failure in another costume.
        transcript = self.transcript(thread_nodes=threads(1), jobs=[job("dev-gate")])
        transcript["rest"] = [entry for entry in transcript["rest"]
                              if entry[0] != "/rules/branches/dev"]
        payload = self.run_cli(transcript, expect=2)
        self.assertEqual(payload["verdict"], forge.UNKNOWN)

    def test_a_malformed_repository_is_a_usage_error(self):
        # Attached with '=' on purpose. Given a space, argparse takes the dash for another
        # option and refuses before this command sees anything; attached, the value reaches
        # argv exactly as a caller could pass it, which is the case the validator exists for.
        self.install(self.transcript(thread_nodes=[], jobs=[]))
        environment = dict(os.environ, PATH=self.tmp + os.pathsep + os.environ.get("PATH", ""),
                           PYTHONPATH=os.path.join(REPO, "src"))
        completed = subprocess.run(
            [sys.executable, "-m", "codex_session_relay.cli", "merge-evidence",
             "--repository=--not-a-repository", "--pull-request", "7"],
            capture_output=True, text=True, env=environment, timeout=120,
        )
        self.assertEqual(completed.returncode, 4, completed.stdout + completed.stderr)
        self.assertIn("usage", json.loads(completed.stdout)["error"])

    def test_restating_a_record_against_a_fresh_reading_catches_the_late_thread(self):
        transcript = self.transcript(thread_nodes=threads(2), jobs=[job("dev-gate")])
        record = os.path.join(self.tmp, "record.json")
        with open(record, "w", encoding="utf-8") as handle:
            json.dump({"headSha": HEAD, "handoff": {"reviewCoverage": {
                "hasNextPage": False, "pagesRead": 1, "totalCount": 1,
                "threadsSeen": ["T1"], "unresolved": 0}}}, handle)
        payload = self.run_cli(transcript, "--restate", record, expect=2)
        self.assertFalse(payload["restatement"]["current"])
        self.assertIn(forge.LATE_FINDING,
                      [one["code"] for one in payload["restatement"]["problems"]])


class FieldExtractionShapes(unittest.TestCase):
    """The derivation, run against source written to escape the one it replaced.

    The coverage test above cannot prove this. Its subject is settings.py, which contains one
    spelling of each shape, so an extraction that had quietly stopped following aliases would
    still find every member it is asked for. This module is written for the purpose: one shape
    per branch the derivation implements, plus one trap per false positive it must not produce.

    The expectations below are exact sets. A widening that stops working drops a member; a rule
    that loosens adds one. Both fail here. That is deliberate and it is not the hand-written list
    the coverage test refuses to keep: these are the fixture's own names, not the recorded
    settings fields, and the point of writing them down is that the fixture is the specimen.

    What is NOT proved here, because the derivation does not claim it: anything inside a nested
    definition's BODY (a nested function's is deferred; a nested class body executes immediately
    but would need its own binding scope), a class-qualified call, an async definition or awaited
    call, a staticmethod or classmethod, any callable reached through an attribute chain other
    than self.<name>, a field read with a non-constant key, and a field buried in a container
    literal. Those are excluded shapes, not missed ones.
    """

    FIXTURE = '''\
AUTHORIZED = "never"


def carry(value):
    return value


def report(payload):
    return payload


def through(record):
    carry(record["reachedPositionally"])


def by_name(record):
    carry(record["reachedByName"])


def collide(record):
    carry(record["reachedAsModuleFunction"])


def positional_only(record, /):
    carry(record["reachedPositionalOnly"])


def keyword_only(*, record):
    carry(record["reachedKeywordOnly"])


def positional_only_by_name(record, /):
    carry(record["wronglyBoundPosonlyKeyword"])


def vararg_capture(*rest):
    carry(rest["wronglyBoundRest"])


def kwarg_capture(**extra):
    carry(extra["wronglyBoundKwargs"])


def duplicate(record):
    carry(record["wronglyBoundDuplicate"])


class Other:
    def reach(self, record):
        carry(record["wrongClass"])

    def send(self, response):
        return None


def varargs_signature(record, *rest):
    carry(record["reachedPastVarargs"])


def kwargs_signature(record, **extra):
    carry(record["reachedPastKwargs"])


def splatted(record):
    carry(record["unreachedBySplat"])


def kw_splatted(record):
    carry(record["unreachedByKwSplat"])


def after_star(first, second):
    carry(second["unreachedAfterStar"])


def twice(record):
    carry(record["memoTransformation"])
    if record.get("memoConstraint") != AUTHORIZED:
        return None


def loops(record):
    loops(record)
    carry(record["reachedThroughRecursion"])


def ping(record):
    pong(record)


def pong(record):
    ping(record)
    carry(record["reachedThroughMutualRecursion"])


def given_a_field(policy):
    kind = policy.get("type")
    if kind == "workspaceWrite":
        return kind
    return carry(policy)


def checked(payload):
    if payload.get("viaHelper") != AUTHORIZED:
        return None
    return payload


class TaskSettings:
    def __init__(self, data):
        self.data = data

    def collide(self):
        carry(self.data["reachedAsMethod"])

    def reach_by_method(self):
        carry(self.data["reachedByMethod"])

    def reach_with(self, record):
        carry(record["reachedByMethodParameter"])

    def reach(self, record):
        carry(record["rightClass"])

    def reach_past_posonly_self(self, /, record):
        carry(record["reachedPastPosonlySelf"])

    def duplicate_method(self, record):
        carry(self.data["wronglyReachedDuplicateBody"])

    def send(self, response):
        row = self.data
        carry(self.data["silentlyMissedEntryField"])
        carry(row["aliased"])
        value = self.data["bound"]
        carry(value)
        defaulted = self.data["ordefault"] or {}
        carry(defaulted)
        carry(self.data.get("viaGet"))
        self.data["viaReceiver"].strip()
        carry(keyword=self.data["keyword"])
        carry(*self.data["starred"])
        carry(**self.data["splat"])
        carry(self.data["dual"])

        through(self.data)
        by_name(record=self.data)
        positional_only(self.data)
        keyword_only(record=self.data)
        positional_only_by_name(record=self.data)
        vararg_capture(self.data)
        kwarg_capture(extra=self.data)
        duplicate(response, record=self.data)
        self.duplicate_method(response, record=self.data)
        self.reach(self.data)
        self.reach_past_posonly_self(record=self.data)
        varargs_signature(self.data)
        kwargs_signature(self.data)
        collide(self.data)
        self.collide()
        self.reach_by_method()
        self.reach_with(self.data)
        twice(response)
        twice(self.data)
        loops(self.data)
        ping(self.data)
        given_a_field(self.data["typed"])
        checked(response)

        splatted(*self.data["packed"])
        kw_splatted(**self.data["packedKw"])
        after_star(*response, self.data)

        report({"buried": self.data["buried"]})

        rebound = self.data["shadowedByRebinding"]
        rebound = response
        carry(rebound)
        accumulated = self.data["shadowedByAugmenting"]
        accumulated += response
        carry(accumulated)
        unpacked = self.data["shadowedByUnpacking"]
        unpacked, ignored = response, response
        carry(unpacked)
        looped = self.data["shadowedByFor"]
        for looped in ("a", "b"):
            carry(looped)
        comprehended = self.data["shadowedByComprehension"]
        [carry(comprehended) for comprehended in response]
        managed = self.data["shadowedByWith"]
        with report(response) as managed:
            carry(managed)
        caught = self.data["shadowedByExcept"]
        try:
            report(response)
        except ValueError as caught:
            carry(caught)
        walrus = self.data["shadowedByWalrus"]
        carry(walrus := response)
        carry(walrus)
        startail = self.data["shadowedByStarredUnpacking"]
        starhead, *startail = response
        carry(startail)
        annotated = self.data["shadowedByAnnotation"]
        annotated: object = response
        carry(annotated)
        other = self.data["shadowedByChainedAssignment"]
        chained = other = response
        carry(other)
        outer = self.data

        def dormant(value=carry(self.data["missedNestedDefault"])):
            outer = response
            carry(self.data["wronglyNestedBody"])

        def annotated_dormant(
            value: carry(self.data["missedParameterAnnotation"]),
        ) -> carry(self.data["missedReturnAnnotation"]):
            return value

        carry(outer["reachedDespiteNestedBinding"])
        for key in ("a", "b"):
            if key == "notAField":
                carry(key)

        bound = response.get("chained")
        same = bound
        if "never" == same:
            return None
        echo = response
        if echo.get("aliasedResponse") != AUTHORIZED:
            return None
        if response["subscripted"] != AUTHORIZED:
            return None
        spin = spin or {}
        carry(spin)
        if response.get("inline") != "never":
            return None
        if response.get("isShape") is AUTHORIZED:
            return None
        if response.get("isNotShape") is not AUTHORIZED:
            return None
        if response.get("dual") != AUTHORIZED:
            return None
        if AUTHORIZED == response.get("chainedCompare") == "other":
            return None
        if response.get("orderedCompare") < "never":
            return None
        nested = response.get("thread") or {}
        if nested.get("sub") != AUTHORIZED:
            return None
        if self.data["recordedSubscripted"] != AUTHORIZED:
            return None
        if self.data.get("recordedViaGet") != "never":
            return None
        if AUTHORIZED == self.data.get("recordedChained") == "other":
            return None
        if self.data.get("recordedOrdered") < "never":
            return None
        if response.get("recorded") != self.data.get("expectedProfile"):
            return None
        return None
'''

    def derived(self):
        tree = ast.parse(self.FIXTURE)
        return (transformed_fields(tree, ["send"]), value_constraints(tree, ["send"]),
                recorded_constraints(tree, ["send"]))

    def test_every_shape_the_transformation_walk_follows_is_followed(self):
        """One name per branch, and nothing the walk must refuse.

        Each name says which shape put it here. The reached* names come through a helper hop of
        some kind; viaGet and viaReceiver are the two access spellings beside the subscript;
        keyword, starred and splat are the three argument forms. The names the set must NOT hold
        say the same thing in reverse: shadowed* is a recorded field whose name was then rebound
        through a construct the walk has to treat as OTHER, wrongly* is a binding Python itself
        would refuse, unreached* is a parameter the walk must not pretend to know, and buried is
        a field inside a container literal handed to a call.
        """
        fields, _constraints, _enforced = self.derived()
        self.assertEqual(
            fields,
{
            "aliased",
            "bound",
            "dual",
            "keyword",
            "memoTransformation",
            "missedNestedDefault",
            "missedParameterAnnotation",
            "missedReturnAnnotation",
            "ordefault",
            "packed",
            "packedKw",
            "reachedAsMethod",
            "reachedAsModuleFunction",
            "reachedByMethod",
            "reachedByMethodParameter",
            "reachedByName",
            "reachedDespiteNestedBinding",
            "reachedKeywordOnly",
            "reachedPastKwargs",
            "reachedPastPosonlySelf",
            "reachedPastVarargs",
            "reachedPositionalOnly",
            "reachedPositionally",
            "reachedThroughMutualRecursion",
            "reachedThroughRecursion",
            "rightClass",
            "silentlyMissedEntryField",
            "splat",
            "starred",
            "typed",
            "viaGet",
            "viaReceiver",
        },
            "a shape the derivation is supposed to follow stopped being followed, or a rule"
            " loosened and invented a field; the difference names which",
        )

    def test_the_constraint_walk_follows_its_shapes_and_nothing_else(self):
        """Comparisons against a fixed value, and the four that only look like one.

        Out: a sub-object of the response, a sub-key read off a field-typed helper parameter, a
        comparator that is itself a record read, and a chained comparison.
        """
        _fields, constraints, _enforced = self.derived()
        self.assertEqual(
            constraints,
{
            ("aliasedResponse", "never"),
            ("chained", "never"),
            ("dual", "never"),
            ("inline", "never"),
            ("isNotShape", "never"),
            ("isShape", "never"),
            ("memoConstraint", "never"),
            ("subscripted", "never"),
            ("viaHelper", "never"),
        },
            "the value-constraint walk changed shape, or a recorded-side name leaked into the"
            " response side",
        )

    def test_the_recorded_constraint_walk_reads_the_record_not_the_response(self):
        """The discriminator, verified where nothing else can verify it.

        `recorded_constraints` differs from `value_constraints` by one word, and every other
        check on it is satisfied by an implementation that got that word wrong. Equality with
        the response set holds trivially for an extractor that IS the response extractor; the
        floor holds; and the behavioural probe holds because the row really is refused. So the
        only place the `field` half of it can be proved is here, against a fixture whose two
        sides are deliberately named apart.

        `memoConstraint` is in BOTH sets and belongs in both: the fixture hands one helper the
        response at one call and the record at another, which is the memoisation shape, so the
        same comparison inside it is a constraint on each. Its presence proves this walk follows
        the helper hop rather than reading only the entry body. The other two names appear on
        the record side alone, so an extractor that matched the response would return nine names
        instead of three and fail.

        Out: a chained comparison, which is not one operator; an ordering comparison, which is
        not an equality; and a record read compared against a RESPONSE read rather than against
        a fixed value, which is a comparison of two unknowns and constrains nothing.
        """
        _fields, constraints, enforced = self.derived()
        self.assertEqual(
            enforced,
            {("memoConstraint", "never"),
             ("recordedSubscripted", "never"),
             ("recordedViaGet", "never")},
            "the recorded-constraint walk changed shape, or it is reading the response",
        )
        self.assertNotEqual(
            enforced, constraints,
            "the two walks returned the same set, which is what an extractor that ignored the"
            " record and read the response would do",
        )
        self.assertEqual(
            enforced & constraints, {("memoConstraint", "never")},
            "the only name both sides may share is the one the memoised helper puts there",
        )

    def test_a_field_derived_as_both_kinds_keeps_one_probe_per_kind(self):
        """The collision the real module cannot show, because its two sets are disjoint.

        dual is handed to a call AND compared against the authorized literal. Keyed by field
        alone the two would collapse into one probe, and the survivor would be asserted against
        the wrong receipt behaviour without the count moving.
        """
        fields, constraints, _enforced = self.derived()
        self.assertIn("dual", fields)
        self.assertIn(("dual", "never"), constraints)
        probes = mutation_probes(fields, constraints)
        self.assertEqual(
            [(kind, field) for kind, field, _mutant in probes if field == "dual"],
            [("constraint", "dual"), ("transformation", "dual")],
            "a field derived as both kinds lost one of its probes",
        )
        self.assertEqual(
            len(probes), len(fields) + len(constraints),
            "the probe set is smaller than the derived members, so two of them merged",
        )
