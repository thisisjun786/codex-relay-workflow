#!/usr/bin/env python3
"""One destination carried from a new install to the recovery of the install it replaced.

Each landed piece already has its own tests: the integrated installer and its diagnosis, the
update that fails and restores what it found, and the completion hook the installer registers.
What none of them states is the sequence a host actually lives through -- on one destination,
with the state that has to survive it seeded before the first install and read again after the
last refusal. A suite of separately passing installer tests is not that sequence, and the
difference is where a host loses a runtime.

So this module composes rather than repeats. It drives the fixture the update tests already
drive through four stages against one temporary destination, one host record, one Codex
configuration, one hook file and one store, and it asserts on what survived rather than on what
a command reported about itself.

Three rules are the point of the composition rather than decoration.

A cell is filled by the reading its own question called for. READINGS declares the cell, the
source that answers it and the path the answer is read from, and read() is the only way a cell
is ever filled. A reading that could not be made reads UNREADABLE: it never reads False, and it
never borrows the value of the cell beside it. crw_runtime/check.py states the same rule for the
eight result fields -- "None of the eight is ever derived from another" -- and this is where the
rule is exercised across three sources instead of inside one payload.

An assertion about survival is worthless unless something happened. Every stage therefore also
asserts what a run that did nothing could not produce: an environment the command built, a
selection naming it, a pointer reaching it, and a named seam that actually fired.

Nothing here is evidence about a host. Every path is a temporary directory, the two build steps
and the relay are stand-ins inherited from the update fixture, the diagnosis runs with HOME,
XDG_STATE_HOME, CODEX_HOME and PATH pointed inside that directory, and PROVENANCE records that a
row filled here is a fixture answer. What a real combination would have to record, and the
procedure for running one, is in docs/runtime-install.md.

A fourth rule arrived as three separate findings and is one rule. A row can reach its success
answer, down the path it declared, on the host it declared, and still be answering about
something this scenario never built: a helper called instead of the registered command, a second
Codex home, a run told its store was absent while the fixture had populated one. What those share
is that something handed to the thing under test was not what the scenario built, and the
difference was nowhere in the claim. So HANDED declares, per function, whether what it hands is
the built value or a stand-in -- and a stand-in says what the scenario has instead and what a row
reading through it therefore does not prove. INJECTIONS does the same for the switches the update
fixture accepts, and names the one this module must not hand.
"""

import ast
import builtins
import copy
import hashlib
import inspect
import json
import os
import shutil
import shlex
import signal
import stat
import subprocess
import sys
import tempfile
import types
import unittest
from pathlib import Path
from unittest import mock

ROOT = Path(__file__).resolve().parents[3]
sys.path.insert(0, str(ROOT / "scripts"))
# Discovery puts this directory on the path; running this file on its own does not, and the
# composition is worth being able to run by itself while working on it.
sys.path.insert(0, str(Path(__file__).resolve().parent))

from crw_runtime import (check, codexconfig, completion, definition, hooks, hostrecord,
                         ownership, pointer, reading, scope, staging, swapgate)

import runtime_install
import test_runtime_install as base

RUNTIME = ROOT / "scripts" / "runtime_install.py"
HERE = Path(__file__).resolve()

try:
    import tomllib
    HAS_READER = True
except ImportError:  # 3.10 has no reader, and approximating TOML is refused rather than tried
    HAS_READER = False


# The seven questions the acceptance criterion asks, each answered on its own reading. Declared
# as one tuple so a row nobody asked for, or a question with no row to answer it, is a failure
# here rather than a silent gap.
SEVEN = (
    "skillLink",
    "runtimeImport",
    "mcpToolExposure",
    "appServerConnection",
    "hookCallback",
    "modelPermissionPreservation",
    "deliveryAcceptance",
)

# Sources that are commands. A payload from one of these carries its own name, and read() checks
# that name before walking a path: handing one command payload to another command row is the
# cheapest way for a reading to answer a question it was never asked.
CLI_COMMANDS = ("diagnose", "hook-status")

# The source for a reading this module performs itself. It has no command name, so its payload
# carries an explicit stamp and read() checks that stamp the same way.
ACCEPTANCE = "acceptance"

# cell, source, the path the answer is read from, the function that produces it.
READINGS = (
    ("skillLink", "diagnose", ("skillLinks",),
     "runtime_install.skill_links"),
    ("runtimeImport", "diagnose", ("checks", "results", "imported"),
     "runtime_install.cmd_diagnose"),
    ("mcpToolExposure", "diagnose", ("checks", "results", "mcpExposed"),
     "runtime_install._mcp_exposed"),
    ("appServerConnection", "diagnose", ("checks", "results", "connected"),
     "runtime_install.cmd_diagnose"),
    ("hookCallback", "hook-status", ("firingJournal",),
     "completion._journal_cell"),
    ("modelPermissionPreservation", ACCEPTANCE, ("preservation",),
     "test_install_acceptance._model_permission_delta"),
    ("deliveryAcceptance", "diagnose", ("checks", "results", "deliveryAccepted"),
     "runtime_install._trial"),
)

# Readings this module needs that are not among the seven. alwaysActive is here rather than in
# SEVEN because the criterion does not ask for it to be judged; it asks that a successful
# install is never marked as it. Declaring it keeps that claim reachable through read() like
# every other cell, instead of through a subscript the rule above would have to make an
# exception for.
BESIDE = (
    ("alwaysActive", "diagnose", ("checks", "results", "alwaysActive"),
     "runtime_install.cmd_diagnose"),
    ("destinationKind", "diagnose", ("checks", "destinationKind"),
     "check.record"),
)

DECLARED = READINGS + BESIDE

# Where a declared producer is resolved: the module OBJECT, not its file. Reading a file for a
# definition passes on a name that survives in a comment or an unreachable branch, and for the
# reading this module performs itself it would have been searching the file that declares the
# producer for the name it declares -- a check answering its own question. Asking the module
# yields the callable a caller would actually reach.
PRODUCER_MODULES = {
    "runtime_install": runtime_install,
    "completion": completion,
    "check": check,
    "test_install_acceptance": sys.modules[__name__],
}

# The places in this module that reach a conclusion by reading source text rather than by asking
# the thing itself, each with the reason it cannot ask. Everything else that once lived here now
# asks: the producers are resolved as attributes, the fixture stand-ins are observed while the
# fixture is applying them, and the declared paths are walked in payloads the sources really
# produced.
#
# The check below does not recognise a shape. source_text_reached derives every way this file can
# reach the text of a source file -- a binding that is a .py path, __file__, a called name that
# hands source back, a string naming a .py file -- accounts for every occurrence of one, and
# fails on an occurrence that is in neither this map nor TOUCHES_SOURCE_WITHOUT_CONCLUDING. What
# it still cannot reach is in SOURCE_OUT_OF_REACH, with a control for each form.
TEXT_EVIDENCE = {
    "test_no_cell_is_filled_without_going_through_the_declared_reading":
        "a style rule about this module's own code has no object to ask: the thing it forbids"
        " is a line that was never written, so the source is the only witness. It is a lint and"
        " says so; the property it approximates is proved by the mutation cases instead.",
    "test_every_text_reading_place_is_declared":
        "the derivation that polices the others has to read this file to find them.",
    "source_text_reached":
        "it IS that derivation: to find the places that conclude from source text it has to"
        " parse this file, and no object can be asked where a spelling appears in a file.",
    "refusals_reached":
        "the same, for refusals: where a refusal is named is a fact about the text, and the"
        " objects can only say what the refusals are, not where they are written.",
    "test_the_reach_of_each_derivation_is_the_declared_one":
        "it runs the derivations, and running the source-text one means reading this file.",
    "test_no_two_places_this_module_declares_can_share_a_name":
        "whether two places share a name is a property of the text: the module object has"
        " already discarded the second by the time it could be asked.",
    "test_no_class_body_binding_shadows_without_asking_whether_it_happened":
        "which branch guards a binding is a property of how bound_in is written, and the module"
        " object cannot be asked: a yield that checks certainty and one that does not are the"
        " same generator to it. Reading the file is the only way to sweep the rule.",
    "test_every_owner_deciding_site_here_measures_how_near_the_binding_is":
        "which branch guards a binding is a property of how bound_in is written, and the module"
        " object cannot be asked: a yield that checks certainty and one that does not are the"
        " same generator to it. Reading the file is the only way to sweep the rule.",
    "test_every_placement_site_here_honours_a_declared_owner":
        "whether a site honours a global or nonlocal declaration is written in the site and"
        " nowhere in the object: a table that exempts one and a table that exempts both are the"
        " same dict once built. Reading the file is the only way to sweep which of them a"
        " placement site consults.",
    "test_every_statement_reader_here_pairs_an_unpacking_off":
        "which helper a site asks for a binding is written in the site and nowhere in the"
        " object: one that reads a statement's whole value and one that reads the pairs answer"
        " identically for every binding that is not an unpacking. Reading the file is the only"
        " way to see which question was put.",
    "test_every_argument_form_outside_the_seating_grammar_is_declared":
        "which argument forms this module writes is a property of the text: the object has"
        " already seated every call by the time it could be asked, and a call it could not"
        " seat looks the same as one it never made. Reading the file is the only way to find"
        " an arrival outside the grammar.",
    "test_no_reader_here_sees_only_the_unannotated_binding":
        "which form a reader here accepts is a property of how it is written, and the module"
        " object cannot be asked: a function that tests ast.Assign and one that tests both are"
        " the same callable to it. Reading the file is the only way to see the difference.",
    "test_every_value_follower_here_reads_the_whole_pass_through_vocabulary":
        "the same for the forms a value is followed through: which of them a resolver reads is"
        " written in the resolver and nowhere in the object, so the vocabulary and the coverage"
        " are both read off this file. This is the derivation that emits its own blind spots,"
        " and it has to read the source to have any to emit.",
    "test_every_place_that_settles_for_a_refusal_is_declared":
        "the places that name a refusal are in the text, so this inventory is derived from this"
        " file the same way -- and it is derived by accounting for every occurrence rather than"
        " by recognising the shape of the call, because a shape sweep is what missed two of them.",
    "test_no_call_site_hands_the_fixture_an_injection_this_module_refuses":
        "the subject IS this file's own calls: which switch a call hands the fixture is a"
        " property of the call site, and every call site is here. Observing them instead would"
        " mean running every scenario inside one case, and one run can only speak for itself."
        " The behavioural half is the agreement case, which reads what a run was actually told.",
}

# Places that reach a source file without concluding anything from its text. The negative rule
# over-collects on purpose: it is cheaper to write down why a place is not one than to discover,
# four times running, that a form nobody enumerated was never looked at.
TOUCHES_SOURCE_WITHOUT_CONCLUDING = {
    "diagnose_for":
        "it runs RUNTIME as a program and reads what the program answered. The text of that"
        " file is never opened here, and what the run answers is the thing being asked.",
    "test_a_decorator_alias_resolves_in_the_scope_that_imported_it":
        "it compiles and runs a sample written into the case itself, and then asks the OBJECT"
        " it built whether a descriptor was installed. No file is read, and the point is"
        " precisely that the expectation comes from running the code rather than from this"
        " module's own reading of it.",
    "test_each_binding_fixpoint_here_halts_on_a_name_bound_twice":
        "it parses a sample written into the case itself, to measure that a derivation comes"
        " back on it. No file is read and nothing about this module's own text is concluded:"
        " the answer it wants is whether the loop halts, not what the loop says.",
    "linked_skills":
        "it names install.py to run it, not to read it.",
    "importable_runtime":
        "it names __init__.py to decide where a package is, which is a question about the"
        " filesystem rather than about what the file says.",
    "source_spellings.is_source_file":
        "it names the .py suffix to decide which bindings are source files. Which files exist"
        " is a question for the filesystem, and it asks the filesystem.",
    "_source_spelled.spelled":
        "the matcher: it names the suffix in order to look for it, which is the question"
        " rather than an answer taken from a file.",
    "asked_over":
        "it names the file it writes for a check to read. What it reads back is what the"
        " CHECK said, which is a behaviour rather than a text.",
}

# Module-level statements that name a source file, keyed by what the statement binds rather than
# by a line, because a line moves whenever anything above it does.
SOURCE_AT_MODULE_LEVEL = {
    "ROOT": "it locates the checkout from this file's path and never opens it",
    "sys.path.insert": "it puts this file's own directory on the import path",
    "RUNTIME": "it names the runtime script so a later call can run it",
    "HERE": "it is the handle itself, which is what the derivation looks for",
}

# Called names whose signature this derivation could not read, so it cannot say whether they hand
# source text back. Reported rather than assumed harmless. Which of them an interpreter can read
# differs by version -- set is readable from 3.11 and not on the 3.10 floor -- so this is the
# UNION, and each entry says where it is unreadable so a name that becomes readable everywhere
# fails its own declaration instead of sitting here forever.
# Names beginning with read that ask ABOUT a file rather than answer with it. The rule below
# recognises a read by that prefix, which errs towards reporting on purpose; these are the
# spellings where the erring is plainly wrong, written down with the reason rather than fixed
# by narrowing the rule to a list of blessed reads -- that would trade an over-report for the
# silence this module exists to refuse.
ASKS_ABOUT_THE_FILE = {
    "readable": "it answers whether the stream can be read, which is a boolean about the"
                " handle and never the text behind it",
}

EVERY_INTERPRETER = "every supported interpreter"
THE_FLOOR = "the interpreters below 3.13"
# Where the floor entries stop being unreadable. Written down because no object can be asked it:
# it is a fact about which interpreter gave its builtin types an introspectable signature.
#
# Measured on the interpreters this machine has rather than recalled: 3.10 and 3.11 both raise
# ValueError for set, frozenset and zip, while 3.13 and 3.14 read all three. 3.12 was not
# available to exercise, so the boundary between it and 3.13 is taken at 3.13 -- the stricter
# side, which makes the check DEMAND these names on an interpreter it cannot confirm rather
# than excuse them. The wrong boundary the other way is the one that costs something: at 3.11
# this check stopped requiring them on two supported interpreters, so the declared reach could
# drift there without anything failing.
READABLE_FROM = (3, 13)
SOURCE_UNDECIDED_CALLS = {
    "getattr": (EVERY_INTERPRETER, "a builtin whose signature is not introspectable"),
    "vars": (EVERY_INTERPRETER, "the same"),
    "str": (EVERY_INTERPRETER, "a builtin type; calling it constructs a string"),
    "dict": (EVERY_INTERPRETER, "a builtin type; calling it constructs a mapping"),
    "type": (EVERY_INTERPRETER, "a builtin type; calling it asks an object what it is"),
    "KeyError": (EVERY_INTERPRETER, "a builtin exception being raised"),
    "TimeoutError": (EVERY_INTERPRETER, "the same, raised to bound a derivation being measured"),
    "min": (EVERY_INTERPRETER,
            "a builtin whose signature is not introspectable; calling it takes the first line"
            " at which the module binds open"),
    "set": (THE_FLOOR, "a builtin type whose signature 3.13 made readable and 3.10, 3.11 and"
                       " 3.12 did not"),
    "frozenset": (THE_FLOOR, "the same, and the pair of them is why this record is a union"),
    "zip": (THE_FLOOR, "a builtin type those interpreters cannot read either"),
    "next": (EVERY_INTERPRETER, "a builtin whose signature is not introspectable"),
    "range": (EVERY_INTERPRETER, "a builtin type; calling it counts the scopes between two"),
    "ast.Name": (EVERY_INTERPRETER,
                 "a node type from the ast module, constructed to stand for the parameter a"
                 " default binds. Its signature is not introspectable and it hands back a"
                 " node rather than anything read from a file."),
    "ast.Store": (EVERY_INTERPRETER, "the context that node is given, and the same answer"),
}

# Every place that asks whether a statement binds a name. `carrier: property = property(getter)`
# binds exactly as `carrier = property(getter)` does, and a table reading one form while the
# table beside it reads both answers differently about one rule -- which this module produced
# four separate times before the question was moved into _assigned(). A place that still reads
# the bare form alone is declared here with the reason it may. The set is derived from this
# file's own text, so a new reader of the bare form fails the module rather than waiting to be
# found, and an entry that stops being true fails too.
ASSIGN_ONLY_ON_PURPOSE = {}

# The lexical rule for a decorator alias, with the expected answer taken from PYTHON rather than
# from this reader. Each sample is RUN and the class it builds is asked whether a property
# descriptor was installed; the reader's inventory is then compared with that. Computing the
# expectation with the lookup under test would be the derived-therefore-complete fallacy with
# one more step in it -- a visibility guard that exists and answers wrongly passes a structural
# sweep and fails a person.
#
# Three cases, all inside the reach this resolver already claims. The third exists because a
# file-wide map passes the first by accident whenever the file order happens to suit it.
DECORATOR_ALIAS_CONTROLS = {
    "two scopes reusing one alias": (
        ("def good():",
         "    import builtins as d",
         "    class Holder:",
         "        @d.property",
         "        def carrier(self):",
         "            return reading.UNREADABLE",
         "        def answer(self):",
         "            return self.carrier",
         "    return Holder",
         "",
         "def bad():",
         "    import json as d",
         "    return d"),
        "good", "good.answer"),
    "an inner scope shadowing an outer binding": (
        ("import builtins as d",
         "",
         "def bad():",
         "    import json as d",
         "    class Holder:",
         "        @d.property",
         "        def carrier(self):",
         "            return reading.UNREADABLE",
         "        def answer(self):",
         "            return self.carrier",
         "    return Holder"),
        "bad", "bad.answer"),
    "the same two scopes written in the other order": (
        ("def bad():",
         "    import json as d",
         "    return d",
         "",
         "def good():",
         "    import builtins as d",
         "    class Holder:",
         "        @d.property",
         "        def carrier(self):",
         "            return reading.UNREADABLE",
         "        def answer(self):",
         "            return self.carrier",
         "    return Holder"),
        "good", "good.answer"),
    "one scope binding the alias to something else": (
        ("def good():",
         "    import json as d",
         "    class Holder:",
         "        @d.property",
         "        def carrier(self):",
         "            return reading.UNREADABLE",
         "        def answer(self):",
         "            return self.carrier",
         "    return Holder"),
        "good", "good.answer"),
}

# Every place the certainty of a class-body binding is decided. A name bound under a branch that
# may not run does not shadow -- whether it ran is a question about the run -- and treating one
# as definite loses the place that really does reach the enclosing name. That direction is a
# MISS: the inventory then accepts a place nobody declared, which is the ground this module
# exists to hold. So each binding yielded in bound_in has to ASK, and one that does not is
# declared here with the reason it may. Derived from bound_in's own text and keyed by the node
# kinds its branch tests, so a new unguarded branch fails here instead of arriving as a finding.
# Every place that decides WHICH binding a name refers to, derived from this file's own text.
#
# EXTRACTION PREDICATE, written down because a derived list is only as complete as its
# predicate, and treating "it is derived" as completeness is this module's own defect one level
# up. A site is recognised when it calls _bound_around -- which asks WHETHER some enclosing
# scope binds the name -- or _nearest, which asks HOW NEAR the nearest binding is. Those two
# are the only helpers through which this reader consults a scope-keyed binding map by name.
#
# OBSERVABLE BOUNDARIES, as data rather than as a sentence. Recognising calls to two names and
# nothing else, the predicate does NOT see a site that walks the scope chain inline, one that
# reads a scope-keyed map directly the way _visible_from does, or one written in another
# module. The control below plants the second of those and requires it to be absent, so the
# boundary is a measured fact rather than a caveat.
#
# A fourth shape, added because review found it after this inventory was built and the first
# three did not cover it. The predicate recognises a site that CONSULTS a scope-keyed map; it
# is blind to one that BUILDS its own map keyed by a bare name, because such a site never
# consults anything and so never calls either helper. _default_applies was exactly that, and
# this inventory would not have caught it. The control below requires that blindness to stay
# true, so it is a known gap rather than a surprise -- but a known gap is what it is, and the
# next site of that shape will reach a reviewer before it reaches this list.
#
# A fifth, and it is the sharpest so far because the sites in question were ALREADY RECOGNISED.
# This sweep asks WHETHER a site measures distance. It cannot ask WHICH OWNER SHAPES it
# measures it for. _source_spelled and _refusal_spelled both call _nearest and both appeared
# clean here while measuring distance only for a bare ast.Name owner -- so a parameter taking
# the root of a qualified owner, io in io.open or crw_runtime in crw_runtime.reading.UNREADABLE,
# walked past a site this list called covered. A name in the recognised set therefore says the
# question is asked somewhere in that function and nothing about the shapes it is asked for.
# There is no control for this one: which owner shapes a site covers is not readable from the
# fact that it calls _nearest, and inventing a predicate that claimed to read it would be the
# same fallacy again. It is written down instead.
#
# A sixth thing this list does not cover at all, said so it is not mistaken for silence:
# execution position. Whether a binding exists YET relative to a statement is a different axis
# from which scope owns a name, and _opens_here decides it alone.
#
# COVERAGE IS NOT CORRECTNESS. This says every site asks the question; the paired cases in
# RESOLVES_LIKE_PYTHON say the answers match Python. Neither claim substitutes for the other.
DECIDES_AN_OWNER_WITHOUT_DISTANCE = {
    "_hands_on":
        "its two sites ask only whether an enclosing scope binds the name -- the instance"
        " table's its_own, and the receiver-alias walk. Both err towards NOT claiming: finding"
        " a binding makes them answer with nothing, so existence alone can cost a place and"
        " cannot invent one. They decide a receiver or an instance class rather than a handle"
        " or an opener, which is what the distance rule was written for, and the distance"
        " question for them is open rather than settled.",
}

BINDS_WITHOUT_ASKING_IF_IT_HAPPENED = {
    "AsyncFunctionDef, FunctionDef":
        "a def under a branch is still a method: the method index answers for it either way and"
        " a call through self reaches it whether or not the branch ran. A class is indexed by"
        " nothing of the kind, which is exactly why ClassDef is not in here beside them.",
}

# Where a site that records which scope binds a name does not honour a global or nonlocal
# declaration. Three findings in a row were about WHERE a binding lives rather than which
# binding a name refers to, and they landed on three different sites, so the set is swept.
PLACES_A_NAME_WITHOUT_ASKING_WHERE_IT_LIVES = {
    "_held_by_class":
        "it asks which local of a method holds the thing before that method writes it onto"
        " self, and a global or nonlocal declaration does not change WHAT the name holds --"
        " only which scope holds it. Measured rather than assumed: a setUp declaring the name"
        " global and then binding self.unread to it reports the reading method exactly as the"
        " plain local does. The direction is towards accounting for the attribute rather than"
        " missing it, so the question here is open rather than settled, and it is written down"
        " instead of being read as coverage this sweep does not have.",
}

# Where a site reads a binding statement without pairing an unpacking off. _assigned answers
# with a statement's targets and its value; _bindings answers with the pairs. Three tables were
# handed the same defect one at a time -- the refusal owner, the opener, the class alias --
# before the set was swept instead of the instances.
READS_A_STATEMENT_WITHOUT_PAIRING_IT = {
    "_hands_on":
        "the property scan, which requires the statement's WHOLE value to be a property() call"
        " before it reads anything. An unpacking fails that guard and is skipped rather than"
        " mis-paired, so the pairing cannot be got wrong here -- what it can do is miss"
        " alias, ignored = property(getter), None, and that narrow miss is the cost stated.",
    "source_text_reached":
        "it asks only WHETHER anything named open is bound by the statement, and walks the"
        " whole target for that spelling. Which value each name receives is not a question it"
        " puts, and a destructured target is already read because walking reaches every Name"
        " in it.",
}

# Where a value-following resolver here reads fewer forms than the pass-through vocabulary.
# `that = self if flag else self` hands the receiver through a conditional, and a resolver
# reading ast.Name alone sees none of it -- which is how a reviewer found one rather than this
# module finding it. Which forms each resolver reads is written in the resolver, so the set is
# DERIVED from this file: the vocabulary comes from _passed_through's own text, and a resolver
# taking values from _bindings without covering it is emitted here and has to be declared.
#
# Each entry carries a sample and the place it must not reach, and the sample is RUN. A blind
# spot with nothing that can fail is the same "derived, therefore complete" claim wearing a
# different sentence, which is the fallacy this module exists to refuse.
FOLLOWS_FEWER_VALUE_FORMS = {
    "_instance_classes": (
        ("class Holder:",
         "    def carrier(self):",
         "        return reading.UNREADABLE",
         "",
         "def consumer(flag):",
         "    holder = Holder() if flag else Holder()",
         "    return holder.carrier()"),
        "consumer",
        "it reads the constructor written beside the name and does not follow a value through"
        " at all, so an instance handed over by a conditional is not tracked and the consumer"
        " below goes unreported. Widening it means deciding which class the name holds when the"
        " arms disagree, and that is a question about the run rather than about the text -- so"
        " the limit is stated here, with the case that measures it, rather than claimed away.",
    ),
}

# What this derivation still cannot see, as data rather than as a sentence. Each form is planted
# by a control that requires the derivation NOT to see it, so the list cannot rot in either
# direction: widening the derivation to cover one makes its control fail.
SOURCE_OUT_OF_REACH = {
    "a handle reached by its name as a string":
        ("getattr(module, \"HERE\")",
         "the name is a string, so there is no Name for the sweep to account. This case is not"
         " hypothetical: the regression cases below reach HERE exactly this way."),
    "a source path assembled at run time":
        ("open(Path(directory) / chosen)",
         "nothing in the expression names a .py file or a handle, so which file it opens is a"
         " fact about the run rather than about the text."),
    "a callable reached through anything other than a name":
        ("readers[key](read)",
         "the derivation follows names: a handle, a called name, a name bound to one. A callable"
         " taken out of a container, off an instance or out of a registry is chosen at run time,"
         " and which one it is is not a fact about this text. This is the end of the flow the"
         " reader follows, and it is written down here rather than left to be discovered."),
    "a helper in another module that hands source text back":
        ("base.source_of(thing)",
         "the derivation follows helpers defined HERE to their callers; a name defined"
         " elsewhere would have to be resolved and read, which is a different question."),
}

# The same floor for source text: these have to keep being derived, ast.parse above all, because
# it is the one form the check this replaces spelled out by hand and it now arrives only by being
# asked for its own signature.
SOURCE_REACH_INCLUDES = {
    "handle": ("HERE", "RUNTIME", "__file__"),
    "hands source": ("ast.parse",),
}

# Rows whose reading is a check.field cell and therefore answers in check.VALUES. The others
# answer in their own producer vocabulary and are NOT translated into this one: a cell rewritten
# into a neighbour vocabulary is the same borrowed answer with better manners.
CHECK_FIELD_ROWS = ("runtimeImport", "mcpToolExposure", "appServerConnection",
                    "deliveryAcceptance")
COMPLETION_CELL_ROWS = ("hookCallback",)
OWN_SHAPE_ROWS = ("skillLink", "modelPermissionPreservation")

PRESERVED = "preserved"
CHANGED = "changed"

# What the supplied interpreter has to answer about itself. Asked of the interpreter rather
# than read out of the text that launches it: a wrapper can mention -S and -E in a comment and
# run neither, and then every path this suite reports is right while the probe had the site
# directory and the inherited import path all along. The flags are a property of the process,
# so the process is the only thing that can establish them.
HERMETIC_FLAGS = {"no_site": 1, "ignore_environment": 1}

_FLAG_PROGRAM = (
    "import json, sys\n"
    "print(json.dumps({'no_site': sys.flags.no_site,\n"
    "                  'ignore_environment': sys.flags.ignore_environment,\n"
    "                  'smuggled': [p for p in sys.path if 'crw-smuggled' in p]}))\n"
)


def hermetic_answers(interpreter, root):
    """Ask the supplied interpreter what it is, with something planted where it must not look."""
    done = subprocess.run(
        [str(interpreter), "-c", _FLAG_PROGRAM], capture_output=True, text=True, timeout=120,
        env=dict(os.environ, PYTHONPATH=str(root / "crw-smuggled")))
    if done.returncode != 0 or not done.stdout.strip():
        return None, "the supplied interpreter did not answer: " + (done.stderr or "")[-200:]
    return json.loads(done.stdout), None


def inside(where, root):
    """Whether a path is under this root, by ancestry rather than by how it is spelled.

    A prefix comparison reads /tmp/root-escape as being inside /tmp/root, which is the one
    mistake a containment check must not make.
    """
    try:
        Path(where).resolve().relative_to(Path(root).resolve())
    except ValueError:
        return False
    return True

# What this module can say about where a row was performed. "fixture" is narrower than the
# record own "temporary": a temporary destination whose build steps and relay are stand-ins.
# A committed row is never "host", and a check enforces it, because the one thing this suite
# must not be able to do is read as a host measurement nobody performed.
PROVENANCE = ("fixture", "temporary", "host")
COMMITTED_PROVENANCE = "fixture"

# The configuration keys carrying the model and the permission posture, read on both sides of an
# install. TaskSettings.REQUIRED names sandbox and approvalPolicy beside model, so a reading that
# watched only the model would miss the half deciding what a restored task may do.
MODEL_PERMISSION_KEYS = ("model", "approval_policy", "sandbox_mode")


def _row(cell):
    """The one declared row for this cell, looked up rather than respelled at each site."""
    for declared in DECLARED:
        if declared[0] == cell:
            return declared
    raise KeyError("no reading is declared for " + repr(cell))


def _unreadable(cell, source, path, detail):
    """A cell whose reading could not be made.

    A distinct answer, not a negative one. Returning False here, or falling through to whatever
    the neighbouring cell said, is the defect this module exists to refuse.
    """
    return {"cell": cell, "value": reading.UNREADABLE, "answeredBy": source,
            "readingPath": path, "readable": False, "detail": detail,
            "performedAgainst": COMMITTED_PROVENANCE}


def read(cell, payloads):
    """Fill one acceptance cell from the reading its own question called for, and nothing else.

    The only accessor. It takes the payload of the declared source, confirms the payload is the
    one that source produces, walks the declared path and returns what it found. Every way of
    failing to reach an answer returns UNREADABLE naming the reason, so a command that did not
    run and a command that answered are never the same row.
    """
    declared_cell, source, path, _producer = _row(cell)
    payload = payloads.get(source)
    if not isinstance(payload, dict):
        return _unreadable(declared_cell, source, path, "no payload was produced by " + source)
    if source in CLI_COMMANDS:
        if payload.get("command") != source:
            return _unreadable(declared_cell, source, path,
                               "the payload names " + repr(payload.get("command"))
                               + ", not " + repr(source))
    elif payload.get("source") != source:
        return _unreadable(declared_cell, source, path,
                           "the payload does not carry the " + repr(source) + " stamp")
    found = payload
    for key in path:
        if not isinstance(found, dict) or key not in found:
            return _unreadable(declared_cell, source, path, "the reading has no " + repr(key))
        found = found[key]
    if found == reading.UNREADABLE:
        # A producer that already said it could not read is not made readable by the fact that
        # its answer was where this table expected it. The path being reachable answers "was
        # there an answer here", which is a different question from "was the reading made", and
        # collapsing the two is how an unread row passes a readability check.
        return _unreadable(declared_cell, source, path,
                           str(payload.get("detail") or "the reading reported itself unreadable"))
    return {"cell": declared_cell, "value": found, "answeredBy": source, "readingPath": path,
            "readable": True, "performedAgainst": COMMITTED_PROVENANCE}


def table(payloads):
    """Every declared cell, each filled by its own reading."""
    return {cell: read(cell, payloads) for cell in SEVEN}


def _configuration_keys(raw):
    """The model and permission keys of a Codex configuration, or a refusal to approximate.

    tomllib arrives in 3.11 and the supported floor is 3.10, so on the older job there is no
    reader for this file. That is answered with a refusal rather than with a regular expression,
    for the reason the installer gives for the same refusal: a value guessed out of TOML is a
    value whose wrongness is invisible, and this row would then report a preservation nobody
    read. A second TOML parser living in a test is also the duplicate statement the runtime
    declines to make.
    """
    if not HAS_READER:
        return None, ("reading a configuration needs tomllib, which this interpreter does not"
                      " have, so preservation here is unread rather than established")
    try:
        parsed = tomllib.loads(raw)
    except (ValueError, TypeError) as error:
        return None, "the configuration could not be read: " + type(error).__name__
    return {key: parsed.get(key) for key in MODEL_PERMISSION_KEYS}, None


def _model_permission_delta(earlier, later):
    """Whether the keys carrying model and permission survived, read on both sides.

    This module own reading, and deliberately not one of the installer own.
    checks.settingsPreserved answers whether config.toml bytes were left alone by a command that
    writes nothing, and settings_usable answers whether a settings value a caller supplied is
    admissible to the relay. Neither looks at model or permission state before an install and
    again afterwards, so neither can say anything was preserved. Filling this cell from either
    would be the borrowed answer refused everywhere else here, so the absence of an installer
    cell for this question is reported as a gap instead of papered over with a nearby verdict.
    """
    was, refusal = _configuration_keys(earlier)
    if refusal is not None:
        return {"source": ACCEPTANCE, "preservation": reading.UNREADABLE, "detail": refusal}
    now, refusal = _configuration_keys(later)
    if refusal is not None:
        return {"source": ACCEPTANCE, "preservation": reading.UNREADABLE, "detail": refusal}
    return {"source": ACCEPTANCE,
            "preservation": PRESERVED if was == now else CHANGED,
            "read": {"earlier": was, "later": now}}


# The settings this composition installs over: the model and permission keys, a table belonging
# to somebody else, and the bridge table the fixture writes. The first two are what an installer
# has no business touching at all.
MODEL_PERMISSION_BLOCK = ('model = "a-model"\n'
                          'approval_policy = "never"\n'
                          'sandbox_mode = "read-only"\n')
FOREIGN_TABLE = '[mcp_servers.cxc]\ncommand = "/opt/cxc"\n\n[mcp_servers.cxc.env]\nA = "b"\n'

# A hook registration belonging to somebody else. Runtime hooks live in their own file, not in
# config.toml, and the update fixture snapshot does not read it -- so preservation of a hook
# setting has to be snapshotted here or it is not being checked at all.
FOREIGN_HOOKS = {
    "version": 1,
    "hooks": {"Stop": [{"hooks": [{"type": "command", "command": "/opt/cxc/stop", "timeout": 9}]}]},
}


def hooks_path(host):
    return host.codex_home / "hooks.json"


def seed_settings(host):
    """Put somebody else settings in front of the installer, and return what must survive."""
    raw = (MODEL_PERMISSION_BLOCK + "\n" + FOREIGN_TABLE + "\n"
           + host.config.read_text(encoding="utf-8"))
    host.config.write_text(raw, encoding="utf-8")
    hooks_path(host).write_text(json.dumps(FOREIGN_HOOKS, indent=2), encoding="utf-8")
    return settings_bytes(host)


def settings_bytes(host):
    """Every settings file an install must leave alone, read as bytes."""
    return {
        "config.toml": host.config.read_bytes(),
        "hooks.json": hooks_path(host).read_bytes() if hooks_path(host).is_file() else None,
    }


def clean(host):
    """Turn the fixture into a host that has nothing installed yet.

    The fixture exists to model an update, so it arrives with a selected runtime, a pointer and a
    recorded install. A new install is the stage before all of that. The store file stays where
    it is: snapshot() reads it to prove the rows survived, and an absent store is reported by the
    readings rather than produced by deleting the evidence the later stages assert on.
    """
    shutil.rmtree(host.previous, ignore_errors=True)
    record = hostrecord.load(host.record_path, host.data["definitionVersion"]).value or {}
    record["selected"] = {}
    record.pop("pointer", None)
    for name in list(record.get("components", {})):
        record["components"][name]["installs"] = []
    hostrecord.save(host.record_path, record)
    if host.pointer_path.is_symlink() or host.pointer_path.exists():
        host.pointer_path.unlink()


def arriving_source(host):
    """A source change: new digests, so the next install builds at a directory of its own.

    Standing in for a source change, which would give a different directory name for the same
    reason. The digests move on the fixture own copy of the definition because the update
    fixture derives its measured digests from that copy too; bumping only what load() returns
    would make the run refuse for a digest disagreement, which is a refusal about the fixture
    rather than about the boundary under test.
    """
    for component in host.data["components"]:
        component["sourceDigest"] = hashlib.sha256(
            ("arrived-" + component["sourceDigest"]).encode()).hexdigest()
    combined = hashlib.sha256(
        "".join(c["sourceDigest"] for c in host.data["components"]).encode()).hexdigest()[:12]
    host.candidate = host.destination / (
        "env-" + str(host.data["definitionVersion"]) + "-" + combined)
    return host.candidate


def updating(host):
    """The definition the arriving source would present, for the length of one run."""
    return (mock.patch.object(runtime_install.definition, "load", return_value=host.data),
            mock.patch.object(runtime_install.definition, "verify", return_value=[]))


def install(host, **injected):
    """One install run against this host, driven the way the update tests drive it."""
    return base.UpdateRecoveryTests()._run(host, **injected)


def isolated(root, host=None):
    """An environment whose homes, state roots and PATH all sit inside this directory.

    Pointing --state at a temporary path is not isolation. The survey runs its own discovery
    without the supplied state, filesystem discovery reads HOME and XDG_STATE_HOME, and an
    installed relay on PATH would be resolved and run. So the child gets homes of its own and a
    PATH that cannot reach an installed entry point, and a check afterwards confirms the paths
    it reported are under this root.
    """
    home = root / "home"
    home.mkdir(exist_ok=True)
    state_root = root / "xdg-state"
    state_root.mkdir(exist_ok=True)
    return {
        "HOME": str(home),
        "XDG_STATE_HOME": str(state_root),
        "CODEX_HOME": str(root / "codex"),
        # The supplied runtime first, then enough to run git. Never enough to reach an
        # installed entry point of this host's own.
        "PATH": ((str(host.candidate / "bin") + ":") if host is not None else "") + "/usr/bin:/bin",
        # An inherited import path is the other way in. Without these the child imports whatever
        # the host has installed, and the resolved-location reading would then be about this
        # machine rather than about the destination under test.
        "PYTHONPATH": "",
        "PYTHONNOUSERSITE": "1",
    }


def answering_relay(directory):
    """A relay whose doctor reports a socket it reached, so the connection cell can move.

    The connection question is the one with no input of its own on the command line: it is
    answered by what the relay says. Without a relay that answers, the cell sits at the same
    value in every direction, and a cell that never moves cannot catch a neighbour copying
    into it -- which is how one reading ends up standing in for another without any case
    noticing.
    """
    path = Path(directory) / "answering-relay"
    path.write_text(
        "#!/usr/bin/env python3\n"
        "import json\n"
        "print(json.dumps({'actorReachability': {'socketConnect': 'ok'}}))\n",
        encoding="utf-8")
    path.chmod(path.stat().st_mode | stat.S_IEXEC | stat.S_IRWXU)
    return path

def standin_relay(directory, answer):
    """A relay that answers the guard, so the hook has something real to call."""
    path = Path(directory) / "codex-session-relay"
    path.write_text(
        "#!/usr/bin/env python3\n"
        "import sys\n"
        "sys.stdin.buffer.read()\n"
        "sys.stdout.write(" + repr(json.dumps(answer)) + ")\n",
        encoding="utf-8")
    path.chmod(path.stat().st_mode | stat.S_IEXEC | stat.S_IRWXU)
    return path


# The Stop payload the host was observed to deliver, forwarded as it arrives.
STOP = {
    "cwd": "/tmp/workspace",
    "hook_event_name": "Stop",
    "last_assistant_message": "I finished the task.",
    "model": "test-model",
    "permission_mode": "default",
    "session_id": "01a0b109-1ea5-7fb3-9adc-87f45ed83688",
    "stop_hook_active": False,
    "transcript_path": "/tmp/transcript.jsonl",
    "turn_id": "turn-1",
}

RELEASED = {"decision": "release", "state": "unmanaged", "observation": "unmanaged",
            "reason": "No marker names this workspace.", "assignmentId": None,
            "counters": {}, "recordedAs": None, "hook_output": {}}


def fire_the_hook(directory):
    """Fire the hook the way the host does, through the command the registration names.

    Calling completion.run() here would have been a reading of this checkout. The row says an
    installed hook fired, and the installed hook is a COMMAND LINE in hooks.json: an interpreter,
    the entry point, and the settings path the install chose. Run the helper directly and that
    whole registration is untested -- a broken entry point, a settings argument pointing
    somewhere else, a stdin or stdout contract that changed, and the row still reaches "1".

    So the hook is installed by the installer, the command is DERIVED from the file it wrote
    rather than written out again here, and that command is executed as a program with the Stop
    payload on its stdin. What the row then reads is a journal entry the registered command
    produced.
    """
    directory = Path(directory)
    standin_relay(directory, RELEASED)
    arguments = base.argparse.Namespace(
        codex_home=str(directory), event=None, hook_command=None, adapter="completion",
        dest=None, relay_command=str(directory / "codex-session-relay"),
        marker_root=str(directory / "marker"), db_path=None,
        journal_root=str(directory / "journal"), python=sys.executable,
        mode=completion.OBSERVE, guard_timeout=5, timeout=10, issue="CRW-69", apply=True)
    with mock.patch.object(runtime_install, "emit"):
        runtime_install.cmd_hook(arguments)

    registered = completion.adapter_entries(
        hooks.read(directory / "hooks.json").value or {}, completion.EVENT)
    before = completion.status(codex_home=str(directory), environ={})
    done = subprocess.run(
        shlex.split(registered[0]["command"]) if registered else ["false"],
        input=json.dumps(STOP).encode("utf-8"), capture_output=True, timeout=120,
        env={**os.environ, "CODEX_HOME": str(directory)})
    after = completion.status(codex_home=str(directory), environ={})
    return before, after, registered, done


# Every boundary an update crosses, as the update tests enumerate them: two build steps, four
# gate answers and two pointer steps. The composition runs the whole sequence once per boundary,
# because "the previous install came back" is a claim about each way of failing, not about one.
BOUNDARIES = (
    ("create environment", None),
    ("install packages", None),
    (None, "running daemon"),
    (None, "handover in flight"),
    (None, "unreadable daemon"),
    (None, "store would be downgraded"),
    ("replace the owned pointer", None),
    ("read the owned pointer back", None),
)

GATE_REFUSAL = "read whether it is safe to swap"


# The answer each acceptance row reads when the thing it judges actually worked. Declared, and
# then required: a row that can never reach its success answer is a row whose assertion accepts
# its own failure, and an install acceptance suite that passes when nothing installed is worse
# than none, because the next person believes the ground is covered.
SUCCESS_ANSWERS = {
    # A listing answers in its own shape, so its success is stated in that shape: the check ran,
    # nothing is missing and nothing conflicts.
    "skillLink": {"exitCode": 0, "missing": [], "conflict": []},
    "runtimeImport": "verified",
    "mcpToolExposure": "verified",
    "appServerConnection": "verified",
    "hookCallback": "1",
    "modelPermissionPreservation": PRESERVED,
    "deliveryAcceptance": "verified",
}

# Every answer that means a question was not settled. Named so the check below can derive the
# places that REQUIRE one, rather than recognising assertions by their shape -- a sweep over
# shapes misses a refusal reached through a helper, and it was a shape sweep that missed twice.
REFUSAL_ANSWERS = ("not_verified", "unknown", "not_applicable", reading.UNREADABLE,
                   reading.ABSENT, reading.ACCESS_ERROR, CHANGED,
                   completion.NOT_READ, completion.NO_JOURNAL)

# The places that require a refusal, and what makes the refusal the right answer there. None of
# them is an acceptance row accepting its own failure: every one of the seven is separately
# required to read its success answer. A new place that settles for a refusal has to say which
# of these it is.
#
# The check derives the set from this file, and it does it by accounting rather than by
# recognising. refusal_spellings asks the objects for every way a refusal can be written -- the
# answers themselves, an attribute an imported module binds to one, a global here bound to one,
# a collection containing one -- and every occurrence of any of them has to land in this map or
# in NAMES_A_REFUSAL_WITHOUT_SETTLING. An assertNotEqual against a refusal is accounted like any
# other occurrence; which side of the question it settles is a judgement the reason below carries
# rather than something the derivation decides for itself.
ACCEPTS_A_REFUSAL = {
    "test_a_daemon_that_could_not_be_read_is_neither_running_nor_stopped":
        "the refusal IS the question: a daemon that could not be read is a third answer, and"
        " collapsing it into running or stopped is the defect.",
    "test_a_missing_registration_and_a_claimed_one_are_not_the_same_answer":
        "absence is the answer being distinguished from a conflict.",
    "test_an_install_that_succeeded_is_not_reported_as_an_activation":
        "not_verified is the correct answer and the criterion: installation is not activation,"
        " so this row succeeds by never reading verified.",
    "test_changing_one_condition_moves_only_the_reading_that_asked_about_it":
        "the refusals are the BEFORE ends of transitions that must finish at verified.",
    "test_every_declared_path_is_one_its_source_actually_writes":
        "the refusal is what proves the walk is load-bearing: the key is removed and the row"
        " has to go unreadable.",
    "test_every_row_is_readable_and_answers_in_its_own_vocabulary":
        "only on the interpreter with no configuration reader, where the row must say it could"
        " not take the reading and name why.",
    "test_the_hook_row_counts_an_invocation_that_really_happened":
        "absence is the BEFORE end: the claim is the transition to exactly one invocation.",
    "test_the_model_and_permission_row_is_not_taken_from_a_neighbouring_verdict":
        "changed is the correct answer to a posture that moved, and unreadable only where there"
        " is no reader.",
    "test_withholding_a_source_leaves_only_its_own_rows_unreadable":
        "the refusal is the property: a command that did not run must not read as an answer.",
    "test_without_a_reader_the_row_reports_itself_unread_rather_than_preserved":
        "the property is that an unread row says so instead of reading as preserved.",
}

# Places that name a refusal without settling for one. The negative rule over-collects, and this
# is where the over-collection is paid for: a producer of a refusal is not a place accepting one.
NAMES_A_REFUSAL_WITHOUT_SETTLING = {
    "test_the_reach_of_each_derivation_is_the_declared_one":
        "it names REFUSAL_REACH_INCLUDES to CHECK that each spelling in it is still derived."
        " The table became a collection holding refusals once the reach stopped stopping one"
        " container short of what the objects hold, and naming a table in order to test it is"
        " the opposite of accepting what it contains as an answer.",
    "test_a_refusal_nested_in_a_collection_is_still_collected":
        "it builds collections holding a refusal and binds each on this module to ask the"
        " derivation whether it finds one. The refusals are the material under test rather"
        " than an answer this place accepts: it asserts what the derivation says about them,"
        " and two of the shapes it builds hold no refusal at all.",
    "_unreadable":
        "it MAKES the refusal. A cell whose reading could not be taken is what this returns,"
        " and returning it is the opposite of settling for one somebody else returned.",
    "read":
        "the accessor, which passes a producer's refusal through unchanged rather than"
        " accepting it as an answer to the question the row asked.",
    "_model_permission_delta":
        "the producer of this module's own reading: with no configuration reader it answers"
        " unreadable, and changed is its verdict about a posture that moved.",
    "refusal_spellings":
        "it derives the spellings from REFUSAL_ANSWERS. Naming the set is how it asks the"
        " question, not an answer it accepted.",
    "refusals_reached":
        "it names the same set for the same reason, one question further on: to ask an"
        " imported module whether what it exports under a borrowed spelling is really one of"
        " the answers. Asking is not settling.",
    "test_a_place_that_settles_for_a_refusal_in_a_form_the_old_sweep_missed_is_still_caught":
        "it plants a refusal in a synthetic file for the check to find; what this case reads"
        " is whether the check failed, not what the planted place asserted.",
}

# Module-level statements naming a refusal, keyed by what the statement binds.
REFUSAL_AT_MODULE_LEVEL = {
    "CHANGED": "the constant itself, which is one of the answers",
    "REFUSAL_ANSWERS": "the declaration of the answers the derivation starts from",
    "REFUSAL_REACH_INCLUDES":
        "it names the spellings that have to keep being derived. Three of those names are"
        " themselves answers, because a vocabulary that spells a constant the way it reads is"
        " the ordinary case rather than a coincidence.",
}

# What this derivation cannot see, as data. Each form is planted by a control that requires the
# derivation NOT to see it, so widening the derivation to cover one makes its control fail.
REFUSAL_OUT_OF_REACH = {
    "a refusal compared as data, never spelled":
        ("self.assertEqual(rows[cell][\"value\"], baseline[cell][\"value\"])",
         "both sides are read at run time and neither names an answer. Attributing this would"
         " mean attributing read() to every acceptance row, and an inventory that names every"
         " row has stopped distinguishing anything."),
    "a refusal reached through a callable this text does not name":
        ("self.assertEqual(row, handlers[key]())",
         "the same end of the same flow: the derivation follows names, and a helper taken out of"
         " a container or a registry is chosen while the suite runs."),
    "a refusal reached by its name as a string":
        ("getattr(completion, \"NOT_READ\")",
         "the name is a string, so there is no attribute for the sweep to account."),
    "a refusal a helper in another module asserts":
        ("base.expect_unreadable(row)",
         "no spelling appears in this file at all. A helper defined HERE is accounted, and its"
         " callers with it when it hands the refusal back rather than a container holding one."),
}


# This reader resolves a name through classes; Python resolves it through objects. Where the two
# disagree the reader is wrong, and eight of these were found by review against this branch: four
# places it lost and four it invented. They are data rather than eight cases because the next
# disagreement belongs beside them, and because each fix needs the form it must NOT break written
# next to it -- a shadow that stops the lookup is right when the binding really happens, and the
# fix for the loop that never runs is one line away from disabling it altogether.
#
# Each entry is (which derivation, the lines producing it, the place at issue, whether that place
# really does reach the declared thing, why). The fourth column is Python's answer, not this
# reader's, which is the whole point of writing it down.
REFUSAL, TEXT = "refusal", "text"

# The grammar this reader seats an argument in: a written argument, a keyword, a written tuple
# or list unpacking, a written mapping. Outside it there is no seating to derive, so nothing is
# seated and the occurrence is REPORTED -- an undeclared one fails acceptance rather than being
# guessed at or dropped. Each entry carries the sample its control plants, so an entry cannot
# outlive the form it describes.
ARGUMENT_FORM_NOT_SEATED = {
    "a set unpacking":
        ("helper(*{ONE, TWO})",
         "a set has no order. Which element fills which slot is not a fact this text holds, and"
         " reading it off the order the source happens to list is an answer taken from"
         " something that is not there -- the same call answered differently depending on which"
         " element was typed first. There is no correct seating to derive, so none is."),
    "an unpacking this text cannot read":
        ("helper(*supplied)",
         "how many values it carries, and which, is a fact about the run. The positional count"
         " stops at it rather than guessing how far it reaches."),
    "a mapping this text cannot read":
        ("helper(**supplied)",
         "which names it carries is the same question, asked of the keywords. A mapping written"
         " out names its own slots and IS seated; one handed over whole does not."),
}

# Where the thing is handed on through a TRANSFORMATION rather than as itself. This reader
# follows names and the pass-through vocabulary; reading .strip() off a refusal or
# .splitlines() off source text hands back a new object, and following that would mean
# following every method call on everything, which is a taint analysis rather than this
# reader. Unlike the two lists above, the place naming the spelling IS still found here -- what
# is lost is the caller behind it -- so the control has to hold both ends: if the named place
# stops being found the boundary is not where this says, and if the caller starts being found
# the boundary has closed and the entry has to go.
HANDED_ON_THROUGH_A_TRANSFORMATION = {
    "a refusal handed on through a formatted string that builds a new one":
        (REFUSAL,
         ("def helper():",
          "    return f\"answer={reading.UNREADABLE}\"",
          "",
          "def consumer():",
          "    return helper()"),
         "helper", "consumer",
         "f\"{answer}\" alone IS the value's own string and is followed; this one is not. Text"
         " beside the value, a conversion or a format spec builds a NEW string, and whether"
         " that still answers as a refusal is a question about what reads it rather than about"
         " this text. The line between the two is exactly the one the language draws, which is"
         " why the exact form is followed and this one is named."),
    "a refusal handed on through a method call":
        (REFUSAL,
         ("def helper():",
          "    return reading.UNREADABLE.strip()",
          "",
          "def consumer():",
          "    return helper()"),
         "helper", "consumer",
         "reading .strip() off the refusal hands back a string that is no longer the refusal"
         " object, and whether it still ANSWERS as one is a fact about the run. The helper is"
         " named because the spelling is written in it; the caller is not, and that is the"
         " cost. Widening it means following an arbitrary method call on the thing, which is"
         " the point at which this reader would stop being one."),
    "source text handed on through a method call":
        (TEXT,
         ("def helper():",
          "    return HERE.read_text().splitlines()",
          "",
          "def consumer():",
          "    return helper()"),
         "helper", "consumer",
         "the same on the source side: the read really is taken, so the helper is named, but"
         " what it hands back is a list built from the text rather than the text. Following"
         " that is the same unbounded step as above."),
}
RESOLVES_LIKE_PYTHON = {
    "a class-body name a loop that may never run does not shadow":
        (REFUSAL,
         ("def carrier():",
          "    return \"not_verified\"",
          "",
          "class Holder:",
          "    for carrier in []:",
          "        pass",
          "    answer = carrier()"),
         "Holder.answer", True,
         "a for target is created only once the iterator yields, so a class body looping over an"
         " empty sequence never binds the name and the call reaches the enclosing one."),
    "a class-body name an ordinary binding does shadow":
        (REFUSAL,
         ("def carrier():",
          "    return \"not_verified\"",
          "",
          "class Holder:",
          "    carrier = None",
          "    answer = carrier()"),
         "Holder.answer", False,
         "the pair of the case above: an unconditional binding really does shadow, so stopping"
         " the outward lookup is right here and the loop fix must not have disabled it."),
    "a property reached through a class alias":
        (REFUSAL,
         ("class Holder:",
          "    @property",
          "    def carrier(self):",
          "        return reading.UNREADABLE",
          "    alias = carrier",
          "    def answer(self):",
          "        return self.alias"),
         "answer", True,
         "reading self.alias runs the same getter, so the alias table has to be consulted for"
         " descriptors exactly as it already is for method calls."),
    "a property reached through super()":
        (REFUSAL,
         ("class Base:",
          "    @property",
          "    def carrier(self):",
          "        return reading.UNREADABLE",
          "",
          "class Child(Base):",
          "    def answer(self):",
          "        return super().carrier"),
         "answer", True,
         "super() is the one receiver spelled as a call rather than a name; the base getter runs"
         " exactly as the base method does for super().carrier()."),
    "an inherited property with nothing standing over it":
        (REFUSAL,
         ("class Base:",
          "    @property",
          "    def carrier(self):",
          "        return reading.UNREADABLE",
          "",
          "class Child(Base):",
          "    def answer(self):",
          "        return self.carrier"),
         "answer", True,
         "the pair of the override cases below: ordinary inheritance still has to be followed,"
         " so stopping at a subclass binding must not have stopped the walk altogether."),
    "a subclass method standing over an inherited property":
        (REFUSAL,
         ("class Base:",
          "    @property",
          "    def carrier(self):",
          "        return reading.UNREADABLE",
          "",
          "class Child(Base):",
          "    def carrier(self):",
          "        return \"fine\"",
          "    def answer(self):",
          "        return self.carrier"),
         "answer", False,
         "the first definition in the lookup order wins, so self.carrier reads Child's method"
         " and the base getter never runs."),
    "an inherited method with nothing standing over it":
        (REFUSAL,
         ("class Base:",
          "    def carrier(self):",
          "        return reading.UNREADABLE",
          "",
          "class Sub(Base):",
          "    def consumer(self):",
          "        return self.carrier()"),
         "consumer", True,
         "the same pairing on the method side."),
    "a subclass binding standing over an inherited method":
        (REFUSAL,
         ("class Base:",
          "    def carrier(self):",
          "        return reading.UNREADABLE",
          "",
          "class Sub(Base):",
          "    carrier = str",
          "    def consumer(self):",
          "        return self.carrier()"),
         "consumer", False,
         "Sub.carrier is str, so the call reaches a builtin and the bases are never consulted."),
    "a staticmethod decorated through an alias":
        (REFUSAL,
         ("sm = staticmethod",
          "",
          "class Holder:",
          "    def carrier(self):",
          "        return reading.UNREADABLE",
          "    @sm",
          "    def consumer(this):",
          "        return this.carrier()"),
         "consumer", False,
         "a static method has no receiver, so its first parameter is whatever a caller passed"
         " and crediting it with the class's carriers invents the call."),
    "a staticmethod decorated by its own name":
        (REFUSAL,
         ("class Holder:",
          "    def carrier(self):",
          "        return reading.UNREADABLE",
          "    @staticmethod",
          "    def consumer(this):",
          "        return this.carrier()"),
         "consumer", False,
         "the pair of the case above, which was already right and has to stay right."),
    "a subclass rebinding an inherited attribute":
        (REFUSAL,
         ("class Base:",
          "    carrier = reading.UNREADABLE",
          "",
          "class Child(Base):",
          "    carrier = \"fine\"",
          "    def answer(self):",
          "        return self.carrier"),
         "answer", False,
         "the attribute side of the same shape: lookup stops at the first class that binds the"
         " name, so Child.carrier is what self.carrier reads and the base's refusal is not"
         " reached through it."),
    "an inherited attribute with nothing standing over it":
        (REFUSAL,
         ("class Base:",
          "    carrier = reading.UNREADABLE",
          "",
          "class Child(Base):",
          "    def answer(self):",
          "        return self.carrier"),
         "answer", True,
         "its pair: an attribute a base binds really is held by everything under it, which is"
         " what the propagation exists for and must keep doing."),
    "a base to the left binding the name the bases to its right carry":
        (REFUSAL,
         ("class Left:",
          "    carrier = str",
          "",
          "class Right:",
          "    def carrier(self):",
          "        return reading.UNREADABLE",
          "",
          "class Child(Left, Right):",
          "    def consumer(self):",
          "        return self.carrier()"),
         "consumer", False,
         "lookup ends at Left, so Right is never consulted. Finding nothing in a base and"
         " finding a binding this reader cannot follow are different answers, and reading the"
         " second as the first walks straight past the class Python stops at."),
    "a base to the left binding nothing, so a base to its right carries":
        (REFUSAL,
         ("class Left:",
          "    pass",
          "",
          "class Right:",
          "    def carrier(self):",
          "        return reading.UNREADABLE",
          "",
          "class Child(Left, Right):",
          "    def consumer(self):",
          "        return self.carrier()"),
         "consumer", True,
         "its pair: an empty base really is not an answer, so the search has to carry on past"
         " it rather than stopping at the first base written."),
    "a property decorated through an alias":
        (REFUSAL,
         ("prop = property",
          "",
          "class Holder:",
          "    @prop",
          "    def carrier(self):",
          "        return reading.UNREADABLE",
          "    def consumer(self):",
          "        return self.carrier"),
         "consumer", True,
         "prop = property then @prop declares the same descriptor, so reading self.carrier runs"
         " the getter. Indexed as an ordinary method it is never read as a call at all, and the"
         " consumer leaves both inventories."),
    "source text handed on from a file object opened in the same expression":
        (TEXT,
         ("def helper():",
          "    return open(HERE).read()",
          "",
          "def consumer():",
          "    return helper()"),
         "consumer", True,
         "the helper was already accounted, because HERE is named in it. What was lost is the"
         " onward step, which HERE.read_text() in the same shape has always carried."),
    "a class-body rebinding that may never happen":
        (REFUSAL,
         ("class Base:",
          "    carrier = reading.UNREADABLE",
          "",
          "class Child(Base):",
          "    if False:",
          "        carrier = \"fine\"",
          "    def answer(self):",
          "        return self.carrier"),
         "answer", True,
         "whether the binding happened is a question about the run, so it cannot be read as a"
         " shadow. Guessing that it did drops the inherited refusal and nobody is asked about"
         " it, which is the direction this reader must not fail in."),
    "a base to the left binding the name safely":
        (REFUSAL,
         ("class Safe:",
          "    carrier = \"fine\"",
          "",
          "class Unsafe:",
          "    carrier = reading.UNREADABLE",
          "",
          "class Child(Safe, Unsafe):",
          "    def answer(self):",
          "        return self.carrier"),
         "answer", False,
         "the held-attribute side of the same ordering: Safe settles the name for the bases to"
         " its right, so Unsafe's refusal is never what the instance reads."),
    "a decorator alias made inside another scope":
        (REFUSAL,
         ("def sm(fn):",
          "    return fn",
          "",
          "def helper():",
          "    sm = staticmethod",
          "    return sm",
          "",
          "class Holder:",
          "    def carrier(self):",
          "        return reading.UNREADABLE",
          "    @sm",
          "    def consumer(self):",
          "        return self.carrier()"),
         "consumer", True,
         "a name bound inside a function is that function's. Reading it as the decorator"
         " everywhere suppresses a receiver the method really has, and the consumer leaves both"
         " inventories -- the widening cost more than it bought until it was scoped."),
    "a parameter spelled like a class":
        (REFUSAL,
         ("class Holder:",
          "    def carrier(self):",
          "        return reading.UNREADABLE",
          "",
          "def consumer(Holder):",
          "    return Holder.carrier()"),
         "consumer", False,
         "which object the parameter holds is a fact about the caller, so the qualifier is not"
         " the class of that name however alike they are spelled."),
    "a nested function declaring the receiver global":
        (REFUSAL,
         ("class Holder:",
          "    def carrier(self):",
          "        return reading.UNREADABLE",
          "    def outer(self):",
          "        def nested():",
          "            global self",
          "            return self.carrier()",
          "        return nested"),
         "outer.nested", False,
         "global self makes the name the module's, so the search for a receiver stops there"
         " rather than reaching the method around it."),
    "a diamond where a class beside another binds the name":
        (REFUSAL,
         ("class A:",
          "    def carrier(self):",
          "        return reading.UNREADABLE",
          "",
          "class B(A):",
          "    pass",
          "",
          "class C(A):",
          "    carrier = str",
          "",
          "class D(B, C):",
          "    def consumer(self):",
          "        return self.carrier()"),
         "consumer", False,
         "the order is D, B, C, A, so C settles the name before A is reached. Walking B's"
         " ancestors first reads A, which is a definition the instance never sees."),
    "a diamond where the class beside another binds nothing":
        (REFUSAL,
         ("class Root:",
          "    carrier = str",
          "",
          "class Left(Root):",
          "    pass",
          "",
          "class Right(Root):",
          "    def carrier(self):",
          "        return reading.UNREADABLE",
          "",
          "class Child(Left, Right):",
          "    def consumer(self):",
          "        return self.carrier()"),
         "consumer", True,
         "its pair, and the reason the order has to be computed rather than approximated:"
         " Root sits behind Right in the line, so stopping at Left's ancestor would lose the"
         " carrier the instance really reaches."),
    "a decorator name rebound above a later definition":
        (REFUSAL,
         ("prop = property",
          "",
          "class First:",
          "    @prop",
          "    def carrier(self):",
          "        return reading.UNREADABLE",
          "",
          "prop = staticmethod",
          "",
          "class Second:",
          "    @prop",
          "    def reader():",
          "        return reading.UNREADABLE",
          "    def consumer(self):",
          "        return self.reader"),
         "consumer", False,
         "below the rebinding @prop is staticmethod, so reading self.reader hands back the"
         " function rather than running it. Indexing it as a getter turns an ordinary attribute"
         " read into a call nobody performs."),
    "an attribute of a class here spelled like an answer":
        (REFUSAL,
         ("class Innocent:",
          "    UNREADABLE = \"fine\"",
          "",
          "def consumer():",
          "    return Innocent.UNREADABLE"),
         "consumer", False,
         "what a class written here binds is known, so the answer comes from that rather than"
         " from the spelling of the name. Otherwise every class with an attribute named like an"
         " answer owes a declaration for a value that is not one."),
    "the module attribute the answers really come from":
        (REFUSAL,
         ("def consumer():",
          "    return reading.UNREADABLE"),
         "consumer", True,
         "its pair: the ordinary spelling has to keep being read, so narrowing the qualifier"
         " must not have narrowed it to nothing."),
    "a class attribute rebound after the class statement":
        (REFUSAL,
         ("class Base:",
          "    carrier = reading.UNREADABLE",
          "",
          "class Child(Base):",
          "    def answer(self):",
          "        return self.carrier",
          "",
          "Child.carrier = \"fine\""),
         "answer", False,
         "an attribute assigned after the class statement takes part in lookup exactly as one"
         " written in the body does, so Child stands in front of what Base holds."),
    "a decorator alias written in a class body":
        (REFUSAL,
         ("def outer():",
          "    class Holder:",
          "        sm = staticmethod",
          "        def carrier(self):",
          "            return reading.UNREADABLE",
          "        @sm",
          "        def consumer(this):",
          "            return this.carrier()",
          "    return Holder"),
         "outer.consumer", False,
         "a decorator in a class body resolves names the body has already bound, so this is a"
         " real alias however the class is written. Scoping the search to the module alone lost"
         " it, which is the cost of the narrower rule and the reason the scope is the class"
         " body rather than the function around it."),
    "a receiver given another name":
        (REFUSAL,
         ("class Holder:",
          "    def carrier(self):",
          "        return reading.UNREADABLE",
          "    def consumer(self):",
          "        that = self",
          "        return that.carrier()"),
         "consumer", True,
         "that = self is the same object, so the call through it reaches the same class. Read"
         " as a class qualifier instead, the consumer leaves the inventory entirely."),
    "source text opened through a keyword argument":
        (TEXT,
         ("def helper():",
          "    return open(file=HERE).read()",
          "",
          "def consumer():",
          "    return helper()"),
         "consumer", True,
         "open takes its file by keyword as readily as by position, and which one a caller"
         " wrote is not a fact about what it opens."),
    "super() where another class stands beside this one":
        (REFUSAL,
         ("class A:",
          "    def carrier(self):",
          "        return \"fine\"",
          "",
          "class B(A):",
          "    pass",
          "",
          "def helper():",
          "    return reading.UNREADABLE",
          "",
          "class C(A):",
          "    carrier = helper",
          "",
          "class D(B, C):",
          "    def consumer(self):",
          "        return super().carrier()"),
         "consumer", True,
         "super() searches the rest of THIS class's line, D then B then C then A, so C answers."
         " Searching each base's own line in turn exhausts B's ancestors and reaches A first,"
         " which is a definition the call never makes."),
    "super() with nothing standing beside this one":
        (REFUSAL,
         ("class A:",
          "    def carrier(self):",
          "        return reading.UNREADABLE",
          "",
          "class D(A):",
          "    def consumer(self):",
          "        return super().carrier()"),
         "consumer", True,
         "its pair: the ordinary single-base super() has to keep reaching the base, so starting"
         " after the class must not have started after the whole line."),
    "a class reached through another name":
        (REFUSAL,
         ("class Holder:",
          "    @staticmethod",
          "    def carrier():",
          "        return reading.UNREADABLE",
          "",
          "Alias = Holder",
          "",
          "def consumer():",
          "    return Alias.carrier()"),
         "consumer", True,
         "Alias = Holder binds the class itself, so the call through it is the same call. A"
         " qualifier a parameter shadows is still not a class, which is the case beside this"
         " one and the reason the two are resolved in that order."),
    "a file object opened onto a name of its own":
        (TEXT,
         ("def helper():",
          "    stream = open(HERE)",
          "    return stream.read()",
          "",
          "def consumer():",
          "    return helper()"),
         "consumer", True,
         "the same read as open(HERE).read(), written in two steps. The helper was accounted"
         " either way because HERE is named in it; what the two-step form lost is the onward"
         " step, and with it every caller downstream."),
    "a method bound from super() rather than called there":
        (REFUSAL,
         ("class A:",
          "    def carrier(self):",
          "        return \"fine\"",
          "",
          "class B(A):",
          "    pass",
          "",
          "def helper():",
          "    return reading.UNREADABLE",
          "",
          "class C(A):",
          "    carrier = helper",
          "",
          "class D(B, C):",
          "    def consumer(self):",
          "        alias = super().carrier",
          "        return alias()"),
         "consumer", True,
         "binding what super() finds and calling it later is the same lookup as calling it"
         " there, so it takes the rest of this class's line too. The call form was corrected"
         " first and this one was left behind it, which is why both are written down."),
    "a decorator imported under another name":
        (REFUSAL,
         ("from builtins import property as prop",
          "",
          "class Holder:",
          "    @prop",
          "    def carrier(self):",
          "        return reading.UNREADABLE",
          "    def consumer(self):",
          "        return self.carrier"),
         "consumer", True,
         "an import binds the decorator as surely as an assignment does, and the descriptor it"
         " makes is the same one. Reading only assignments indexed the getter as an ordinary"
         " method and lost the reader."),
    "a decorator alias another class bound":
        (REFUSAL,
         ("def sm(fn):",
          "    return fn",
          "",
          "class Other:",
          "    sm = staticmethod",
          "",
          "class Holder:",
          "    def carrier(self):",
          "        return reading.UNREADABLE",
          "    @sm",
          "    def consumer(self):",
          "        return self.carrier()"),
         "consumer", True,
         "a class body binds for itself. Letting one class's sm = staticmethod decide what @sm"
         " means in the next suppresses a receiver that class never gave up, and the name Holder"
         " actually reaches is the module-level one."),
    "an attribute read off an instance of a class here":
        (REFUSAL,
         ("class Innocent:",
          "    NOT_READ = \"fine\"",
          "",
          "innocent = Innocent()",
          "",
          "def consumer():",
          "    return innocent.NOT_READ"),
         "consumer", False,
         "an instance answers to its class, and Innocent binds that name to something that is"
         " not an answer. Matching on the attribute name alone makes every object with an"
         " attribute spelled like one owe a declaration."),
    "an attribute read off an instance of a class that holds one":
        (REFUSAL,
         ("class Holder:",
          "    unread = reading.UNREADABLE",
          "",
          "holder = Holder()",
          "",
          "def consumer():",
          "    return holder.unread"),
         "consumer", True,
         "its pair, and it was wrong in the other direction: asking the class answers both, so"
         " the instance that really does hold a refusal stops being missed at the same time."),
    "a read taken on what a call other than open returned":
        (TEXT,
         ("def make(thing):",
          "    return thing",
          "",
          "def helper():",
          "    return make(HERE).read()",
          "",
          "def consumer():",
          "    return helper()"),
         "consumer", False,
         "the helper is accounted either way, because HERE is named in it. What make answers"
         " with is a question about the run, so the read taken on it says nothing about source"
         " text and every caller downstream would be claimed on a guess."),
    "a descriptor made by calling property":
        (REFUSAL,
         ("class Holder:",
          "    def carrier(self):",
          "        return completion.NOT_READ",
          "    alias = property(carrier)",
          "    def consumer(self):",
          "        return self.alias"),
         "consumer", True,
         "property(carrier) makes the same descriptor the decorator does, so reading self.alias"
         " runs the getter. Indexed only from decorators, the real consumer leaves both"
         " inventories."),
    "a base named through an alias":
        (REFUSAL,
         ("class Base:",
          "    def carrier(self):",
          "        return reading.UNREADABLE",
          "",
          "Alias = Base",
          "",
          "class Child(Alias):",
          "    def consumer(self):",
          "        return self.carrier()"),
         "consumer", True,
         "the line a name is searched in is built from what the bases NAME, not from how they"
         " are spelled. Left unresolved, Alias is an unknown class that ends the line and the"
         " whole inheritance below it goes missing."),
    "a decorator name the module defines itself":
        (REFUSAL,
         ("def staticmethod(fn):",
          "    return fn",
          "",
          "class Holder:",
          "    def carrier(self):",
          "        return reading.UNREADABLE",
          "    @staticmethod",
          "    def consumer(self):",
          "        return self.carrier()"),
         "consumer", True,
         "a binding above the use wins over the builtin's own name. Reading this @staticmethod"
         " as the builtin suppresses a receiver the method really has, which is the same cost"
         " the alias cases pay in the other direction."),
    "a parameter spelled like a refusal global":
        (REFUSAL,
         ("def consumer(CHANGED):",
          "    return CHANGED"),
         "consumer", False,
         "the parameter is that function's, so the global of that spelling is not what this"
         " reads and the place settles for nothing."),
    "the refusal global itself":
        (REFUSAL,
         ("def consumer():",
          "    return CHANGED"),
         "consumer", True,
         "its pair: a scope that does NOT bind the name reads the global, so honouring shadows"
         " must not have stopped the ordinary reading."),
    "a parameter spelled like a source handle":
        (TEXT,
         ("def consumer(HERE):",
          "    return HERE.read_text()"),
         "consumer", False,
         "the same shadow rule on the source side: which file the parameter holds is a fact"
         " about the caller, so a read taken on it says nothing about this module's handle."),
    "a source handle held by an instance":
        (TEXT,
         ("class Holder:",
          "    path = HERE",
          "",
          "holder = Holder()",
          "",
          "def consumer():",
          "    return holder.path.read_text()"),
         "consumer", True,
         "an instance answers to its class here too, so what holder.path names is what Holder"
         " binds."),
    "a read taken through the handle's own open":
        (TEXT,
         ("def helper():",
          "    return HERE.open().read()",
          "",
          "def consumer():",
          "    return helper()"),
         "consumer", True,
         "the handle is the receiver of open rather than its argument, and it is the same read"
         " either way."),
    "a callable made by calling staticmethod":
        (REFUSAL,
         ("class Holder:",
          "    def carrier(self):",
          "        return reading.UNREADABLE",
          "    alias = staticmethod(carrier)",
          "    def consumer(self):",
          "        return self.alias()"),
         "consumer", True,
         "the constructor form of the descriptor, as property(carrier) is on the reading side:"
         " it binds the same function under a second name, so calling through it runs the one"
         " that was wrapped."),
    "a local of the scope around a nested one":
        (REFUSAL,
         ("def outer():",
          "    accepted = reading.UNREADABLE",
          "    def consumer():",
          "        return accepted",
          "    def user():",
          "        return consumer()",
          "    return user"),
         "outer.user", True,
         "a nested function reads what the function around it bound, so the helper hands the"
         " same thing on and its caller takes it. Consulting only the nested scope's own names"
         " ends the chain at the first closure."),
    "a decorator alias bound under an if":
        (REFUSAL,
         ("def sm(fn):",
          "    return fn",
          "",
          "if False:",
          "    sm = staticmethod",
          "",
          "class Holder:",
          "    def carrier(self):",
          "        return reading.UNREADABLE",
          "    @sm",
          "    def consumer(self):",
          "        return self.carrier()"),
         "consumer", True,
         "whether that binding happened is a question about the run, so it cannot be the"
         " definite meaning of the name. Taking it suppresses a receiver the method really has"
         " and the carrier call goes with it."),
    "a parameter spelled like a refusal module":
        (REFUSAL,
         ("def consumer(reading):",
          "    return reading.UNREADABLE"),
         "consumer", False,
         "the qualifier is the parameter's, so an imported module of that name is not what this"
         " reads and what the object holds is the caller's fact."),
    "the refusal module itself":
        (REFUSAL,
         ("def consumer():",
          "    return reading.UNREADABLE"),
         "consumer", True,
         "its pair: a scope that does not bind the qualifier reads the module, so honouring the"
         " shadow must not have stopped the ordinary reading."),
    "a class alias bound inside a function":
        (REFUSAL,
         ("class Base:",
          "    def carrier(self):",
          "        return reading.UNREADABLE",
          "",
          "class Clean:",
          "    pass",
          "",
          "def unrelated():",
          "    Alias = Base",
          "    return Alias",
          "",
          "Alias: type = Clean",
          "",
          "class Child(Alias):",
          "    def consumer(self):",
          "        return self.carrier()"),
         "consumer", False,
         "the name a base list reads is the module's, and an annotated binding is a binding."
         " A name bound inside a function is that function's and cannot say what a class"
         " written at module level inherits."),
    "a class alias bound at module level":
        (REFUSAL,
         ("class Base:",
          "    def carrier(self):",
          "        return reading.UNREADABLE",
          "",
          "Alias = Base",
          "",
          "class Child(Alias):",
          "    def consumer(self):",
          "        return self.carrier()"),
         "consumer", True,
         "its pair: the module's own alias really is what the base names, so scoping the scan"
         " must not have scoped it to nothing."),
    "a held name reached in three steps":
        (REFUSAL,
         ("class Cases:",
          "    def setUp(self):",
          "        self.a = self.b",
          "        self.b = self.c",
          "        self.c = \"not_verified\"",
          "    def consumer(self):",
          "        return self.a"),
         "consumer", True,
         "the outer pass has to keep growing until nothing moves. Sharing the sets with the"
         " round before adds a name to both, so the convergence test reads as settled while the"
         " pass is still finding things -- a fixpoint that stops early, which looks like a pass"
         " and is an omission."),
    "a question asked about a handle rather than a read of it":
        (TEXT,
         ("def helper():",
          "    stream = open(HERE)",
          "    return stream.readable()",
          "",
          "def consumer():",
          "    return helper()"),
         "consumer", False,
         "readable() answers whether the stream can be read, which is a boolean about the"
         " handle and never the text behind it, so nothing is handed on."),
    "a read of a handle that really returns its lines":
        (TEXT,
         ("def helper():",
          "    stream = open(HERE)",
          "    return stream.readlines()",
          "",
          "def consumer():",
          "    return helper()"),
         "consumer", True,
         "its pair: the read prefix still recognises a read, so naming the one question that"
         " is not one must not have narrowed the rule to a list of blessed spellings."),
    "a decorator another object owns":
        (REFUSAL,
         ("class Decorators:",
          "    @staticmethod",
          "    def staticmethod(fn):",
          "        return fn",
          "",
          "class Holder:",
          "    def carrier(self):",
          "        return reading.UNREADABLE",
          "    @Decorators.staticmethod",
          "    def consumer(self):",
          "        return self.carrier()"),
         "consumer", True,
         "a qualified decorator belongs to whatever owns it. Reducing every dotted name to its"
         " last part reads this as the builtin and takes away a receiver the method has."),
    "a receiver alias made by the scope around a nested one":
        (REFUSAL,
         ("class Holder:",
          "    def carrier(self):",
          "        return reading.UNREADABLE",
          "    def outer(self):",
          "        that = self",
          "        def consumer():",
          "            return that.carrier()",
          "        return consumer()"),
         "outer.consumer", True,
         "the closure reads the alias the method around it made, so the aliases have to be"
         " gathered from every scope between the receiver and the use, not from the innermost"
         " one alone."),
    "a parameter spelled like an instance bound at module level":
        (REFUSAL,
         ("class Holder:",
          "    unread = reading.UNREADABLE",
          "",
          "holder = Holder()",
          "",
          "def consumer(holder):",
          "    return holder.unread"),
         "consumer", False,
         "the shadow has to be consulted BEFORE the instance table, or resolving the spelling"
         " to its class answers with a refusal the caller's object never held."),
    "a handle name another scope derived":
        (TEXT,
         ("def helper():",
          "    stream = open(HERE)",
          "    return stream.read()",
          "",
          "def consumer(stream):",
          "    return stream.read()"),
         "consumer", False,
         "a derived handle is one in the scope that derived it and the scopes inside that."
         " Read file-wide, an unrelated parameter of the same spelling reads as this module's"
         " source and owes a declaration for a file it never opens."),
    "a local of an enclosing scope spelled like a refusal global":
        (REFUSAL,
         ("def outer():",
          "    CHANGED = \"fine\"",
          "    def consumer():",
          "        return CHANGED",
          "    return consumer"),
         "outer.consumer", False,
         "the closure reads the enclosing local, so the shadow has to be looked for in every"
         " scope around the use and not in the innermost one alone."),
    "a class alias bound in the function the class is written in":
        (REFUSAL,
         ("class Base:",
          "    def carrier(self):",
          "        return reading.UNREADABLE",
          "",
          "def outer():",
          "    Alias = Base",
          "    class Child(Alias):",
          "        def consumer(self):",
          "            return self.carrier()",
          "    return Child"),
         "outer.consumer", True,
         "a class written in a function really does see that function's names, so scoping the"
         " aliases to the module body alone lost the inheritance instead of only the pollution"
         " it was meant to stop."),
    "a handle bound by a with statement":
        (TEXT,
         ("def helper():",
          "    with open(HERE) as stream:",
          "        return stream.read()",
          "",
          "def consumer():",
          "    return helper()"),
         "consumer", True,
         "with binds the name to what the expression answered exactly as an assignment does,"
         " and it is the ordinary way to open a file."),
    "an opener the module defines itself":
        (TEXT,
         ("def open(value):",
          "    return value",
          "",
          "def helper():",
          "    return open(HERE).read()",
          "",
          "def consumer():",
          "    return helper()"),
         "consumer", False,
         "a module that defines its own open has shadowed the builtin, so what that call"
         " answers with is a question about the run rather than a file's text."),
    "a parameter spelled like an instance holding a handle":
        (TEXT,
         ("class Holder:",
          "    path = HERE",
          "",
          "holder = Holder()",
          "",
          "def consumer(holder):",
          "    return holder.path.read_text()"),
         "consumer", False,
         "the source side owes the same ordering the refusal side has: the shadow is consulted"
         " before the instance table, or the parameter's spelling answers with this module's"
         " handle."),
    "a descriptor qualified by the module that exports it":
        (REFUSAL,
         ("import functools",
          "",
          "class Holder:",
          "    @functools.cached_property",
          "    def carrier(self):",
          "        return reading.UNREADABLE",
          "    def consumer(self):",
          "        return self.carrier"),
         "consumer", True,
         "an imported owner is not a user-defined one: functools.cached_property is the"
         " descriptor it is spelled like, and rejecting every qualified decorator loses every"
         " reader of the getter."),
    "an instance the scope itself built":
        (REFUSAL,
         ("class Holder:",
          "    unread = reading.UNREADABLE",
          "",
          "def consumer():",
          "    holder = Holder()",
          "    return holder.unread"),
         "consumer", True,
         "a name bound to a constructor in the scope that reads it IS the instance, so the"
         " shadow rule must not discard it along with a parameter of the same spelling."),
    "a refusal imported by name":
        (REFUSAL,
         ("from completion import NOT_READ",
          "",
          "def consumer():",
          "    return NOT_READ"),
         "consumer", True,
         "the import makes the answer readable here without an attribute to match, and it is an"
         " ordinary refactor rather than a form out of reach."),
    "two scopes binding one spelling to different classes":
        (REFUSAL,
         ("class Clean:",
          "    unread = \"fine\"",
          "",
          "class Bad:",
          "    unread = reading.UNREADABLE",
          "",
          "def first():",
          "    holder = Clean()",
          "    return holder.unread",
          "",
          "def second():",
          "    holder = Bad()",
          "    return holder.unread"),
         "second", True,
         "which instance a spelling names depends on where it is read, so the table is keyed by"
         " scope as well. Keeping the first binding answers the second function with the wrong"
         " class and loses the place that really does settle."),
    "a decorator qualified by a module nothing here can read":
        (REFUSAL,
         ("import arbitrary_decorators as deco",
          "",
          "class Holder:",
          "    def carrier(self):",
          "        return reading.UNREADABLE",
          "    @deco.staticmethod",
          "    def consumer(self):",
          "        return self.carrier()"),
         "consumer", True,
         "an imported owner is asked what it exports, and only modules already imported here"
         " are asked -- a module named in the source under analysis is not something to import"
         " in order to answer a question about the text. One that cannot be read answers no,"
         " which keeps the receiver and errs towards reporting."),
    "a class-body binding written under an if":
        (REFUSAL,
         ("def outer():",
          "    class Holder:",
          "        if True:",
          "            CHANGED = \"fine\"",
          "    def consumer():",
          "        return CHANGED",
          "    return consumer"),
         "outer.consumer", True,
         "a statement under an if in a class body is still the class's. Marking only the body's"
         " immediate statements left the nested one looking like a local of the function."),
    "a class alias the scope around the call bound":
        (REFUSAL,
         ("class Holder:",
          "    @staticmethod",
          "    def carrier():",
          "        return reading.UNREADABLE",
          "",
          "def outer():",
          "    Alias = Holder",
          "    def consumer():",
          "        return Alias.carrier()",
          "    return consumer"),
         "outer.consumer", True,
         "a name the enclosing scope binds to a class IS that class, which the shadow rule must"
         " not undo -- an alias is a local binding too, and the two rules pull opposite ways on"
         " the same name."),
    "a parameter of an enclosing scope spelled like a class":
        (REFUSAL,
         ("class Holder:",
          "    @staticmethod",
          "    def carrier():",
          "        return reading.UNREADABLE",
          "",
          "def outer(Holder):",
          "    def consumer():",
          "        return Holder.carrier()",
          "    return consumer"),
         "outer.consumer", False,
         "its pair: the closure reads the parameter the function around it was given, so the"
         " qualifier is not the class however alike they are spelled."),
    "a descriptor constructor another object owns":
        (REFUSAL,
         ("class Decorators:",
          "    @staticmethod",
          "    def property(fn):",
          "        return fn",
          "",
          "class Holder:",
          "    def carrier(self):",
          "        return reading.UNREADABLE",
          "    alias = Decorators.property(carrier)",
          "    def consumer(self):",
          "        return self.alias"),
         "consumer", False,
         "property(carrier) and @property are one question, so the constructor is resolved"
         " through its owner the way the decorator already was. Reading somebody's identity"
         " function as the descriptor makes an attribute read look like a call."),
    "a handle opened by a parameter spelled like the opener":
        (TEXT,
         ("def helper(open):",
          "    stream = open(HERE)",
          "    return stream.read()",
          "",
          "def consumer():",
          "    return helper()"),
         "consumer", False,
         "the name propagation recognised an opener by spelling while the matcher beside it"
         " resolved one lexically. Both ask the same question now, so a parameter called open"
         " does not make its caller read this module's source."),
    "a method called on an instance the scope built":
        (REFUSAL,
         ("class Holder:",
          "    def carrier(self):",
          "        return reading.UNREADABLE",
          "",
          "def consumer():",
          "    holder = Holder()",
          "    return holder.carrier()"),
         "consumer", True,
         "the instance table already knew what holder holds; the call resolution was the one"
         " place not asking it, so the qualifier was searched for as a class of that spelling"
         " and the consumer went missing."),
    "a base named through an enclosing class body's alias":
        (REFUSAL,
         ("class Base:",
          "    def carrier(self):",
          "        return reading.UNREADABLE",
          "",
          "class Outer:",
          "    Alias = Base",
          "    class Child(Alias):",
          "        def consumer(self):",
          "            return self.carrier()"),
         "consumer", True,
         "a base name is resolved in the enclosing CLASS namespace while the inner class runs,"
         " which the function scope alone does not say. Without it the line omits Base and the"
         " whole inheritance below it goes with it."),
    "a decorator alias an enclosing function bound":
        (REFUSAL,
         ("def outer():",
          "    prop = property",
          "    class Holder:",
          "        @prop",
          "        def carrier(self):",
          "            return reading.UNREADABLE",
          "        def consumer(self):",
          "            return self.carrier",
          "    return Holder"),
         "outer.consumer", True,
         "a class body closes over the function around it, so that binding is a real alias"
         " there. Its pair is the function-local alias that must NOT leak into an unrelated"
         " class: the difference is whether the class is written inside that function."),
    "a handle name copied in a scope that did not derive it":
        (TEXT,
         ("def helper():",
          "    stream = open(HERE)",
          "    return stream.read()",
          "",
          "def consumer(stream):",
          "    alias = stream",
          "    return alias.read()"),
         "consumer", False,
         "the scope check was on the matcher and not on the propagation, so copying an"
         " unrelated parameter to a second name minted a handle the module never declared."),
    "an open another object owns":
        (TEXT,
         ("import innocent",
          "",
          "def helper():",
          "    return innocent.open(HERE).read()",
          "",
          "def consumer():",
          "    return helper()"),
         "consumer", False,
         "only a handle's own open opens the file this module asks about. Accepting any"
         " qualified name spelled open reads somebody's method as the builtin, and HERE.open()"
         " beside it is the case that must keep working."),
    "an instance built through a class alias":
        (REFUSAL,
         ("class Holder:",
          "    unread = reading.UNREADABLE",
          "",
          "Alias = Holder",
          "",
          "def consumer():",
          "    holder = Alias()",
          "    return holder.unread"),
         "consumer", True,
         "the constructor is a name like any other, so it is resolved where it is written"
         " before the instance is recorded."),
    "an instance built by another module's class of the same name":
        (REFUSAL,
         ("import innocent",
          "",
          "class Holder:",
          "    unread = reading.UNREADABLE",
          "",
          "def consumer():",
          "    holder = innocent.Holder()",
          "    return holder.unread"),
         "consumer", False,
         "its pair: a qualified constructor is that owner's class, not this file's, whatever"
         " its last part is spelled."),
    "a method bound through a class alias in a function":
        (REFUSAL,
         ("class Holder:",
          "    @staticmethod",
          "    def carrier():",
          "        return reading.UNREADABLE",
          "",
          "def outer():",
          "    Alias = Holder",
          "    alias = Alias.carrier",
          "    return alias()"),
         "outer", True,
         "binding a method through an alias is the same lookup as calling it through one, so"
         " the attribute path consults the scope-aware table the call path already did."),
    "a decorator imported from a module that does not export it":
        (REFUSAL,
         ("from innocent import staticmethod as sm",
          "",
          "class Holder:",
          "    def carrier(self):",
          "        return reading.UNREADABLE",
          "    @sm",
          "    def consumer(self):",
          "        return self.carrier()"),
         "consumer", True,
         "an import records a spelling, not a meaning. The owner is asked whether it really"
         " exports the builtin, and one nothing here can read reaches nothing -- which keeps"
         " the receiver the method has."),
    "two classes of one spelling in different functions":
        (REFUSAL,
         ("def first():",
          "    class Holder:",
          "        def carrier(self):",
          "            return \"fine\"",
          "        def consumer(self):",
          "            return self.carrier()",
          "    return Holder",
          "",
          "def second():",
          "    class Holder:",
          "        def carrier(self):",
          "            return reading.UNREADABLE",
          "        def consumer(self):",
          "            return self.carrier()",
          "    return Holder"),
         "second.consumer", True,
         "two classes, and one key for both answers in both directions at once: the second's"
         " consumer is lost and the first's would be invented. This was declared a limit of the"
         " reader for a long while; it was a limit of cost, and only a form that cannot be"
         " widened may be declared."),
    "an attribute assigned through a qualified class owner":
        (REFUSAL,
         ("class Outer:",
          "    class Inner:",
          "        def consumer(self):",
          "            return self.unread",
          "",
          "Outer.Inner.unread = reading.UNREADABLE"),
         "consumer", True,
         "the owner has to be resolved to the same scoped key the other tables use. Reduced to"
         " its last part it stores the attribute under a key nothing looks up, which is the"
         " bill for keying classes by scope and has to be paid everywhere at once."),
    "a nested parameter spelled like the receiver alias":
        (REFUSAL,
         ("class Holder:",
          "    def carrier(self):",
          "        return reading.UNREADABLE",
          "    def outer(self):",
          "        that = self",
          "        def inner(that):",
          "            return that.carrier()",
          "        return inner"),
         "outer.inner", False,
         "the alias is inherited by the scopes inside the one that made it, and a nested"
         " parameter of that spelling is the inner function's own. Its pair is the ordinary"
         " closure that DOES read the enclosing alias, and the scope that made it keeps it or"
         " nothing would inherit at all."),
    "a receiver alias bound with an annotation":
        (REFUSAL,
         ("class Holder:",
          "    def carrier(self):",
          "        return reading.UNREADABLE",
          "    def consumer(self):",
          "        that: Holder = self",
          "        return that.carrier()"),
         "consumer", True,
         "the ordinary typed spelling of the same line. Reading only plain assignments made an"
         " annotation a form nobody looked at, which is why this goes through the same binding"
         " abstraction as everything else."),
    "a method written under a class-body if":
        (REFUSAL,
         ("class Holder:",
          "    if True:",
          "        def consumer(self):",
          "            CHANGED = \"fine\"",
          "            return CHANGED"),
         "consumer", False,
         "a def opens a scope whose names are its own wherever in the body it is written. The"
         " class-body walk promised to stop at definitions and did not, so the method's own"
         " local was read as the class's and the module constant was reported instead."),
    "a global declared by an enclosing scope":
        (REFUSAL,
         ("def outer():",
          "    global CHANGED",
          "    CHANGED = reading.UNREADABLE",
          "    def consumer():",
          "        return CHANGED",
          "    return consumer"),
         "outer.consumer", True,
         "the declaration makes the assignment the module's, so every scope inside reads that"
         " rather than a local. Exempting only the consumer's own scope left the enclosing"
         " assignment looking like a shadow."),
    "a helper handing an aliased refusal onward":
        (REFUSAL,
         ("settled = reading.UNREADABLE",
          "",
          "def consumer():",
          "    return settled"),
         "consumer", True,
         "a function that hands the thing on is a place the thing reaches, whether or not"
         " anybody calls it here. Reporting only its callers loses the one doing the handing,"
         " and a helper nobody calls yet is lost entirely."),
    "a name a function deletes":
        (REFUSAL,
         ("def consumer():",
          "    del CHANGED",
          "    return CHANGED"),
         "consumer", False,
         "del makes the name local for the whole function, so the later read raises rather than"
         " reaching the module's. A binding is not only a Store."),
    "a base named by its qualified owner":
        (REFUSAL,
         ("class First:",
          "    class Base:",
          "        bad = str",
          "",
          "class Second:",
          "    class Base:",
          "        def bad(self):",
          "            return reading.UNREADABLE",
          "",
          "class Child(Second.Base):",
          "    def consumer(self):",
          "        return self.bad()"),
         "consumer", True,
         "Second.Base is that class and not whichever Base is found first. Stripping the owner"
         " sends the line to a different class of the same terminal name, which is what keying"
         " classes by scope was for and has to reach the base list too."),
    "an attribute read through a qualified class owner":
        (REFUSAL,
         ("class Outer:",
          "    class Inner:",
          "        unread = reading.UNREADABLE",
          "",
          "def consumer():",
          "    return Outer.Inner.unread"),
         "consumer", True,
         "the reading side of the owner the assignment side already resolved. Reduced to Inner"
         " it looks up a key the held table does not have, and the consumer goes unreported"
         " while the attribute itself is."),
    "a qualified attribute an unrelated module exports":
        (REFUSAL,
         ("import innocent",
          "",
          "def consumer():",
          "    return innocent.NOT_READ"),
         "consumer", False,
         "the owner has to be a module that really exports an answer. Matching the attribute"
         " name alone makes any object with a same-named attribute hold one, which is the"
         " direct-import case over again on the qualified side."),
    "a qualified attribute the answers really come from":
        (REFUSAL,
         ("def consumer():",
          "    return completion.NOT_READ"),
         "consumer", True,
         "its pair: the ordinary qualified spelling has to keep being read, so validating the"
         " owner must not have narrowed it to nothing."),
    "a class body binding inside a function":
        (REFUSAL,
         ("def outer():",
          "    class Holder:",
          "        CHANGED = \"fine\"",
          "    def consumer():",
          "        return CHANGED",
          "    return consumer"),
         "outer.consumer", True,
         "a class body is not the function around it. The name it binds creates no local there,"
         " so reading it as a shadow drops the module constant the closure really sees."),
    "a refusal name imported from a module that does not export one":
        (REFUSAL,
         ("from innocent import NOT_READ",
          "",
          "def consumer():",
          "    return NOT_READ"),
         "consumer", False,
         "the spelling occurring in some other module's inventory is not the answer here. The"
         " owner is asked whether it exports it, and one nothing here has imported cannot say"
         " yes."),
    "a refusal name imported from the module that does export it":
        (REFUSAL,
         ("from completion import NOT_READ",
          "",
          "def consumer():",
          "    return NOT_READ"),
         "consumer", True,
         "its pair: the ordinary import really does make the answer readable here, so"
         " validating the owner must not have rejected every one."),
    "a parameter spelled like the opener":
        (TEXT,
         ("def helper(open):",
          "    return open(HERE).read()",
          "",
         "def consumer():",
         "    return helper()"),
        "consumer", False,
        "deciding once per module that open is the builtin misses the scope that binds it"
        " itself, and what a caller's open answers with is not this module's source."),
    "a name an inner parameter binds under an enclosing global":
        (REFUSAL,
         ("def outer():",
          "    global CHANGED",
          "    CHANGED = reading.UNREADABLE",
          "    def consumer(CHANGED):",
          "        return CHANGED",
          "    return consumer"),
         "outer.consumer", False,
         "the global declaration belongs to outer, and a parameter of the function inside it"
         " binds the name again. Reading the outermost declaration first hands the inner scope"
         " a constant its own signature has already taken."),
    "a name only the enclosing global binds":
        (REFUSAL,
         ("def outer():",
          "    global CHANGED",
          "    CHANGED = reading.UNREADABLE",
          "    def consumer():",
          "        return CHANGED",
          "    return consumer"),
         "outer.consumer", True,
         "its pair: with nothing inner binding the name, the closure really does read what the"
         " declaration made the module's, so resolving innermost-first must not have stopped the"
         " lookup from reaching outward at all."),
    "a constructor qualified by a class written here":
        (REFUSAL,
         ("class Outer:",
          "    class Holder:",
          "        unread = reading.UNREADABLE",
          "",
          "def consumer():",
          "    holder = Outer.Holder()",
          "    return holder.unread"),
         "consumer", True,
         "the instance side of the qualified owner the attribute side already resolved. Reduced"
         " to its terminal name the call looks up a key the class table does not have, and the"
         " name it binds stops answering to the class it was built from."),
    "the opener imported under another name":
        (TEXT,
         ("from builtins import open as fopen",
          "",
          "def helper():",
          "    stream = fopen(HERE)",
          "    return stream.read()",
          "",
          "def consumer():",
          "    return helper()"),
         "consumer", True,
         "open is a name and an import can rebind it. Matching the word rather than the object"
         " leaves the call unrecognised, so the handle is never derived and the helper stops"
         " counting as one that hands its caller source text."),
    "the opener reached through a module that holds it":
        (TEXT,
         ("import io",
          "",
          "def helper():",
          "    return io.open(HERE).read()",
          "",
          "def consumer():",
          "    return helper()"),
         "consumer", True,
         "io.open IS builtins.open, one function under a second name. Requiring the qualifier to"
         " be a handle is right for a file object's own open method and wrong for the module"
         " that simply holds the builtin."),
    "a module-level name bound to the opener":
        (TEXT,
         ("fopen = open",
          "",
          "def helper():",
          "    return fopen(HERE).read()",
          "",
          "def consumer():",
          "    return helper()"),
         "consumer", True,
         "the same rebinding written as an assignment rather than as an import. A derivation"
         " that follows one and not the other sees the file opened in one module and misses it"
         " in the module beside it."),
    "an opener name imported from a module whose open is a different function":
        (TEXT,
         ("from os import open as fopen",
          "",
          "def helper():",
          "    return fopen(HERE).read()",
          "",
          "def consumer():",
          "    return helper()"),
         "consumer", False,
         "the pair the alias fix must not break: os.open answers with a descriptor and has no"
         " read of its own, so Python reaches no source text here either. The owner is asked"
         " whether what it exports IS the builtin, rather than whether it is spelled open."),
    "an open reached through a module that holds a different one":
        (TEXT,
         ("import os",
          "",
          "def helper():",
          "    return os.open(HERE).read()",
          "",
         "def consumer():",
         "    return helper()"),
        "consumer", False,
        "its qualified pair. Accepting any module attribute spelled open would make the"
        " descriptor call a source read, which is the direct-import case over again on the"
        " qualified side."),
    "a name a nested def binds in the function around it":
        (REFUSAL,
         ("def outer():",
          "    def CHANGED():",
          "        return \"fine\"",
          "    def consumer():",
          "        return CHANGED",
          "    return consumer"),
         "outer.consumer", False,
         "a def takes its name in the scope that WRITES it, so the closure under it reads that"
         " function rather than the module constant. Filing the name under the scope the"
         " definition OPENS makes a function shadow itself and leaves the scope that really"
         " binds it holding nothing."),
    "a name a nested class binds in the function around it":
        (REFUSAL,
         ("def outer():",
          "    class CHANGED:",
          "        pass",
          "    def consumer():",
          "        return CHANGED",
          "    return consumer"),
         "outer.consumer", False,
         "the same binding written as a class. A reader that follows one and not the other"
         " answers two ways about one rule, which is how these tables drift apart."),
    "a name a def binds where the read is written beside it":
        (REFUSAL,
         ("def outer():",
          "    def CHANGED():",
          "        return \"fine\"",
          "    return CHANGED"),
         "outer", False,
         "the read in the defining scope rather than in a closure under it. The two tables"
         " disagreed about exactly this: one left definitions out and the other filed them one"
         " scope too deep, so the name was reachable from neither position or from both."),
    "a name a def binds under an if in the function around it":
        (REFUSAL,
         ("def outer():",
          "    if True:",
          "        def CHANGED():",
          "            return \"fine\"",
          "    def consumer():",
          "        return CHANGED",
          "    return consumer"),
         "outer.consumer", False,
         "a def under an if is still written in the function, so the binding has to be found"
         " through the statements that open no scope. Reading only a body's direct statements"
         " would cover the case above and miss this one."),
    "a name nothing between the read and the module binds":
        (REFUSAL,
         ("def outer():",
          "    def consumer():",
          "        return CHANGED",
         "    return consumer"),
        "outer.consumer", True,
        "the pair the four above must not break: with no definition taking the name, the"
        " closure really does read the module constant, and counting definitions as bindings"
        " sits one line from stopping every outward lookup."),
    "a qualified owner an import rebinds to another module":
        (REFUSAL,
         ("import json as reading",
          "",
          "def consumer():",
          "    return reading.UNREADABLE"),
         "consumer", False,
         "the qualifier's spelling is not the module it names. Comparing it with the names this"
         " module happens to bind lets any source borrow a spelling and be read as the owner,"
         " which is the same-named-attribute case one level up."),
    "a qualified owner reached under a name the source chose":
        (REFUSAL,
         ("import crw_runtime.reading as r",
          "",
          "def consumer():",
          "    return r.UNREADABLE"),
         "consumer", True,
         "its other half: the owner really is that module, written under the name the source"
         " gave it. Matching spellings misses this one and accepts the one above, and both"
         " stop once the qualifier is resolved through the source's own imports."),
    "a decorator alias written as an annotated binding":
        (REFUSAL,
         ("prop: object = property",
          "",
          "class Holder:",
          "    @prop",
          "    def carrier(self):",
          "        return reading.UNREADABLE",
          "    def answer(self):",
          "        return self.carrier"),
         "answer", True,
         "prop: object = property is the same binding with a type written on it. Reading only"
         " the bare assignment leaves the alias reaching nothing, so the class is indexed as"
         " holding an ordinary method and the descriptor read is never seen."),
    "a decorator alias written as a bare binding":
        (REFUSAL,
         ("prop = property",
          "",
          "class Holder:",
          "    @prop",
          "    def carrier(self):",
          "        return reading.UNREADABLE",
          "    def answer(self):",
          "        return self.carrier"),
         "answer", True,
         "its pair: the unannotated form was already right, so admitting the annotated one must"
         " not have changed what the bare one binds."),
    "an annotated alias to something that is not a descriptor":
        (REFUSAL,
         ("prop: object = staticmethod",
          "",
          "class Holder:",
          "    @prop",
          "    def carrier():",
          "        return reading.UNREADABLE",
          "    def answer(self):",
          "        return self.carrier"),
         "answer", False,
         "the other pair: an annotation does not make a binding a descriptor. Reading"
         " self.carrier through a staticmethod alias hands back the function rather than"
         " running it, so inventing a call here would be the borrowed answer again."),
    "a refusal a wildcard import brings in":
        (REFUSAL,
         ("from crw_runtime.reading import *",
          "",
          "def consumer():",
          "    return UNREADABLE"),
         "consumer", True,
         "a star is not a name. Treating it as one leaves the imported answers empty and the"
         " consumer disappears, though Python really does bind the refusal here."),
    "a wildcard import from a module that exports no answer":
        (REFUSAL,
         ("from json import *",
          "",
         "def consumer():",
         "    return UNREADABLE"),
        "consumer", False,
        "its pair: the owner is still asked what it exports. Expanding a star into every bare"
        " name in sight would make the spelling alone enough, which is what asking the owner"
        " exists to prevent."),
    "a refusal module a function imports for itself":
        (REFUSAL,
         ("def consumer():",
          "    import crw_runtime.reading as r",
          "    return r.UNREADABLE"),
         "consumer", True,
         "the scope binds the qualifier, and what binds it is an import of the module that owns"
         " the answer. Rejecting every local binding of a qualifier throws this one away with"
         " the impostors, and the declaration it owes disappears."),
    "a qualifier a function binds to something of its own":
        (REFUSAL,
         ("def consumer():",
          "    reading = object()",
          "    return reading.UNREADABLE"),
         "consumer", False,
         "its pair: a local binding that is NOT an import of the owner is still an impostor"
         " wearing the spelling, so admitting the import above must not have admitted"
         " everything a scope binds."),
    "a nested class answering for a spelling the module imports":
        (REFUSAL,
         ("from innocent import Holder",
          "",
          "class Outer:",
          "    class Holder:",
          "        def carrier(self):",
          "            return reading.UNREADABLE",
          "",
          "def consumer():",
          "    holder = Holder()",
          "    return holder.carrier()"),
         "consumer", False,
         "a bare Holder at module level is whatever the module binds, and here that is the"
         " import. Registering a nested class under the scope its OWNER is written in puts it"
         " into the module's namespace, where it answers for a name it does not have."),
    "an opener a function binds for itself":
        (TEXT,
         ("def helper():",
          "    fopen = open",
          "    stream = fopen(HERE)",
          "    return stream.read()",
          "",
          "def consumer():",
          "    return helper()"),
         "consumer", True,
         "the rebinding written inside the function rather than beside it. Following only the"
         " module's own statements leaves the local alias unrecognised, and the scope that"
         " wrote it is then read as having shadowed the opener it just took."),
    "a receiver alias a parameter in between takes back":
        (REFUSAL,
         ("class Holder:",
          "    def carrier(self):",
          "        return reading.UNREADABLE",
          "    def outer(self):",
          "        that = self",
          "        def middle(that):",
          "            def consumer():",
          "                return that.carrier()",
          "            return consumer",
          "        return middle"),
         "outer.middle.consumer", False,
         "the alias is made in outer and read in its grandchild, and the function between binds"
         " the name as a parameter. Checking only the reading scope carries the receiver past a"
         " scope that took the name, and what the grandchild closes over is its caller's"
         " object."),
    "a receiver alias nothing in between takes":
        (REFUSAL,
         ("class Holder:",
          "    def carrier(self):",
          "        return reading.UNREADABLE",
          "    def outer(self):",
          "        that = self",
          "        def middle():",
          "            def consumer():",
          "                return that.carrier()",
          "            return consumer",
         "        return middle"),
        "outer.middle.consumer", True,
        "its pair: with no parameter taking the name, the grandchild really does read the"
        " alias the method made, so stopping at intervening scopes must not have stopped"
        " inheritance itself."),
    "a qualifier two functions import differently":
        (REFUSAL,
         ("def good():",
          "    import crw_runtime.reading as r",
          "    return r.UNREADABLE",
          "",
          "def bad():",
          "    import json as r",
          "    return r.UNREADABLE"),
         "good", True,
         "which module an alias names belongs to the scope that imported it. Keyed file-wide,"
         " the later import overwrites the earlier and answers for both, so whichever function"
         " is written second decides what the first one read."),
    "a qualifier the scope beside it imports from elsewhere":
        (REFUSAL,
         ("def bad():",
          "    import json as r",
          "    return r.UNREADABLE",
          "",
          "def good():",
          "    import crw_runtime.reading as r",
          "    return r.UNREADABLE"),
         "bad", False,
         "the same two functions in the other order, which is the half a file-wide answer gets"
         " wrong here: the owning import lands last and answers for the scope that imported"
         " json, so a function reading nothing of the kind is reported as settling for one."),
    "a qualifier the scope beside it imports from elsewhere, read the other way":
        (REFUSAL,
         ("def bad():",
          "    import json as r",
          "    return r.UNREADABLE",
          "",
          "def good():",
          "    import crw_runtime.reading as r",
          "    return r.UNREADABLE"),
         "good", True,
         "and the function that really does read the refusal in that same order. Two entries"
         " over one sample because one file-wide answer can only be right about one of them,"
         " and which it fails is decided by nothing but the order they are written in."),
    "a property built by an annotated binding":
        (REFUSAL,
         ("class Holder:",
          "    def _getter(self):",
          "        return reading.UNREADABLE",
          "    carrier: property = property(_getter)",
          "    def answer(self):",
          "        return self.carrier"),
         "answer", True,
         "property(getter) installs the same descriptor whether or not a type is written on the"
         " binding, so reading only the bare form leaves self.carrier looking like an ordinary"
         " attribute and the getter's refusal never reaches the method that reads it."),
    "a property built by a bare binding":
        (REFUSAL,
         ("class Holder:",
          "    def _getter(self):",
          "        return reading.UNREADABLE",
          "    carrier = property(_getter)",
          "    def answer(self):",
          "        return self.carrier"),
         "answer", True,
         "its pair: the unannotated construction was already followed, so admitting the"
         " annotated one must not have changed what the bare one builds."),
    "an annotation that binds nothing at all":
        (REFUSAL,
         ("class Holder:",
          "    carrier: property",
          "    def answer(self):",
          "        return self.carrier"),
         "answer", False,
         "the other pair: an annotation with no value declares a type and leaves the name"
         " unbound. Treating every AnnAssign as a binding would invent a descriptor out of a"
         " line that builds nothing."),
    "an opener alias written as an annotated binding":
        (TEXT,
         ("def helper():",
          "    fopen: object = open",
          "    stream = fopen(HERE)",
          "    return stream.read()",
          "",
         "def consumer():",
         "    return helper()"),
        "consumer", True,
        "the same rebinding with a type on it. This is the fourth table to have read one form"
        " and not the other, which is why the question now has one answer rather than four."),
    "a handle derived beside the functions that read it":
        (TEXT,
         ("stream = open(HERE)",
          "",
          "def helper():",
          "    return stream.read()",
          "",
          "def consumer():",
          "    return helper()"),
         "helper", True,
         "module level encloses every function, and it is written as a name of its own rather"
         " than as a prefix of theirs. Asking whether the reading scope starts with the owning"
         " one therefore says no to every function, and the handle beside them is read as a"
         " different object wearing the same name."),
    "a handle derived in a function and read in its child":
        (TEXT,
         ("def outer():",
          "    stream = open(HERE)",
          "    def inner():",
          "        return stream.read()",
          "    return inner"),
         "outer.inner", True,
         "its pair on the side that already worked: a scope really is reached by the chain"
         " below it, so admitting module level must not have been done by admitting everything."),
    "a parameter spelled like a handle another scope derived":
        (TEXT,
         ("def maker():",
          "    stream = open(HERE)",
          "    return stream",
          "",
          "def unrelated(stream):",
          "    return stream.read()"),
         "unrelated", False,
         "the other pair: a derived handle belongs to the scope that derived it, and a"
         " parameter of the same spelling in a scope beside it holds whatever its caller"
         " passed. Treating every scope as reaching every handle would make this one a reader."),
    "a refusal a function imports by name for itself":
        (REFUSAL,
         ("def consumer():",
          "    from crw_runtime.completion import NOT_READ as answer",
          "    return answer"),
         "consumer", True,
         "the bare-name half of the local import already fixed on the qualified side. The scope"
         " binds the name, and what binds it is the import that made it an answer, so rejecting"
         " every local binding throws away the one that IS the refusal."),
    "a local name spelled like an imported refusal":
        (REFUSAL,
         ("def consumer():",
          "    answer = object()",
         "    return answer"),
        "consumer", False,
        "its pair: a local binding that is not that import is still something wearing the"
        " spelling, so the exemption has to be for the import rather than for the scope."),
    "a decorator an imported module owns through a path":
        (REFUSAL,
         ("import innocent.decorators",
          "",
          "class Holder:",
          "    def carrier(self):",
          "        return reading.UNREADABLE",
          "    @innocent.decorators.staticmethod",
          "    def answer(self):",
          "        return self.carrier()"),
         "answer", True,
         "the owner is a path through an imported module, not a name of its own. Reduced to its"
         " last part it matches nothing here and the terminal name is read as the builtin it is"
         " spelled like, which removes a receiver the method really has."),
    "the decorator that really is the builtin":
        (REFUSAL,
         ("class Holder:",
          "    def carrier(self):",
          "        return reading.UNREADABLE",
          "    @staticmethod",
          "    def answer(self):",
          "        return self.carrier()"),
         "answer", False,
         "its pair: a plain @staticmethod really does take the receiver away, so resolving the"
         " owner must not have stopped the bare spelling from being recognised."),
    "a property read through an instance the scope built":
        (REFUSAL,
         ("class Holder:",
          "    @property",
          "    def carrier(self):",
          "        return reading.UNREADABLE",
          "",
          "def consumer():",
          "    holder = Holder()",
          "    return holder.carrier"),
         "consumer", True,
         "the receiver table holds the spellings a method was given, and a constructed instance"
         " is not one of them. The class is already known, and reading a property on it runs"
         " the getter exactly as self.carrier does -- the one case where a property read is"
         " reached through no call at all."),
    "a parameter spelled like an instance the module built":
        (REFUSAL,
         ("class Holder:",
          "    def carrier(self):",
          "        return reading.UNREADABLE",
          "",
          "holder = Holder()",
          "",
          "def consumer(holder):",
          "    return holder.carrier()"),
         "consumer", False,
         "the instance table is consulted before the lexical one on the call path, so a"
         " parameter inherits the class of the module-level name it is spelled like. Which"
         " object it holds is a fact about the caller, and a declaration is owed for a call"
         " that never happens."),
    "an instance the module built and a scope really reads":
        (REFUSAL,
         ("class Holder:",
          "    def carrier(self):",
          "        return reading.UNREADABLE",
          "",
          "holder = Holder()",
          "",
          "def consumer():",
          "    return holder.carrier()"),
         "consumer", True,
         "its pair: with nothing taking the name, the module's instance really is what the"
         " scope reads, so consulting the lexical binding first must not have stopped the"
         " instance table from answering at all."),
    "a refusal carried past a parameter in between":
        (REFUSAL,
         ("def outer():",
          "    accepted = reading.UNREADABLE",
          "    def middle(accepted):",
          "        def consumer():",
          "            return accepted",
          "        return consumer",
          "    return middle"),
         "outer.middle.consumer", False,
         "the carrier side of the receiver alias already fixed: checking only the reading"
         " scope's own bindings carries the outer refusal through a parameter that took the"
         " name, and the grandchild closes over its caller's object."),
    "a refusal nothing in between takes":
        (REFUSAL,
         ("def outer():",
          "    accepted = reading.UNREADABLE",
          "    def middle():",
          "        def consumer():",
          "            return accepted",
          "        return consumer",
         "    return middle"),
        "outer.middle.consumer", True,
        "its pair: with no parameter taking the name the grandchild really does read what the"
        " outer scope bound, so stopping at intervening scopes must not have stopped"
        " inheritance itself."),
    "a receiver handed through a conditional":
        (REFUSAL,
         ("class Holder:",
          "    def carrier(self):",
          "        return reading.UNREADABLE",
          "    def answer(self, flag):",
          "        that = self if flag else self",
          "        return that.carrier()"),
         "answer", True,
         "the receiver reaches the class on either branch. Reading ast.Name alone sees no"
         " branch at all, which is the whole family of pass-through forms this resolver was"
         " missing rather than one spelling of it."),
    "a conditional that hands through something else":
        (REFUSAL,
         ("class Holder:",
          "    def carrier(self):",
          "        return reading.UNREADABLE",
          "    def answer(self, flag, other):",
          "        that = other if flag else other",
          "        return that.carrier()"),
         "answer", False,
         "its pair: following the branches must not make every conditional a receiver. What"
         " those arms hold is the caller's, and a call through it says nothing about this"
         " class."),
    "a class-body name another class body bound":
        (REFUSAL,
         ("x = \"fine\"",
          "",
          "class First:",
          "    x = reading.UNREADABLE",
          "",
          "class Second:",
          "    y = x",
          "    def answer(self):",
          "        return self.y"),
         "answer", False,
         "two class bodies written at module level are the same scope to a table keyed by"
         " function, so one class's temporary is carried into the other. Python binds Second.y"
         " to the module's ordinary value, and a declaration is owed for a refusal that is not"
         " there."),
    "a class-body name its own body bound":
        (REFUSAL,
         ("class First:",
          "    x = reading.UNREADABLE",
          "    y = x",
          "    def answer(self):",
          "        return self.y"),
         "answer", True,
         "its pair: a body really does see what it bound a line earlier, so separating siblings"
         " must not have closed each class off from itself."),
    "a property built from a lambda written above it":
        (REFUSAL,
         ("class Holder:",
          "    getter = lambda self: reading.UNREADABLE",
          "    carrier = property(getter)",
          "    def answer(self):",
          "        return self.carrier"),
         "answer", True,
         "the getter is a lambda bound in the class body, and the property scan looked it up"
         " before the loop that registers those ran. The answer depended on which loop this"
         " file happens to write first, which is a fact about this module and not about the"
         " class it reads."),
    "a property whose getter is named by keyword":
        (REFUSAL,
         ("class Holder:",
          "    def getter(self):",
          "        return reading.UNREADABLE",
          "    carrier = property(fget=getter)",
          "    def answer(self):",
          "        return self.carrier"),
         "answer", True,
         "property(fget=getter) installs the getter that property(getter) does. Reading only"
         " positional arguments makes the same descriptor invisible for the sake of where its"
         " argument was written."),
    "a property whose getter is positional":
        (REFUSAL,
         ("class Holder:",
          "    def getter(self):",
          "        return reading.UNREADABLE",
          "    carrier = property(getter)",
         "    def answer(self):",
         "        return self.carrier"),
        "answer", True,
        "its pair: the positional spelling was already read, so admitting the keyword must not"
        " have changed what the ordinary one builds."),
    "a refusal read before the scope rebinds the qualifier":
        (REFUSAL,
         ("def consumer():",
          "    import crw_runtime.reading as r",
          "    accepted = r.UNREADABLE",
          "    import json as r",
          "    return accepted"),
         "consumer", True,
         "the read happened while the name held the owner, and these tables are keyed by scope"
         " rather than by position in it. Taking the last binding loses a read that really"
         " occurred, so a name that EVER holds the owner is treated as holding it -- the"
         " direction this reader errs in everywhere else."),
    "a qualifier the scope binds only to another module":
        (REFUSAL,
         ("def consumer():",
          "    import json as r",
         "    return r.UNREADABLE"),
        "consumer", False,
        "its pair: reading every binding of the name must not make every qualifier an owner."
        " A scope that binds the alias only to something else still names nothing here."),
    "a nested class a branch may never bind":
        (TEXT,
         ("def carrier():",
          "    return HERE.read_text()",
          "",
          "class Holder:",
          "    if False:",
          "        class carrier:",
          "            pass",
          "    verdict = \"needle\" in carrier()"),
         "Holder.verdict", True,
         "whether the branch ran is a question about the run, so the class body still reaches"
         " the module function. The exemption beside this one belongs to a def -- the method"
         " index answers for it either way -- and nothing indexes a nested class that way, so"
         " treating it as a certain shadow loses a place that really does read the source."),
    "a nested class the body certainly binds":
        (TEXT,
         ("def carrier():",
          "    return HERE.read_text()",
          "",
          "class Holder:",
          "    class carrier:",
          "        pass",
          "    verdict = \"needle\" in carrier()"),
         "Holder.verdict", False,
         "its pair: an unconditional nested class really does shadow, so honouring the flag"
         " must not have stopped a class body shadowing at all."),
    "a class body with nothing shadowing the name":
        (TEXT,
         ("def carrier():",
          "    return HERE.read_text()",
          "",
         "class Holder:",
         "    verdict = \"needle\" in carrier()"),
        "Holder.verdict", True,
        "the control beside both: with no nested class at all the body plainly reaches the"
        " module function, which is what the conditional case has to agree with."),
    "a nested class an except branch may never bind":
        (TEXT,
         ("def carrier():",
          "    return HERE.read_text()",
          "",
          "class Holder:",
          "    try:",
          "        pass",
          "    except Exception:",
          "        class carrier:",
          "            pass",
          "    verdict = \"needle\" in carrier()"),
         "Holder.verdict", True,
         "the same rule under a different compound statement. Nobody reported this one; it was"
         " found by asking which constructs decide certainty rather than by fixing the branch"
         " that was reported, and it was failing in the same direction."),
    "a nested class a match arm may never bind":
        (TEXT,
         ("def carrier():",
          "    return HERE.read_text()",
          "",
          "class Holder:",
          "    match 1:",
          "        case 1:",
          "            class carrier:",
          "                pass",
         "    verdict = \"needle\" in carrier()"),
        "Holder.verdict", True,
        "and the third, for the same reason. An arm that matches is a fact about the run, so"
        " the body still reaches the module function and the place that reads it is owed a"
        " declaration."),
    "an opener alias handed through a conditional":
        (TEXT,
         ("def helper(flag):",
          "    fopen = open if flag else open",
          "    stream = fopen(HERE)",
          "    return stream.read()",
          "",
          "def consumer():",
          "    return helper(True)"),
         "consumer", True,
         "the same rebinding written with a branch. Asking _dotted for the value answers None"
         " for every pass-through form, so the alias was lost and the handle with it -- and the"
         " check that emits value followers did not see this loop either, because it anchored"
         " on one way of taking a binding rather than on taking one."),
    "a source-reading alias a parameter shadows":
        (TEXT,
         ("reader = ast.parse",
          "",
          "def helper(reader):",
          "    return reader(HERE)",
          "",
          "def consumer():",
          "    return helper(lambda value: \"ordinary\")"),
         "consumer", False,
         "the spelling table is this module's; the binding is the analysed scope's. A parameter"
         " called reader holds whatever its caller passed, so a call through it says nothing"
         " about source text and the declaration it forced was owed to nobody."),
    "a refusal owner an assignment aliases":
        (REFUSAL,
         ("answers = reading",
          "",
          "def consumer():",
          "    return answers.UNREADABLE"),
         "consumer", True,
         "answers = reading names the module an import would name. Recording qualifiers only"
         " from import statements leaves the ordinary assignment unaccounted, and a place that"
         " settles for a refusal through it is never reported at all."),
    "an assignment aliasing something that owns no answer":
        (REFUSAL,
         ("answers = json",
          "",
         "def consumer():",
         "    return answers.UNREADABLE"),
        "consumer", False,
        "its pair: following assignments must not make every name a module that exports an"
        " answer. The owner is still asked, exactly as it is for an import."),
    "source text a parameter default holds":
        (TEXT,
         ("def outer(text=HERE.read_text()):",
          "    def consumer():",
          "        return text",
          "    return consumer"),
         "outer.consumer", True,
         "a default is a binding: the parameter holds that text whenever the caller omits the"
         " argument, and the closure under it reads what the default left there. Seeding only"
         " from assignment-like statements loses the name, and the place reading it is never"
         " reported at all."),
    "a refusal a parameter default holds":
        (REFUSAL,
         ("def outer(accepted=reading.UNREADABLE):",
          "    def consumer():",
          "        return accepted",
          "    return consumer"),
         "outer.consumer", True,
         "the same binding on the refusal side, because a default is a default whichever"
         " question is being asked of it."),
    "an ordinary parameter default":
        (REFUSAL,
         ("def outer(accepted=\"fine\"):",
          "    def consumer():",
          "        return accepted",
          "    return consumer"),
         "outer.consumer", False,
         "its pair: reading defaults must not make every parameter hold an answer. What the"
         " default is remains the question, and an ordinary string is not one."),
    "a parameter default the inner scope takes back":
        (REFUSAL,
         ("def outer(accepted=reading.UNREADABLE):",
          "    def consumer(accepted):",
          "        return accepted",
         "    return consumer"),
        "outer.consumer", False,
        "the other pair: the closure binds the name itself, so what it reads is its caller's"
        " and not the default outside. Seeding from defaults must not step over the shadow"
        " rule that was built for exactly this."),
    "a qualified source reader a parameter shadows":
        (TEXT,
         ("import ast as parser",
          "",
          "def helper(parser):",
          "    return parser.parse(HERE.read_text())",
          "",
          "def consumer():",
          "    return helper(None)"),
         "consumer", False,
         "a parameter called parser shadows parser.parse exactly as one called reader shadows"
         " reader. Asking the shadow rule about the whole dotted spelling instead of the root"
         " of the chain covered the bare name and left the qualified one forcing a declaration"
         " nobody owes."),
    "a qualified source reader nothing shadows":
        (TEXT,
         ("import ast as parser",
          "",
          "def helper():",
          "    return parser.parse(HERE.read_text())",
          "",
          "def consumer():",
          "    return helper()"),
         "consumer", True,
         "its pair: the module alias really is what the call reaches when no scope takes the"
         " name, so asking about the root must not have stopped qualified readers being read"
         " at all."),
    "an opener a function imports for itself":
        (TEXT,
         ("def helper():",
          "    from builtins import open as fopen",
          "    stream = fopen(HERE)",
          "    return stream.read()",
          "",
         "def consumer():",
         "    return helper()"),
        "consumer", True,
        "the import binds the name in the scope that wrote it exactly as an assignment does,"
        " so that scope is not shadowing the opener it has just taken. Recording only"
        " assignments left the function importing the opener rejected as having hidden it."),
    "a source reader a scope names for itself":
        (TEXT,
         ("def helper():",
          "    reader = ast.parse",
          "    return reader(HERE.read_text())",
          "",
          "def consumer():",
          "    return helper()"),
         "consumer", True,
         "reader = ast.parse written in the scope that calls it IS the reader, not a shadow of"
         " one. Rejecting every local binding of the name throws away the binding that made it"
         " a reader in the first place."),
    "a refusal import an inner parameter takes back":
        (REFUSAL,
         ("def outer():",
          "    from crw_runtime.reading import UNREADABLE as answer",
          "    def consumer(answer):",
          "        return answer",
          "    return consumer"),
         "outer.consumer", False,
         "the exemption for a scope that imports the answer is a fact about THAT scope. A"
         " parameter written nearer the read is a fact about this one and wins, so an outer"
         " import must not survive an inner function taking the name from its caller."),
    "a refusal import nothing nearer takes":
        (REFUSAL,
         ("def outer():",
          "    from crw_runtime.reading import UNREADABLE as answer",
          "    def consumer():",
          "        return answer",
          "    return consumer"),
         "outer.consumer", True,
         "its pair: with nothing nearer binding the name the closure really does read what the"
         " outer import left, so measuring distance must not have cancelled the exemption"
         " altogether."),
    "a qualified refusal import an inner parameter takes back":
        (REFUSAL,
         ("def outer():",
          "    import crw_runtime.reading as r",
          "    def consumer(r):",
          "        return r.UNREADABLE",
          "    return consumer"),
         "outer.consumer", False,
         "the qualified spelling of the case above, and it needed saying separately because"
         " each of these rules has needed its other spelling checked on its own."),
    "a qualified refusal import nothing nearer takes":
        (REFUSAL,
         ("def outer():",
          "    import crw_runtime.reading as r",
          "    def consumer():",
          "        return r.UNREADABLE",
         "    return consumer"),
        "outer.consumer", True,
        "and its pair, for the same reason."),
    "a derived handle an inner parameter takes back":
        (TEXT,
         ("stream = open(HERE)",
          "",
          "def helper(stream):",
          "    return stream.read()",
          "",
          "def consumer():",
          "    return helper(None)"),
         "consumer", False,
         "a derived handle is a fact about the scope that derived it, and a parameter written"
         " nearer the read holds whatever its caller passed. The exemption that lets a derived"
         " handle be read where it was made was bypassing the shadow check entirely, so the"
         " helper and every caller below it were inventoried for a file none of them opens."),
    "a derived handle nothing nearer takes":
        (TEXT,
         ("stream = open(HERE)",
          "",
          "def helper():",
          "    return stream.read()",
          "",
          "def consumer():",
          "    return helper()"),
         "consumer", True,
         "its pair: the module's handle really is what the helper reads when nothing takes the"
         " name, so measuring distance must not have stopped a derived handle being read at"
         " all."),
    "an opener alias an inner parameter takes back":
        (TEXT,
         ("def outer():",
          "    fopen = open",
          "    def helper(fopen):",
          "        stream = fopen(HERE)",
          "        return stream.read()",
          "    def reader():",
          "        return helper(None)",
          "    return reader"),
         "outer.reader", False,
         "the same rule on the opener side: the alias belongs to the scope that wrote it, and"
         " a call through a parameter of that name opens whatever the caller handed over. The"
         " helper still names HERE and is still reported; what must not propagate is the read"
         " to its caller."),
    "an opener alias nothing nearer takes":
        (TEXT,
         ("def outer():",
          "    fopen = open",
          "    def helper():",
          "        stream = fopen(HERE)",
          "        return stream.read()",
          "    def reader():",
          "        return helper()",
          "    return reader"),
         "outer.reader", True,
         "its pair: with nothing taking the name the alias really is the opener, and the read"
         " does reach the caller."),
    "a handle a parameter default holds, left to the default":
        (TEXT,
         ("def helper(path=HERE):",
          "    return path.read_text()",
          "",
          "def consumer():",
          "    return helper()"),
         "consumer", True,
         "the call omits the argument, so the default binds the parameter and the text really"
         " does reach the caller. The propagation read assignment-like statements only, so the"
         " name was never a handle and the consumer went unreported."),
    "a handle a parameter default holds, overridden by the call":
        (TEXT,
         ("def helper(path=HERE):",
          "    return path.read_text()",
          "",
         "def consumer(other):",
         "    return helper(other)"),
        "consumer", False,
        "the pair that decides whether the fix above is a fix or a flood. Recording every"
        " default as a handle unconditionally taints the supplied argument too and turns one"
        " missing consumer into a crowd of invented ones, so a default every call overrides"
        " binds nothing here."),
    "two scopes writing the same helper and parameter":
        (TEXT,
         ("def good():",
          "    def helper(path=HERE):",
          "        return path.read_text()",
          "    def reader(other):",
          "        return helper(other)",
          "    return reader",
          "",
          "def bad():",
          "    def helper(path=HERE):",
          "        return path.read_text()",
          "    def reader():",
          "        return helper()",
          "    return reader"),
         "good.reader", False,
         "whether a default is ever left to itself belongs to the definition, not to the"
         " spelling of its name. Keyed by name across the file, the scope whose call omits the"
         " argument answers for the scope whose call supplies one -- which is the file-wide"
         " keying this module keeps removing, written into the fix that removed the last of"
         " it."),
    "the scope whose call really does omit the argument":
        (TEXT,
         ("def good():",
          "    def helper(path=HERE):",
          "        return path.read_text()",
          "    def reader(other):",
          "        return helper(other)",
          "    return reader",
          "",
          "def bad():",
          "    def helper(path=HERE):",
          "        return path.read_text()",
          "    def reader():",
          "        return helper()",
         "    return reader"),
        "bad.reader", True,
        "the other half of the same sample: separating the two definitions must not stop the"
        " one that really is left to its default from being read."),
    "a qualified opener whose module name a parameter takes":
        (TEXT,
         ("import io",
          "",
          "def helper(io):",
          "    stream = io.open(HERE)",
          "    return stream.read()",
          "",
          "def consumer():",
          "    return helper(None)"),
         "consumer", False,
         "the nearest-binding rule reached a bare owner and not the root of a qualified one, so"
         " a parameter called io left io.open still reading as the imported builtin. The helper"
         " names HERE and is reported either way; what must not propagate is the read to its"
         " caller."),
    "a qualified opener whose module name nothing takes":
        (TEXT,
         ("import io",
          "",
          "def helper():",
          "    stream = io.open(HERE)",
          "    return stream.read()",
          "",
          "def consumer():",
          "    return helper()"),
         "consumer", True,
         "its pair: io.open really is the builtin under its module's name when nothing takes"
         " that name, so reaching the root must not have stopped the qualified opener being"
         " read at all."),
    "a qualified owner whose root a parameter takes":
        (REFUSAL,
         ("import crw_runtime.reading",
          "",
          "def helper(crw_runtime):",
          "    return crw_runtime.reading.UNREADABLE"),
         "helper", False,
         "the same extension on the refusal side. The guard ran only when the immediate owner"
         " was a bare Name, so a two-part owner skipped it entirely and the parameter was read"
         " as the imported module."),
    "a qualified owner whose root nothing takes":
        (REFUSAL,
         ("import crw_runtime.reading",
          "",
          "def helper():",
          "    return crw_runtime.reading.UNREADABLE"),
         "helper", True,
         "its pair: the import really does make the module readable here, so walking to the"
         " root must not have rejected every qualified owner."),
    "a handle opened above a later binding of the opener":
        (TEXT,
         ("stream = open(HERE)",
          "",
          "def helper():",
          "    return stream.read()",
          "",
          "def consumer():",
          "    return helper()",
          "",
          "open = None"),
         "consumer", True,
         "a FALSE NEGATIVE rather than a false positive, and the only one of its round: the"
         " statement ran with the builtin before anything took the name, so the handle is real"
         " and the consumer below it is owed a declaration. A flag for the whole file erased"
         " both. This is when a binding exists relative to a statement, not which scope owns a"
         " name, which is why it is decided apart from the two above."),
    "a handle opened below an earlier binding of the opener":
        (TEXT,
         ("open = None",
          "",
          "stream = open(HERE)",
          "",
          "def helper():",
          "    return stream.read()",
          "",
          "def consumer():",
          "    return helper()"),
        "consumer", False,
        "its pair, and the reason the rule is positional rather than simply relaxed: the same"
        " two statements in the other order really do read whatever the module bound, so"
        " honouring position must not have turned the shadow off."),
    "a default every call overrides":
        (REFUSAL,
         ("def helper(answer=reading.UNREADABLE):",
          "    return answer",
          "",
          "def consumer():",
          "    return helper('fine')"),
         "consumer", False,
         "the refusal side read a default as bound whatever the calls did, so a parameter"
         " nobody ever leaves alone still made its function a refusal and pulled its callers in"
         " behind it. The handle side had been asking whether a call supplies the argument for"
         " several rounds and this side never did, which is the inverse of an earlier fix"
         " rather than a new kind of error. helper itself stays named because its signature"
         " writes the spelling, and a literal occurrence is not a derived one."),
    "a default a call leaves to itself":
        (REFUSAL,
         ("def helper(answer=reading.UNREADABLE):",
          "    return answer",
          "",
          "def consumer():",
          "    return helper()"),
         "consumer", True,
         "its pair, and the protection the entry above must not have removed: a default really"
         " is what the parameter holds when the call omits it, so the refusal does reach the"
         " caller and asking whether calls supply it must not have turned defaults off."),
    "a default in the slot a bound receiver moves":
        (TEXT,
         ("class Holder:",
          "    def helper(self, path=HERE):",
          "        return path.read_text()",
          "",
          "holder = Holder()",
          "",
          "def consumer(other):",
          "    return holder.helper(other)"),
         "consumer", False,
         "counting a call's written arguments against a method's slots is off by one, because"
         " the receiver fills the first slot without being written. holder.helper(other) does"
         " supply path, and reading one argument against slot one answered that it did not, so"
         " a caller-supplied value was read as the HERE default and every caller behind it was"
         " inventoried on a read that never happens."),
    "a default in a bound method a call leaves alone":
        (TEXT,
         ("class Holder:",
          "    def helper(self, path=HERE):",
          "        return path.read_text()",
          "",
          "holder = Holder()",
          "",
          "def consumer():",
          "    return holder.helper()"),
         "consumer", True,
         "its pair: the receiver moves the slot, it does not fill the parameter, so a call"
         " writing no arguments still leaves path at HERE and that read is real. Shifting the"
         " count must not have stopped a method's default applying at all."),
    "a default a plain call overrides with no receiver in the way":
        (TEXT,
         ("def helper(path=HERE):",
          "    return path.read_text()",
          "",
          "def consumer(other):",
          "    return helper(other)"),
         "consumer", False,
         "SUPPORT rather than a regression: this answer was already right before the correction"
         " above and is recorded because it is what pins that defect to the receiver rather"
         " than to defaults in general. The offset is added only where a receiver is really"
         " handed one, so a plain call has to keep counting its arguments straight against its"
         " own slots."),
    "a default behind a receiver the call writes itself":
        (TEXT,
         ("class Holder:",
          "    def helper(self, path=HERE):",
          "        return path.read_text()",
          "",
          "def consumer(obj):",
          "    return Holder.helper(obj)"),
         "consumer", True,
         "SUPPORT by the evidence standard, since it was already right at the parent, and kept"
         " because the first draft of the correction above broke it. Holder.helper(obj) writes"
         " the receiver, so no slot moves and path is still left to HERE. Reading every"
         " attribute call as bound turned a real read into a dropped one, which is the inverse"
         " error of the finding it was fixing. Whether the owner names a class is asked per"
         " scope, not across the file."),
    "a default behind a written receiver the call also supplies":
        (TEXT,
         ("class Holder:",
          "    def helper(self, path=HERE):",
          "        return path.read_text()",
          "",
          "def consumer(obj, other):",
          "    return Holder.helper(obj, other)"),
         "consumer", False,
         "its pair: the unbound form still has to count, so writing both the receiver and the"
         " argument really does supply path and nothing here reads the source."),
    "a default behind a call that unpacks its arguments":
        (REFUSAL,
         ("def helper(answer=reading.UNREADABLE):",
          "    return answer",
          "",
          "def consumer():",
          "    return helper(*())"),
         "consumer", True,
         "helper(*()) supplies no positional argument at all, but the call carries one Starred"
         " node and counting nodes said the default had been overridden. The default really"
         " applies, so the consumer receiving the refusal was dropped -- a place escaping the"
         " inventory, which is the direction that costs something rather than a sentence."),
    "a default behind an unpacking this text cannot count":
        (REFUSAL,
         ("def helper(answer=reading.UNREADABLE):",
          "    return answer",
          "",
          "def consumer(supplied):",
          "    return helper(*supplied)"),
         "consumer", True,
         "the sibling shape, and the reason the rule is stated over unpackings rather than over"
         " the empty one: how many arguments *supplied carries cannot be read from this text at"
         " all, so the count is refused rather than guessed and the default stays applicable."
         " The pair that keeps this honest is 'a default every call overrides' above, where the"
         " argument IS written and the default still has to be suppressed."),
    "a carrier a global statement gives to the module":
        (REFUSAL,
         ("def helper():",
          "    global answer",
          "    answer = reading.UNREADABLE",
          "",
          "def consumer():",
          "    return answer"),
         "consumer", True,
         "global answer writes the MODULE binding, so the refusal is readable from anywhere,"
         " but it was filed under the function that wrote it and no reader outside that"
         " function could see it. The assigning helper was reported and the consumer that"
         " actually returns the refusal was not."),
    "a carrier a nonlocal statement gives to the scope around it":
        (REFUSAL,
         ("def outer():",
          "    answer = \"fine\"",
          "",
          "    def setter():",
          "        nonlocal answer",
          "        answer = reading.UNREADABLE",
          "",
          "    def consumer():",
          "        return answer",
          "    return consumer"),
         "outer.consumer", True,
         "the same defect through the other declaration, checked rather than assumed to follow:"
         " nonlocal hands the binding to the function around this one, so the sibling reading it"
         " sees the refusal. Both sides of the module consult one derived table for this, since"
         " asking it twice is how these two have drifted apart before."),
    "a carrier written with no declaration moving it":
        (REFUSAL,
         ("def helper():",
          "    answer = reading.UNREADABLE",
          "    return None",
          "",
          "def consumer():",
          "    return answer"),
         "consumer", False,
         "its pair, and the protection the two above must not have removed: an ordinary local"
         " write really is local, and consumer reads a name this text never binds where it can"
         " see it. Routing declared names elsewhere must not have routed undeclared ones too."),
    "a handle a global statement gives to the module":
        (TEXT,
         ("def helper():",
          "    global stream",
          "    stream = open(HERE)",
          "",
          "def consumer():",
          "    return stream.read()"),
         "consumer", True,
         "the handle side had the same defect and it was not reported -- it was found by asking"
         " the sibling loop the same question rather than waiting for the next round. The scope"
         " a handle was MADE in decides who can see it, and a global write makes it in the"
         " module."),
    "a handle written with no declaration moving it":
        (TEXT,
         ("def helper():",
          "    stream = open(HERE)",
          "    return None",
          "",
          "def consumer():",
          "    return stream.read()"),
         "consumer", False,
         "its pair on the handle side: a local handle stays local, and a consumer reading an"
         " unrelated name of the same spelling is not reading this one."),
    "a nonlocal declaration two scopes below its binding":
        (REFUSAL,
         ("def outer():",
          "    answer = \"fine\"",
          "",
          "    def middle():",
          "        def setter():",
          "            nonlocal answer",
          "            answer = reading.UNREADABLE",
          "        return setter",
          "",
          "    def consumer():",
          "        return answer",
          "    return consumer"),
         "outer.consumer", True,
         "the fix above this one reached the immediately enclosing scope and stopped there, and"
         " called the rest a declared boundary. It was not one: nothing prevented walking"
         " outward, and the criterion only allows a declared limit where the form cannot be"
         " widened. nonlocal binds at the NEAREST enclosing scope that really binds the name,"
         " so filing it one scope short dropped the sibling that reads it at the owning scope."),
    "a lambda default a call leaves to itself":
        (REFUSAL,
         ("helper = lambda answer=reading.UNREADABLE: answer",
          "",
          "def consumer():",
          "    return helper()"),
         "consumer", True,
         "a REGRESSION this branch introduced rather than an old defect, and worth naming as"
         " one. Gating defaults on whether a call supplies them made both readers ask this"
         " question, but the question was only ever answered for def, so every lambda default"
         " was read as overridden and its consumers dropped. A lambda answers to what it was"
         " bound to, so the binding is what makes helper() resolve to it."),
    "a lambda default a call supplies":
        (REFUSAL,
         ("helper = lambda answer=reading.UNREADABLE: answer",
          "",
          "def consumer():",
          "    return helper(\"fine\")"),
         "consumer", False,
         "its pair, and it is stricter than the answer before the regression above: reading"
         " lambda defaults unconditionally reported this consumer too, which was the other"
         " error. Registering the definition answers both directions instead of trading one"
         " for the other."),
    "a handle a lambda default holds":
        (TEXT,
         ("helper = lambda path=HERE: path.read_text()",
          "",
          "def consumer():",
          "    return helper()"),
         "consumer", True,
         "the handle side, where this was never right rather than newly broken: it has gated"
         " defaults for longer, so a lambda reading the source file through its default has"
         " been dropping its consumers all along. One registration answers both sides."),
    "a lambda default behind a conditional binding":
        (REFUSAL,
         ("def pick(flag):",
          "    first = lambda answer=reading.UNREADABLE: answer",
          "    second = lambda answer=reading.UNREADABLE: answer",
          "    helper = first if flag else second",
          "",
          "    def consumer():",
          "        return helper()",
          "    return consumer"),
         "pick.consumer", True,
         "the module's own value-follower guard asked for this one rather than a reviewer:"
         " registering lambdas by reading a bare Lambda made this pass a resolver that follows"
         " values through fewer forms than the vocabulary, and the guard failed until it read"
         " the whole of it. Widening was correct here and a declared gap would not have been,"
         " because a conditional binding really does hand one of the two over."),
    "a nonlocal owner that binds the name with a loop target":
        (REFUSAL,
         ("def outer():",
          "    for answer in ():",
          "        pass",
          "",
          "    def middle():",
          "        def setter():",
          "            nonlocal answer",
          "            answer = reading.UNREADABLE",
          "        return setter",
          "",
          "    def consumer():",
          "        return answer",
          "    return consumer"),
         "outer.consumer", True,
         "the residual the round before this one wrote down, come back as a finding, which is"
         " what a residual is for. The outward walk read assignments and parameters only, so a"
         " scope that owns the name through a loop target was walked straight past and the"
         " carrier was filed one scope too deep. A Store is a Store however it is spelled."),
    "a nonlocal owner that binds the name with an import":
        (REFUSAL,
         ("def outer():",
          "    import json as answer",
          "",
          "    def middle():",
          "        def setter():",
          "            nonlocal answer",
          "            answer = reading.UNREADABLE",
          "        return setter",
          "",
          "    def consumer():",
          "        return answer",
          "    return consumer"),
         "outer.consumer", True,
         "the same gap through a binding form that is not a Store node at all, which is why it"
         " is its own case rather than assumed to follow from the one above."),
    "a nonlocal owner that binds the name with a def":
        (REFUSAL,
         ("def outer():",
          "    def answer():",
          "        return None",
          "",
          "    def middle():",
          "        def setter():",
          "            nonlocal answer",
          "            answer = reading.UNREADABLE",
          "        return setter",
          "",
          "    def consumer():",
          "        return answer",
          "    return consumer"),
         "outer.consumer", True,
         "the third form, and the one that needed the scope read the other way round: a def is"
         " labelled with the scope it OPENS and binds its name in the one around it, so reading"
         " its own label would have registered the binding inside the function it names."),
    "an alias a named expression binds":
        (REFUSAL,
         ("alias = (answers := reading)",
          "",
          "def consumer():",
          "    return answers.UNREADABLE"),
         "consumer", True,
         "the shared helper every alias table is built on answered None for a walrus, so the"
         " refusal owners, the class aliases and the opener aliases all lost the same spelling"
         " at once, while _bindings beside it had always read one. One helper, one answer."),
    "an alias an ordinary assignment binds":
        (REFUSAL,
         ("answers = reading",
          "",
          "def consumer():",
          "    return answers.UNREADABLE"),
         "consumer", True,
         "its pair, and green before the fix above: reading a named expression must not have"
         " changed what a plain assignment answers."),
    "a conditional property getter whose earlier arm carries":
        (REFUSAL,
         ("class Holder:",
          "    getter = (",
          "        (lambda self: reading.UNREADABLE)",
          "        if flag",
          "        else (lambda self: \"fine\")",
          "    )",
          "    carrier = property(getter)",
          "",
          "    def answer(self):",
          "        return self.carrier"),
         "answer", True,
         "a getter bound by a conditional is still a getter, and reading only a bare Lambda"
         " registered neither arm as a method, so the property resolved to nothing and the"
         " method that reads it went unaccounted for."),
    "a conditional property getter whose later arm carries":
        (REFUSAL,
         ("class Holder:",
          "    getter = (",
          "        (lambda self: \"fine\")",
          "        if flag",
          "        else (lambda self: reading.UNREADABLE)",
          "    )",
          "    carrier = property(getter)",
          "",
          "    def answer(self):",
          "        return self.carrier"),
         "answer", True,
         "the case that decided the shape, and it is why keeping the FIRST arm was not enough:"
         " with the carrier in the other arm, a single-valued answer resolved the property to"
         " the getter that does not carry and dropped the method again. The method and property"
         " tables hold every arm now, the way the alias table beside them already did."),
    "a default every call through an alias overrides":
        (REFUSAL,
         ("def helper(answer=reading.UNREADABLE):",
          "    return answer",
          "",
          "alias = helper",
          "",
          "def consumer():",
          "    return alias('fine')"),
         "consumer", False,
         "a FALSE POSITIVE, so no declared limit was available for it: the claim exceeding the"
         " verified reach is the same fault as the claim falling short. Resolving a call to a"
         " definition read the literal spelling only, so a call through an alias reached no"
         " definition -- and since a definition no call reaches keeps its default, a helper"
         " whose every call overrides it was reported as taking it. _hands_on had resolved the"
         " same alias all along, which is the two-tables-one-question shape again."),
    "a default an alias call leaves to itself":
        (REFUSAL,
         ("def helper(answer=reading.UNREADABLE):",
          "    return answer",
          "",
          "alias = helper",
          "",
          "def consumer():",
          "    return alias()"),
         "consumer", True,
         "its pair: resolving the alias must not have made every aliased call look like it"
         " supplies the argument. The call really does omit it, so the refusal reaches here."),
    "a handle default an alias call supplies":
        (TEXT,
         ("def helper(path=HERE):",
          "    return path.read_text()",
          "",
          "alias = helper",
          "",
          "def consumer(other):",
          "    return alias(other)"),
         "consumer", False,
         "the handle side of the same defect, reported once and fixed in the shared answer"
         " rather than twice."),
    "a default behind an alias a conditional binds":
        (REFUSAL,
         ("def helper(answer=reading.UNREADABLE):",
          "    return answer",
          "",
          "def other(answer=reading.UNREADABLE):",
          "    return answer",
          "",
          "def pick(flag):",
          "    alias = helper if flag else other",
          "",
          "    def consumer():",
          "        return alias('fine')",
          "    return consumer"),
         "pick.consumer", False,
         "the module's value-follower guard asked for this one rather than a reviewer, for the"
         " second round running: resolving the alias off a bare Name made this a resolver"
         " reading fewer forms than the vocabulary. Routing through _passed_through answers it"
         " and covers a conditional alias at the same time."),
    "a method whose receiver is not spelled self":
        (TEXT,
         ("class Holder:",
          "    def helper(this, path=HERE):",
          "        return path.read_text()",
          "",
          "holder = Holder()",
          "",
          "def consumer(other):",
          "    return holder.helper(other)"),
         "consumer", False,
         "self is a name a method chooses, not one the language requires, so reading the first"
         " parameter's spelling to decide whether a receiver is handed over missed every"
         " method that spells it otherwise. The call does supply path, and counting it against"
         " the wrong slot inventoried a caller on a read that never happens. Whether a def is"
         " a method is decided by its being written in a class body."),
    "a staticmethod whose first parameter is spelled self":
        (TEXT,
         ("class Holder:",
          "    @staticmethod",
          "    def helper(self, path=HERE):",
          "        return path.read_text()",
          "",
          "holder = Holder()",
          "",
          "def consumer(other):",
          "    return holder.helper(other)"),
         "consumer", True,
         "the converse of the case above, and the reason the spelling test was wrong in both"
         " directions at once: staticmethod hands no receiver over, so the single written"
         " argument fills self and path really is left to HERE. Reading the spelling suppressed"
         " a default that genuinely applies and dropped the consumer. A decorator this text"
         " cannot read is treated as removing the receiver too, which reports a place rather"
         " than dropping one."),
    "a callee a nearer parameter stands in front of":
        (REFUSAL,
         ("held = reading.UNREADABLE",
          "",
          "def helper(answer=held):",
          "    return answer",
          "",
          "def unrelated(helper):",
          "    return helper('fine')"),
         "helper", True,
         "resolving a call by its terminal spelling walked straight past the parameter that"
         " shadows it, so an unrelated call answered for this definition and recorded its"
         " default as overridden. The real helper is never called, so its default applies and"
         " it hands the refusal on; suppressing it lost the propagation entirely. The refusal"
         " is held by a module name on purpose, so helper's presence here is DERIVED rather"
         " than the literal spelling its own signature would otherwise write. The outward walk"
         " stops at the first scope that binds the name, the way every other lookup here"
         " already does."),
    "a callee a qualified call on an unrelated owner does not reach":
        (REFUSAL,
         ("import other",
          "",
          "held = reading.UNREADABLE",
          "",
          "def helper(answer=held):",
          "    return answer",
          "",
          "def unrelated():",
          "    return other.helper('fine')"),
         "helper", True,
         "the same collision through a qualifier: other.helper is not this helper, and letting"
         " the terminal spelling answer for it let an imported module's call contradict a"
         " default here. A qualified call reaches a definition written here only when its"
         " owner names a class written here or holds an instance of one."),
    "a callee a call in the same scope really does reach":
        (REFUSAL,
         ("held = reading.UNREADABLE",
          "",
          "def helper(answer=held):",
          "    return answer",
          "",
          "def unrelated():",
          "    return helper('fine')"),
         "helper", False,
         "the pair for both of the two above, and the one that keeps them honest: with nothing"
         " standing in front of the name and no qualifier in the way, the call really is this"
         " definition's and the default really is overridden. Stopping at a nearer binding must"
         " not have stopped resolution altogether."),
    "a classmethod reached through its own class":
        (TEXT,
         ("class Holder:",
          "    @classmethod",
          "    def helper(cls, path=HERE):",
          "        return path.read_text()",
          "",
          "def consumer(other):",
          "    return Holder.helper(other)"),
         "consumer", False,
         "PLACEMENT rather than lookup: the question is where the written argument lands, not"
         " which binding a name refers to. An ordinary method spelled Holder.helper(obj) writes"
         " its receiver, so no slot moves -- but a classmethod is bound through the class as"
         " well, hands cls over unwritten, and the one written argument really does fill path."
         " Reading the owner as a class and stopping there inventoried a caller on a read that"
         " cannot happen."),
    "a classmethod reached through its own class with nothing written":
        (TEXT,
         ("class Holder:",
          "    @classmethod",
          "    def helper(cls, path=HERE):",
          "        return path.read_text()",
          "",
          "def consumer():",
          "    return Holder.helper()"),
         "consumer", True,
         "SUPPORT, green at the parent, and kept because it is the pair the fix above must not"
         " break: with nothing written the default really is left alone and the read is real."),
    "a nonlocal write is not a local shadow":
        (TEXT,
         ("path = None",
          "",
          "def outer():",
          "    path = None",
          "",
          "    def helper():",
          "        nonlocal path",
          "        path = HERE",
          "        return path.read_text()",
          "",
          "    def consumer():",
          "        return helper()",
          "    return consumer"),
         "outer.consumer", True,
         "the declared-owner rule reached the table that files the handle and not the table"
         " that decides what shadows it, which exempted global alone. The write under nonlocal"
         " was read as a nearer local standing over the handle it had just made, so the read"
         " was hidden and the function returning helper() was dropped. The same declaration,"
         " asked at both tables."),
    "a comprehension walrus binds in the scope around it":
        (REFUSAL,
         ("def outer():",
          "    [(answer := item) for item in ()]",
          "",
          "    def middle():",
          "        def setter():",
          "            nonlocal answer",
          "            answer = reading.UNREADABLE",
          "        return setter",
          "",
          "    def consumer():",
          "        return answer",
          "    return consumer"),
         "outer.consumer", True,
         "a walrus inside a comprehension binds in the scope AROUND the comprehension, and the"
         " NamedExpr was moved there while its target Name was left labelled with the"
         " comprehension's own scope. Every table keyed off where a name is bound then read the"
         " binding as the comprehension's, so the outward walk for the nonlocal missed the real"
         " owner and filed the carrier one scope short."),
    "an owner module handed over by an unpacking":
        (REFUSAL,
         ("answers, ignored = reading, None",
          "",
          "def consumer():",
          "    return answers.UNREADABLE"),
         "consumer", True,
         "the alias propagation read the whole right-hand side as one value, so a tuple handed"
         " neither name anything and answers never became an owner. Every reader of it was then"
         " dropped. _bindings had paired the elements off all along -- this pass was the one"
         " asking the question of the statement instead of the binding."),
    "an owner module handed over plainly":
        (REFUSAL,
         ("answers = reading",
          "",
          "def consumer():",
          "    return answers.UNREADABLE"),
         "consumer", True,
         "SUPPORT, green at the parent: pairing the elements off must not have stopped an"
         " ordinary single binding naming the owner."),
    "a class body that rebinds a spelling before reading it":
        (REFUSAL,
         ("class Holder:",
          "    CHANGED = \"fine\"",
          "    answer = CHANGED"),
         "Holder.answer", False,
         "the shadow table skipped every class statement, which is right for a METHOD reading"
         " the name -- that resolves outside the class entirely -- and wrong for a load written"
         " in the body, which reads what the body has already bound. Leaving both out reported"
         " the module constant at a place that never names it. A class body binds in order, so"
         " the line decides."),
    "a class body that reads a spelling it never binds":
        (REFUSAL,
         ("class Holder:",
          "    answer = CHANGED"),
         "Holder.answer", True,
         "SUPPORT, green at the parent, and the pair that stops the fix above from hiding every"
         " class-body read: with nothing bound in the body the load really is the module's."),
    "a class body that reads a spelling before rebinding it":
        (REFUSAL,
         ("class Holder:",
          "    answer = CHANGED",
          "    CHANGED = \"fine\""),
         "Holder.answer", True,
         "SUPPORT, and the reason the rule is positional rather than merely present: the same"
         " two statements in the other order really do read the module constant, because the"
         " class body had not bound the name yet when the load ran."),
    "a method reading a spelling its class body binds":
        (REFUSAL,
         ("class Holder:",
          "    CHANGED = \"fine\"",
          "",
          "    def answer(self):",
          "        return CHANGED"),
         "answer", True,
         "SUPPORT, and the half of the old behaviour that was correct: a class namespace is not"
         " in a method's lookup chain, so the bare name there reaches the module however plainly"
         " the body above it rebound the spelling. Teaching the body to shadow must not have"
         " taught the methods inside it to."),
    "an opener handed over by an unpacking":
        (TEXT,
         ("fopen, ignored = open, None",
          "",
          "def helper():",
          "    return fopen(HERE).read()",
          "",
          "def consumer():",
          "    return helper()"),
         "consumer", True,
         "the same defect as the refusal owner one commit earlier, on the opener table, and"
         " reported rather than found by me -- which is what made it worth sweeping the set"
         " instead of fixing a second instance. The alias walk accepted only a top-level Name"
         " target, so an unpacked opener was never recognised and the helper was reported only"
         " because it spells HERE while its caller was dropped."),
    "an opener handed over plainly":
        (TEXT,
         ("fopen = open",
          "",
          "def helper():",
          "    return fopen(HERE).read()",
          "",
          "def consumer():",
          "    return helper()"),
         "consumer", True,
         "SUPPORT, green at the parent: pairing the elements off must not have stopped an"
         " ordinary single binding naming the opener."),
    "a class alias handed over by an unpacking":
        (REFUSAL,
         ("class First:",
          "    def carrier(self):",
          "        return reading.UNREADABLE",
          "",
          "Alias, ignored = First, None",
          "",
          "class Child(Alias):",
          "    def answer(self):",
          "        return self.carrier()"),
         "answer", True,
         "the THIRD table with the same shape, and nobody reported this one: it came out of"
         " asking the question of every _assigned site after the second was handed to me. A"
         " base named by an unpacked alias resolved to no class, so the inherited carrier was"
         " never reached and the method reading it went unaccounted for."),
    "a decorator spelling the text rebinds":
        (TEXT,
         ("classmethod = staticmethod",
          "",
          "class Holder:",
          "    @classmethod",
          "    def helper(cls, path=HERE):",
          "        return path.read_text()",
          "",
          "def consumer(other):",
          "    return Holder.helper(other)"),
         "consumer", True,
         "reading the decorator's spelling rather than resolving it: with classmethod bound to"
         " staticmethod the method is handed no receiver, so the one written argument fills cls"
         " and path really is left at HERE. Marking it a bound classmethod suppressed a default"
         " that applies and dropped the consumer. A decorator is evaluated in the scope around"
         " the def, so the spelling is trusted only where nothing in that chain takes the name."),
    "a decorator spelling nothing rebinds":
        (TEXT,
         ("class Holder:",
          "    @classmethod",
          "    def helper(cls, path=HERE):",
          "        return path.read_text()",
          "",
          "def consumer(other):",
          "    return Holder.helper(other)"),
         "consumer", False,
         "SUPPORT, green at the parent, and the pair that stops the fix above from refusing"
         " every decorator: with the name untaken the word means its builtin, the receiver is"
         " handed over, and the written argument really does fill path."),
    "a carrier handed to a parameter":
        (REFUSAL,
         ("def identity(answer):",
          "    return answer",
          "",
          "def helper():",
          "    return identity(reading.UNREADABLE)",
          "",
          "def consumer():",
          "    return helper()"),
         "consumer", True,
         "parameters were seeded from applicable defaults and from nothing else, so a refusal"
         " handed over as an ARGUMENT bound no parameter, neither identity nor helper became a"
         " carrier, and the caller below them was dropped. An argument is the binding of the"
         " parameter it fills. Keywords name their own slot; positions are read only where no"
         " receiver may be in the way, because a call on an attribute may hand one over"
         " unwritten and guessing which would bind the wrong slot."),
    "a source handle kept through Path construction":
        (TEXT,
         ("def helper():",
          "    return Path(__file__).read_text()",
          "",
          "def consumer():",
          "    return helper()"),
         "consumer", True,
         "the receiver of the read had no dotted spelling, so the read was not recognised as"
         " taken on a handle and the helper was reported only for naming the file. This is NOT"
         " the run-time-assembled path declared out of reach: there nothing in the expression"
         " names a source file, and here the handle is written inside the call. Path keeps the"
         " path its argument names."),
    "a loop target from an iterable written here":
        (REFUSAL,
         ("def helper():",
          "    for answer in (reading.UNREADABLE,):",
          "        return answer",
          "",
          "def consumer():",
          "    return helper()"),
         "consumer", True,
         "a for target is a binding like any other when the iterable is written out, and the"
         " pairing helper answered nothing at all for ast.For. Every table built on it lost the"
         " loop, so the helper reading the target was not a carrier and its caller went"
         " unaccounted for."),
    "a source handle from an iterable written here":
        (TEXT,
         ("def helper():",
          "    for path in (HERE,):",
          "        return path.read_text()",
          "",
          "def consumer():",
          "    return helper()"),
         "consumer", True,
         "the handle side of the same omission, fixed in the shared pairing helper rather than"
         " twice."),
    "a loop target from an iterable this text cannot read":
        (REFUSAL,
         ("def helper(values):",
          "    for answer in values:",
          "        return answer",
          "",
          "def consumer():",
          "    return helper()"),
         "consumer", False,
         "SUPPORT, green at the parent, and the limit the fix above stops at: what an iterable"
         " this text cannot read yields is a question about the run, so the loop binds nothing"
         " rather than a guess. Only the written forms are paired."),
    "a carrier handed to a keyword-only parameter":
        (REFUSAL,
         ("def identity(*, answer):",
          "    return answer",
          "",
          "def helper():",
          "    return identity(answer=reading.UNREADABLE)",
          "",
          "def consumer():",
          "    return helper()"),
         "consumer", True,
         "the slot table was built from the positional parameters alone, so the keyword filter"
         " beside it could never match a slot no position reaches. The rule was written one"
         " commit earlier and applied to half the parameters it names -- a half-applied rule"
         " reads as a working one until someone writes the other half's form."),
    "a carrier handed positionally to a bound method":
        (REFUSAL,
         ("class Holder:",
          "    def identity(self, answer):",
          "        return answer",
          "",
          "holder = Holder()",
          "",
          "def helper():",
          "    return holder.identity(reading.UNREADABLE)",
          "",
          "def consumer():",
          "    return helper()"),
         "consumer", True,
         "I declared this one a limit a commit ago on the grounds that a receiver might be in"
         " the way and binding one slot off would be worse than binding nothing. That was"
         " wrong: which places take a receiver is already derived here, and the criterion only"
         " allows a declared limit where the form cannot be widened. A receiver MOVES the"
         " slots rather than making them unreadable."),
    "a carrier destructured past a starred target":
        (REFUSAL,
         ("def helper():",
          "    answer, *ignored = (reading.UNREADABLE,)",
          "    return answer",
          "",
          "def consumer():",
          "    return helper()"),
         "consumer", True,
         "the pairing required equal lengths, so a star made the whole target pair with the"
         " whole value and nothing downstream accepted it. Python binds this deterministically:"
         " the names before the star take the first values and the names after it take the"
         " last. The starred name holds a list of what remains, which is a container rather"
         " than the thing, so it is paired with nothing."),
    "a carrier iterated from a dictionary written here":
        (REFUSAL,
         ("def helper():",
          "    for answer in {reading.UNREADABLE: None}:",
          "        return answer",
          "",
          "def consumer():",
          "    return helper()"),
         "consumer", True,
         "a dict is iterated over its KEYS, which are written out exactly as a tuple's elements"
         " are. Listing the three sequence forms and stopping was the same half-applied rule as"
         " the keyword-only slots above, in the commit that added it."),
    "a carrier handed through a written unpacking":
        (REFUSAL,
         ("def identity(answer):",
          "    return answer",
          "",
          "def helper():",
          "    return identity(*(reading.UNREADABLE,))",
          "",
          "def consumer():",
          "    return helper()"),
         "consumer", True,
         "the positional zip took the Starred NODE as the argument, so a slot was bound to a"
         " piece of syntax rather than to what the call hands over. A written unpacking supplies"
         " the values it writes, so they are expanded first."),
    "a carrier handed through a written mapping":
        (REFUSAL,
         ("def identity(answer):",
          "    return answer",
          "",
          "def helper():",
          "    return identity(**{\"answer\": reading.UNREADABLE})",
          "",
          "def consumer():",
          "    return helper()"),
         "consumer", True,
         "the other half of the same shape: a double-star carries arg None, so the keyword"
         " filter matched nothing at all. A mapping written out names its own slots exactly as"
         " a keyword written out does."),
    "a carrier handed through an unpacking this text cannot read":
        (REFUSAL,
         ("def identity(answer):",
          "    return answer",
          "",
          "def helper(supplied):",
          "    return identity(*supplied)",
          "",
          "def consumer():",
          "    return helper()"),
         "consumer", False,
         "SUPPORT, green at the parent, and the limit the two above stop at: how many values"
         " *supplied carries cannot be read here, so the positional count stops at it rather"
         " than guessing how far it reaches."),
    "a carrier handed positionally to a classmethod through its class":
        (REFUSAL,
         ("class Holder:",
          "    @classmethod",
          "    def identity(cls, answer):",
          "        return answer",
          "",
          "def helper():",
          "    return Holder.identity(reading.UNREADABLE)",
          "",
          "def consumer():",
          "    return helper()"),
         "consumer", True,
         "naming the class was read as the receiver having been written, which is true of an"
         " ordinary method and false of a classmethod: that one is bound through its class too"
         " and hands cls over unwritten. The same correction the default-applicability side"
         " needed several rounds ago, arriving here because this table was newer."),
    "a carrier handed positionally to an unbound method through its class":
        (REFUSAL,
         ("class Holder:",
          "    def identity(self, answer):",
          "        return answer",
          "",
          "def helper(obj):",
          "    return Holder.identity(obj, reading.UNREADABLE)",
          "",
          "def consumer():",
          "    return helper(None)"),
         "consumer", True,
         "SUPPORT, green at the parent, and the pair that keeps the correction above honest:"
         " an ordinary method reached through its class really does write its receiver, so the"
         " slots must not move for it."),
    "a carrier unpacked from a tuple written second":
        (REFUSAL,
         ("def identity(first, second):",
          "    return first",
          "",
          "def helper():",
          "    return identity(*(\"fine\", reading.UNREADABLE))",
          "",
          "def consumer():",
          "    return helper()"),
         "consumer", False,
         "SUPPORT, green at the parent, and the form INSIDE the seating grammar that keeps the"
         " declared boundary from creeping: a tuple IS ordered, so the second element really"
         " does fill the second slot and the function returning the first never sees the"
         " refusal. Declining to seat an unordered unpacking must not have stopped seating an"
         " ordered one. The unordered form is not an entry here at all, because Python may"
         " answer either way and this table records the answer Python gives -- it is declared"
         " in ARGUMENT_FORM_NOT_SEATED with a control on each side instead."),
    "a method reached through a class alias":
        (REFUSAL,
         ("class Holder:",
          "    def identity(self, answer):",
          "        return answer",
          "",
          "Alias = Holder",
          "",
          "def helper(obj):",
          "    return Alias.identity(obj, reading.UNREADABLE)",
          "",
          "def consumer():",
          "    return helper(None)"),
         "consumer", True,
         "Alias = Holder makes Alias.identity(obj, x) the same unbound call, and the receiver"
         " seating read the class spelling table alone. An alias was taken for an instance, so"
         " a slot the call had written itself was moved and the argument behind it landed one"
         " place over. The alias table was already derived here and beside it."),
    "a method reached through its own class":
        (REFUSAL,
         ("class Holder:",
          "    def identity(self, answer):",
          "        return answer",
          "",
          "def helper(obj):",
          "    return Holder.identity(obj, reading.UNREADABLE)",
          "",
          "def consumer():",
          "    return helper(None)"),
         "consumer", True,
         "SUPPORT, green at the parent: reading the alias table beside the spelling one must"
         " not have changed what the plain spelling answers."),
    "a default overridden through a written unpacking":
        (REFUSAL,
         ("def helper(answer=reading.UNREADABLE):",
          "    return answer",
          "",
          "def consumer():",
          "    return helper(*(\"fine\",))"),
         "consumer", False,
         "the seating grammar expands a written tuple or list, and this consumer did not: it"
         " read every Starred as a count it could not know, so a call that really does supply"
         " the argument left the default active and invented a carrier. The declaration and"
         " the implementation disagreeing about the same grammar is the shape this issue names,"
         " one layer in."),
    "a default overridden through a class alias":
        (REFUSAL,
         ("class Holder:",
          "    def helper(self, answer=reading.UNREADABLE):",
          "        return answer",
          "",
          "Alias = Holder",
          "holder = Holder()",
          "",
          "def consumer():",
          "    return Alias.helper(holder, \"fine\")"),
         "consumer", False,
         "the SAME rule the receiver seating learned one commit earlier, asked at the other"
         " consumer and answered differently for a whole commit. Both ask it through one"
         " helper now. The site inventory could not have caught this: its predicate asks"
         " whether a site measures DISTANCE, and which owner SHAPE a site recognises is a"
         " different question, which is written into that sweep's own declared boundary."),
    "a default overridden through the class itself":
        (REFUSAL,
         ("class Holder:",
          "    def helper(self, answer=reading.UNREADABLE):",
          "        return answer",
          "",
          "holder = Holder()",
          "",
          "def consumer():",
          "    return Holder.helper(holder, \"fine\")"),
         "consumer", False,
         "SUPPORT, green at the parent: teaching the owner filter aliases must not have"
         " changed what the plain class spelling answers."),
    "a refusal handed on through a formatted string that is only the value":
        (REFUSAL,
         ("def helper():",
          "    return f\"{reading.UNREADABLE}\"",
          "",
          "def consumer():",
          "    return helper()"),
         "consumer", True,
         "f\"{answer}\" with nothing beside it, no conversion and no format spec is the value's"
         " own string, so for a refusal spelled as a string the helper hands on the same"
         " string. This form was neither followed nor declared, which is the original defect"
         " rather than a narrow claim: the line between it and the form that builds a new"
         " string is the one the language draws, so the exact one is followed and the other is"
         " named in HANDED_ON_THROUGH_A_TRANSFORMATION."),
}

# The spellings this module actually relies on. Not the reach -- the reach is derived and may go
# further, which is not a failure. This is the floor: a kind that quietly stopped resolving, an
# import that moved or a constant that was renamed, loses one of these and fails. Occurrence
# counts alone would not catch it, because a spelling nobody writes has no occurrence to lose.
REFUSAL_REACH_INCLUDES = {
    "module attribute": ("UNREADABLE", "ABSENT", "ACCESS_ERROR", "NOT_READ", "NO_JOURNAL"),
    "own global": ("CHANGED",),
    "collection": ("REFUSAL_ANSWERS", "VALUES"),
}

# Which path each reading actually travels: the thing this run installed or registered, or a
# stand-in this suite supplied. Reaching a success answer and reaching it down the path the row
# names are two questions, and the second is the one a source-level shortcut passes silently.
# Declared per row so a reading that quietly moves onto a shortcut has to move a sentence too.
READING_PATHS = {
    "skillLink":
        "installed, on the run's own Codex home: the links this run created under it, listed"
        " by the repository check the diagnosis itself runs.",
    "runtimeImport":
        "installed, on the run's own destination: the recorded interpreter imports the packages"
        " copied into the candidate, and the success case requires the resolved locations to be"
        " inside it.",
    "mcpToolExposure":
        "installed, on the run's own Codex home: the registration register-mcp --apply wrote"
        " into it, compared with the tool names supplied. The tool list is an input; on a host"
        " it comes from a session that listed them, which the procedure says.",
    "appServerConnection":
        "stand-in, on the run's own state directory: a relay executable this suite wrote"
        " answers the doctor about that state. No App Server runs here, and the claim is"
        " narrowed to match -- the row is fixture, and the procedure states that a live socket"
        " is what a host reading needs.",
    "hookCallback":
        "registered, on the run's own Codex home: the command line in the hook file the"
        " installer wrote THERE, read back out of that file and executed as a program with the"
        " Stop payload on its stdin. Calling the adapter helper directly would have left the"
        " entry point, the settings argument and the stdin contract untested; firing into a"
        " Codex home of its own would have answered from a machine the other six never saw.",
    "modelPermissionPreservation":
        "installed, on the run's own Codex home: the configuration the install acted over,"
        " read on both sides of it.",
    "deliveryAcceptance":
        "stand-in, on the run's own state directory: a relay executable this suite wrote"
        " carries the eight steps against that state, while the preflight asks the relay"
        " predicate imported from the copy inside the candidate. No live relay completes a"
        " delivery here, and the row is fixture for that reason.",
}
# Rows whose success answer cannot be required on every supported interpreter, with the reason.
# The exception exists because absence is a real state here and the answer set can say so; it
# is not a place to file a row that simply never succeeds.
CONDITIONAL_SUCCESS = {
    "modelPermissionPreservation":
        "reading a configuration needs tomllib, which arrives in 3.11, and the supported floor",
    "mcpToolExposure":
        "exposure is verified by comparing the REGISTERED command with the tools observed, and"
        " the registration lives in the configuration -- so on the interpreter with no reader"
        " for it this row cannot reach verified either, for the same one reason",
}


def succeeding(root):
    """One run in which every one of the seven readings has something that worked to read.

    The skills are really linked, the registration really names the command the diagnosis is
    asked about, both components really import from this checkout, the relay really answers the
    doctor and every step of a trial, and the hook has really fired. Nothing here is a fixture
    standing in for an answer: the stand-ins are the things being asked, and what they answer is
    what the readings report.
    """
    host = base._Host(root)
    clean(host)
    bridge = base.component_of_for_test(host.data, runtime_install.MCP_NAME)
    registered = str(host.pointer_path / "bin" / bridge["consoleScript"])
    # The fixture arrives with the bridge already registered, which would let the exposure row
    # read verified while register-mcp wrote nothing at all. The table is taken away so the
    # registration this run performs is the one the diagnosis compares against.
    host.config.write_text("", encoding="utf-8")
    seed_settings(host)
    earlier = host.config.read_text(encoding="utf-8")
    install(host)
    later = host.config.read_text(encoding="utf-8")

    linked_skills(host)
    # The supplied runtime first: it writes the console scripts the entry points resolve to and
    # records the interpreter, and only then is that interpreter given a path it can import from.
    stand_in_runtime(host)
    sources = importable_runtime(host, root, paths=installed_components(host))
    registration = base.run("register-mcp", "--codex-home", str(host.codex_home),
                            "--bridge-command", registered, "--apply")

    # The SAME Codex home the runtime was installed into, the skills were linked into and the
    # registration was written into. Firing into a second home of its own would have answered
    # this row from a clean hook file with no relation to the host the other six readings
    # describe -- a composition of readings about two different machines is not a composition.
    _before, after, _registered, _done = fire_the_hook(host.codex_home)

    return {
        "diagnose": diagnose_for(
            root, host,
            "--bridge-command", registered, "--observed-tool", "get_capabilities",
            "--relay-command", str(delivering_relay(root)),
            "--trial", *trial_inputs(root),
            import_path=sources),
        "hook-status": after,
        ACCEPTANCE: _model_permission_delta(earlier, later),
    }, host, registration

class ComposedLifecycleTests(unittest.TestCase):
    """One destination, four stages, and the install that has to come back at the end of them.

    The update tests already inject a failure at every boundary and prove that what was there
    before is still there afterwards. What they cannot prove is that the runtime which survived
    is one this command installed: their previous installation is a directory the fixture places
    on disk and selects, never promoted by the command under test. An installer that had lost
    the ability to install would leave every one of those tests green.

    So stage one installs it, stage two proves a repeat changes nothing, stage three fails an
    update to an arriving source, and stage four asserts that the environment recovered is the
    one stage one promoted -- then installs again, because a host that is merely not broken is
    not the same as a host that still works.
    """

    def _fired(self, label, breaking, gate, failure, arriving):
        """The seam named actually refused, on the candidate it was supposed to be replacing.

        Without this the sequence could fail before it reached the seam, or fail against the
        wrong environment, and every surviving-state assertion would still pass.
        """
        self.assertEqual(failure.get("environment"), str(arriving),
                         label + ": the run must be acting on the arriving source")
        if breaking:
            self.assertEqual(failure.get("failedStep"), breaking,
                             label + ": a reader must not have to infer where it stopped")
        else:
            self.assertEqual(failure.get("failedStep"), GATE_REFUSAL, label)
            verdict = (failure.get("swapGate") or {}).get("verdict")
            self.assertIn(verdict, (swapgate.BLOCKED, swapgate.UNESTABLISHED), label)

    def test_a_new_install_a_rerun_a_failed_update_and_the_recovery_of_what_it_replaced(self):
        for breaking, gate in BOUNDARIES:
            label = breaking or gate
            with self.subTest(label):
                with tempfile.TemporaryDirectory() as temporary:
                    host = base._Host(temporary)
                    clean(host)
                    settings = seed_settings(host)

                    # Stage one: a new install, onto a destination that has nothing in it.
                    empty = sorted(p.name for p in host.destination.iterdir())
                    new, new_payload = install(host)
                    installed = host.candidate
                    settled = host.snapshot()
                    reached = pointer.read(host.pointer_path).get("target")
                    record = hostrecord.load(host.record_path,
                                             host.data["definitionVersion"]).value or {}
                    selected = record.get("selected") or {}
                    claimed = (staging.read_claim(installed).value or {}).get("state")
                    after_new = sorted(p.name for p in host.destination.iterdir())

                    # Stage two: the same run again, which must build and write nothing.
                    repeat, repeat_payload = install(host)
                    after_repeat = host.snapshot()
                    claim = (staging.read_claim(installed).value or {}).get("state")
                    after_again = sorted(p.name for p in host.destination.iterdir())

                    # Stage three: a source arrives and the update to it fails at this seam.
                    arriving = arriving_source(host)
                    loaded, verified = updating(host)
                    with loaded, verified:
                        failed, failure = install(host, breaking=breaking, gate=gate)
                    recovered = host.snapshot()
                    still_reaches = pointer.read(host.pointer_path).get("target")
                    left_behind = sorted(p.name for p in host.destination.iterdir())

                    # Stage four: the host is asked to install the source it already has.
                    host.data = definition.load()
                    host.candidate = installed
                    usable, usable_payload = install(host)
                    settings_now = settings_bytes(host)

                self.assertEqual(empty, [], label + ": the fixture was cleaned first")
                self.assertEqual(new, 0, json.dumps(new_payload)[:1200])
                self.assertTrue(new_payload["promoted"], label + ": stage one must install")
                self.assertEqual(reached, str(installed),
                                 label + ": the pointer reaches what stage one promoted")
                self.assertTrue(selected, label + ": a new install records what it selected")
                self.assertTrue(all(str(installed) in str(where) for where in selected.values()),
                                label + ": and what it selected is what it built")
                self.assertEqual(claimed, staging.COMPLETE,
                                 label + ": the claim is settled by the install that made it,"
                                 " not first read after a later run")

                self.assertEqual(repeat, 0, json.dumps(repeat_payload)[:1200])
                self.assertTrue(repeat_payload["alreadyInstalled"],
                                label + ": a repeat of a settled install installs nothing")
                self.assertEqual(repeat_payload["stagingDecision"], staging.SETTLED, label)
                self.assertEqual(claim, staging.COMPLETE, label)
                self.assertEqual(after_repeat, settled,
                                 label + ": and it changes nothing that was there")
                self.assertEqual(after_again, after_new,
                                 label + ": a repeat leaves no directory behind either, which"
                                 " the state snapshot does not inventory")

                self.assertNotEqual(arriving, installed,
                                    label + ": the arriving source must build somewhere else,"
                                    " or stage three is not an update at all")
                self.assertEqual(failed, 1, label + ": a failed update must not report success")
                self._fired(label, breaking, gate, failure, arriving)

                self.assertEqual(recovered, settled,
                                 label + ": everything the update found is still as it found it")
                self.assertEqual(still_reaches, str(installed),
                                 label + ": the runtime recovered is the one stage one installed,"
                                 " not a directory the fixture placed")
                self.assertNotEqual(still_reaches, str(host.previous), label)
                self.assertNotIn(arriving.name, left_behind,
                                 label + ": the candidate it could not promote is not left in"
                                 " the destination")
                self.assertTrue(failure.get("retriable"),
                                label + ": a host that kept everything can try again")
                residual = failure.get("residualPaths") or []
                self.assertNotIn(str(arriving), residual,
                                 label + ": the candidate it could not promote is not left for"
                                 " an operator to clear")
                self.assertNotIn(str(installed), residual,
                                 label + ": and neither is the install that has to keep working")
                self.assertEqual(settings_now, settings,
                                 label + ": the settings it was handed are byte-identical")

                self.assertEqual(usable, 0, json.dumps(usable_payload)[:1200])
                self.assertTrue(usable_payload["alreadyInstalled"],
                                label + ": after the recovery the host still works")

    def test_the_rows_seeded_before_the_first_install_survive_every_stage(self):
        """The store is the half an update can lose, so it is read at the end, not asserted at."""
        with tempfile.TemporaryDirectory() as temporary:
            host = base._Host(temporary)
            clean(host)
            opening = host.snapshot()
            install(host)
            arriving_source(host)
            loaded, verified = updating(host)
            with loaded, verified:
                install(host, breaking="install packages")
            closing = host.snapshot()

        self.assertEqual(closing["storeRows"], opening["storeRows"])
        self.assertEqual(closing["storeInode"], opening["storeInode"],
                         "the store was not moved or recreated")
        self.assertEqual(closing["storeBytes"], opening["storeBytes"])

    def test_the_store_the_run_is_told_about_is_the_store_the_fixture_built(self):
        """What the run was told, read back and compared with what this scenario put on disk.

        Every install above used to hand the fixture a switch reporting the store absent and its
        tables unknown, while the fixture had built a populated one at that same path. Nothing
        failed: a run told there is no store settles the store cell as established absence and
        moves on, so a regression that detected a store and then lost it stayed green underneath
        -- the run under it had been told there was nothing to lose.

        The switch is refused now. This is the reading that keeps it refused rather than merely
        deleted: the cells the run took are read back out of its own payload and compared with
        the store this scenario built, and the two of them answer from different readings -- the
        schema comparison, and the doctor the in-flight count came from.
        """
        with tempfile.TemporaryDirectory() as temporary:
            host = base._Host(temporary)
            clean(host)
            code, payload = install(host)
            built = host.snapshot()
            where = str(host.store)
            cells = (payload.get("swapGate") or {}).get("cells") or {}

        self.assertEqual(code, 0, json.dumps(payload)[:800])
        self.assertTrue(built["storeRows"],
                        "the fixture built a store with nothing in it, so there is nothing here"
                        " a run could have been told the wrong thing about")

        tables = cells["storeTables"]
        self.assertTrue(tables["readable"], str(tables.get("detail")))
        self.assertEqual(tables["answer"], swapgate.AGREES,
                         "the run read " + repr(tables["answer"]) + " about a store holding "
                         + repr(built["storeRows"]) + ", so what it was handed and what this"
                         " scenario built are two different stores")
        self.assertEqual((tables["evidence"] or {}).get("dbPath"), where,
                         "and the reading it took names a store somewhere else")

        self.assertEqual(cells["inFlight"]["command"], ["doctor"],
                         "the in-flight count came from the presence probe rather than from the"
                         " doctor, which is what it answers with when it was told no store"
                         " exists to have an open attempt in")

    def test_a_failed_update_that_never_reached_its_seam_is_not_a_recovery(self):
        """The guard on the guard.

        Every recovery assertion is an equality between two readings of the same state, and a run
        that refused before it touched anything satisfies all of them. So the sequence also
        requires the seam to have named itself and the arriving environment, and this case states
        that requirement directly: a refusal that never reached the seam names a different step.
        """
        with tempfile.TemporaryDirectory() as temporary:
            host = base._Host(temporary)
            clean(host)
            install(host)
            arriving = arriving_source(host)
            loaded, verified = updating(host)
            with loaded, verified:
                code, payload = install(host, breaking="create environment")

        self.assertEqual(code, 1)
        self.assertEqual(payload.get("failedStep"), "create environment")
        self.assertEqual(payload.get("environment"), str(arriving),
                         "the refusal names the environment it was building, so a refusal about"
                         " something else cannot be read as this one")

    def test_a_restoration_it_could_not_read_back_is_reported_rather_than_claimed(self):
        """The pointer seam, where the disk and the answer are allowed to disagree.

        The injected failure lands the replacement and then raises, which is the case the
        rollback exists for. The restoration puts the previous target back the same way and is
        stopped the same way, so the link on disk reaches the stage one install while the command
        could not read that back. It reports a residual pointer instead of claiming a
        restoration, and that is the honest direction: a host reading this result is told to look
        at the link rather than told it is fine.
        """
        with tempfile.TemporaryDirectory() as temporary:
            host = base._Host(temporary)
            clean(host)
            install(host)
            installed = host.candidate
            arriving_source(host)
            loaded, verified = updating(host)
            with loaded, verified:
                code, payload = install(host, breaking="replace the owned pointer")
            reached = pointer.read(host.pointer_path).get("target")

        self.assertEqual(code, 1)
        restored = (payload.get("pointer") or {}).get("pointerRestored") or {}
        self.assertFalse(restored.get("verified"),
                         "the restoration could not be read back, so it is not claimed")
        self.assertEqual(restored.get("residualPointer"), str(host.pointer_path),
                         "and the path a reader has to look at is named")
        self.assertEqual(reached, str(installed),
                         "while on disk the link does reach the install that has to keep working")
        self.assertTrue(payload.get("retriable"),
                        "nothing was left in the destination, so the destination can be retried")


def stand_in_runtime(host):
    """An interpreter with nothing of this machine in it, and the record pointing at it.

    The probes resolve where a component lives by importing it, and the bridge exercise starts
    a server, both under whatever interpreter the record names. Left to fall back, that is the
    interpreter running this suite, and on a host with these packages installed the probe would
    import and run them. Clearing PYTHONPATH and the user site does not reach a system site
    directory, and detecting the host location afterwards is too late: the import already
    happened.

    So the runtime is supplied rather than discovered, the same way the relay cases hand the
    preflight a runtime that HAS the relay in it. This one is the inverse: -S -E, so no site
    directory and no inherited import path, and the record names it for every component. What
    the probes can reach is then a decision this suite made rather than a property of the
    machine it happened to run on.
    """
    binaries = host.candidate / "bin"
    binaries.mkdir(parents=True, exist_ok=True)
    interpreter = binaries / "python"
    interpreter.write_text(
        "#!/bin/sh\nexec \"" + sys.executable + "\" -S -E \"$@\"\n", encoding="utf-8")
    interpreter.chmod(interpreter.stat().st_mode | stat.S_IEXEC | stat.S_IRWXU)

    record = hostrecord.load(host.record_path, host.data["definitionVersion"]).value or {}
    for component in host.data["components"]:
        script = binaries / component["consoleScript"]
        script.write_text("#!" + str(interpreter) + "\n", encoding="utf-8")
        script.chmod(script.stat().st_mode | stat.S_IEXEC | stat.S_IRWXU)
        for install in hostrecord.component(record, component["component"])["installs"]:
            install["interpreterPath"] = str(interpreter)
    hostrecord.save(host.record_path, record)
    return binaries, interpreter

def importable_runtime(host, root, paths=None):
    """A supplied runtime that CAN import the components, from inside the temporary directory.

    The import row is the one question whose answer nothing here was moving. A cell that holds
    the same value in every direction cannot catch a neighbour answering with its value, and it
    cannot tell a real reading from a command that stopped attempting one -- both look like
    not_verified forever.

    So the runtime is rebuilt to find two packages this test wrote, and nothing else. It keeps
    -S, so no site directory of this machine is reachable, and the only import path it is given
    is a directory under the temporary root. What moves is what can be imported, not where it
    is allowed to look.
    """
    if paths is None:
        stubs = root / "importable"
        for component in host.data["components"]:
            package = stubs / component["module"]
            package.mkdir(parents=True, exist_ok=True)
            (package / "__init__.py").write_text("", encoding="utf-8")
        paths = str(stubs)
    interpreter = host.candidate / "bin" / "python"
    interpreter.parent.mkdir(parents=True, exist_ok=True)
    interpreter.write_text(
        "#!/bin/sh\nexec \"" + sys.executable + "\" -S \"$@\"\n", encoding="utf-8")
    interpreter.chmod(interpreter.stat().st_mode | stat.S_IEXEC | stat.S_IRWXU)
    return paths

def diagnose_for(root, host, *extra, import_path=None):
    """Ask this host for a diagnosis, with whatever extra input the case is varying."""
    environment = dict(os.environ, **isolated(root, host))
    if import_path is not None:
        # Replacing, never appending: what the probe may import stays a decision of this suite.
        environment["PYTHONPATH"] = str(import_path)
    done = subprocess.run(
        [sys.executable, str(RUNTIME), "diagnose",
         "--codex-home", str(host.codex_home),
         "--dest", str(host.destination),
         "--record", str(host.record_path),
         "--state", str(host.state),
         "--socket", str(root / "no.sock"),
         # The supplied relay entry point, not a path that is missing: a console script that
         # does not resolve sends the probe back to whatever interpreter is running this suite,
         # which is the fallback this arrangement exists to avoid.
         "--relay-command", str(host.candidate / "bin" / "codex-session-relay"),
         "--temporary", *extra],
        capture_output=True, text=True, timeout=180,
        env=environment)
    return json.loads(done.stdout)


# Everything a trial sends, answered. A relay that refuses a step leaves the delivery row at
# not_verified, which is an honest answer to "did a delivery complete" and no answer at all to
# "can this command complete one".
TRIAL_ANSWERS = {
    "assignment-find": {},
    "register": {"relationshipId": "rel-1"},
    "settings-record": {},
    "generation-open": {"executionGeneration": 1},
    "generation-bind": {},
    "admit-turn": {},
    "emit": {"receipt": {"eventId": "event-1"}},
    "deliver": {"attempt": {"turnId": "turn-1"}},
    "doctor": {"contents": {"available": True, "openAttempts": 0},
               "actorReachability": {"socketConnect": "ok"}},
}

# What the relay own predicate requires of a recipient settings document, supplied in full so
# the preflight passes for the reason it exists rather than by being skipped.
RECIPIENT_SETTINGS = {"sandbox": {"type": "readOnly"}, "approvalPolicy": "never",
                      "cwd": "/tmp", "runtimeWorkspaceRoots": ["/tmp"], "model": "a-model",
                      "reasoningEffort": "low", "environments": {}}


def delivering_relay(root, *, doctor=True):
    """A relay that answers every step of a trial, so a delivery can actually complete.

    Its first line names a real interpreter inside the temporary directory. A console script
    whose shebang is a wrapper cannot be executed -- the kernel does not follow one shebang to
    another -- and one naming env answers the preflight with the literal two-word string, which
    is the trampoline shape the installer documents.
    """
    interpreter = root / "relay-python"
    if not interpreter.exists():
        interpreter.symlink_to(sys.executable)
    path = root / "delivering-relay"
    answers = dict(TRIAL_ANSWERS)
    if not doctor:
        # The connection cell asks the doctor and the delivery cell does not, so a relay used to
        # move delivery answers the trial and stays silent about reachability. Otherwise one
        # input would move two questions and neither could be said to answer on its own.
        answers["doctor"] = {"contents": {"available": True, "openAttempts": 0}}
    path.write_text(
        "#!" + str(interpreter) + "\n"
        "import json, sys\n"
        "answers = json.loads(" + repr(json.dumps(answers)) + ")\n"
        "step = next((a for a in sys.argv[1:] if a in answers), \"\")\n"
        "print(json.dumps(answers.get(step, {})))\n",
        encoding="utf-8")
    path.chmod(path.stat().st_mode | stat.S_IEXEC | stat.S_IRWXU)
    return path


def trial_inputs(root):
    """The required trial inputs, every one of them, written where the command can read them."""
    artifacts = root / "artifacts"
    artifacts.mkdir(exist_ok=True)
    artifact = artifacts / "result.txt"
    artifact.write_text("a deliverable this trial names", encoding="utf-8")
    settings = root / "recipient-settings.json"
    settings.write_text(json.dumps(RECIPIENT_SETTINGS), encoding="utf-8")
    return ["--issue", "CRW-69", "--parent-task", "parent", "--child-task", "child",
            "--recipient", "parent", "--artifact-root", str(artifacts),
            "--artifact", str(artifact), "--turn-thread", "child", "--turn-id", "turn-1",
            "--dispatch-turn-id", "anchor-1", "--recipient-settings", "@" + str(settings)]


def linked_skills(host):
    """Actually install the skill links, so the link row has something to read as linked."""
    done = subprocess.run(
        [sys.executable, str(ROOT / "scripts" / "install.py"), "--apply",
         "--dest", str(host.codex_home / "skills")],
        capture_output=True, text=True, timeout=120)
    return done


def installed_components(host):
    """Put the real packages INSIDE the candidate, so an import resolves in the installation.

    Pointing the import path at this checkout made the row read verified from source that was
    never installed: the build steps are stand-ins, so the candidate held neither package while
    the reading reported locations in the working tree. Source being available is not the
    question the row asks, and answering it that way is the substitution this module refuses.

    Copied rather than linked, and copied real rather than stubbed, because the trial preflight
    asks the relay own reader and predicate and a stub has neither.
    """
    site = host.candidate / "site"
    for component in definition.load()["components"]:
        target = site / component["module"]
        # Into the directory the build stand-in already made, not past it: it creates the module
        # directory empty, so a copy that skipped an existing path left a package with no module
        # in it and the import failed for a reason that had nothing to do with the question.
        shutil.copytree(ROOT / component["packageLocation"], target, dirs_exist_ok=True)
    return str(site)

def observe_all(root):
    """Every reading the criterion asks for, each taken from the source that answers it.

    One temporary host, installed once, then asked three separate questions in three separate
    ways. The point of gathering them here rather than in one payload is that no answer can be
    produced by another answer: the diagnosis never sees the hook, the hook never sees the
    configuration comparison, and the comparison never sees either.
    """
    host = base._Host(root)
    clean(host)
    seed_settings(host)
    earlier = host.config.read_text(encoding="utf-8")
    install(host)
    later = host.config.read_text(encoding="utf-8")

    stand_in_runtime(host)
    # Linked for real, so the listing row has a success to read here too rather than a baseline
    # of "every skill missing" that an assertion would then have to accept.
    linked_skills(host)

    # The SAME Codex home the runtime was installed into, the skills were linked into and the
    # registration was written into. Firing into a second home of its own would have answered
    # this row from a clean hook file with no relation to the host the other six readings
    # describe -- a composition of readings about two different machines is not a composition.
    _before, after, _registered, _done = fire_the_hook(host.codex_home)

    return {
        "diagnose": diagnose_for(root, host),
        "hook-status": after,
        ACCEPTANCE: _model_permission_delta(earlier, later),
    }, host


# ---------------------------------------------------------------------------
# What the two derived lists below can actually see, derived rather than remembered.
#
# Both checks used to recognise FORMS: one shape of ast.parse call, and three spellings of a
# refusal inside a call whose name begins with assert. A form nobody had thought of was a silent
# pass, and one of the three spellings was a hardcoded set of constant names -- the enumerated
# literal standing in for a derivation that this repository's evidence rule refuses. Four times
# now a derivation in this project has reached less far than the sentence describing it, and each
# time the argument for the sentence was that the set was derived. A derivation is only as
# complete as its predicate.
#
# So the rule here is the negative one. Derive every spelling of the declared thing by asking the
# objects, account for EVERY occurrence of one, and fail on an occurrence nobody wrote down. A
# form nobody anticipated is still an occurrence, so it arrives as a failure rather than as
# another review round. What the derivation still cannot reach is not argued away in a sentence:
# it is returned as data, declared, and each declared form carries a control that plants it and
# requires the derivation not to see it.
#
# Where the reader cannot tell, it errs in one direction on purpose. It follows names -- a handle,
# a called name, a name bound to one, an attribute a class binds -- and it does NOT follow flow.
# So a name that ever holds the thing is treated as holding it for that scope, even where a later
# line rebinds it. That is wrong in the direction where somebody has to write a sentence rather
# than in the direction where a place settles for a refusal unasked. Being precise about which
# binding a particular line saw would mean following flow, and a precise-looking answer this
# reader cannot actually justify is the thing this whole module exists to refuse.
#
# That is the direction it prefers where it can see at all, and it is not a claim of
# completeness. The forms it cannot see are the ones in SOURCE_OUT_OF_REACH and
# REFUSAL_OUT_OF_REACH: a name spelled as a string, a callable taken out of a registry, a helper
# in another module. A place arriving in one of those settles for a refusal and is never asked
# about, which is the cost of following names, and the reason those are written down as data with
# a control each rather than argued away here.
#
# Where a name IS in the text, agreeing with Python is not optional, and this reader resolves
# through classes while Python resolves through objects. Every measured disagreement between the
# two is in RESOLVES_LIKE_PYTHON with the sample that produces it and the answer Python gives.
#
# A class is keyed by the scope it is written in as well as its spelling, so two classes of one
# name in different functions are two classes to every table at once: the method index, the
# held-attribute table, the property index and the base list. This was recorded here as a limit
# for a long while, on the grounds that telling them apart would mean carrying the enclosing
# chain through all of those together. It would, and that is what it now does -- a limit of cost
# is not a limit of reach, and only what the reader genuinely cannot see belongs in a record of
# what it cannot see.
#
# Place names are the other question and still leave classes out of the chain, so two methods of
# one name in one function collide on purpose and the pin below fails rather than letting one
# declaration speak for both. That is what keeps every declaration from rotting: widening
# the derivation later makes the control for the form it now covers fail, which is the prompt to
# delete the entry rather than leave a limitation standing that stopped being true.

MODULE_LEVEL = "<module>"


def _dotted(node):
    """The name an expression spells, or None when it is not a plain dotted name."""
    if isinstance(node, ast.Name):
        return node.id
    if isinstance(node, ast.Attribute):
        base = _dotted(node.value)
        return base + "." + node.attr if base else None
    return None


def _places(tree):
    """Which place each node sits in, and which class, with MODULE_LEVEL for neither.

    The INNERMOST function, named by the chain of functions around it, because a nested helper
    and a lambda are places of their own: the check this replaces walked every FunctionDef and
    would have reported one, and attributing them to whatever encloses them would let a new
    conclusion arrive inside an already declared place and be absorbed. The chain is what keeps
    the names apart -- two helpers may both be called spelled -- while a top-level function or a
    method still answers to the plain name the declarations are keyed by. Classes are not part of
    the chain, so two methods of one name in one function collide on purpose, and the pin below
    fails rather than letting one declaration speak for both.

    The CLASS each node sits in is named the other way, by the scope it is written in as well as
    its spelling, because two classes of one name in different functions are two classes and the
    tables keyed by them must not merge the pair.
    """
    found = {}

    def walk(node, chain, klass):
        for child in ast.iter_child_nodes(node):
            if isinstance(child, (ast.FunctionDef, ast.AsyncFunctionDef, ast.Lambda)):
                # A decorator, a default and an annotation are evaluated where the def is
                # written and not inside it, so they stay in the scope around it.
                args = child.args
                every = (args.posonlyargs + args.args + args.kwonlyargs
                         + ([args.vararg] if args.vararg else [])
                         + ([args.kwarg] if args.kwarg else []))
                outside = (list(getattr(child, "decorator_list", []))
                           + list(args.defaults)
                           + [value for value in args.kw_defaults if value]
                           + [arg.annotation for arg in every if arg.annotation]
                           + ([child.returns] if getattr(child, "returns", None) else []))
                for beside in outside:
                    found[id(beside)] = (".".join(chain) if chain else MODULE_LEVEL, klass)
                    walk(beside, chain, klass)
            inner, owner = chain, klass
            if isinstance(child, (ast.FunctionDef, ast.AsyncFunctionDef)):
                inner = chain + [child.name]
            elif isinstance(child, ast.Lambda):
                # By line, because two lambdas in one class body are two places and a single
                # name for both would make one of them answer for the other.
                inner = chain + ["<lambda@" + str(child.lineno) + ">"]
            elif isinstance(child, (ast.ListComp, ast.SetComp, ast.DictComp,
                                    ast.GeneratorExp)):
                # A walrus inside a comprehension binds in the scope AROUND it, which is the
                # one special case Python carved out of comprehension scoping.
                def walruses(node):
                    """The walruses this comprehension owns, not the ones a lambda in it does."""
                    for inner in ast.iter_child_nodes(node):
                        if isinstance(inner, (ast.Lambda, ast.FunctionDef,
                                              ast.AsyncFunctionDef)):
                            continue
                        if isinstance(inner, ast.NamedExpr):
                            yield inner
                        yield from walruses(inner)

                outer_place = (".".join(chain) if chain else MODULE_LEVEL, klass)
                held_walruses = list(walruses(child))
                # A comprehension has a scope of its own on Python 3: its target shadows an
                # outer name INSIDE it and not after it. Its first iterable is the exception,
                # evaluated outside before that scope exists.
                if child.generators:
                    first = child.generators[0].iter
                    found[id(first)] = (".".join(chain) if chain else MODULE_LEVEL, klass)
                    walk(first, chain, klass)
                inner = chain + ["<comprehension@" + str(child.lineno) + ">"]
                # The target binds outside; what computes it is still evaluated inside, so only
                # the NamedExpr itself moves out and its value stays where it is written.
                for held in held_walruses:
                    found[id(held)] = outer_place
                    # The TARGET moves with it. Leaving the Name labelled with the
                    # comprehension's own scope meant every table keyed off where a name is
                    # bound read the binding as the comprehension's and missed the real one.
                    if isinstance(held.target, ast.Name):
                        found[id(held.target)] = outer_place
            elif isinstance(child, ast.ClassDef):
                # Qualified by the scope it is written in. Two classes spelled the same in
                # different functions are two classes, and a table keyed by the bare spelling
                # makes one of them answer for the other.
                owner = (klass + "." + child.name if klass
                         else (".".join(chain) + "." + child.name if chain else child.name))
            # setdefault, because a default or a decorator was already placed in the scope
            # that evaluates it and walking into the def must not take it back.
            found.setdefault(id(child), (".".join(inner) if inner else MODULE_LEVEL, owner))
            walk(child, inner, owner)

    walk(tree, [], None)
    return found


def _module_statements(tree):
    """Each top-level statement, named by what it binds or by the call it performs.

    A module-level occurrence has no function to be attributed to, and a line number is a key
    that moves whenever anything above it moves. What does not move is what the statement is
    for, so the statement answers with the names it binds.
    """
    named = {}
    statements = []
    def spread(prefix, body):
        # A class body first, so a statement in it is named by the class and what it binds
        # rather than by the class statement that happens to enclose it. Nested classes carry
        # the whole chain, since Outer.Inner is how a reader spells one.
        for statement in body:
            if isinstance(statement, ast.ClassDef):
                spread((prefix + "." if prefix else "") + statement.name, statement.body)
            statements.append((prefix, statement))

    spread("", tree.body)
    for klass, statement in statements:
        bound = []
        if isinstance(statement, ast.Assign):
            for target in statement.targets:
                bound += [n.id for n in ast.walk(target) if isinstance(n, ast.Name)]
        elif isinstance(statement, ast.AnnAssign):
            bound += [n.id for n in ast.walk(statement.target) if isinstance(n, ast.Name)]
        elif isinstance(statement, ast.Expr) and isinstance(statement.value, ast.Call):
            bound = [_dotted(statement.value.func) or "a call"]
        elif isinstance(statement, ast.Expr):
            # A walrus written as a statement binds a name, and that name is what it is for.
            bound = [inner.target.id for inner in ast.walk(statement.value)
                     if isinstance(inner, ast.NamedExpr)
                     and isinstance(inner.target, ast.Name)]
        if not bound and isinstance(statement, (ast.FunctionDef, ast.AsyncFunctionDef,
                                                ast.ClassDef)):
            # A def evaluates its decorators and defaults out here, so the statement answers
            # with the name it declares rather than with the kind of node it is.
            bound = [statement.name]
        name = ", ".join(bound) if bound else type(statement).__name__
        if klass:
            name = klass + "." + name
        for node in ast.walk(statement):
            named.setdefault(id(node), name)
    return named


def _owned_by_a_class(tree):
    """Every node a class body owns, by id: a walrus and a match case as much as a statement.

    Not what a def or a class inside it owns -- those are scopes of their own -- so the walk
    stops there rather than claiming their insides for the class.
    """
    def under(nodes):
        for node in nodes:
            if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef,
                                 ast.Lambda)):
                continue
            yield id(node)
            yield from under(ast.iter_child_nodes(node))

    return {inner for node in ast.walk(tree) if isinstance(node, ast.ClassDef)
            for inner in under(node.body)}


def _binds_locally(node):
    """Every name this statement binds locally, whatever construct does the binding."""
    targets = []
    if isinstance(node, ast.Assign):
        targets = list(node.targets)
    elif isinstance(node, (ast.AnnAssign, ast.AugAssign)):
        targets = [node.target]
    elif isinstance(node, (ast.For, ast.AsyncFor, ast.comprehension)):
        # A for target binds in the function; a comprehension target binds in the
        # comprehension, which is now a scope of its own, so both are collected here and
        # each lands in the place it belongs to.
        targets = [node.target]
    elif isinstance(node, ast.withitem):
        targets = [node.optional_vars] if node.optional_vars else []
    elif isinstance(node, ast.ExceptHandler):
        return {node.name} if node.name else set()
    elif isinstance(node, (ast.Import, ast.ImportFrom)):
        return {alias.asname or alias.name.split(".")[0] for alias in node.names}
    elif isinstance(node, ast.NamedExpr):
        targets = [node.target]
    elif isinstance(node, (ast.ClassDef, ast.FunctionDef, ast.AsyncFunctionDef)):
        # A class or a function defined in a scope takes that name in it.
        return {node.name}
    elif isinstance(node, (ast.MatchAs, ast.MatchStar)):
        return {node.name} if node.name else set()
    elif isinstance(node, ast.MatchMapping):
        return {node.rest} if node.rest else set()
    elif isinstance(node, ast.Delete):
        # del makes the name local for the whole function: a later read raises rather than
        # reaching the module's, so it shadows exactly as an assignment does.
        return {inner.id for target in node.targets for inner in ast.walk(target)
                if isinstance(inner, ast.Name)}
    found = set()
    for target in targets:
        # Only what the target BINDS. mapping[carrier] = value and carrier.attr = value read
        # the name rather than binding it, and marking those local would stop the search
        # before a definition the call really does reach.
        found.update(inner.id for inner in ast.walk(target)
                     if isinstance(inner, ast.Name) and isinstance(inner.ctx, ast.Store))
    return found


def _bindings(node):
    """Each name a statement binds paired with what it is given, unpacking included.

    alias, other = carrier, str gives each name its own value; reading the tuple as one would
    give both names both values and lose which is which.
    """
    if isinstance(node, ast.NamedExpr):
        holders, answer = [node.target], node.value
    elif isinstance(node, (ast.Assign, ast.AnnAssign)) and node.value is not None:
        holders = node.targets if isinstance(node, ast.Assign) else [node.target]
        answer = node.value
    elif isinstance(node, ast.withitem) and node.optional_vars is not None:
        # with open(HERE) as stream binds the name to what the expression answered, exactly as
        # an assignment does, and a read taken on it reaches the same file.
        holders, answer = [node.optional_vars], node.context_expr
    elif isinstance(node, (ast.For, ast.AsyncFor)) and isinstance(
            node.iter, (ast.Tuple, ast.List, ast.Set, ast.Dict)):
        # for answer in (reading.UNREADABLE,) binds the target to each element written in the
        # iterable, and the loop really does run. A dict is iterated over its KEYS, which are
        # as plainly written here as a tuple's elements. An iterable this text cannot read
        # binds nothing rather than a guess about what it yields, which is why only the
        # written forms are paired.
        holders, answer = [], None
    else:
        return []
    def pair(holder, value):
        if (isinstance(holder, (ast.Tuple, ast.List))
                and isinstance(value, (ast.Tuple, ast.List))):
            starred = [index for index, inner in enumerate(holder.elts)
                       if isinstance(inner, ast.Starred)]
            if len(starred) == 1 and len(value.elts) >= len(holder.elts) - 1:
                # answer, *ignored = (reading.UNREADABLE,) binds deterministically: the names
                # before the star take the first values and the names after it take the last.
                # The starred name itself holds a LIST of what is left, which is a container
                # rather than the thing, so it is paired with nothing.
                at, after = starred[0], len(holder.elts) - starred[0] - 1
                return ([entry for inner, given in zip(holder.elts[:at], value.elts[:at])
                         for entry in pair(inner, given)]
                        + ([entry for inner, given in zip(
                            holder.elts[at + 1:], value.elts[len(value.elts) - after:])
                            for entry in pair(inner, given)] if after else []))
        if (isinstance(holder, (ast.Tuple, ast.List))
                and isinstance(value, (ast.Tuple, ast.List))
                and len(holder.elts) == len(value.elts)):
            # Nested, because other, (alias,) = str, (carrier,) unpacks twice.
            return [entry for inner, given in zip(holder.elts, value.elts)
                    for entry in pair(inner, given)]
        return [(holder, value)]

    if isinstance(node, (ast.For, ast.AsyncFor)):
        written = node.iter.keys if isinstance(node.iter, ast.Dict) else node.iter.elts
        return [entry for element in written if element is not None
                for entry in pair(node.target, element)]
    return [entry for holder in holders for entry in pair(holder, answer)]


def _reachable(expression, spelled, klass, bound):
    """Whether the thing is reachable in this expression without entering a container.

    The distinction decides what a helper hands on. A helper returning the thing itself -- or a
    conditional, a boolean expression or a name bound to it -- hands it to its caller, and the
    caller is then a place nothing would otherwise account for. A helper returning a dict that
    happens to carry one does not: read() answers with a row whose value may be a refusal, and
    attributing that to every caller would attribute read() to every acceptance row, which would
    leave the inventory unable to distinguish anything.

    So this descends only where the expression's own value comes from: the branches of a
    conditional, the operands of a boolean, what an await waits for and what a named expression
    binds. It does not descend into a comparison or into a
    call's arguments, because mentioning the thing while answering a different question -- as
    value.suffix == ".py" does -- is not handing it back.
    """
    if spelled(expression, klass) is not None:
        return True
    if isinstance(expression, ast.Name) and expression.id in bound:
        return True
    if isinstance(expression, ast.IfExp):
        return (_reachable(expression.body, spelled, klass, bound)
                or _reachable(expression.orelse, spelled, klass, bound))
    if isinstance(expression, ast.BoolOp):
        return any(_reachable(value, spelled, klass, bound) for value in expression.values)
    if isinstance(expression, ast.Await):
        return _reachable(expression.value, spelled, klass, bound)
    if isinstance(expression, ast.NamedExpr):
        return _reachable(expression.value, spelled, klass, bound)
    if isinstance(expression, ast.JoinedStr):
        # f"{answer}" with nothing beside it, no conversion and no format spec IS the value's
        # own string: for a refusal spelled as a string the result is the same string, so the
        # helper really does hand it on. Anything else -- text around it, !r, a format spec --
        # builds a NEW string, which is the transformation boundary declared in
        # HANDED_ON_THROUGH_A_TRANSFORMATION rather than a value handed on.
        written = [part for part in expression.values
                   if not (isinstance(part, ast.Constant) and part.value == "")]
        if (len(written) == 1 and isinstance(written[0], ast.FormattedValue)
                and written[0].conversion == -1 and written[0].format_spec is None):
            return _reachable(written[0].value, spelled, klass, bound)
        return False
    return False


def _class_aliases(tree):
    """Each name bound to a class written here, paired with the class it names.

    Alias = Base makes class Child(Alias) inherit from Base, so the line a name is searched in
    is built from what the bases NAME rather than from how they are spelled. To a fixpoint,
    because an alias of an alias names the same class.

    The fixpoint is monotone by construction: what grows is the SET of classes each name has
    ever been bound to in a scope, and a set only ever gains members, so the loop runs at most
    once per (scope, name, class) and then stops. Writing the answer itself and re-running while
    it differs is not a fixpoint at all -- two classes bound to one name flip it forever -- and
    a derivation that does not halt is worse than one that answers narrowly.

    One class is an alias. Two different ones in the same scope mean which it holds depends on
    where the read is written, and these tables are keyed by scope rather than by position in
    it, so the name names no class here rather than naming whichever was seen last.
    """
    places = _places(tree)
    by_spelling = _class_spellings(tree, places)
    classes = {places.get(id(node), (MODULE_LEVEL, None))[1] for node in ast.walk(tree)
               if isinstance(node, ast.ClassDef)}
    # Per scope, and unconditional only. A name bound inside a function is that function's --
    # which a class written in that same function really does see -- and one under an if is a
    # guess about the run. An annotated binding is a binding.
    bodies = [(MODULE_LEVEL, list(getattr(tree, "body", ())))]
    for node in ast.walk(tree):
        if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
            bodies.append((places.get(id(node), (MODULE_LEVEL, None))[0], list(node.body)))
        elif isinstance(node, ast.ClassDef):
            # A class body is a scope a nested class really does close over: Python resolves a
            # base name from the enclosing class namespace while it executes the inner one.
            bodies.append((places.get(id(node), (MODULE_LEVEL, None))[0] + "." + node.name
                           if places.get(id(node), (MODULE_LEVEL, None))[0] != MODULE_LEVEL
                           else node.name, list(node.body)))
    named, ever, growing = {}, {}, True
    while growing:
        growing = False
        for scope, body in bodies:
            here = named.setdefault(scope, {})
            held = ever.setdefault(scope, {})
            for node in body:
                # Pair by pair: Alias, ignored = First, None names the class with ONE of them,
                # and reading the statement's whole value paired the class with both or with
                # neither. The third table handed this after the opener and the refusal owner,
                # which is why the sweep below asks it of every one of them.
                for target, value in _bindings(node):
                    # Every name the value may hand through, so Alias = First if flag else
                    # Second is read as the two bindings it is. Arms that agree resolve; arms
                    # that name different classes make the alias undecidable by the rule below,
                    # which is the honest answer rather than whichever arm was written first.
                    spellings_here = [source.id for source in _passed_through(value)]
                    if not spellings_here:
                        spellings_here = [(_dotted(value) or "").rpartition(".")[2]]
                    for spelling in spellings_here:
                        named_key = _class_named(by_spelling, spelling, scope)
                        reached = (named_key if named_key in classes
                                   else here.get(spelling)
                                   or named.get(MODULE_LEVEL, {}).get(spelling))
                        bound = _dotted(target)
                        if not bound:
                            continue
                        seen = held.setdefault(bound, set())
                        if len(held.get(spelling, ())) > 1:
                            # An alias OF an undecidable name is undecidable too. Asked before the
                            # value is resolved, because an undecidable name resolves to nothing and
                            # would otherwise leave whichever pass ran first standing -- which makes
                            # the answer depend on the order these bodies are walked.
                            if not seen >= held[spelling]:
                                seen |= held[spelling]
                                growing = True
                            here.pop(bound, None)
                            continue
                        if not reached:
                            continue
                        if reached in seen:
                            continue
                        seen.add(reached)
                        growing = True
                        if len(seen) == 1:
                            here[bound] = reached
                        else:
                            # Undecidable rather than last-seen, and it stays undecidable, which is
                            # what keeps the measure growing instead of oscillating.
                            here.pop(bound, None)
    return named


def _defined_in_scope(tree, places):
    """Which scope each def and each class name is bound in: the one that WRITES it.

    _places labels a definition with the scope it OPENS. That is the right answer for its
    parameters and the wrong one for its own name. `def CHANGED()` inside outer() binds CHANGED
    in outer, so a closure under it reads that rather than the module's, and filing the name
    under the definition's own scope makes a function shadow itself while the scope that really
    binds it holds nothing.

    Derived once and read by both tables that ask the question, because the two had drifted
    apart: one left definitions out altogether and the other filed them one scope too deep.

    A class body is not the function around it, so a def written in one is that body's and is
    not reported here.
    """
    def defined_here(body):
        """Every def or class name this body takes, down through the statements that open no
        scope of their own. A def under an if is still written here; one inside another def
        belongs to that def."""
        for statement in body:
            if isinstance(statement, (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef)):
                yield statement.name
                continue
            for child in ast.iter_child_nodes(statement):
                yield from defined_here([child])

    bound = {}
    for node in ast.walk(tree):
        if not isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
            continue
        where = places.get(id(node), (MODULE_LEVEL, None))[0]
        for named in defined_here(node.body):
            bound.setdefault(where, set()).add(named)
    return bound


def _visible_from(scope, made):
    """Whether a use in this scope sees a handle derived in one of those scopes.

    Module level encloses every function, and it is written as a name of its own rather than as
    a prefix of theirs, so the chain test alone cannot see it: a handle derived beside the
    functions looked local to none of them, and every reader of it was dropped as a different
    object wearing the same name.

    Asked in one place because two tables asked it separately and would drift apart, which is
    how most of the defects in this file have arrived.
    """
    return any(owner == MODULE_LEVEL or scope == owner or scope.startswith(owner + ".")
               for owner in made)


def _defaults(node):
    """Each parameter paired with the default it is given, which is a binding like any other.

    def outer(text=HERE.read_text()) makes text hold that text whenever the caller omits the
    argument, and a closure under outer reads it. Seeding only from assignment-like statements
    loses the name entirely, so the place that reads it is never reported.

    Asked of the definition node, because _places labels that with the scope the definition
    OPENS -- which is exactly the scope a parameter is bound in, however the default beside it
    was written.
    """
    args = getattr(node, "args", None)
    if not isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef, ast.Lambda)):
        return []
    positional = args.posonlyargs + args.args
    given = list(args.defaults)
    paired = list(zip(positional[len(positional) - len(given):], given)) if given else []
    paired += [(arg, default) for arg, default in zip(args.kwonlyargs, args.kw_defaults)
               if default is not None]
    return [(ast.copy_location(ast.Name(id=arg.arg, ctx=ast.Store()), arg), default)
            for arg, default in paired]


def _lambda_named(value):
    """Each lambda a value expression may hand over, the branches of a conditional included.

    The vocabulary beside _passed_through, asked for lambdas rather than for names.
    helper = lambda path=HERE: ... if flag else other binds a lambda through a form a bare
    isinstance check does not see, and a reader that sees only a bare Lambda would claim a
    reach it does not have. Await is in the list because the vocabulary is, not because a
    lambda is awaitable.
    """
    if isinstance(value, ast.Lambda):
        yield value
    elif isinstance(value, ast.IfExp):
        yield from _lambda_named(value.body)
        yield from _lambda_named(value.orelse)
    elif isinstance(value, ast.BoolOp):
        for inner in value.values:
            yield from _lambda_named(inner)
    elif isinstance(value, (ast.NamedExpr, ast.Await)):
        yield from _lambda_named(value.value)


def _bound_names(tree, places):
    """Every name each scope binds, by whatever construct binds it.

    A name is the scope's own if anything in it writes the name: an assignment, a loop target,
    a with target, a walrus, a parameter, an import, an except handler, a def or a class. Two
    questions need this -- which scope owns a nonlocal, and whether a nearer binding stands in
    front of a definition a call might otherwise reach -- and they are asked here once rather
    than answered twice, which is how most of the defects in this file have arrived.

    A name the scope declares global or nonlocal is NOT bound here, however plainly the
    assignment under that declaration is written: the write lands in the scope the declaration
    names. Answering otherwise makes a declaring scope look like the owner and stops both of
    the questions above one scope short.
    """
    binds, declared = {}, {}
    for node in ast.walk(tree):
        if isinstance(node, (ast.Global, ast.Nonlocal)):
            declared.setdefault(places.get(id(node), (MODULE_LEVEL, None))[0],
                                set()).update(node.names)
    for node in ast.walk(tree):
        at = places.get(id(node), (MODULE_LEVEL, None))[0]
        if isinstance(node, ast.Name) and isinstance(node.ctx, ast.Store):
            # Assignments, loop targets, with targets, walrus and augmented writes all spell
            # the bound name as a Store, so one test covers the lot.
            if node.id not in declared.get(at, ()):
                binds.setdefault(at, set()).add(node.id)
        elif isinstance(node, (ast.Import, ast.ImportFrom)):
            for imported in node.names:
                spelled = (imported.asname or imported.name).partition(".")[0]
                if spelled not in declared.get(at, ()):
                    binds.setdefault(at, set()).add(spelled)
        elif isinstance(node, ast.ExceptHandler) and node.name:
            if node.name not in declared.get(at, ()):
                binds.setdefault(at, set()).add(node.name)
        elif isinstance(node, ast.ClassDef):
            # _places labels a class with the scope it is WRITTEN in, which is the scope its
            # name is bound in.
            if node.name not in declared.get(at, ()):
                binds.setdefault(at, set()).add(node.name)
        if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef, ast.Lambda)):
            if not isinstance(node, ast.Lambda):
                # A def is labelled with the scope it OPENS, but binds its name in the one
                # around it.
                around = at.rpartition(".")[0] or MODULE_LEVEL
                if node.name not in declared.get(around, ()):
                    binds.setdefault(around, set()).add(node.name)
            # A parameter binds its name in the scope the definition opens.
            args = node.args
            for arg in (args.posonlyargs + args.args + args.kwonlyargs
                        + ([args.vararg] if args.vararg else [])
                        + ([args.kwarg] if args.kwarg else [])):
                binds.setdefault(at, set()).add(arg.arg)
    return binds


def _seating_not_established(tree):
    """Every call argument whose seating is outside the grammar this reader supports.

    Seating an argument means saying which parameter it fills. The forms that answer that are
    a written argument, a keyword, a written tuple or list unpacking, and a written mapping.
    A SET unpacking is not one of them: a set has no order, so which element fills which slot
    is not a fact the text holds, and reading it off the order the source happens to list
    would be an answer taken from something that is not there. An unpacking or a mapping this
    text cannot read is outside for the plainer reason.

    Reported rather than guessed at AND rather than dropped. Nothing is seated past one of
    these, so the claim matches what was verified; and the occurrence is emitted here so an
    undeclared one fails acceptance instead of passing quietly. That is the whole of what this
    reader offers outside its grammar: not an answer, and not silence either.
    """
    outside = {}
    for node in ast.walk(tree):
        if not isinstance(node, ast.Call):
            continue
        for given in node.args:
            if not isinstance(given, ast.Starred) or isinstance(
                    given.value, (ast.Tuple, ast.List)):
                continue
            outside.setdefault("a set unpacking" if isinstance(given.value, ast.Set)
                               else "an unpacking this text cannot read",
                               set()).add(given.lineno)
        for word in node.keywords:
            if word.arg is None and not isinstance(word.value, ast.Dict):
                outside.setdefault("a mapping this text cannot read", set()).add(node.lineno)
    return {form: sorted(lines) for form, lines in outside.items()}


def _declared_owner(tree, places):
    """Each (scope, name) that a global or nonlocal statement hands to a different scope.

    global answer inside a function writes the MODULE binding, so a name bound under that
    declaration belongs to the module and a reader anywhere sees it. nonlocal hands it to the
    function around this one. Filing such a binding under the scope that WROTE it hides it from
    every reader outside that scope, and the consumer that really receives it goes unreported --
    a place escaping the inventory, which is the direction that costs something.

    Asked once and consulted by both the refusal side and the handle side, because these two
    have answered the same question separately before and drifted apart.

    nonlocal is resolved the way Python resolves it: outward to the NEAREST enclosing scope
    that really binds the name, however many scopes that is. Filing it against the immediately
    enclosing function instead was one scope short as soon as the declaration sat two functions
    below its binding, and the sibling reading it at the owning scope was dropped.
    """
    # A nonlocal names whatever the enclosing scope binds, however it binds it.
    binds = _bound_names(tree, places)
    owner = {}
    for node in ast.walk(tree):
        if not isinstance(node, (ast.Global, ast.Nonlocal)):
            continue
        at = places.get(id(node), (MODULE_LEVEL, None))[0]
        if at == MODULE_LEVEL:
            continue
        for name in node.names:
            if isinstance(node, ast.Global):
                owner[(at, name)] = MODULE_LEVEL
                continue
            # Outward from the scope around this one, stopping at the first that binds it.
            reach = at.split(".")
            reach.pop()
            while reach and name not in binds.get(".".join(reach), set()):
                reach.pop()
            owner[(at, name)] = (".".join(reach) if reach
                                 else at.rpartition(".")[0] or MODULE_LEVEL)
    return owner


def _default_applies(tree, places):
    """Which parameter defaults any call in this source actually leaves to the default.

    A default binds the parameter only when the caller omits the argument. Recording every
    default as a handle unconditionally taints the ordinary supplied argument too, which turns
    one missing consumer into a crowd of invented ones -- so a default every call overrides is
    not a binding here at all.

    Keyed by the DEFINITION's own scope, not by its bare name: two scopes may each write a
    helper with the same parameter, and merging their calls lets one scope's omitted argument
    answer for the other's supplied one. A call resolves to the definition its own scope can
    see, innermost first, the way every other name is resolved here.

    Answered per definition and parameter across that definition's calls rather than per call
    site: if ANY call omits it the default really does apply somewhere, and this reader errs
    towards reporting. A definition NO resolved call in this text reaches keeps its default for
    the same reason: nothing here overrides it, so the parameter holds what the signature says.
    Only a definition every resolved call supplies is read as never taking it.

    Observable boundary, stated because it is one: a call through a name this text cannot
    resolve to a definition is not counted. It cannot contradict the default either, so such a
    function is read as TAKING it -- the reporting direction, which is the one that costs a
    sentence rather than a place.
    """
    supplied, defined, owned, receives, bound_to_its_class = {}, {}, {}, set(), set()
    by_spelling = _class_spellings(tree, places)
    alias_of = _class_aliases(tree)
    bound_here = _bound_names(tree, places)
    _classes_here, holds_an, _built_here = _instance_classes(tree)

    def in_a_class_body(body):
        """Every def a class body makes, including ones written under an if or a try."""
        for statement in body:
            if isinstance(statement, (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef)):
                if not isinstance(statement, ast.ClassDef):
                    yield statement
                continue
            for child in ast.iter_child_nodes(statement):
                yield from in_a_class_body([child])

    # Which defs are methods, which is what decides whether a receiver is handed over
    # unwritten. The SPELLING of the first parameter does not decide it: def helper(this, ...)
    # is a method and a staticmethod whose first parameter happens to be named self is not.
    methods_here = {id(made) for node in ast.walk(tree) if isinstance(node, ast.ClassDef)
                    for made in in_a_class_body(node.body)}

    def names_a_class(scope, name):
        """Whether this spelling names a class here, through an alias too.

        One shared question rather than a copy: the receiver seating asks the same thing, and
        when this one knew only the original spelling an aliased owner was read as an instance.
        """
        return _owner_names_a_class(by_spelling, alias_of, scope, name)

    def taken_anywhere(name, scope):
        """Whether anything in this scope chain binds the name, innermost first.

        Asked of a decorator spelling: classmethod = staticmethod makes @classmethod hand no
        receiver over, so the word cannot be read as its builtin once the text takes the name.
        """
        reach = [] if scope == MODULE_LEVEL else scope.split(".")
        while True:
            if name in bound_here.get(".".join(reach) if reach else MODULE_LEVEL, set()):
                return True
            if not reach:
                return False
            reach.pop()

    for node in ast.walk(tree):
        if not isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef, ast.Lambda)):
            continue
        at = places.get(id(node), (MODULE_LEVEL, None))[0]
        if not isinstance(node, ast.Lambda):
            owned.setdefault(at.rpartition(".")[0] or MODULE_LEVEL,
                             {}).setdefault(node.name, set()).add(at)
        spelled = node.args.posonlyargs + node.args.args
        if spelled and id(node) in methods_here:
            # A bound method is handed its receiver before any written argument, so the slot a
            # default sits in is one further along than the call's own arguments count. Only
            # decorators known to keep that receiver qualify: staticmethod removes it, and a
            # decorator this text cannot read might, so an unrecognised one means no offset --
            # the direction that reports a place rather than dropping one.
            worn = {(_dotted(dressed) or "").rpartition(".")[2]
                    for dressed in getattr(node, "decorator_list", ())}
            # A decorator is resolved where it is written before its spelling is read: the
            # decorators are evaluated in the scope AROUND the def, and a name this text
            # rebinds there does not mean what it spells. Rebound means unrecognised, which
            # means no offset -- the direction that names a place rather than dropping one.
            if (worn <= {"property", "cached_property", "classmethod", "abstractmethod"}
                    and not any(taken_anywhere(name, at.rpartition(".")[0] or MODULE_LEVEL)
                                for name in worn)):
                receives.add(at)
                if "classmethod" in worn:
                    # A classmethod is bound through the CLASS as well as through an instance,
                    # so Holder.helper(x) hands cls over unwritten where an ordinary method
                    # spelled that way writes its receiver.
                    bound_to_its_class.add(at)
        given = list(node.args.defaults)
        for index, argument in enumerate(spelled[len(spelled) - len(given):] if given else []):
            defined[(at, argument.arg)] = len(spelled) - len(given) + index
        for argument, default in zip(node.args.kwonlyargs, node.args.kw_defaults):
            if default is not None:
                defined[(at, argument.arg)] = None
    # A lambda answers to whatever it was bound to rather than to a name of its own, so the
    # binding is what makes helper() resolve to it. _defaults already reads a lambda's
    # parameters; leaving the definition out here meant its pair never entered the answer and
    # every consumer of a lambda default was dropped once this question started gating them.
    for node in ast.walk(tree):
        for named, value in _bindings(node):
            if not isinstance(named, ast.Name):
                continue
            for made in _lambda_named(value):
                owned.setdefault(places.get(id(node), (MODULE_LEVEL, None))[0],
                                 {}).setdefault(named.id, set()).add(
                                     places.get(id(made), (MODULE_LEVEL, None))[0])
    # alias = helper makes alias() the same call. Reading only the literal spelling left every
    # call through an alias unresolved, which reads as nobody calling helper at all -- and a
    # definition no call reaches keeps its default, so a helper whose every call overrides the
    # default was reported as taking it. To a fixpoint, because an alias of an alias is one.
    # The scope's own definition wins: a rebinding is not registered over a def written here.
    spreading = True
    while spreading:
        spreading = False
        for node in ast.walk(tree):
            at = places.get(id(node), (MODULE_LEVEL, None))[0]
            for named, value in _bindings(node):
                if not isinstance(named, ast.Name):
                    continue
                if named.id in owned.get(at, {}):
                    continue
                # Through the pass-through vocabulary, so an alias bound by a conditional is
                # still an alias rather than a form this loop cannot see.
                for spelled_as in _passed_through(value):
                    reach = [] if at == MODULE_LEVEL else at.split(".")
                    target = set()
                    while reach and not target:
                        target = set(owned.get(".".join(reach), {}).get(spelled_as.id) or ())
                        reach.pop()
                    if not target:
                        target = set(owned.get(MODULE_LEVEL, {}).get(spelled_as.id) or ())
                    # EVERY arm a conditional may hand over, not the first: with one arm's
                    # default left uncontradicted it stays applicable and reports a caller
                    # that can never receive it.
                    if target - owned.get(at, {}).get(named.id, set()):
                        owned.setdefault(at, {}).setdefault(named.id, set()).update(target)
                        spreading = True
    for node in ast.walk(tree):
        if not isinstance(node, ast.Call):
            continue
        named = (_dotted(node.func) or "").rpartition(".")[2]
        if not named:
            continue
        at = places.get(id(node), (MODULE_LEVEL, None))[0]
        if isinstance(node.func, ast.Attribute):
            # A qualified call reaches a definition written here only when the owner is a
            # class written here or a name holding an instance of one. other.helper("fine")
            # on an imported module is not this helper, and letting the terminal spelling
            # answer for it made an unrelated call contradict this definition's default.
            spelled_owner = (_dotted(node.func.value) or "").rpartition(".")[2]
            if not (spelled_owner
                    and (names_a_class(at, spelled_owner)
                         or (isinstance(node.func.value, ast.Name)
                             and id(node.func.value) in holds_an))):
                continue
        elif not isinstance(node.func, ast.Name):
            continue
        # Outward, stopping at the FIRST scope that binds the name: a definition there is the
        # callee, and any other binding stands in front of it. Walking past a parameter named
        # like the function let an unrelated call answer for the definition, which suppressed
        # a default the real function never has supplied.
        reach = [] if at == MODULE_LEVEL else at.split(".")
        target = set()
        while True:
            here = ".".join(reach) if reach else MODULE_LEVEL
            target = set(owned.get(here, {}).get(named) or ())
            if target or named in bound_here.get(here, set()) or not reach:
                break
            reach.pop()
        if not target:
            continue
        for (place, argument), where in defined.items():
            if place not in target:
                continue
            # The receiver moves the slot only when the call does not WRITE it. holder.helper(x)
            # hands the instance over unwritten, so x lands one slot further along; the unbound
            # Holder.helper(obj, x) writes it, and counting a slot it never occupied there would
            # read a supplied argument as an omitted one and drop a real read.
            owner = node.func.value if isinstance(node.func, ast.Attribute) else None
            spelled_owner = (_dotted(owner) or "").rpartition(".")[2] if owner is not None else ""
            seated = 1 if (place in receives and owner is not None and spelled_owner
                           and (place in bound_to_its_class
                                or not names_a_class(
                                    places.get(id(node), (MODULE_LEVEL, None))[0],
                                    spelled_owner))
                           ) else 0
            # How many arguments a call really writes. Counting the Starred node itself said
            # helper(*()) supplied one and dropped every consumer that receives the default.
            # The seating grammar expands a written tuple or list, so helper(*("fine",)) does
            # supply it; a set or an unpacking this text cannot read stops the count, which is
            # the same boundary ARGUMENT_FORM_NOT_SEATED declares -- this consumer reading it
            # differently from that declaration was the claim and the implementation
            # disagreeing about the same grammar.
            written, countable = 0, True
            for given in node.args:
                if not isinstance(given, ast.Starred):
                    written += 1
                elif isinstance(given.value, (ast.Tuple, ast.List)):
                    written += len(given.value.elts)
                else:
                    countable = False
                    break
            # A written mapping names its own slots, the same way a keyword written out does.
            spelled_out = {key.value for word in node.keywords
                           if word.arg is None and isinstance(word.value, ast.Dict)
                           for key in word.value.keys
                           if isinstance(key, ast.Constant) and isinstance(key.value, str)}
            handed = (countable and where is not None
                      and written > where - seated) or argument in spelled_out or any(
                word.arg == argument for word in node.keywords)
            supplied.setdefault((place, argument), set()).add(handed)
    # Absent from supplied means no resolved call reached that definition at all, which is not
    # evidence the default is overridden. Reading absence as "never takes its default" lost
    # every closure over a default in a function this text never calls.
    return {pair for pair in defined if False in supplied.get(pair, {False})}


def _passed_through(value):
    """Every name a value expression may hand through, the branches of a conditional included.

    `that = self if flag else self` binds the receiver whichever branch runs, and `a or b`,
    `(y := a)` and `await a` hand one through just as plainly. A resolver reading only ast.Name
    sees none of them, which is a narrower reach than the sentence beside it claims.

    This is the vocabulary of pass-through forms this module reads; a resolver that follows a
    value is expected to follow it through here, and the derived reach check below fails any
    that reads ast.Name alone without saying why.
    """
    if isinstance(value, ast.Name):
        yield value
    elif isinstance(value, ast.IfExp):
        yield from _passed_through(value.body)
        yield from _passed_through(value.orelse)
    elif isinstance(value, ast.BoolOp):
        for inner in value.values:
            yield from _passed_through(inner)
    elif isinstance(value, (ast.NamedExpr, ast.Await)):
        yield from _passed_through(value.value)


def _assigned(node):
    """The targets and value of a binding statement, annotated or not, or None if it is neither.

    `carrier: property = property(getter)` installs the same descriptor as the bare assignment,
    and `fopen: object = open` rebinds the opener exactly as `fopen = open` does. A table that
    reads only ast.Assign answers differently from the table beside it, which is the defect this
    module keeps producing, so the question is asked in one place.

    An annotation with no value binds nothing: `carrier: property` declares a type and leaves
    the name unbound, which is not the same as binding it to something this reader cannot read.

    A named expression binds too: (answers := reading) gives the name the module exactly as the
    bare assignment does. Answering None for it left every table built on this helper -- refusal
    owners, class aliases, opener aliases -- unable to see an alias spelled that way, while
    _bindings beside it had always read one.
    """
    if isinstance(node, ast.Assign):
        return node.targets, node.value
    if isinstance(node, ast.AnnAssign) and node.value is not None:
        return [node.target], node.value
    if isinstance(node, ast.NamedExpr):
        return [node.target], node.value
    return None


def _names_module(qualifies, scope, spelled_as, fallback):
    """Which modules a qualifier may name, read innermost-first from the scope that wrote it.

    Two functions may import different modules under one alias, so the answer belongs to a
    scope rather than to the file: keying it file-wide lets the later import overwrite the
    earlier one and answers for both with whichever landed last.

    Within ONE scope the answer is every module the alias is bound to, not the last. A function
    may import the owner, read an answer off it, and rebind the name afterwards; the read
    already happened, and these tables are keyed by scope rather than by position in it. Taking
    the last binding silently loses that read, so a name that EVER holds the owner is treated
    as holding it -- the direction this reader errs in everywhere else.
    """
    reach = [] if scope == MODULE_LEVEL else scope.split(".")
    while reach:
        found = qualifies.get(".".join(reach), {}).get(spelled_as)
        if found:
            return frozenset(found)
        reach.pop()
    found = qualifies.get(MODULE_LEVEL, {}).get(spelled_as)
    return frozenset(found) if found else frozenset({fallback} if fallback else ())


def _nearest(bound, scope, name):
    """How near the innermost scope binding that name is, counted outward from the module.

    An exemption keyed to an enclosing scope is a fact about THAT scope. A parameter written
    closer to the read is a fact about this one, and it wins: an outer import of a refusal does
    not survive an inner function taking the same name from its caller. -1 when nothing binds
    it, so an exemption with no shadow beside it still stands.
    """
    depth = 0 if name in bound.get(MODULE_LEVEL, ()) else -1
    reach = [] if scope == MODULE_LEVEL else scope.split(".")
    here = []
    for part in reach:
        here.append(part)
        if name in bound.get(".".join(here), ()):
            depth = len(here)
    return depth


def _bound_around(taken, scope, name):
    """Whether this scope or one enclosing it binds that name.

    A closure reads the parameter the function around it was given, so a qualifier spelled like
    a class is not that class there either.
    """
    reach = []
    for part in ([] if scope == MODULE_LEVEL else scope.split(".")):
        reach.append(part)
        if name in taken.get(".".join(reach), ()):
            return True
    return False


def _class_named(by_spelling, spelled_as, scope):
    """The class a spelling reaches from that scope, innermost first, then the module.

    Classes are keyed by their scope, so a base list and a qualified call have to be turned
    into the same key the tables use before anything is looked up.
    """
    reach = [] if scope == MODULE_LEVEL else scope.split(".")
    while reach:
        found = by_spelling.get(".".join(reach), {}).get(spelled_as)
        if found:
            return found
        reach.pop()
    return by_spelling.get(MODULE_LEVEL, {}).get(spelled_as, spelled_as)


def _base_key(by_spelling, known_keys, alias_of, spelled_as, scope, terminal=True):
    """The scoped class key a base expression names, qualified spellings included.

    Second.Base is that class and not whichever Base is found first: stripping the owner sends
    the line to a different class of the same terminal name, and the inheritance below it with
    that.

    Whether an unresolved qualified spelling may fall back to its terminal name is the caller's
    to say. A base written Second.Base is still a base when Second is not read here, but
    innocent.Holder() builds that module's Holder, and answering with the Holder written beside
    it would hand the call a class it never names.
    """
    if spelled_as in known_keys:
        return spelled_as
    parts = [part for part in (spelled_as or "").split(".") if part]
    if not parts:
        return spelled_as if terminal else None
    key = _class_named(by_spelling, _base_named(alias_of, parts[0], scope), scope)
    for part in parts[1:]:
        key = key + "." + part
    if key in known_keys:
        return key
    return parts[-1] if terminal else None


def _class_spellings(tree, places):
    """How a class spelling reaches its table key, per scope it is written in.

    A class written in another class body is reachable by its bare name from inside that body,
    not from the module around it. _places labels it with the scope the OWNER is written in, so
    registering it there puts a nested Holder into the module's namespace, and a module that
    imports an unrelated Holder then has the import answered by the class beside it.

    So a nested class is registered under its owner's key, which is the scope a base list
    inside that body is already resolved in. Its qualified spelling stays reachable on its own,
    because Outer.Holder is a key in the table rather than a lookup.
    """
    by_spelling, owner_of = {}, {}
    for node in ast.walk(tree):
        if not isinstance(node, ast.ClassDef):
            continue
        for statement in node.body:
            if isinstance(statement, ast.ClassDef):
                owner_of[id(statement)] = places.get(id(node), (MODULE_LEVEL, None))[1]
    for node in ast.walk(tree):
        if not isinstance(node, ast.ClassDef):
            continue
        where, key = places.get(id(node), (MODULE_LEVEL, None))
        owner = owner_of.get(id(node))
        by_spelling.setdefault(owner or where, {}).setdefault(node.name, key)
    return by_spelling


def _written_in(tree, places):
    """The scope each class's bases are resolved in, enclosing classes included.

    Python resolves a base name from the enclosing CLASS namespace while it executes an inner
    class, which the function scope alone does not say: class Outer: Alias = Base; class
    Child(Alias) really does inherit Base.
    """
    owner, written = {}, {}
    for node in ast.walk(tree):
        if not isinstance(node, ast.ClassDef):
            continue
        for inner in node.body:
            if isinstance(inner, ast.ClassDef):
                owner[id(inner)] = places.get(id(node), (MODULE_LEVEL, None))[1]
    for node in ast.walk(tree):
        if not isinstance(node, ast.ClassDef):
            continue
        where = places.get(id(node), (MODULE_LEVEL, None))[0]
        held = owner.get(id(node))
        written[id(node)] = (where if not held
                             else (held if where == MODULE_LEVEL else where + "." + held))
    return written


def _base_named(alias_by_scope, spelled_as, scope):
    """The class a base name reaches, resolved in the scope the class is written in.

    Innermost first, then the module: a class defined in a function sees that function's names
    before the module's, the way every other name written there is resolved.
    """
    reach = [] if scope == MODULE_LEVEL else scope.split(".")
    while reach:
        found = alias_by_scope.get(".".join(reach), {}).get(spelled_as)
        if found:
            return found
        reach.pop()
    return alias_by_scope.get(MODULE_LEVEL, {}).get(spelled_as, spelled_as)


def _owner_names_a_class(by_spelling, alias_by_scope, scope, spelled_as):
    """Whether a qualified call's owner names a class here, through an alias as well.

    Alias = Holder makes Alias.helper(holder, x) the same unbound call as Holder.helper(...),
    so an owner is a class when its spelling IS one or when it is bound to one. Innermost
    scope first, because a class written in one function does not make that spelling a class
    in another.

    Asked in one place because two consumers asked it separately and drifted apart exactly as
    this module keeps predicting: the receiver seating learned aliases a commit before the
    default applicability did, and the gap between them was a live false positive for that
    whole commit. The site inventory did not catch it either, and could not -- its predicate
    asks whether a site measures DISTANCE, which is a different question from which owner
    SHAPE it recognises, and that limit is written into its own declared boundary.
    """
    if not spelled_as:
        return False
    reach = [] if scope == MODULE_LEVEL else scope.split(".")
    while True:
        at = ".".join(reach) if reach else MODULE_LEVEL
        if spelled_as in by_spelling.get(at, {}) or spelled_as in alias_by_scope.get(at, {}):
            return True
        if not reach:
            return False
        reach.pop()


def _instance_classes(tree):
    """Each name holding an instance of a class written here, paired with that class.

    holder = Holder() makes holder.path the attribute Holder binds, so the question about it is
    asked of the class rather than answered from the spelling of the attribute. To a fixpoint,
    because second = first holds the same instance.
    """
    places = _places(tree)
    by_spelling = _class_spellings(tree, places)
    classes = frozenset(places.get(id(node), (MODULE_LEVEL, None))[1]
                        for node in ast.walk(tree) if isinstance(node, ast.ClassDef))
    alias_of = _class_aliases(tree)

    def held_in(made, scope, name):
        """The class this name holds an instance of, looked up innermost first."""
        reach = [] if scope == MODULE_LEVEL else scope.split(".")
        while reach:
            found = made.get((".".join(reach), name))
            if found:
                return found
            reach.pop()
        return made.get((MODULE_LEVEL, name))

    # Keyed by scope as well as spelling. Two functions may each bind holder, to different
    # classes, and a table that keeps the first answers the second with the wrong one.
    made, growing = {}, True
    # Monotone for the same reason _class_aliases is: what grows is the SET of classes each
    # name has ever been built from, and a set only gains members. Writing the answer and
    # re-running while it differs is not a fixpoint -- holder = First() followed by
    # holder = Second() flips it forever. Nobody reported this one; it is the same shape as the
    # alias table beside it, and the shape is what was wrong.
    ever = {}
    while growing:
        growing = False
        for node in ast.walk(tree):
            scope = places.get(id(node), (MODULE_LEVEL, None))[0]
            for target, value in _bindings(node):
                carried = ""
                if isinstance(value, ast.Call):
                    # A bare name, resolved where it is written: Alias = Holder makes Alias()
                    # a Holder. A qualified one is that owner's class, not this file's,
                    # whatever its last part is spelled.
                    builds = (_class_named(by_spelling,
                                           _base_named(alias_of, value.func.id, scope), scope)
                              if isinstance(value.func, ast.Name)
                              # Outer.Holder() is a class written here; innocent.Holder() is
                              # not, and only the key decides which.
                              else _base_key(by_spelling, classes, alias_of,
                                             _dotted(value.func) or "", scope,
                                             terminal=False))
                else:
                    carried = _dotted(value) or ""
                    builds = held_in(made, scope, carried)
                bound = _dotted(target)
                if not bound:
                    continue
                seen = ever.setdefault((scope, bound), set())
                carries = ever.get((scope, carried), set()) if carried else set()
                if len(carries) > 1:
                    # Copied from a name that is itself undecidable, so this one is too --
                    # asked before the value resolves, or whichever pass ran first would stand.
                    if not seen >= carries:
                        seen |= carries
                        growing = True
                    made.pop((scope, bound), None)
                    continue
                if builds not in classes or builds in seen:
                    continue
                seen.add(builds)
                growing = True
                if len(seen) == 1:
                    made[(scope, bound)] = builds
                else:
                    # Which class the name holds depends on where it is read, and this table is
                    # keyed by scope rather than by position in it.
                    made.pop((scope, bound), None)
    # Answered per NODE, because which instance a spelling names depends on where it is read.
    by_node, built = {}, set()
    for node in ast.walk(tree):
        if not isinstance(node, ast.Name):
            continue
        scope = places.get(id(node), (MODULE_LEVEL, None))[0]
        reached = held_in(made, scope, node.id)
        if reached:
            by_node[id(node)] = reached
        if (scope, node.id) in made:
            # Built in the scope that reads it, so it IS the instance rather than a shadow.
            built.add(id(node))
    return classes, by_node, built


def _shadowing_names(tree):
    """Name nodes whose own scope binds the name, so a module global spelled that way is not
    what they read.

    A parameter or a local called CHANGED is that function's, and reading it as the constant
    reports a place that settles for nothing of the kind. Module level is not shadowing: there
    the binding IS the constant. A scope saying the name is global is not shadowing either.
    """
    places, taken, said_global, said_nonlocal = _places(tree), {}, {}, {}
    # A class body is not the function around it. _places labels its statements with the
    # enclosing scope, but a name bound there creates no local in that function, so reading it
    # as a shadow drops the module constant a nested function really does see.
    def owned_by_the_body(body):
        """Every node a class body owns, down through its ifs and loops but not into a def.

        A statement written under an if in a class body is still the class's, and a def opens a
        scope of its own whose locals really are that scope's.
        """
        for statement in body:
            if isinstance(statement, (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef)):
                continue
            # Walked rather than ast.walk'd, because that descends into a def written under an
            # if and marks its locals as the class's. A def opens a scope whose names are its
            # own, wherever in the body it is written.
            def owned(node):
                yield id(node)
                for child in ast.iter_child_nodes(node):
                    if isinstance(child, (ast.FunctionDef, ast.AsyncFunctionDef,
                                          ast.ClassDef, ast.Lambda)):
                        continue
                    yield from owned(child)

            yield from owned(statement)

    in_a_class_body = {owned for node in ast.walk(tree) if isinstance(node, ast.ClassDef)
                       for owned in owned_by_the_body(node.body)}
    for node in ast.walk(tree):
        where, _klass = places.get(id(node), (MODULE_LEVEL, None))
        if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef, ast.Lambda)):
            args = node.args
            for parameter in args.posonlyargs + args.args + args.kwonlyargs:
                taken.setdefault(where, set()).add(parameter.arg)
            for extra in (args.vararg, args.kwarg):
                if extra:
                    taken.setdefault(where, set()).add(extra.arg)
        if isinstance(node, ast.Global):
            said_global.setdefault(where, set()).update(node.names)
        if isinstance(node, ast.Nonlocal):
            # A scope declaring the name nonlocal does not bind it locally either: the
            # assignment under that declaration is the enclosing function's. Exempting only
            # global read such a write as a nearer local and hid what it really wrote.
            said_nonlocal.setdefault(where, set()).update(node.names)
        if not isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef)):
            if id(node) in in_a_class_body:
                continue
            for named in _binds_locally(node):
                # A scope that declares the name global does not bind it locally: the
                # assignment under that declaration is the module's.
                if (named not in said_global.get(where, ())
                        and named not in said_nonlocal.get(where, ())):
                    taken.setdefault(where, set()).add(named)
    # A def or a class written in a function takes that name IN the function, which is why the
    # loop above leaves definitions to the table that knows which scope writes them.
    for where, names in _defined_in_scope(tree, places).items():
        for named in names:
            if named not in said_global.get(where, ()):
                taken.setdefault(where, set()).add(named)
    shadowed = set()
    for node in ast.walk(tree):
        if not isinstance(node, ast.Name):
            continue
        where, _klass = places.get(id(node), (MODULE_LEVEL, None))
        if where == MODULE_LEVEL or node.id in said_global.get(where, ()):
            continue
        # Every scope from the module's children down to this one: a closure reads the name the
        # function around it bound, so a local of an enclosing scope shadows just as its own
        # does. The module itself is not shadowing -- there the binding IS the constant.
        # Innermost first: the nearest scope that either binds the name or declares it the
        # module's is the one that decides, and an inner binding beats an enclosing global.
        reach, hidden = where.split("."), False
        while reach:
            here = ".".join(reach)
            if node.id in taken.get(here, ()):
                hidden = True
                break
            if node.id in said_global.get(here, ()):
                break
            reach.pop()
        if hidden:
            shadowed.add(id(node))
    # A class body binds IN ORDER and only the class body sees it. The loop above skips class
    # statements because a name bound there makes no local in the function around it -- true
    # for a method reading the name, which resolves outside the class entirely, and wrong for
    # a load written in the body itself, which reads what the body has already bound. Leaving
    # both out reported the module constant at a place that never names it.
    written_at, body_binds = {}, {}
    for node in ast.walk(tree):
        if not isinstance(node, ast.ClassDef):
            continue
        where, key = places.get(id(node), (MODULE_LEVEL, None))
        written_at[key] = where
        for statement in node.body:
            if isinstance(statement, (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef)):
                continue
            # Directly in the body and unconditional: a binding under an if is a guess about
            # the run, which is the rule the certainty sweep already states for this table.
            for named in _binds_locally(statement):
                body_binds.setdefault(key, {}).setdefault(named, statement.lineno)
    for node in ast.walk(tree):
        if not isinstance(node, ast.Name) or not isinstance(node.ctx, ast.Load):
            continue
        where, klass = places.get(id(node), (MODULE_LEVEL, None))
        if klass is None or written_at.get(klass) != where:
            # Written inside a method rather than in the body, so the class namespace is not
            # in its lookup chain at all.
            continue
        bound_at = body_binds.get(klass, {}).get(node.id)
        if bound_at is not None and node.lineno > bound_at:
            shadowed.add(id(node))
    return frozenset(shadowed), taken


def _linearised(parents):
    """Each class paired with the classes it searches, in the order Python searches them.

    C3, the order type(...).__mro__ reports. Walking each base's ancestors in turn instead
    reaches a grandparent before a base written beside it, and answers with a definition the
    instance never sees: with D(B, C) where B(A) overrides nothing and C binds the name, Python
    reads C and a depth-first walk reads A.

    A hierarchy C3 cannot linearise is one Python refuses to create, so the declaration order is
    taken there rather than calling the file unreadable. Bases this file does not define end the
    line, because what they hold is not a fact about this text.
    """
    def of(klass, seen=frozenset()):
        if klass is None:
            return []
        if klass in seen or klass not in parents:
            return [klass]
        bases = [base for base in parents.get(klass, ()) if base]
        chains = [chain for chain in
                  [list(of(base, seen | {klass})) for base in bases] + [list(bases)] if chain]
        line = [klass]
        while chains:
            head = None
            for chain in chains:
                if not any(chain[0] in rest[1:] for rest in chains):
                    head = chain[0]
                    break
            if head is None:
                head = chains[0][0]
            line.append(head)
            chains = [[item for item in chain if item != head] for chain in chains]
            chains = [chain for chain in chains if chain]
        return line

    return {klass: of(klass) for klass in parents}


def _held_by_class(tree, spelled, over=None):
    """Attribute names a class binds the thing to, so the rest of that class can read one.

    A setUp binding self.unread to a refusal and a test asserting self.unread are one place
    spread over two methods, and the method that settles is the one with no spelling in it. The
    binding is derived rather than declared, so that shape arrives accounted.
    """
    # Copied a set at a time. dict() alone shares the nested sets with the round before, so a
    # name found in a later round is added to BOTH and the caller's wider != held reads as
    # converged while the pass is still growing. A fixpoint that stops early is an omission,
    # and it is the kind that looks like a pass.
    places = _places(tree)
    held = {owner: set(names) for owner, names in (over or {}).items()}
    # Which statements a class body actually owns: a bare name bound in one is an attribute of
    # that class, and the same name bound inside a method is that method's local.
    in_class_body = _owned_by_a_class(tree)
    by_spelling = _class_spellings(tree, places)
    known_keys = {places.get(id(node), (MODULE_LEVEL, None))[1] for node in ast.walk(tree)
                  if isinstance(node, ast.ClassDef)}

    def owner_key(dotted, scope):
        """The scoped class key a dotted owner names.

        Outer.Inner is the inner class, and reducing it to Inner stores the attribute under a
        key no table looks up. A spelling that names no class here answers with its last part,
        which is what an imported owner was always answered with.
        """
        parts = [part for part in (dotted or "").split(".") if part]
        if not parts:
            return None
        key = _class_named(by_spelling, parts[0], scope)
        for part in parts[1:]:
            key = key + "." + part
        return key if key in known_keys else parts[-1]

    # A local name that holds the thing first, so setUp doing value = reading.UNREADABLE and then
    # self.unread = value is one binding in two steps rather than two unrelated lines.
    bound, spreading = {}, True
    while spreading:
        spreading = False
        for node in ast.walk(tree):
            function, klass = places.get(id(node), (MODULE_LEVEL, None))
            # Keyed by the class body as well as the scope: two class bodies written at module
            # level are both <module> here, and merging them carries one class's temporary into
            # the other's. A body still sees what the scope AROUND it bound, which is the union
            # taken below, so this separates siblings without closing the class off.
            local = (bound.setdefault((function, klass), set())
                     | bound.get((function, None), set()))
            for named, value in _bindings(node):
                if not _reachable(value, spelled, klass, local):
                    continue
                if isinstance(named, ast.Name) and named.id not in local:
                    bound.setdefault((function, klass), set()).add(named.id)
                    spreading = True

    for node in ast.walk(tree):
        function, klass = places.get(id(node), (MODULE_LEVEL, None))
        for target, value in _bindings(node):
            through_class = (isinstance(target, ast.Attribute)
                             and _dotted(target.value) not in (None, "self", "cls"))
            if (klass is None and not through_class) or not _reachable(
                    value, spelled, klass,
                    bound.get((function, klass), set()) | bound.get((function, None), set())):
                continue
            if isinstance(target, ast.Attribute):
                through = _dotted(target.value)
                # Example.unread = ... names the class as statically as self.unread does.
                owner = (klass if through in ("self", "cls")
                         else owner_key(through, function))
                if owner is not None:
                    held.setdefault(owner, set()).add(target.attr)
            elif (isinstance(target, ast.Name) and klass is not None
                    and any(id(statement) in in_class_body
                            for statement in ast.walk(node) if statement is node)):
                # A bare binding in a class BODY, wherever the class is written. One inside a
                # method is that method's local and no attribute of anything.
                held.setdefault(klass, set()).add(target.id)
    # Which names a class body binds for itself, whatever it binds them to, so a subclass
    # standing in front of an inherited name can be told from one that inherits it. The value
    # decides whether a name is HELD; the binding alone decides whether it SHADOWS.
    #
    # Only a binding the body certainly makes. One under an if, a for target over a sequence
    # that may be empty, or a self.attr written in a method that may never run are all
    # questions about the run, and taking one for a shadow would drop an inherited refusal
    # nobody is then asked about. Not shadowing on a guess leaves the name reported instead,
    # which is the direction this reader errs in.
    rebound = {}
    for node in ast.walk(tree):
        if not isinstance(node, ast.ClassDef):
            continue
        for statement in node.body:
            if isinstance(statement, (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef)):
                rebound.setdefault(places.get(id(node), (MODULE_LEVEL, None))[1],
                                   set()).add(statement.name)
            elif not isinstance(statement, (ast.For, ast.AsyncFor)):
                rebound.setdefault(places.get(id(node), (MODULE_LEVEL, None))[1],
                                   set()).update(_binds_locally(statement))
    # A class attribute assigned after the class statement takes part in lookup exactly as one
    # written in the body does. Only at the top level of the module, where it certainly runs:
    # one under an if or inside a function is the same guess about the run as before.
    for statement in getattr(tree, "body", ()):
        if not isinstance(statement, (ast.Assign, ast.AnnAssign)):
            continue
        for target in ([statement.target] if isinstance(statement, ast.AnnAssign)
                       else statement.targets):
            if not isinstance(target, ast.Attribute):
                continue
            owner = owner_key(_dotted(target.value), MODULE_LEVEL)
            if owner is not None:
                rebound.setdefault(owner, set()).add(target.attr)
    # An attribute declared on a base is held by everything under it, the way a method is.
    alias_of = _class_aliases(tree)
    written = _written_in(tree, places)
    by_spelling = _class_spellings(tree, places)
    # Every class, not one per spelling: the by-spelling table keeps the first of a name and
    # the whole point here is that the second exists too.
    every_key = {places.get(id(node), (MODULE_LEVEL, None))[1] for node in ast.walk(tree)
                 if isinstance(node, ast.ClassDef)}
    parents = {places.get(id(node), (MODULE_LEVEL, None))[1]:
               [_base_key(by_spelling, every_key, alias_of, _dotted(base) or "",
                          written.get(id(node), MODULE_LEVEL))
                for base in node.bases]
               for node in ast.walk(tree) if isinstance(node, ast.ClassDef)}
    mro = _linearised(parents)
    growing = True
    while growing:
        growing = False
        for klass in parents:
            # Lookup stops at the first class that binds the name, so a base written to the
            # left settles it for the ones to its right whether or not what it binds is a
            # refusal. Taking each base independently reads past the class Python stops at.
            settled = set(held.get(klass, ())) | set(rebound.get(klass, ()))
            for base in mro.get(klass, (klass,))[1:]:
                gained = held.get(base, set()) - settled
                if gained:
                    held.setdefault(klass, set()).update(gained)
                    growing = True
                settled |= set(held.get(base, ())) | set(rebound.get(base, ()))
    return held


def _hands_on(tree, spelled):
    """Functions that hand the thing to their callers, and the callers that take it.

    One hop is not enough: a helper calling a helper hands it on again, and stopping at the
    first hop would leave the second silent at one remove. So the callers are taken to a
    fixpoint, and each is reported at the line of the call that reached it.

    Two kinds of name resolution, because Python has two. A bare call is resolved outwards from
    the scope that made it, since a nested helper shadows a module-level one of the same name.
    A call on self is resolved against the plain names only: a nested helper inside a method
    never answers self.helper(), and treating it as though it did would send the call to
    something the interpreter would not reach. A name bound to a function is followed too,
    because alias = helper is an ordinary refactor and not a place to lose one.
    """
    places = _places(tree)
    alias_of = _class_aliases(tree)
    written = _written_in(tree, places)
    by_spelling = _class_spellings(tree, places)
    # Once, not per node: which defaults any call actually leaves to the default. The walk
    # below runs inside two nested fixpoints, so asking per node would re-derive the whole
    # module on every pass.
    left_to_the_default = _default_applies(tree, places)
    declared_owner = _declared_owner(tree, places)
    # Which parameter each place takes, so an argument can be read as the binding of the
    # parameter it fills: the positional ones in order, and the keyword-only ones as a set,
    # because def identity(*, answer) has a slot no position can reach.
    takes = {places.get(id(node), (MODULE_LEVEL, None))[0]:
             ([arg.arg for arg in node.args.posonlyargs + node.args.args],
              {arg.arg for arg in node.args.kwonlyargs})
             for node in ast.walk(tree)
             if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef, ast.Lambda))}
    every_key = {places.get(id(node), (MODULE_LEVEL, None))[1] for node in ast.walk(tree)
                 if isinstance(node, ast.ClassDef)}
    parents = {places.get(id(node), (MODULE_LEVEL, None))[1]:
               [_base_key(by_spelling, every_key, alias_of, _dotted(base) or "",
                          written.get(id(node), MODULE_LEVEL))
                for base in node.bases]
               for node in ast.walk(tree) if isinstance(node, ast.ClassDef)}
    mro = _linearised(parents)
    # An instance answers to its class here too: holder = Holder() makes holder.carrier() the
    # same call as Holder.carrier(), which the qualifier alone cannot say.
    _classes_here, holds_an, built_here = _instance_classes(tree)
    # A class body is a scope of its own, and it is not the module: a class written inside a
    # function has one too, and two class bodies do not share their names.
    class_scope = {}
    for node in ast.walk(tree):
        if isinstance(node, ast.ClassDef):
            where, key = places.get(id(node), (MODULE_LEVEL, None))
            class_scope.setdefault(key, []).append(
                ("<class " + node.name + " in " + where + ">", where))
    # Directly in a class body is what makes a def a method: a helper nested inside a method is
    # not one, and a class written inside a function still has methods.
    def in_body(body):
        """Every def a class body makes, including ones written under an if or a try."""
        for statement in body:
            if isinstance(statement, (ast.FunctionDef, ast.AsyncFunctionDef)):
                yield statement
            elif isinstance(statement, ast.match_case):
                yield from in_body(statement.body)
            elif not isinstance(statement, ast.ClassDef):
                for field, value in ast.iter_fields(statement):
                    if isinstance(value, list):
                        yield from in_body([item for item in value
                                            if isinstance(item, (ast.stmt, ast.ExceptHandler,
                                                                 ast.match_case))])

    is_method = {id(inner) for node in ast.walk(tree) if isinstance(node, ast.ClassDef)
                 for inner in in_body(node.body)}
    # Module-level bindings in the order the module makes them. A decorator name means what the
    # last binding ABOVE it gave, so prop = property followed later by prop = staticmethod
    # leaves the getters above the rebinding getters and the definitions below it static
    # methods. One module-wide set cannot say that, and reading a static method as a getter
    # makes an ordinary attribute read look like a call nobody performs.
    #
    # Module level and class bodies. A decorator written in a class body resolves names the body
    # has already bound, so sm = staticmethod beside the methods it decorates is a real alias
    # wherever that class is written. A name bound inside a FUNCTION is that function's, and
    # treating one as the decorator everywhere would suppress a receiver a method really has.
    # A class body's bindings stay in that class. Merging them into one table lets one class's
    # sm = staticmethod decide what @sm means in the next class, which is a name that body never
    # bound. An import binds the decorator as surely as an assignment does, so both are read.
    # Only a statement the module certainly runs. One under an if at module level is the same
    # guess about the run that a conditional class binding is, and reading it as the definite
    # meaning of a decorator name suppresses a receiver the method really has.
    module_statements = {id(statement) for statement in getattr(tree, "body", ())}
    owns_a_body = {id(statement): places.get(id(node), (MODULE_LEVEL, None))[1]
                   for node in ast.walk(tree)
                   if isinstance(node, ast.ClassDef) for statement in node.body}
    # Which function body owns a statement, so a class written inside that function closes over
    # what it bound. An unrelated function's binding is still that function's.
    owns_a_run = {id(statement): places.get(id(node), (MODULE_LEVEL, None))[0]
                  for node in ast.walk(tree)
                  if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef))
                  for statement in node.body}
    module_bindings, class_bindings, run_bindings = [], {}, {}
    # Keyed by the scope that wrote the import, not by the file. Two functions may import
    # different modules under one alias, and a file-wide last-write-wins map answers for both
    # with whichever landed later -- so a decorator the first one really applied is read
    # against the second one's module. Looked up innermost-first by _names_module, the same
    # lexical lookup the qualifier map already uses.
    import_names_at, imported_at = {}, {}
    for node in ast.walk(tree):
        if isinstance(node, (ast.Import, ast.ImportFrom)):
            bound = [(node.lineno, alias.asname or alias.name.split(".")[0],
                      alias.name.rpartition(".")[2]) for alias in node.names]
            if isinstance(node, ast.ImportFrom):
                # from X import property as prop binds the descriptor only if X really exports
                # it. The owner is asked, in this module's namespace first and then the ones
                # already imported; one nothing here can read reaches nothing, which keeps a
                # receiver the method may really have.
                owner = (vars(sys.modules[__name__]).get(node.module or "")
                         or sys.modules.get(node.module or ""))
                bound = []
                for alias in node.names:
                    exported = (getattr(owner, alias.name, None)
                                if isinstance(owner, types.ModuleType) else None)
                    builtin = getattr(sys.modules.get("builtins"), alias.name, None)
                    if builtin is None:
                        builtin = getattr(sys.modules.get("functools"), alias.name, None)
                    bound.append((node.lineno, alias.asname or alias.name,
                                  alias.name if exported is not None and exported is builtin
                                  else None))
            at = places.get(id(node), (MODULE_LEVEL, None))[0]
            for _line, name, _value in bound:
                import_names_at.setdefault(at, {}).setdefault(name, set()).add(name)
            if isinstance(node, ast.Import):
                for alias in node.names:
                    imported_at.setdefault(at, {}).setdefault(
                        alias.asname or alias.name.split(".")[0], set()).add(alias.name)
        elif isinstance(node, ast.Assign):
            spelling = (_dotted(node.value) or "").rpartition(".")[2]
            bound = [(node.lineno, inner.id, spelling) for target in node.targets
                     for inner in ast.walk(target)
                     if isinstance(inner, ast.Name) and isinstance(inner.ctx, ast.Store)]
        elif isinstance(node, ast.AnnAssign) and node.value is not None:
            # prop: object = property is the same binding with a type written on it. Reading
            # only ast.Assign here makes the annotated form reach nothing, and the class using
            # @prop is then indexed as holding an ordinary method.
            spelling = (_dotted(node.value) or "").rpartition(".")[2]
            bound = ([(node.lineno, node.target.id, spelling)]
                     if isinstance(node.target, ast.Name) else [])
        elif isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef)):
            # A def or a class of that name shadows the builtin it is spelled like, and what it
            # reaches is not the decorator, so it is recorded as reaching nothing.
            bound = [(node.lineno, node.name, None)]
        else:
            continue
        owner = owns_a_body.get(id(node))
        where = places.get(id(node), (MODULE_LEVEL, None))[0]
        if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef)):
            # A def is bound in the scope AROUND it, while places names the scope it opens.
            where = where.rpartition(".")[0] or MODULE_LEVEL
        if owner is not None:
            class_bindings.setdefault(owner, []).extend(bound)
        elif where == MODULE_LEVEL and id(node) in module_statements:
            module_bindings += bound
        elif id(node) in owns_a_run:
            run_bindings.setdefault(owns_a_run[id(node)], []).extend(bound)

    def visible_to(within, scope):
        """The bindings a decorator written there resolves against, innermost first.

        Its own class body, then every function enclosing it, then the module. A binding made
        in a function this one is NOT inside stays that function's.
        """
        seen, reach = list(class_bindings.get(within, [])), []
        for part in ([] if scope == MODULE_LEVEL else scope.split(".")):
            reach.append(part)
            seen += run_bindings.get(".".join(reach), [])
        return seen + module_bindings

    def means(name, seed, at, within=None, scope=MODULE_LEVEL, followed=0):
        """Whether this decorator name reaches one of these builtins at that line.

        A static method has no receiver, so reading its first parameter as the instance credits
        it with every carrier the class holds. A property is called by being read, so missing
        one loses the reader entirely. An alias of an alias is followed, to a bounded depth,
        because a cycle written at module level would not have run either.

        A binding above the use wins over the builtin's own name. A module that defines its own
        staticmethod has shadowed the builtin, and reading @staticmethod as the builtin there
        suppresses a receiver the method really has.
        """
        visible = visible_to(within, scope)
        if followed > len(visible):
            return name in seed
        above = sorted((line, value) for line, bound, value in visible
                       if bound == name and line < at)
        if not above:
            return name in seed
        line, value = above[-1]
        if value is None:
            return False
        return means(value, seed, line, within, scope, followed + 1)

    def the_objects(seed):
        """What these names denote, asked of the interpreter rather than listed.

        Only modules this process has ALREADY imported are consulted. A module named in the
        source under analysis is not something to import in order to answer a question about
        the text, and one that cannot be read answers no rather than yes -- which keeps the
        receiver, and errs towards reporting.
        """
        found = set()
        for holder in (sys.modules.get("builtins"), sys.modules.get("functools")):
            for name in seed:
                thing = getattr(holder, name, None) if holder is not None else None
                if thing is not None:
                    found.add(thing)
        return found

    def resolves_to(mark, seed, within, scope=MODULE_LEVEL):
        """Whether this decorator or constructor name reaches one of these builtins.

        The same question for @property and for property(carrier): a qualified name belongs to
        whatever owns it, and reducing it to its last part reads somebody's identity function
        as the descriptor it is spelled like.
        """
        spelled_as = _dotted(mark) or ""
        owner, _, last = spelled_as.rpartition(".")
        held_by = owner.rpartition(".")[2]
        if owner and _names_module(import_names_at, scope, held_by, None):
            # Every module that spelling is bound to IN THE SCOPE THAT CAN SEE IT. Within one
            # scope a name may be bound more than once, and which binding a decorator ran under
            # is a question about position; a name that ever names the owner is treated as
            # naming it, the direction this reader errs in everywhere else.
            return any(getattr(sys.modules.get(named), last, None) in the_objects(seed)
                       for named in _names_module(imported_at, scope, held_by, None))
        if owner and _names_module(import_names_at, scope, owner.split(".")[0], None):
            # import innocent.decorators binds innocent, so innocent.decorators names a path
            # through that module rather than a name of its own. Reduced to its last part the
            # owner matches nothing here, and the terminal name is then read as the builtin it
            # is spelled like -- which removes a receiver the method really has. A module this
            # process cannot read answers no rather than answering the builtin.
            module = sys.modules.get(owner)
            return module is not None and getattr(module, last, None) in the_objects(seed)
        if owner and (held_by in parents
                      or any(bound == held_by for _line, bound, _value in module_bindings)):
            return False
        return means(last, seed, getattr(mark, "lineno", 0), within, scope)

    def decorated_by(node, seed, within=None, scope=MODULE_LEVEL):
        """Whether any decorator on this definition reaches one of these builtins."""
        return any(resolves_to(mark, seed, within, scope)
                   for mark in getattr(node, "decorator_list", []))

    def names_class(name, at, followed=0):
        """The class this name reaches at that line: its own name, or one bound to it.

        Alias = Holder makes Alias.carrier() the same call as Holder.carrier(), and the binding
        is a name like any other. Ordered the same way as the decorator names, by the last
        binding above the use, and bounded so a cycle answers rather than recurses.
        """
        if name in parents:
            return name
        if followed > len(module_bindings):
            return None
        above = sorted((line, value) for line, bound, value in module_bindings
                       if bound == name and line < at)
        if not above:
            return None
        line, value = above[-1]
        return names_class(value, line, followed + 1)
    defined, methods, plain, receivers, properties = set(), {}, set(), {}, {}
    bound_to_its_class = set()
    for node in ast.walk(tree):
        if not isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef, ast.Lambda)):
            continue
        where, klass = places.get(id(node), (MODULE_LEVEL, None))
        defined.add(where)
        # A method is reached through an instance or its class, never as a bare name inside
        # another method, so it is kept out of the lexical lookup.
        if id(node) not in is_method:
            plain.add(where)
        # And the instance is whatever the first parameter is called: self is a convention.
        args = node.args
        first = (args.posonlyargs + args.args)[:1]
        standalone = decorated_by(node, ("staticmethod",), klass, where)
        if id(node) in is_method and first and not standalone:
            receivers[where] = first[0].arg
        if (id(node) in is_method and first and not standalone
                and decorated_by(node, ("classmethod",), klass, where)):
            # A classmethod is bound through the CLASS as well as through an instance, so
            # Holder.identity(x) hands cls over unwritten where an ordinary method spelled
            # that way writes its receiver.
            bound_to_its_class.add(where)
        # Which class a method belongs to, because self.name reaches a method of THIS class and
        # not a module-level function or another class's method that happens to share the name.
        if id(node) in is_method:
            methods.setdefault((klass, where.rpartition(".")[2]), set()).add(where)
            # A property is called by being read, so an attribute access naming one is a call.
            if decorated_by(node, ("property", "cached_property"), klass, where):
                properties.setdefault((klass, where.rpartition(".")[2]), set()).add(where)
    # Lambda methods FIRST, because the property scan below looks a getter up by name and
    # getter = lambda self: ... is registered here. Reading them in the other order makes the
    # answer depend on which loop this module happens to write first, which is a fact about
    # this file rather than about the class it is reading.
    for node in ast.walk(tree):
        # carrier = lambda self: ... in a class body binds a method named carrier, and the
        # lambda's own place is what a call through it reaches.
        if not isinstance(node, (ast.Assign, ast.AnnAssign)) or node.value is None:
            continue
        # Through the pass-through vocabulary rather than off a bare Lambda: a getter bound by
        # a conditional is still a getter, and reading only the direct spelling registered
        # neither arm, which left the method that reads the property unaccounted for.
        made = list(_lambda_named(node.value))
        if not made:
            continue
        where, klass = places.get(id(node), (MODULE_LEVEL, None))
        if klass is None or not any(around == where
                                    for _scope, around in class_scope.get(klass, ())):
            # Bound in a class BODY, not merely somewhere under a class: a lambda made inside a
            # method is that method's local and never answers self.name().
            continue
        for written in made:
            seat = places.get(id(written), (MODULE_LEVEL, None))[0]
            first = (written.args.posonlyargs + written.args.args)[:1]
            for target in (node.targets if isinstance(node, ast.Assign) else [node.target]):
                if isinstance(target, ast.Name):
                    # EVERY arm, not the first: when a conditional binds two getters and only
                    # the later one carries, keeping the first answered that the property does
                    # not reach and dropped the method that reads it.
                    methods.setdefault((klass, target.id), set()).add(seat)
                    if first:
                        receivers[seat] = first[0].arg

    # A descriptor made by calling property() rather than by decorating. alias = property(carrier)
    # in a class body is the same getter under a second name, and reading self.alias runs it.
    for node in ast.walk(tree):
        if not isinstance(node, ast.ClassDef):
            continue
        for statement in node.body:
            binding = _assigned(statement)
            if binding is None or not isinstance(statement.value, ast.Call):
                continue
            targets, _value = binding
            if not resolves_to(statement.value.func, ("property", "cached_property"),
                               places.get(id(node), (MODULE_LEVEL, None))[1],
                               places.get(id(node), (MODULE_LEVEL, None))[0]):
                continue
            # property(getter) and property(fget=getter) install the same descriptor, so the
            # keyword spelling is read beside the positional one rather than left out.
            for given in (list(statement.value.args)[:1]
                          + [word.value for word in statement.value.keywords
                             if word.arg == "fget"]):
                reached = methods.get((places.get(id(node), (MODULE_LEVEL, None))[1],
                                       (_dotted(given) or "").rpartition(".")[2]))
                if not reached:
                    continue
                for target in targets:
                    for inner in ast.walk(target):
                        if isinstance(inner, ast.Name) and isinstance(inner.ctx, ast.Store):
                            properties.setdefault(
                                (places.get(id(node), (MODULE_LEVEL, None))[1], inner.id),
                                set()).update(reached)
    # The places that ARE getters, so a class alias naming one can be recognised as naming a
    # property rather than an ordinary method.
    property_places = {seat for seats in properties.values() for seat in seats}
    # What a class body binds, and where. A def binds its own name there as surely as an
    # assignment does, and a binding written after a default has not happened yet when that
    # default runs, so the line is part of the answer.
    class_bound = {}
    for node in ast.walk(tree):
        if not isinstance(node, ast.ClassDef):
            continue
        def bound_in(body, certain=True):
            """Every name this class body binds, and whether the binding is certain to happen.

            A def written under an if is still a method, because the method index answers for
            it either way. An ordinary binding written under one is NOT treated as a shadow:
            whether it happened is a question about the run, and blocking the outward lookup on
            a guess would lose the place that really does reach the module name. So only an
            unconditional binding shadows, which errs towards reporting.

            A for target is conditional for the same reason and is not written here at all: the
            target is created only once the iterator yields, so a class body looping over an
            empty sequence never binds it and the name still reaches the enclosing scope.
            """
            for statement in body:
                if isinstance(statement, (ast.FunctionDef, ast.AsyncFunctionDef)):
                    yield statement.name, statement.lineno
                    continue
                if isinstance(statement, ast.ClassDef):
                    # A class is not a method, and no index answers for it the way the method
                    # index answers for a def, so the exemption above is not its. It shadows
                    # only when the binding is certain, which is the rule the ordinary bindings
                    # below already follow: whether a branch ran is a question about the run.
                    if certain:
                        yield statement.name, statement.lineno
                    continue
                if certain and not isinstance(statement, (ast.For, ast.AsyncFor)):
                    for named in _binds_locally(statement):
                        yield named, statement.lineno
                for field, value in ast.iter_fields(statement):
                    if isinstance(value, list):
                        yield from bound_in([item for item in value
                                             if isinstance(item, (ast.stmt, ast.ExceptHandler,
                                                                   ast.match_case))], False)

        for named, line in bound_in(node.body):
            class_bound.setdefault(places.get(id(node), (MODULE_LEVEL, None))[1],
                                   {}).setdefault(named, line)

    # Where a scope says a name belongs to the module, so a receiver search stops there instead
    # of finding the method around it: global self makes self the module's object, whatever the
    # enclosing method was handed.
    says_global = {}
    for node in ast.walk(tree):
        if isinstance(node, ast.Global):
            where, _klass = places.get(id(node), (MODULE_LEVEL, None))
            says_global.setdefault(where, set()).update(node.names)

    # A method may give its receiver another name -- that = self -- and a call through that name
    # reaches the same class. Collected per scope, and followed to a fixpoint below, because
    # that = self and other = that are two steps onto one object.
    receiver_alias = {}
    for node in ast.walk(tree):
        where, _klass = places.get(id(node), (MODULE_LEVEL, None))
        for target, value in _bindings(node):
            # Through the same binding abstraction as everything else, so that: Holder = self
            # is the ordinary typed spelling of the same line rather than a form nobody reads.
            for source in _passed_through(value):
                for inner in ast.walk(target):
                    if isinstance(inner, ast.Name) and isinstance(inner.ctx, ast.Store):
                        receiver_alias.setdefault(where, {}).setdefault(
                            source.id, set()).add(inner.id)

    def its_own(node, function):
        """Whether the instance table's answer for this name is this scope's to read.

        A name built in the scope that reads it IS the instance. Otherwise a scope between the
        construction and the read may have taken the name -- a parameter spelled like a
        module-level instance holds whatever its caller passed, and inheriting the class would
        oblige a declaration for a call that never happens.
        """
        return (id(node) in built_here
                or not _bound_around(taken_names, function, node.id))

    def instance(function):
        """How this scope spells its instance: whatever the first parameter is called.

        Looked up outwards, because a closure with no parameters of its own still sees the one
        the method around it was given.

        And every name the scope gives that object: that = self reaches the same class, so a
        call through the second name is the same call. A name that ever holds the receiver is
        treated as holding it, which is the direction this reader errs in.
        """
        chain = [] if function == MODULE_LEVEL else function.split(".")
        while chain:
            scope = ".".join(chain)
            spelled = receivers.get(scope)
            if spelled:
                found, growing = {spelled}, True
                while growing:
                    growing = False
                    for name in sorted(found):
                        # Every scope from the one holding the receiver down to this one: a
                        # nested function reads the alias the method around it made.
                        gained = set()
                        chain, reach = function.split(".") if function != MODULE_LEVEL else [], []
                        for part in chain:
                            reach.append(part)
                            here = ".".join(reach)
                            for alias in receiver_alias.get(here, {}).get(name, set()):
                                # An alias bound by ANY scope between the one that made it and
                                # the one reading it is that scope's own name: a parameter
                                # called that is the inner function's, not the receiver the
                                # method around it named, and a grandchild then closes over its
                                # caller's object. The scope that made the alias still keeps
                                # it, or nothing would ever inherit.
                                between = ([] if function == MODULE_LEVEL
                                           else function.split("."))
                                if any(alias in taken_names.get(".".join(between[:depth]),
                                                                set())
                                       for depth in range(len(reach) + 1, len(between) + 1)):
                                    continue
                                gained.add(alias)
                        gained -= found
                        if gained:
                            found |= gained
                            growing = True
                return found
            # A scope of its own that binds the same name is where the search stops: a nested
            # parameter called self is that function's, not the method's around it.
            # Any scope between here and the method binding the same name stops the search,
            # however many there are.
            below = chain[:-1]
            while below:
                outer = receivers.get(".".join(below))
                if outer:
                    # Every scope between the one that MADE the alias and the one reading it,
                    # not just the innermost. A parameter anywhere along that line takes the
                    # name, and what the grandchild closes over is then its caller's object
                    # rather than the receiver the method named.
                    between = [] if scope == MODULE_LEVEL else scope.split(".")
                    stopped = any(
                        outer in taken_names.get(".".join(between[:depth]), set())
                        or outer in says_global.get(".".join(between[:depth]), set())
                        for depth in range(len(below) + 1, len(between) + 1))
                    if stopped:
                        return set()
                    break
                below.pop()
            chain.pop()
        return set()

    def inherited(klass, named, seen=(), aliases=None, after=False):
        """The method this class reaches by that name, its own or one it inherits.

        Walked in the order Python searches, so a class settles the name for everything after it
        in the line and finding nothing in one is not the same answer as finding a binding this
        reader cannot follow. An alias made in a class body binds the same method object, so
        self.alias() reaches what self.carrier() does.

        With after, the class itself is skipped and the REST OF ITS OWN line is searched, which
        is what super() does. Searching each base's line in turn instead exhausts the first
        base's ancestors before reaching the base written beside it.
        """
        if klass is None:
            return set()
        for reached_in in list(mro.get(klass, (klass,)))[1 if after else 0:]:
            if (reached_in, named) in methods:
                return set(methods[(reached_in, named)])
            for scope, _around in class_scope.get(reached_in, ()):
                held_alias = (aliases or {}).get(scope, {}).get(named)
                if held_alias:
                    # Every method the alias may name: a conditional one names both arms, and
                    # answering with the first would lose whichever of them is the carrier.
                    return set(held_alias)
            if named in class_bound.get(reached_in, {}):
                # Bound to something this reader cannot follow -- carrier = str. Lookup ends
                # here, and the classes after it in the line are never reached. What the
                # binding holds instead is a question for whatever follows names, and where
                # that ends is already written down as out of reach.
                return set()
        return set()

    def scopes(caller):
        """The scope this call sits in and every one enclosing it, innermost first."""
        chain = [] if caller == MODULE_LEVEL else caller.split(".")
        while chain:
            yield ".".join(chain)
            chain.pop()
        yield MODULE_LEVEL

    taken_names = {}
    for node in ast.walk(tree):
        if not isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef, ast.Lambda)):
            continue
        where = places.get(id(node), (MODULE_LEVEL, None))[0]
        args = node.args
        taken_names[where] = {arg.arg for arg in
                              (args.posonlyargs + args.args + args.kwonlyargs
                               + ([args.vararg] if args.vararg else [])
                               + ([args.kwarg] if args.kwarg else []))}
    # A name a scope assigns is local to that scope for the whole of it, whatever it is assigned.
    # An alias is looked up first, so this only stops the search where the binding is something
    # this reader cannot follow -- carrier = str -- which is a name Python resolves locally and
    # never to the enclosing definition.
    owned_by_class = _owned_by_a_class(tree)
    for node in ast.walk(tree):
        where = places.get(id(node), (MODULE_LEVEL, None))[0]
        if where == MODULE_LEVEL or id(node) in owned_by_class:
            # A name bound in a class body belongs to that body. Letting it into the function
            # the class is written in would stop the outward lookup for everything after the
            # class, which is not what Python does with a class attribute.
            continue
        # A definition's name belongs to the scope that writes it rather than to the one it
        # opens, so it is left to _defined_in_scope below.
        bound_here = (set() if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef,
                                                 ast.ClassDef))
                      else _binds_locally(node))
        if bound_here:
            taken_names.setdefault(where, set()).update(bound_here)
    for where, names in _defined_in_scope(tree, places).items():
        taken_names.setdefault(where, set()).update(names)
    for node in ast.walk(tree):
        # global and nonlocal say the name belongs to another scope, so assigning it here does
        # not make it this one's own and the search must not stop at it.
        if not isinstance(node, (ast.Global, ast.Nonlocal)):
            continue
        where = places.get(id(node), (MODULE_LEVEL, None))[0]
        taken_names.setdefault(where, set()).difference_update(node.names)

    declared_global, declared_nonlocal = {}, {}
    for node in ast.walk(tree):
        if not isinstance(node, (ast.Global, ast.Nonlocal)):
            continue
        where = places.get(id(node), (MODULE_LEVEL, None))[0]
        which = declared_global if isinstance(node, ast.Global) else declared_nonlocal
        which.setdefault(where, set()).update(node.names)

    def outwards(caller, named, aliases=None, klass=None, at=None):
        """Every place this name may reach, innermost scope first.

        A set, not one place. A name can be assigned twice and this reader does not decide which
        assignment a given call saw, so it answers with all of them; picking one would alternate
        forever between them, and picking the first would be a guess wearing a precise face.

        A parameter is different from a rebinding. Which object a parameter holds is a fact about
        the run, but that the name belongs to the parameter and not to a module-level function of
        the same name is a fact about the text, so the search stops there.
        """
        # A class body executes with the names it has already bound, so a default or a
        # decorator written there reaches a method or an alias made above it, and an ordinary
        # binding made above it shadows the module the same way a local one does. Only in the
        # class body: a bare name inside a method resolves outside the class, not in it.
        written = class_bound.get(klass, {}).get(named) if klass is not None else None
        inside = [scope for scope, around in class_scope.get(klass, ()) if around == caller]
        if written is not None and inside and (at is None or written < at):
            reached = methods.get((klass, named))
            if reached:
                return set(reached)
            for scope in inside:
                if aliases and named in aliases.get(scope, {}):
                    return set(aliases[scope][named])
            return set()
        if named in declared_global.get(caller, ()):
            # global says the module, not the next scope out that happens to share the name,
            # and what the module holds under it may be an alias rather than a def.
            if aliases and named in aliases.get(MODULE_LEVEL, {}):
                return set(aliases[MODULE_LEVEL][named])
            return {named} if named in plain else set()
        outer = list(scopes(caller))
        if named in declared_nonlocal.get(caller, ()):
            # nonlocal says a scope AROUND this one, which is not the module either.
            outer = outer[1:]
        for scope in outer:
            if aliases and named in aliases.get(scope, {}):
                return set(aliases[scope][named])
            candidate = named if scope == MODULE_LEVEL else scope + "." + named
            if candidate in plain:
                return {candidate}
            if named in taken_names.get(scope, ()):
                return set()
        return set()

    # A name bound to a function, kept for the whole scope that binds it and visible to the
    # scopes inside it, the way a closure sees one. Chains are followed to a fixpoint, because
    # second = first = helper is two hops and stopping at one loses the second.
    #
    # It is never cleared. A name can be rebound, and where that happens this reader cannot say
    # which binding a given call reached without following flow it does not follow. So it answers
    # that the name still holds what it once held: the cost of being wrong is a sentence somebody
    # has to write, which is the direction this whole module errs in on purpose, rather than a
    # place that settles for a refusal and is never asked about it.
    aliases, growing = {}, True
    while growing:
        growing = False
        for node in ast.walk(tree):
            # A named expression binds a name as surely as an assignment does.
            if isinstance(node, ast.NamedExpr):
                holders, answer = [node.target], node.value
            elif isinstance(node, (ast.Assign, ast.AnnAssign)) and node.value is not None:
                holders = node.targets if isinstance(node, ast.Assign) else [node.target]
                answer = node.value
            else:
                continue
            function, _klass = places.get(id(node), (MODULE_LEVEL, None))
            def names(expression):
                """Every place an expression may name, both arms of a conditional included."""
                if isinstance(expression, ast.IfExp):
                    return names(expression.body) | names(expression.orelse)
                if isinstance(expression, ast.NamedExpr):
                    return names(expression.value)
                if isinstance(expression, ast.Name):
                    # Resolved where the expression is evaluated, which is not always where the
                    # name it feeds is bound: a comprehension walrus binds outside and computes
                    # inside.
                    inside, in_class = places.get(id(expression), (function, None))
                    return outwards(inside, expression.id, aliases, in_class,
                                    expression.lineno)
                if isinstance(expression, ast.Await):
                    return names(expression.value)
                if isinstance(expression, ast.BoolOp):
                    return set().union(*(names(value) for value in expression.values))
                if isinstance(expression, ast.Lambda):
                    return {places.get(id(expression), (MODULE_LEVEL, None))[0]}
                if isinstance(expression, ast.Call):
                    # alias = staticmethod(carrier) binds the same function under a second
                    # name, and calling through it runs the one that was wrapped.
                    if expression.args and means(
                            (_dotted(expression.func) or "").rpartition(".")[2],
                            ("staticmethod", "classmethod", "property", "cached_property"),
                            expression.lineno, _klass):
                        return names(expression.args[0])
                    return set()
                if isinstance(expression, ast.Attribute):
                    through = _dotted(expression.value)
                    _where, klass = places.get(id(node), (MODULE_LEVEL, None))
                    if (through is None and isinstance(expression.value, ast.Call)
                            and _dotted(expression.value.func) == "super"):
                        # alias = super().carrier binds what super().carrier() would call, so
                        # it searches the rest of THIS class's line, not each base's in turn.
                        return inherited(klass, expression.attr, (), aliases, after=True)
                    qualifier = (through or "").rpartition(".")[2] or None
                    aliased = (_base_named(alias_of, qualifier, function)
                               if qualifier else None)
                    if aliased is not None and aliased != qualifier and aliased in parents:
                        # A name the scope binds to a class IS that class here too, which the
                        # module-only resolver below cannot see.
                        qualifier = aliased
                    elif qualifier is not None:
                        qualifier = names_class(qualifier, expression.lineno) or qualifier
                    return inherited(klass if through in instance(function) else qualifier,
                                     expression.attr, (), aliases)
                return set()

            # One traversal for every shape a right-hand side can take: a name, a bound
            # method, a lambda, a conditional, a named expression, or one wrapped in another.
            # Unpacking is paired off first, so alias, other = carrier, str gives each name
            # what it is actually given rather than the union of both.
            for named, value in _bindings(node):
                targets = names(value)
                if not targets or not isinstance(named, ast.Name):
                    continue
                # global and nonlocal say which scope the name belongs to, so the alias is
                # recorded there rather than here, where the lookup would never consult it.
                holder_scope = function
                if _klass is not None and any(
                        around == function for _scope, around in class_scope.get(_klass, ())):
                    # A name bound in a class body belongs to that body, not to the scope the
                    # class was written in, so two class bodies cannot read each other's.
                    holder_scope = next(scope for scope, around
                                        in class_scope[_klass] if around == function)
                if named.id in declared_global.get(function, ()):
                    holder_scope = MODULE_LEVEL
                elif named.id in declared_nonlocal.get(function, ()):
                    # The nearest scope outside this one that binds the name, however many
                    # scopes down the declaration sits.
                    holder_scope = MODULE_LEVEL
                    outer_chain = function.split(".")[:-1]
                    while outer_chain:
                        candidate = ".".join(outer_chain)
                        if named.id in taken_names.get(candidate, ()):
                            holder_scope = candidate
                            break
                        outer_chain.pop()
                known = aliases.setdefault(holder_scope, {}).setdefault(named.id, set())
                if not targets <= known:
                    known |= targets
                    growing = True

    def a_property(klass, named, seen=(), after=False):
        """The property this class reaches by that name, its own or one it inherits.

        With after, the rest of this class's own line, which is where super() looks.

        Every getter the name may run rather than one of them: a conditional binding installs
        whichever arm the run picks, and answering with the first loses the carrier whenever it
        is the other.
        """
        if klass is None:
            return set()
        for reached_in in list(mro.get(klass, (klass,)))[1 if after else 0:]:
            if (reached_in, named) in properties:
                return set(properties[(reached_in, named)])
            for scope, _around in class_scope.get(reached_in, ()):
                # alias = carrier in the class body is a second name for the same getter, and
                # reading self.alias runs it exactly as reading self.carrier does.
                for reached in sorted((aliases or {}).get(scope, {}).get(named) or ()):
                    if reached in property_places:
                        return {reached}
            if named in class_bound.get(reached_in, {}):
                # A method or a binding of the same name stands in front of the descriptor, so
                # the getter never runs and the classes after it are not reached.
                return set()
        return set()

    def read_as_a_call(node, function):
        """A property read: self.carrier with no parentheses still runs carrier.

        Through an INSTANCE only. Reading Example.carrier off the class hands back the
        descriptor rather than running the getter, so counting that would invent an occurrence
        nobody performs.

        super().carrier is the one receiver that is a call rather than a name, and it runs the
        base getter exactly as super().carrier() runs the base method, so it starts the same
        lookup one class up.
        """
        if not isinstance(node, ast.Attribute) or not isinstance(node.ctx, ast.Load):
            return set()
        _where, klass = places.get(id(node), (MODULE_LEVEL, None))
        if (_dotted(node.value) is None and isinstance(node.value, ast.Call)
                and _dotted(node.value.func) == "super"):
            reached = a_property(klass, node.attr, after=True)
            return set(reached)
        if _dotted(node.value) in instance(function):
            reached = a_property(klass, node.attr)
            return set(reached)
        if (isinstance(node.value, ast.Name) and id(node.value) in holds_an
                and its_own(node.value, function)):
            # holder = Holder() and then holder.carrier: the name's class is already known, and
            # reading a property on it runs the getter exactly as self.carrier does. The
            # receiver table holds only the spellings a method was given, so without this the
            # constructed instance is the one case where a property read is not a call.
            reached = a_property(holds_an[id(node.value)], node.attr)
            return set(reached)
        return set()

    def called(node, function):
        """Every place this call may reach."""
        if isinstance(node.func, ast.Name):
            _where, in_class = places.get(id(node), (MODULE_LEVEL, None))
            return outwards(function, node.func.id, aliases, in_class, node.lineno)
        if isinstance(node.func, ast.Attribute):
            through = _dotted(node.func.value)
            if (through is None and isinstance(node.func.value, ast.Call)
                    and _dotted(node.func.value.func) == "super"):
                # super().name in a subclass starts the same lookup one class up, and which
                # class that is, is written right here.
                _where, klass = places.get(id(node), (MODULE_LEVEL, None))
                return inherited(klass, node.func.attr, (), aliases, after=True)
            # cls.name in a classmethod names a method of this class exactly as self.name does,
            # and Example.name names one of Example's just as statically.
            _where, klass = places.get(id(node), (MODULE_LEVEL, None))
            named_class = (through or "").rpartition(".")[2] or None
            aliased = (_base_named(alias_of, named_class, function)
                       if named_class else None)
            aliased = (_class_named(by_spelling, aliased, function)
                       if aliased is not None else None)
            if (isinstance(node.func.value, ast.Name) and id(node.func.value) in holds_an
                    and its_own(node.func.value, function)):
                # A name holding an instance names its class, whatever it is spelled.
                named_class = holds_an[id(node.func.value)]
            elif aliased is not None and aliased != named_class and aliased in parents:
                # A name the scope binds to a class IS that class, which the shadow rule below
                # must not undo: an alias is a local binding too.
                named_class = aliased
            elif (isinstance(node.func.value, ast.Name)
                    and _bound_around(taken_names, function, node.func.value.id)):
                # A parameter or a local of that name is not the class of that name: which
                # object it holds is a fact about the caller, and a closure reads the one the
                # scope around it bound.
                named_class = None
            elif named_class is not None:
                named_class = names_class(named_class, node.lineno) or named_class
            return inherited(klass if through in instance(function) else named_class,
                             node.func.attr, (), aliases)
        return set()

    # A function hands the thing back when its returned expression IS the thing, and it keeps
    # handing it back when a call that hands it back flows into that expression. Calling one
    # somewhere in the body is not enough: a helper answering {"value": _hands_refusal()} builds
    # a payload, and treating that as handing it on would drag in every caller of every payload
    # builder, which is the fan-out that gets a sweep deleted.
    #
    # The local names are re-read on every round rather than once, because a name bound to the
    # RESULT of a carrier only becomes reachable once that carrier is known. An assignment
    # replaces what a name holds instead of adding to it, so a name overwritten with an ordinary
    # value stops counting rather than staying marked for the rest of the function.
    carriers, growing = set(), True
    while growing:
        growing = False
        bound = {}

        def visible(function):
            """Every name holding the thing in this scope or in one enclosing it.

            A nested function reads what the function around it bound, so a helper returning a
            local of its enclosing scope hands the same thing on. The scopes are the prefixes of
            the chain that names this one.
            """
            chain = [] if function == MODULE_LEVEL else function.split(".")
            scope, scopes = [], [MODULE_LEVEL]
            for part in chain:
                scope.append(part)
                scopes.append(".".join(scope))
            seen = set()
            for index, here in enumerate(scopes):
                for name in bound.get(here, ()):
                    # Any scope between the one that binds the name and the one reading it
                    # takes it: an inner parameter beats an enclosing local, and a parameter on
                    # a function in the middle takes it from the grandchild just as surely.
                    if any(name in taken_names.get(scopes[deeper], set())
                           for deeper in range(index + 1, len(scopes))):
                        continue
                    seen.add(name)
            return seen

        def reaching(function, klass):
            def hands(expression, inner):
                if isinstance(expression, ast.Call):
                    reached = called(expression, function) & carriers
                    if reached:
                        return "through " + sorted(reached)[0]
                read = read_as_a_call(expression, function) & carriers
                if read:
                    return "through " + sorted(read)[0]
                return spelled(expression, inner)
            return hands

        # A name that ever holds the thing in a function holds it for that function. Which
        # binding a particular return saw is a question about flow, and following flow is what
        # this reader does not do: a conditional return before an overwrite, and two arms of an
        # if, are both cases where a precise-looking answer would be a guess. Erring towards
        # holding it costs a sentence; erring the other way costs a place nobody is asked about.
        spreading = True
        while spreading:
            spreading = False
            for node in ast.walk(tree):
                function, klass = places.get(id(node), (MODULE_LEVEL, None))
                # A default binds only when a call leaves it to the default, the same condition
                # the handle side already applies. Reading them unconditionally here made every
                # refusal-valued default a refusal even where every call supplies one.
                for named, value in list(_bindings(node)) + [
                        (named, default) for named, default in _defaults(node)
                        if (function, named.id) in left_to_the_default]:
                    if not _reachable(value, reaching(function, klass), klass,
                                      visible(function)):
                        continue
                    if not isinstance(named, ast.Name):
                        continue
                    # The scope the LANGUAGE gives the name, not the one that wrote it: a
                    # binding under global or nonlocal belongs elsewhere, and filing it here
                    # left its readers outside this scope unable to see it at all.
                    owns = declared_owner.get((function, named.id), function)
                    held = bound.setdefault(owns, set())
                    if named.id not in held:
                        held.add(named.id)
                        spreading = True
                # An argument binds the parameter it fills: identity(answer) makes answer
                # whatever the caller handed over, and seeding parameters from defaults alone
                # left every carrier passed in this way unaccounted for. A receiver moves the
                # slots rather than making them unreadable -- the resolver already knows which
                # places take one -- so a method's positions are counted from after it, and an
                # unbound call through the class writes its receiver and moves nothing.
                if isinstance(node, ast.Call):
                    for reached in called(node, function):
                        slots, only_by_keyword = takes.get(reached, ([], set()))
                        handed = [(word.arg, word.value) for word in node.keywords
                                  if word.arg in only_by_keyword or word.arg in slots]
                        standing = list(slots)
                        if isinstance(node.func, ast.Attribute) and reached in receivers:
                            owner = (_dotted(node.func.value) or "").rpartition(".")[2]
                            # The same question the default applicability asks, asked once:
                            # Alias = Holder makes Alias.identity(obj, x) the same unbound
                            # call, and a copy of this walk in each consumer is how the two
                            # last drifted apart.
                            through_a_class = _owner_names_a_class(
                                by_spelling, alias_of, function, owner)
                            # A classmethod is bound through its class too, so naming the class
                            # does not mean the receiver was written.
                            if not through_a_class or reached in bound_to_its_class:
                                standing = standing[1:]
                        # A written unpacking supplies the values it writes: identity(*(x,))
                        # hands x over, and zipping the Starred node itself binds a slot to a
                        # syntax node rather than to what was passed. One this text cannot read
                        # stops the positional count rather than guessing how far it reaches.
                        #
                        # A SET is not ordered, so there is no seating to derive from one at
                        # all: reading it off the order the source happens to list would be an
                        # answer taken from something that is not there. It is outside the
                        # grammar this seating supports, so nothing is seated past it and the
                        # occurrence is reported by _seating_not_established instead, where an
                        # undeclared one fails acceptance rather than passing quietly.
                        filling = 0
                        for given in node.args:
                            if filling >= len(standing):
                                break
                            if not isinstance(given, ast.Starred):
                                handed.append((standing[filling], given))
                                filling += 1
                            elif isinstance(given.value, (ast.Tuple, ast.List)):
                                for element in given.value.elts:
                                    if filling < len(standing):
                                        handed.append((standing[filling], element))
                                        filling += 1
                            else:
                                break
                        # And a written mapping names its own slots, the same way a keyword
                        # written out does.
                        for word in node.keywords:
                            if word.arg is not None or not isinstance(word.value, ast.Dict):
                                continue
                            for key, given in zip(word.value.keys, word.value.values):
                                if (isinstance(key, ast.Constant)
                                        and isinstance(key.value, str)
                                        and (key.value in only_by_keyword
                                             or key.value in slots)):
                                    handed.append((key.value, given))
                        for name, given in handed:
                            if not _reachable(given, reaching(function, klass), klass,
                                              visible(function)):
                                continue
                            held = bound.setdefault(reached, set())
                            if name not in held:
                                held.add(name)
                                spreading = True

        for node in ast.walk(tree):
            if isinstance(node, ast.Return) and node.value is not None:
                answer = node.value
            elif isinstance(node, ast.Lambda):
                # A lambda has no return statement; its body IS what it hands back.
                answer = node.body
            else:
                continue
            function, klass = places.get(id(node), (MODULE_LEVEL, None))
            if function == MODULE_LEVEL or function in carriers:
                continue
            if _reachable(answer, reaching(function, klass), klass, visible(function)):
                carriers.add(function)
                growing = True

    statements = _module_statements(tree)
    taken = []
    for node in ast.walk(tree):
        if not isinstance(node, (ast.Call, ast.Attribute)):
            continue
        function, _klass = places.get(id(node), (MODULE_LEVEL, None))
        # A carrier is reported here too. One that only forwards -- return await helper() --
        # names nothing itself, so skipping it would drop the very hop that made it a carrier.
        # And a call made at module level is a place the same way a spelling written there is.
        at_module = function == MODULE_LEVEL
        where = statements.get(id(node), "a statement") if at_module else function
        reached = (called(node, function) if isinstance(node, ast.Call)
                   else read_as_a_call(node, function))
        for place in sorted(reached & carriers):
            taken.append((at_module, where, node.lineno, "through " + place))
    return carriers, taken


def _occurrences(tree, spelled):
    """Every place a spelling appears, one entry per occurrence.

    Per occurrence rather than per distinct spelling, because deleting one of two identical
    occurrences is a change this has to see rather than a narrowing it can sleep through.
    """
    places, statements = _places(tree), _module_statements(tree)
    found = []
    for node in ast.walk(tree):
        function, klass = places.get(id(node), (MODULE_LEVEL, None))
        spelling = spelled(node, klass)
        if spelling is None:
            continue
        at_module = function == MODULE_LEVEL
        where = statements.get(id(node), "a statement") if at_module else function
        found.append((at_module, where, node.lineno, spelling))
    carriers, taken = _hands_on(tree, spelled)
    # A function that hands the thing on is a place the thing reaches, whether or not anybody
    # calls it here. Reporting only its callers loses the one that does the handing, and a
    # helper nobody calls yet loses it entirely.
    at = {}
    for node in ast.walk(tree):
        if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef, ast.Lambda)):
            where, _klass = places.get(id(node), (MODULE_LEVEL, None))
            at.setdefault(where, node.lineno)
    handing = [(False, place, at[place], "hands it on")
               for place in carriers if place in at]
    return sorted(found + taken + handing)


def refusal_spellings():
    """Every way a refusal can be spelled here, asked of the objects rather than listed.

    The answers are REFUSAL_ANSWERS. Each spelling of one is then derived: an attribute an
    imported module binds to one, a global of this module bound to one, and a collection --
    here or on an imported module -- that contains one. Nothing in the result is written down,
    so a constant added to REFUSAL_ANSWERS widens what the check sees without a second edit,
    which is exactly what the hardcoded set of constant names it replaces could not do.
    """
    answers = frozenset(REFUSAL_ANSWERS)

    def is_answer(value):
        return isinstance(value, str) and value in answers

    def collects(value, depth=0):
        """Whether this value holds a refusal, at any depth of written container.

        Reading the direct members only was a reach that stopped one container short of what
        the objects hold: ((UNREADABLE,),) collects one just as plainly as (UNREADABLE,) does,
        and a name bound to it is spelled the same way at the use site. A row settling for a
        refusal through the nested form would not have been inventoried.

        Bounded rather than open: the containers are the written ones, and the descent stops at
        a depth no constant in this module approaches, so a value that somehow refers to itself
        answers narrowly instead of not answering at all.
        """
        if depth > 8:
            return False
        if isinstance(value, (tuple, list, set, frozenset)):
            return any(is_answer(item) or collects(item, depth + 1) for item in value)
        if isinstance(value, dict):
            # Keys as well as values: "answer in mapping" asks about the keys, and a mapping
            # from a refusal to a label is an ordinary way for one to be written down.
            return any(is_answer(item) or collects(item, depth + 1) for item in value) or any(
                is_answer(item) or collects(item, depth + 1) for item in value.values())
        return False

    attributes, mine, held, owners = set(), set(), set(), set()
    for name, value in list(vars(sys.modules[__name__]).items()):
        if isinstance(value, types.ModuleType):
            for attribute, inner in list(vars(value).items()):
                if attribute.startswith("_"):
                    continue
                if is_answer(inner):
                    attributes.add(attribute)
                    # And which module binds it, so a same-named attribute on an unrelated
                    # object is not read as this answer.
                    owners.add(name)
                elif collects(inner):
                    held.add(attribute)
                    owners.add(name)
        elif is_answer(value):
            mine.add(name)
        elif collects(value):
            held.add(name)
    return {"answer": answers, "module attribute": frozenset(attributes),
            "own global": frozenset(mine), "collection": frozenset(held),
            "owner": frozenset(owners)}


def source_spellings(tree):
    """Every way the text of a source file can be reached here, and what could not be read.

    Three derivations and no list. A binding of this module whose value is a path to a .py file
    that exists, or a collection of them, is a handle, and so is the __file__ every module
    carries. A name in call position is ASKED whether it hands source back: its own name says
    source, or the first parameter of the callable it resolves to is named "source" -- which is
    where ast.parse comes from, so the one form the old check spelled out by hand is now derived
    from ast.parse's own signature. A string ending in .py names a source file.

    A name bound to one of those callables answers for it as well, to a fixpoint. That binding is
    never taken back and is not kept per function: where a name is rebound, or where another
    function happens to use the same name for something else, this reader answers that it still
    reads source. Being wrong that way costs a sentence somebody has to write; being wrong the
    other way costs a place that concludes from source text and is never asked about it.

    A called name whose signature cannot be read is not assumed to be harmless. It comes back as
    undecided and has to be written down, because a name nobody could read reported as a name
    that does not read source is a plausible default standing in for an answer nobody got.
    """
    namespace = dict(vars(sys.modules[__name__]))
    # An import written inside a function never reaches the module namespace, so the name it
    # binds is read out of the import itself and resolved to what it actually imports.
    # An import inside a function binds only in that function, and this namespace is one for
    # the whole file, so two scopes using one alias for two things cannot both be represented.
    # Every candidate is collected and the one that reads source wins, because taking the other
    # would be the silent pass this module exists to refuse; taking this one only over-reports.
    imported = {}
    for node in ast.walk(tree):
        if isinstance(node, ast.ImportFrom) and node.module:
            owner = sys.modules.get(node.module)
            if owner is None:
                continue
            for alias in node.names:
                value = getattr(owner, alias.name, None)
                if value is not None:
                    imported.setdefault(alias.asname or alias.name, []).append(value)
        elif isinstance(node, ast.Import):
            for alias in node.names:
                owner = sys.modules.get(alias.name)
                if owner is not None:
                    imported.setdefault(alias.asname or alias.name.split(".")[0],
                                        []).append(owner)

    def reads_source(value):
        own = getattr(value, "__name__", "")
        if "source" in own.lower():
            return True
        try:
            return list(inspect.signature(value).parameters)[:1] == ["source"]
        except (ValueError, TypeError):
            return False

    for name, candidates in imported.items():
        reading_ones = [value for value in candidates if reads_source(value)]
        namespace[name] = (reading_ones or candidates)[0]

    def is_source_file(value):
        return isinstance(value, Path) and value.suffix == ".py" and value.exists()

    handles = {"__file__"}
    for name, value in list(namespace.items()):
        if is_source_file(value):
            handles.add(name)
        elif isinstance(value, (tuple, list, set, frozenset)) and value and all(
                is_source_file(item) for item in value):
            handles.add(name)

    def asked(spelling):
        """Does this name hand source back? A reason, an unreadable signature, or neither."""
        head, _, attribute = spelling.rpartition(".")
        if head:
            owner = namespace.get(head)
            if not isinstance(owner, types.ModuleType):
                return None, None, False
            value = getattr(owner, attribute, None)
        else:
            value = namespace.get(attribute, getattr(builtins, attribute, None))
            if isinstance(value, types.FunctionType) and value.__module__ == __name__:
                return None, None, False
        if not callable(value):
            return None, None, False
        if "source" in attribute.lower():
            return "its own name says it hands source", None, True
        own = getattr(value, "__name__", "")
        if "source" in own.lower():
            # from inspect import getsource as reader: the local spelling says nothing, and the
            # callable still says it. Asked of the object, like everything else here.
            return "the callable it resolves to is named " + own, None, True
        try:
            first = list(inspect.signature(value).parameters)[:1]
        except (ValueError, TypeError):
            return None, "its signature cannot be read here", True
        if first == ["source"]:
            return "its first parameter is named source", None, True
        return None, None, True

    hands_source, undecided, called = {}, {}, set()
    for node in ast.walk(tree):
        if not isinstance(node, ast.Call):
            continue
        spelling = _dotted(node.func)
        if spelling is None:
            continue
        why, unread, resolved = asked(spelling)
        if not resolved:
            continue
        called.add(spelling)
        if why:
            hands_source[spelling] = why
        elif unread:
            undecided[spelling] = unread

    # And a name bound to one of those answers for it, because reader = inspect.getsource is an
    # ordinary line and the call after it reaches exactly as far. Taken to a fixpoint so an alias
    # of an alias is followed too.
    growing = True
    while growing:
        growing = False
        def spelled_by(expression):
            """Every name a right-hand side may be, a conditional's arms included."""
            if isinstance(expression, ast.IfExp):
                return spelled_by(expression.body) + spelled_by(expression.orelse)
            if isinstance(expression, (ast.NamedExpr, ast.Await)):
                return spelled_by(expression.value)
            if isinstance(expression, ast.BoolOp):
                return [name for value in expression.values for name in spelled_by(value)]
            named = _dotted(expression)
            return [named] if named else []

        for node in ast.walk(tree):
            if isinstance(node, ast.NamedExpr):
                holders, answer = [node.target], node.value
            elif isinstance(node, (ast.Assign, ast.AnnAssign)) and node.value is not None:
                holders = node.targets if isinstance(node, ast.Assign) else [node.target]
                answer = node.value
            else:
                continue
            for holder, value in _bindings(node):
              for named in spelled_by(value):
                if named not in hands_source:
                    # The name may hand source back without ever being called here: reader =
                    # inspect.getsource puts it behind a local name and the call names only that.
                    why, unread, _resolved = asked(named)
                    if unread:
                        # A name nobody could read stays unreadable when it is put behind
                        # another name; treating the alias as harmless is the plausible default
                        # this module refuses everywhere else.
                        undecided.setdefault(named, unread)
                    if not why:
                        continue
                    hands_source[named] = why
                    growing = True
                bound = _dotted(holder)
                if bound and bound not in hands_source:
                    hands_source[bound] = "it is bound to " + named + ", which does"
                    growing = True
    return frozenset(handles), hands_source, undecided, frozenset(called)


def _refusal_spelled(spellings, held, classes=(), as_class=None, shadowed=(), built=(),
                     imported_answers=(), known_keys=(), qualifies=None, places=None,
                     answers_at=None, taken_at=None):
    """The matcher: which node is a refusal, spelled any of the derived ways."""
    answers = spellings["answer"]
    attributes = spellings["module attribute"] | spellings["collection"]
    names = spellings["own global"] | spellings["collection"] | frozenset(imported_answers)
    owners = spellings.get("owner", frozenset())
    qualifies = qualifies or {}
    places = places or {}
    answers_at = answers_at or {}
    taken_at = taken_at or {}

    def spelled(node, klass):
        if isinstance(node, ast.Constant) and isinstance(node.value, str):
            return repr(node.value) if node.value in answers else None
        if isinstance(node, ast.Attribute):
            through = _dotted(node.value)
            reader = (klass if through in ("self", "cls")
                      else (through or "").rpartition(".")[2] or None)
            # A qualified owner names the class it spells out: Outer.Inner is the inner class,
            # and the held table is keyed that way.
            if through in known_keys:
                reader = through
            # The ROOT of the chain is what a parameter takes: import crw_runtime.reading with
            # def helper(crw_runtime) binds the root, and crw_runtime.reading.UNREADABLE then
            # reads whatever the caller handed over. Asking only when the immediate owner is a
            # bare Name covered one-part owners and left every longer one assuming the import.
            stem = node.value
            while isinstance(stem, ast.Attribute):
                stem = stem.value
            if (through not in ("self", "cls") and isinstance(stem, ast.Name)
                    and id(stem) in shadowed and id(stem) not in built
                    and not (_names_module(qualifies,
                                           places.get(id(node), (MODULE_LEVEL, None))[0],
                                           through, None) & owners
                             and _nearest(qualifies,
                                          places.get(id(node), (MODULE_LEVEL, None))[0],
                                          through)
                             >= _nearest(taken_at,
                                         places.get(id(node), (MODULE_LEVEL, None))[0],
                                          stem.id))):
                # The scope binds that qualifier itself, so neither an imported module nor an
                # instance bound at module level under the same spelling is what this reads.
                # Unless what binds it is an import of the module that owns the answer: a
                # function importing crw_runtime.reading as r binds r locally, and r IS that
                # module rather than something wearing its name.
                return None
            # A name holding an instance answers to its class: what innocent.NOT_READ reads is
            # what Innocent binds, and what holder.unread reads is what Holder binds.
            if isinstance(node.value, ast.Name):
                reader = (as_class or {}).get(id(node.value), reader)
            if reader is not None and node.attr in held.get(reader, ()):
                return (through or "") + "." + node.attr
            if reader in classes:
                # A class written here is asked through the table above, which knows what it
                # binds. Falling through would match on the attribute name alone, and then any
                # class with an attribute spelled like an answer would hold one -- a spurious
                # declaration owed for a value that is not a refusal.
                return None
            # The owner has to be a module that really exports one. Matching the attribute name
            # alone makes any object with a same-named attribute hold an answer.
            # Which module a qualifier names is the analysed source's own import to answer:
            # import json as reading wears a spelling this module happens to own, and
            # import crw_runtime.reading as r wears one it does not.
            named_modules = _names_module(qualifies,
                                          places.get(id(node), (MODULE_LEVEL, None))[0],
                                          through, (through or "").rpartition(".")[2])
            if node.attr in attributes and (named_modules & owners):
                return (through or "") + "." + node.attr
            return None
        if isinstance(node, ast.Name) and node.id in names:
            # Unless the scope binds that name itself, in which case the global of that
            # spelling is not what this reads.
            # Unless what binds it is the import that made it an answer: a function writing
            # from completion import NOT_READ as answer binds answer locally, and that local
            # binding IS the refusal rather than something wearing its name.
            where = places.get(id(node), (MODULE_LEVEL, None))[0]
            if id(node) in shadowed and not (
                    _bound_around(answers_at, where, node.id)
                    and _nearest(answers_at, where, node.id)
                    >= _nearest(taken_at, where, node.id)):
                return None
            return node.id
        return None

    return spelled


def _opens_here(shadowed_at, line, scope):
    """Whether open is still the builtin at this point in the module.

    A module-level binding of open shadows the builtin from ITS OWN LINE onward. A statement
    written above it already ran with the builtin, so erasing it for the whole file loses a
    handle the module really did open -- which is a different question from WHICH SCOPE owns a
    name, and is why it is decided here rather than by the nearest-binding rule.

    Inside a function the answer depends on when the function is called, and that is not a fact
    about this text. There the binding shadows throughout: the direction that costs a place
    rather than inventing one.
    """
    if shadowed_at is None:
        return True
    return scope == MODULE_LEVEL and line < shadowed_at


def _opener_spellings(tree, places):
    """Every spelling this source can call the builtin opener by, derived from its own imports.

    open is a name like any other, and a name can be rebound. `from builtins import open as
    fopen`, `import io` followed by `io.open`, and a module-level `fopen = open` each leave a
    call that opens a file under a spelling that is not the word open, and a check that matches
    the word sees none of them. Which modules hand back the builtin itself is read off the
    objects rather than listed here: io.open IS builtins.open, one function under a second
    name, while os.open is a different function answering with a descriptor.

    The bare names and the dotted spellings come back apart because they are not the same
    claim. A bare name is the opener outright. A dotted one is either a module holding it,
    whose arguments are the whole of what it was handed, or a file object's own open method,
    whose receiver has to be a handle before the call says anything about this module's file.

    Blind spot, said plainly because it is one: a module this process never imported is not in
    the set, so an opener re-exported by an unimported module is not seen. Nothing readable
    from here says what an unimported name holds, and a guess would be worse than the gap.
    """
    named = builtins.open.__name__
    holders, rebound = set(), {}
    for module_name, module in list(sys.modules.items()):
        if module is None:
            continue
        try:
            held = getattr(module, named, None)
        except Exception:
            continue
        if held is builtins.open:
            holders.add(module_name)
    bare, dotted, holders_at = {named}, set(), {}
    for node in ast.walk(tree):
        if isinstance(node, ast.ImportFrom):
            if not node.level and node.module in holders:
                for alias in node.names:
                    if alias.name == named:
                        bare.add(alias.asname or alias.name)
                        # The import BINDS the name in the scope that wrote it, exactly as an
                        # assignment does, so that scope is not shadowing the opener it just
                        # took. Recording only assignments left a function importing the
                        # opener rejected as having hidden it.
                        rebound.setdefault(
                            places.get(id(node), (MODULE_LEVEL, None))[0], set()).add(
                                alias.asname or alias.name)
        elif isinstance(node, ast.Import):
            for alias in node.names:
                if alias.name in holders:
                    dotted.add((alias.asname or alias.name) + "." + named)
                    # Which scope wrote the import, so a nearer binding of the module's own
                    # name can take it back.
                    holders_at.setdefault(
                        places.get(id(node), (MODULE_LEVEL, None))[0], set()).add(
                            (alias.asname or alias.name).split(".")[0])
    # fopen = open is the same rebinding written as an assignment, and b = fopen is one more
    # step of it, so the statements are followed until they stop adding names. A function may
    # write one too, and the scope it wrote it in is kept: the name is then bound there rather
    # than being read as some other scope's, which is what a shadow means everywhere else here.
    growing = True
    while growing:
        growing = False
        for node in ast.walk(tree):
            where = places.get(id(node), (MODULE_LEVEL, None))[0]
            # Pair by pair, because fopen, ignored = open, None hands the opener to ONE of the
            # names and reading the statement's whole value followed neither.
            for target, value in _bindings(node):
                if not isinstance(target, ast.Name):
                    continue
                # Through the pass-through vocabulary, because fopen = open if flag else open
                # is the same rebinding written with a branch. _dotted answers None for every
                # one of those forms, so reading it alone lost the alias and the handle with it.
                spelling = _dotted(value)
                if not (any(source.id in bare for source in _passed_through(value))
                        or (spelling is not None and spelling in dotted)):
                    continue
                if target.id not in bare:
                    bare.add(target.id)
                    growing = True
                if target.id not in rebound.get(where, ()):
                    rebound.setdefault(where, set()).add(target.id)
                    growing = True
    return frozenset(bare), frozenset(dotted), rebound, holders_at


def _opener_call(func, bare, dotted):
    """Whether this callee spells a call that opens a file, under any of the three spellings."""
    if isinstance(func, ast.Name):
        return func.id in bare
    spelling = _dotted(func) or ""
    return spelling in dotted or spelling.rpartition(".")[2] == builtins.open.__name__


def _handle_names(tree, handles, shadowed=(), opens_a_file=True, openers=None, taken_at=None):
    """Every name that reaches a source file, the module's own and the local ones bound to them.

    path = HERE is an ordinary line, and a read taken on path reaches the same file.

    stream = open(HERE) is the same line in two steps: the name holds the file the handle named,
    and a read taken on it reaches that file. The call is recognised rather than followed, since
    what open answers with is the file it was given and that is the whole of what is needed here.
    """
    places, known, where_from, growing = _places(tree), set(handles), {}, True
    declared = frozenset(handles)
    bare, dotted, rebound, holders_at = openers or _opener_spellings(tree, places)
    taken_at = taken_at or {}
    # Once, not per node: which defaults any call actually leaves to the default.
    left_to_the_default = _default_applies(tree, places)
    declared_owner = _declared_owner(tree, places)

    def seen_in(scope, made):
        """Whether a use in this scope sees a handle derived in one of those."""
        return _visible_from(scope, made)

    while growing:
        growing = False
        def reaches(expression, scope=MODULE_LEVEL):
            """Whether this right-hand side names a handle, either arm of a conditional too."""
            if isinstance(expression, ast.IfExp):
                return reaches(expression.body, scope) or reaches(expression.orelse, scope)
            if isinstance(expression, (ast.NamedExpr, ast.Await)):
                return reaches(expression.value, scope)
            if isinstance(expression, ast.BoolOp):
                return any(reaches(value, scope) for value in expression.values)
            if (isinstance(expression, ast.Call)
                    and (_dotted(expression.func) or "").rpartition(".")[2] == "Path"
                    and not _opener_call(expression.func, bare, dotted)):
                # Path(HERE) and Path(__file__) name the same file the argument names: the
                # conversion keeps the path rather than assembling one. That is what separates
                # this from the run-time-assembled path declared out of reach below, where
                # nothing in the expression names a source file at all -- here the handle is
                # written in the call. Only the arguments are read, because the constructor
                # itself is not a handle.
                return any(reaches(given, scope) for given in list(expression.args)
                           + [given.value for given in expression.keywords])
            if (isinstance(expression, ast.Call)
                    and _opener_call(expression.func, bare, dotted)):
                opener, receiver = expression.func, []
                if isinstance(opener, ast.Name):
                    # A scope that binds open itself, or a module that defines one, has
                    # shadowed the builtin: what that call answers with is not this file.
                    # Unless the binding IS the opener: fopen = open written in a function
                    # binds the name there, and what it holds is still this file's opener.
                    if not _opens_here(opens_a_file, expression.lineno, scope) or (
                                            id(opener) in shadowed
                                            and _nearest(rebound, scope, opener.id)
                                            < _nearest(taken_at, scope, opener.id)):
                        return False
                elif (_dotted(opener) or "") in dotted:
                    # io.open is the builtin under its module's name rather than an object's
                    # own method, so the module is not one of the things handed to it.
                    # Unless a nearer binding took the module's name: def helper(io) makes io
                    # whatever the caller passed, and io.open is then that object's method.
                    stem = opener.value
                    while isinstance(stem, ast.Attribute):
                        stem = stem.value
                    if (isinstance(stem, ast.Name) and id(stem) in shadowed
                            and _nearest(holders_at, scope, stem.id)
                            < _nearest(taken_at, scope, stem.id)):
                        return False
                elif not (isinstance(opener, ast.Attribute)
                          and reaches(opener.value, scope)):
                    # innocent.open is that object's own method. Only a handle's own open --
                    # HERE.open() -- opens the file this module is asking about.
                    return False
                else:
                    receiver = [opener.value]
                return any(reaches(given, scope) for given in list(expression.args)
                           + [given.value for given in expression.keywords] + receiver)
            named = _dotted(expression)
            if named is None or named not in known:
                return False
            if isinstance(expression, ast.Name) and id(expression) in shadowed:
                return False
            # A DERIVED handle belongs to the scope that derived it; a declared one is the
            # module's everywhere. Otherwise an unrelated parameter of the same spelling
            # becomes a handle simply by being copied to a second name.
            return named in declared or seen_in(scope, where_from.get(named, ()))

        for node in ast.walk(tree):
            scope, _klass = places.get(id(node), (MODULE_LEVEL, None))
            # A default is a binding here too: def helper(path=HERE) makes path hold the
            # handle whenever the caller omits the argument, and the read taken on it reaches
            # the same file.
            for target, value in list(_bindings(node)) + [
                    (named, default) for named, default in _defaults(node)
                    if (scope, named.id) in left_to_the_default]:
                if not reaches(value, scope):
                    continue
                named = _dotted(target)
                if not named:
                    continue
                if named not in known:
                    known.add(named)
                    growing = True
                # Where the name was made a handle. A name the MODULE declares is a handle
                # everywhere; one derived from a binding is one in that scope and the scopes
                # inside it, and an unrelated function's parameter of the same spelling is not.
                # A binding under global or nonlocal was made in the scope the LANGUAGE gives
                # it, so it is filed there rather than where it was written.
                where_from.setdefault(named, set()).add(
                    declared_owner.get((scope, named), scope))
    return frozenset(known), where_from


def _source_spelled(handles, hands_source, held, as_class=None, shadowed=(), declared=(),
                    astray=(), built=(), opens_a_file=True, openers=None, places=None,
                    reader_at=None, taken_at=None, derived_at=None):
    """The matcher: which node reaches the text of a source file, spelled any of the derived ways."""
    bare, dotted, rebound, holders_at = openers or (
        frozenset({builtins.open.__name__}), frozenset(), {}, {})
    places = places or {}
    reader_at, taken_at = reader_at or {}, taken_at or {}
    derived_at = derived_at or {}

    def spelled(node, klass):
        if isinstance(node, ast.Name):
            # Unless the scope binds that name itself: a parameter called HERE is not the
            # module's handle, and which file it holds is a fact about the caller. Only the
            # handles the MODULE declares can be shadowed that way -- stream = open(HERE) is a
            # local binding too, and it is a handle BECAUSE of it.
            at = places.get(id(node), (MODULE_LEVEL, None))[0]
            # And unless something NEARER binds it: a derived handle is a fact about the scope
            # that derived it, and a parameter written closer to the read holds whatever its
            # caller passed.
            taken_back = (id(node) in shadowed
                          and _nearest(derived_at, at, node.id)
                          < _nearest(taken_at, at, node.id))
            if node.id in handles and id(node) not in astray and not taken_back and not (
                    node.id in declared and id(node) in shadowed):
                return node.id
            return None
        if isinstance(node, ast.Attribute):
            through = _dotted(node.value)
            reader = (klass if through in ("self", "cls")
                      else (through or "").rpartition(".")[2] or None)
            if (through not in ("self", "cls") and isinstance(node.value, ast.Name)
                    and id(node.value) in shadowed and id(node.value) not in built):
                # The scope binds that qualifier itself, so neither an imported module nor an
                # instance bound at module level under the same spelling is what this reads.
                return None
            # A name holding an instance answers to its class, the same way a refusal does.
            if isinstance(node.value, ast.Name):
                reader = (as_class or {}).get(id(node.value), reader)
            if reader is not None and node.attr in held.get(reader, ()):
                return (through or "") + "." + node.attr
            return "." + node.attr if node.attr == "__file__" else None
        if isinstance(node, ast.Constant) and isinstance(node.value, str):
            return repr(node.value) if node.value.endswith(".py") else None
        if isinstance(node, ast.Call):
            spelling = _dotted(node.func)
            root = node.func
            while isinstance(root, ast.Attribute):
                root = root.value
            at = places.get(id(node), (MODULE_LEVEL, None))[0]
            hidden = (isinstance(root, ast.Name) and id(root) in shadowed
                      and _nearest(reader_at, at, root.id)
                      < _nearest(taken_at, at, root.id))
            if spelling in hands_source and not hidden:
                # Unless the scope binds that name itself: a parameter called reader holds
                # whatever its caller passed, and a call through it says nothing about this
                # module's source. The spelling table is this module's; the binding is the
                # analysed scope's. Asked of the ROOT of the chain, because a parameter called
                # parser shadows parser.parse exactly as one called reader shadows reader --
                # and unless the binding IS the alias, because reader = ast.parse written in
                # the scope that calls it is the reader rather than a shadow of one.
                return spelling
            # A READ taken on a handle hands source text back: a helper answering
            # HERE.read_text() gives its caller the text as surely as one answering ast.parse.
            # Only a read. HERE.exists() and HERE.with_suffix() answer about the file rather
            # than with it, and counting those would put every filesystem question in the
            # inventory. Which spellings mean a read is recognised by the name beginning with
            # read, which is narrower than the class of ways to get a file's contents. The place
            # itself is accounted either way, because the handle is named there; what a narrower
            # rule costs is the CALLER of a helper that reads some other way.
            head, _, attribute = (spelling or "").rpartition(".")
            through, _, last = head.rpartition(".")
            reader = klass if through in ("self", "cls") else None
            on_a_handle = head in handles or (reader is not None
                                              and last in held.get(reader, ()))
            taken = node.func.value if isinstance(node.func, ast.Attribute) else None
            if isinstance(taken, ast.Name) and (
                    id(taken) in astray
                    or (taken.id in declared and id(taken) in shadowed)
                    or (id(taken) in shadowed
                        and _nearest(derived_at,
                                     places.get(id(taken), (MODULE_LEVEL, None))[0],
                                     taken.id)
                        < _nearest(taken_at,
                                   places.get(id(taken), (MODULE_LEVEL, None))[0],
                                   taken.id))):
                # A scope that binds the handle's name reads its own, so a read taken on it
                # says nothing about the file the module's handle names.
                on_a_handle = False
            if not on_a_handle and isinstance(node.func, ast.Attribute):
                # Path(HERE).read_text(): the conversion keeps the path its argument names, so
                # the read IS taken on this module's handle even though the receiver has no
                # dotted spelling of its own. This is not the run-time-assembled path declared
                # out of reach below -- there nothing in the expression names a source file,
                # and here the handle is written inside the call.
                made_by = node.func.value
                if (isinstance(made_by, ast.Call)
                        and (_dotted(made_by.func) or "").rpartition(".")[2] == "Path"):
                    kept = [_dotted(given) for given in list(made_by.args)
                            + [given.value for given in made_by.keywords]]
                    named = next((one for one in kept if one and one in handles), None)
                    if named is not None:
                        on_a_handle = True
                        attribute = node.func.attr
                        spelling = named + "." + attribute
            if on_a_handle and attribute.startswith("read"):
                return None if attribute in ASKS_ABOUT_THE_FILE else spelling
            # open(HERE).read(): the file object has no name of its own, so there is no dotted
            # spelling to match, and the handle it was opened on is written in the same
            # expression. Nothing about the run decides which file that is, so this is read
            # rather than left out: the place itself was already accounted through HERE, and
            # what this recovers is the helper handing the text ON to its caller.
            if (isinstance(node.func, ast.Attribute) and node.func.attr.startswith("read")
                    and isinstance(node.func.value, ast.Call)
                    and _opens_here(opens_a_file, node.lineno,
                                    places.get(id(node), (MODULE_LEVEL, None))[0])
                    and _opener_call(node.func.value.func, bare, dotted)):
                # HERE.open().read() names the handle as the receiver of open rather than as
                # its argument, and it is the same read either way.
                opener, receiver = node.func.value.func, []
                if isinstance(opener, ast.Name):
                    if (id(opener) in shadowed
                            and _nearest(rebound,
                                         places.get(id(opener), (MODULE_LEVEL, None))[0],
                                         opener.id)
                            < _nearest(taken_at,
                                       places.get(id(opener), (MODULE_LEVEL, None))[0],
                                       opener.id)):
                        # The scope binds the opener itself, so what it answers with is
                        # the caller's file rather than this module's.
                        return None
                elif (_dotted(opener) or "") in dotted:
                    # io.open is the builtin under its module's name: the module is not a
                    # handle, and what it was handed is all in the arguments.
                    # Unless a nearer binding took the module's name, the same way a bare
                    # alias loses to a parameter written closer to the read.
                    stem = opener.value
                    while isinstance(stem, ast.Attribute):
                        stem = stem.value
                    at = places.get(id(stem), (MODULE_LEVEL, None))[0]
                    if (isinstance(stem, ast.Name) and id(stem) in shadowed
                            and _nearest(holders_at, at, stem.id)
                            < _nearest(taken_at, at, stem.id)):
                        return None
                elif not (isinstance(opener, ast.Attribute)
                          and spelled(opener.value, klass)):
                    # Only a handle's own open opens the file this module asks about.
                    return None
                else:
                    receiver = [opener.value]
                for argument in (list(node.func.value.args)
                                 + [given.value for given in node.func.value.keywords]
                                 + receiver):
                    opened = spelled(argument, klass)
                    if opened:
                        return opened + "." + node.func.attr
            return None
        return None

    return spelled


def planted_source(declared, spelling, planted, lines, beside=()):
    """A synthetic module: one stub per declared place, and one place spelled the planted way.

    Generated FROM the declared maps rather than written out, so it stays a balanced control while
    they move. Each stub carries a spelling the check recognises, so the only thing separating the
    planted place from the declared ones is the form it is written in.

    The companion maps are looked up by name rather than referenced, because this same case is
    run against the commit BEFORE they existed in order to measure the red it produces there, and
    a reference would make that an attribute error instead of the refusal being measured. What
    this control establishes is that the complaint about the planted place is caused by the
    plant; it does not make the synthetic module a copy of this one, and the check may still say
    other things about a module that is not the file it was written for.
    """
    text = []
    names = set(declared)
    for other in beside:
        names |= set(globals().get(other) or ())
    for name in sorted(names):
        # A declared place may be a helper inside another one, and its name is the chain that
        # keeps it apart. The stub is written as that same chain so the derivation computes the
        # name the declaration is keyed by, rather than one the check would call undeclared.
        parts = name.split(".")
        for depth, part in enumerate(parts):
            text.append("    " * depth + "def " + part + "(self):")
        text += ["    " * len(parts) + spelling, ""]
    if planted is not None:
        text += ["def " + planted + "(self):"] + ["    " + line for line in lines] + [""]
    return "\n".join(text)


# The companion maps, named rather than referenced, so a control can be run against a commit
# where they do not exist yet.
BESIDE_TEXT = ("TOUCHES_SOURCE_WITHOUT_CONCLUDING",)
BESIDE_REFUSAL = ("NAMES_A_REFUSAL_WITHOUT_SETTLING",)


def asked_over(check, source):
    """Run one of the checks over a source of our own, and report what it refused to accept.

    The check reads HERE, so HERE is what is moved. Reaching the handle by its name as a string
    is the one form the derivation cannot see, which SOURCE_OUT_OF_REACH says, and this is the
    case that proves the sentence is about something real.
    """
    with tempfile.TemporaryDirectory() as temporary:
        written = Path(temporary) / "planted.py"
        written.write_text(source, encoding="utf-8")
        with mock.patch.object(sys.modules[__name__], "HERE", written):
            try:
                check()
            except AssertionError as refused:
                return str(refused)
    return None


def refusals_reached(source):
    """Every occurrence of a refusal in this source, and the spellings the derivation used."""
    tree = ast.parse(source)
    spellings = refusal_spellings()
    # Which names in this source are classes written here, so an attribute read off one is
    # answered by what the class binds rather than by the spelling of its name.
    classes, as_class, built = _instance_classes(tree)
    shadowed, taken_at = _shadowing_names(tree)
    places = _places(tree)
    known_keys = frozenset(places.get(id(node), (MODULE_LEVEL, None))[1]
                           for node in ast.walk(tree) if isinstance(node, ast.ClassDef))
    # A refusal imported by name is that answer under a bare name: from completion import
    # NOT_READ makes the constant readable here without an attribute to match.
    answers_here = spellings["module attribute"] | spellings["collection"]
    # The owner is asked whether it really exports that answer. A name occurring in some other
    # module's inventory is not one here, and only modules this process has already imported
    # are consulted -- one named in the source under analysis is not something to import in
    # order to answer a question about the text.
    imported_answers, answers_at = set(), {}
    for node in ast.walk(tree):
        if not isinstance(node, ast.ImportFrom) or not node.module:
            continue
        # Asked of THIS module's namespace first, which is where the producers are resolved
        # everywhere else here: a module imported for its own sake is bound by name rather than
        # registered under one.
        owner = vars(sys.modules[__name__]).get(node.module) or sys.modules.get(node.module)
        if not isinstance(owner, types.ModuleType):
            continue
        where = places.get(id(node), (MODULE_LEVEL, None))[0]
        for alias in node.names:
            if alias.name == "*":
                # from X import * imports every name the module exports, so the answers among
                # them arrive here bare and under no alias. Only a module this process has
                # already imported can say which those are, and __all__ decides when the
                # module declares one.
                exported = getattr(owner, "__all__", None)
                if exported is None:
                    exported = [name for name in vars(owner) if not name.startswith("_")]
                for name in exported:
                    if name in answers_here and getattr(owner, name, None) in REFUSAL_ANSWERS:
                        imported_answers.add(name)
                        answers_at.setdefault(where, set()).add(name)
                continue
            if alias.name in answers_here and getattr(owner, alias.name, None) in REFUSAL_ANSWERS:
                imported_answers.add(alias.asname or alias.name)
                answers_at.setdefault(where, set()).add(alias.asname or alias.name)
    # Which module each qualifier in THIS source names, so an owner is decided by the module a
    # spelling reaches rather than by the spelling itself.
    qualifies = {}
    for node in ast.walk(tree):
        where = places.get(id(node), (MODULE_LEVEL, None))[0]
        if isinstance(node, ast.Import):
            for alias in node.names:
                qualifies.setdefault(where, {}).setdefault(
                    alias.asname or alias.name, set()).add(alias.name.rpartition(".")[2])
        elif isinstance(node, ast.ImportFrom) and node.module:
            for alias in node.names:
                if alias.name != "*":
                    qualifies.setdefault(where, {}).setdefault(
                        alias.asname or alias.name, set()).add(alias.name)
    owners_here = spellings.get("owner", frozenset())
    # answers = reading names the same module an import would, so a qualifier reached that way
    # is the owner just as plainly. Followed to a fixpoint because b = a is one more step, and
    # through the pass-through vocabulary for the same reason every other binding here is.
    spreading = True
    while spreading:
        spreading = False
        for node in ast.walk(tree):
            where = places.get(id(node), (MODULE_LEVEL, None))[0]
            # Pair by pair rather than whole-statement: answers, ignored = reading, None hands
            # the module to ONE of the names, and reading the tuple as a single value followed
            # neither, so the alias was never recorded and every reader of it was dropped.
            for target, value in _bindings(node):
                if not isinstance(target, ast.Name):
                    continue
                for source in _passed_through(value):
                    named = (qualifies.get(where, {}).get(source.id)
                             or qualifies.get(MODULE_LEVEL, {}).get(source.id)
                             or ({source.id} if source.id in owners_here else set()))
                    holds = qualifies.setdefault(where, {}).setdefault(target.id, set())
                    if named - holds:
                        holds |= named
                        spreading = True
    # To a fixpoint, because self.second = self.first holds the refusal only once the pass
    # knows that self.first does.
    held, growing = {}, True
    while growing:
        wider = _held_by_class(
            tree, _refusal_spelled(spellings, held, classes, as_class, shadowed, built,
                                   imported_answers, known_keys, qualifies, places,
                                   answers_at, taken_at), held)
        growing = wider != held
        held = wider
    return (_occurrences(tree, _refusal_spelled(spellings, held, classes, as_class, shadowed,
                                                built, imported_answers, known_keys,
                                                qualifies, places, answers_at, taken_at)),
            spellings)


def source_text_reached(source):
    """Every occurrence of a reach into source text, the spellings, the unread names and the calls."""
    tree = ast.parse(source)
    handles, hands_source, undecided, called = source_spellings(tree)
    declared = frozenset(handles)
    shadowed, taken_at = _shadowing_names(tree)
    # Whether open is the builtin here. A module that defines its own open has shadowed it, and
    # what that one answers with is a question about the run rather than a file's text.
    # The LINE the module first binds open, rather than a flag for the whole file: a read
    # written above that line took the builtin and really did open the file.
    takes_the_name = [statement.lineno for statement in getattr(tree, "body", ())
                      if (isinstance(statement, (ast.FunctionDef, ast.AsyncFunctionDef,
                                                 ast.ClassDef)) and statement.name == "open")
                      or any(inner.id == "open"
                             for target in (_assigned(statement) or ((), None))[0]
                             for inner in ast.walk(target) if isinstance(inner, ast.Name))]
    opens_a_file = min(takes_the_name) if takes_the_name else None
    _classes, as_class, built = _instance_classes(tree)
    places = _places(tree)
    openers = _opener_spellings(tree, places)
    handles, where_from = _handle_names(tree, handles, shadowed, opens_a_file, openers,
                                        taken_at)
    # Which scope derived each handle, so a name bound nearer the read can take it back. The
    # propagation records WHERE a handle came from; this is the same fact keyed the way the
    # distance rule reads it.
    derived_at = {}
    for named, made_in in where_from.items():
        for at in made_in:
            derived_at.setdefault(at, set()).add(named)
    # Which scope gives a name to something that hands source back. reader = ast.parse binds
    # the name in the scope that wrote it, so that scope is not shadowing the reader it just
    # named -- the same rule the opener alias already follows.
    reader_at = {}
    for node in ast.walk(tree):
        at = places.get(id(node), (MODULE_LEVEL, None))[0]
        # Pair by pair, the same reason the opener alias walk beside this one does it: an
        # unpacking hands the reader to one of the names and the statement's whole value to
        # neither.
        for target, value in _bindings(node):
            if not isinstance(target, ast.Name):
                continue
            # Through the pass-through vocabulary as well as the dotted spelling, the same way
            # the opener alias walk reads its value: reader = ast.parse is dotted, and a name
            # handed through a branch is not.
            if ({(_dotted(value) or "")}
                    | {source.id for source in _passed_through(value)}) & set(hands_source):
                reader_at.setdefault(at, set()).add(target.id)

    def inside(scope, made):
        """Whether a use in this scope sees a handle derived in one of those."""
        return _visible_from(scope, made)

    # A derived handle read in a scope that did not derive it is a different object wearing the
    # same name, so it is treated as a shadow rather than as this module's source.
    astray = {id(node) for node in ast.walk(tree)
              if isinstance(node, ast.Name) and node.id in where_from
              and node.id not in declared
              and not inside(places.get(id(node), (MODULE_LEVEL, None))[0],
                             where_from[node.id])}
    held, growing = {}, True
    while growing:
        wider = _held_by_class(
            tree, _source_spelled(handles, hands_source, held, as_class, shadowed, declared,
                                  astray, built, opens_a_file, openers, places, reader_at,
                                  taken_at, derived_at),
            held)
        growing = wider != held
        held = wider
    return (_occurrences(tree, _source_spelled(handles, hands_source, held, as_class,
                                               shadowed, declared, astray, built,
                                               opens_a_file, openers, places, reader_at,
                                               taken_at, derived_at)),
            {"handle": handles, "hands source": frozenset(hands_source)}, undecided, called)


def places_reached(which, source):
    """The places one derivation names in a source of our own.

    The samples are whole modules rather than planted lines, because every form they record is
    about class structure and a single line cannot carry one.
    """
    if which == TEXT:
        found, _reach, _undecided, _called = source_text_reached(source)
    else:
        found, _spellings = refusals_reached(source)
    return sorted({row[1] for row in found})


class SevenReadingsTests(unittest.TestCase):
    """Seven questions, seven readings, and no answer standing in for another.

    The criterion this module exists for. The seven are not one payload: two are commands and
    one is a comparison this module performs, so the easy failure is a table that quietly fills a
    cell from whatever was nearest. Each check below removes one way of doing that -- a row that
    reads a path nobody writes, a row that reads another row answer, a cell filled without going
    through read() at all -- and the mutation case states the property directly rather than
    trusting the declaration.
    """

    def setUp(self):
        self.maxDiff = None

    def test_every_question_has_exactly_one_declared_reading(self):
        """Asked of the accessor, because two lists agreeing is not a reading being reachable.

        Comparing the table with the list of questions is two declarations agreeing with each
        other. What the seven actually need is for the accessor to resolve each of them, so each
        is put through read() and has to come back as itself.
        """
        for cell in SEVEN:
            with self.subTest(cell):
                self.assertEqual(read(cell, {})["cell"], cell,
                                 cell + ": the accessor does not resolve a declared reading")
        declared = [row[0] for row in READINGS]
        self.assertEqual(len(set(declared)), len(declared), "each question is answered once")
        self.assertEqual(set(declared) - set(SEVEN), set(),
                         "a row nobody asked for is an answer to nothing")

    def test_no_two_cells_read_the_same_answer(self):
        places = [(row[1], row[2]) for row in DECLARED]
        self.assertEqual(len(set(places)), len(places),
                         "two cells reading one place is one reading answering two questions")

    def test_every_declared_producer_exists_where_it_is_declared(self):
        """Asked of the module, not read out of its file.

        A definition found in source text can be a name in a comment or in a branch nothing
        reaches, and for the producer this module owns the file being searched is the file that
        declares it -- so the search would have found its own declaration. The module is asked
        for the attribute instead, which is the thing a caller reaches.
        """
        for cell, _source, _path, producer in DECLARED:
            module, _, name = producer.rpartition(".")
            with self.subTest(cell):
                self.assertIn(module, PRODUCER_MODULES,
                              cell + ": nothing resolves " + module)
                resolved = getattr(PRODUCER_MODULES[module], name, None)
                self.assertIsNotNone(resolved, cell + ": " + producer + " does not exist")
                self.assertTrue(callable(resolved),
                                cell + ": " + producer + " is not something that can produce"
                                " a reading")


    def test_every_text_reading_place_is_declared(self):
        """The inventory, derived by accounting rather than by recognising a shape.

        Three times a check in this module concluded something about behaviour by reading source
        text, and each time the repair was the instance. The instances share a shape: a claim
        about what code does, settled by how the code is spelled -- and where the file being read
        is the file making the claim, the check can be satisfied by its own declaration.
        AGENTS.md states the rule for this repository: a text-matching test is not proof of
        behaviour.

        What this check used to see was one form, a call to ast.parse inside a function, and it
        said so. Anything else -- read_text compared directly, a phrase searched for, a name
        imported from ast, a helper handing the text on -- was simply not looked at, so the
        sentence above it reached further than the check did. It now derives instead: every
        binding of this module that is a path to a .py file that exists, the __file__ every
        module carries, every called name that hands source back (asked of the callable, which
        is where ast.parse comes from, so the one form that used to be spelled out by hand is
        now read off ast.parse's own signature), and every string naming a .py file. Then it
        accounts for EVERY occurrence of one, attributed to the innermost function it sits in and
        named by the chain around it, so a nested helper or a lambda inside an already declared
        place is its own place rather than something that place absorbs. A place that reaches
        source text some way nobody anticipated is still an occurrence of a handle, so it arrives
        here as a failure.

        This claims exactly that much. A name this derivation could not read is in
        SOURCE_UNDECIDED_CALLS rather than assumed harmless, and the forms it cannot reach at all
        are in SOURCE_OUT_OF_REACH, each with a control that plants it and requires the
        derivation not to see it.
        """
        found, reach, undecided, called = source_text_reached(
            HERE.read_text(encoding="utf-8"))

        unaccounted = [row for row in found
                       if (row[1] not in SOURCE_AT_MODULE_LEVEL if row[0]
                           else row[1] not in TEXT_EVIDENCE
                           and row[1] not in TOUCHES_SOURCE_WITHOUT_CONCLUDING)]
        self.assertEqual(unaccounted, [],
                         "a place reaches the text of a source file and nobody wrote down what"
                         " it is doing there. Say whether it concludes from the text or only"
                         " names the file, or teach the derivation the form: "
                         + json.dumps(unaccounted))

        reached = {row[1] for row in found}
        stale = sorted((set(TEXT_EVIDENCE) | set(TOUCHES_SOURCE_WITHOUT_CONCLUDING)
                        | set(SOURCE_AT_MODULE_LEVEL)) - reached)
        self.assertEqual(stale, [],
                         "a declaration outlived the place it described: " + json.dumps(stale))

        arrived = sorted(set(undecided) - set(SOURCE_UNDECIDED_CALLS))
        self.assertEqual(arrived, [],
                         "a called name this derivation cannot read is not written down, so"
                         " whether it hands source back is nobody's answer: "
                         + json.dumps(arrived))
        for name, (where, why) in sorted(SOURCE_UNDECIDED_CALLS.items()):
            with self.subTest(name):
                self.assertTrue(why.strip(), name + " is undecided without a reason")
                self.assertIn(name, called,
                              name + " is written down as unreadable and nothing here calls it")
                if where == THE_FLOOR and sys.version_info < READABLE_FROM:
                    self.assertIn(name, undecided,
                                  name + " is declared unreadable on the floor and the floor"
                                  " read it, so the declaration is now wrong")
                if where == EVERY_INTERPRETER:
                    self.assertIn(name, undecided,
                                  name + " is declared unreadable on every supported"
                                  " interpreter and this one read it, so the declaration is"
                                  " now wrong and the entry has to say so")

        for place, why in sorted(TEXT_EVIDENCE.items()):
            with self.subTest(place):
                self.assertTrue(why.strip(),
                                place + " is declared without the reason it cannot ask")
        for place, why in sorted(TOUCHES_SOURCE_WITHOUT_CONCLUDING.items()):
            with self.subTest(place):
                self.assertTrue(why.strip(),
                                place + " touches a source file without saying why that is not"
                                " a conclusion drawn from its text")
        self.assertTrue(reach["handle"] and reach["hands source"],
                        "a derivation kind came back empty, so the sweep stopped matching")


    def test_every_declared_path_is_one_its_source_actually_writes(self):
        """A rename fails here, and it is the payload that says so rather than the file text.

        Searching the producing source for the key passes on a mention in a comment, in an
        unrelated branch, or -- for the reading this module performs itself -- on the entry in
        this very table, which would have left the check answering its own question. So the
        payloads are real, the declared path is walked in them, and the case then removes the
        last key to show the walk was load-bearing rather than incidental.
        """
        with tempfile.TemporaryDirectory() as temporary:
            payloads, _host = observe_all(Path(temporary).resolve())

        for cell, source, path, _producer in DECLARED:
            with self.subTest(cell):
                found = payloads.get(source)
                for key in path:
                    self.assertIsInstance(found, dict,
                                          cell + ": " + source + " does not reach " + repr(key))
                    self.assertIn(key, found,
                                  cell + ": " + source + " no longer writes " + repr(key))
                    found = found[key]

                pruned = copy.deepcopy(payloads)
                target = pruned[source]
                for key in path[:-1]:
                    target = target[key]
                del target[path[-1]]
                self.assertEqual(read(cell, pruned)["value"], reading.UNREADABLE,
                                 cell + ": the row survived its own key being taken away, so"
                                 " the path it declares is not the path it reads")

    def test_no_cell_is_filled_without_going_through_the_declared_reading(self):
        """Lint, not proof: the proof is the mutation case below.

        It catches the cheap version of the defect -- a test reaching into a payload for a key
        this table declares -- which is how a borrowed answer usually arrives.
        """
        keys = {key for row in DECLARED for key in row[2]}
        allowed = {"read", "_row", "_unreadable", "observe_all"}
        reached = []

        def walk(node):
            for child in ast.iter_child_nodes(node):
                if isinstance(child, (ast.FunctionDef, ast.AsyncFunctionDef)):
                    if child.name in allowed:
                        continue
                if isinstance(child, ast.Subscript) and isinstance(child.slice, ast.Constant):
                    if child.slice.value in keys:
                        reached.append(child.slice.value)
                if (isinstance(child, ast.Call) and isinstance(child.func, ast.Attribute)
                        and child.func.attr == "get" and child.args
                        and isinstance(child.args[0], ast.Constant)
                        and child.args[0].value in keys):
                    reached.append(child.args[0].value)
                walk(child)

        walk(ast.parse(HERE.read_text(encoding="utf-8")))
        self.assertEqual(reached, [],
                         "a declared reading key is reached outside read(): " + repr(reached))

    def test_every_row_is_readable_and_answers_in_its_own_vocabulary(self):
        """The baseline the independence cases need.

        An independence check over seven unreadable rows proves nothing: they would stay equal
        to each other through any change. So the baseline is established first, and every row
        answers in the vocabulary of the thing that produced it rather than in a shared one.
        """
        with tempfile.TemporaryDirectory() as temporary:
            payloads, _host = observe_all(Path(temporary))
        rows = table(payloads)

        for cell in SEVEN:
            with self.subTest(cell):
                if cell == "modelPermissionPreservation" and not HAS_READER:
                    # The one row this interpreter cannot take. It has to say so, and say why:
                    # an unread row that reports itself readable is the failure this whole
                    # arrangement is against, and it would be invisible on exactly one job.
                    self.assertFalse(rows[cell]["readable"])
                    self.assertEqual(rows[cell]["value"], reading.UNREADABLE)
                    self.assertIn("tomllib", rows[cell]["detail"],
                                  "the refusal names the reader it did not have")
                    continue
                self.assertTrue(rows[cell]["readable"],
                                cell + ": " + str(rows[cell].get("detail")))

        for cell in CHECK_FIELD_ROWS:
            with self.subTest(cell):
                self.assertIn(rows[cell]["value"]["value"], check.VALUES)

        for cell in COMPLETION_CELL_ROWS:
            with self.subTest(cell):
                answer = rows[cell]["value"]["value"]
                self.assertIsInstance(answer, str)
                self.assertNotIn(answer, check.VALUES,
                                 cell + ": the hook answers in its own vocabulary, and merging"
                                 " it into the result vocabulary would be a translation nobody"
                                 " performed")

        links = rows["skillLink"]["value"]
        self.assertIn("command", links)
        self.assertEqual(links.get("exitCode"), 0,
                         "the listing ran and found nothing missing; accepting a failed check"
                         " here would leave this row green with no skill linked at all")
        self.assertEqual(links.get("missing"), [])
        self.assertTrue(links.get("linked"), "and it names what it linked")

        preservation = rows["modelPermissionPreservation"]["value"]
        if HAS_READER:
            self.assertEqual(preservation, PRESERVED,
                             "an install has no business changing the model or the permission"
                             " posture it was installed over")
        else:
            self.assertEqual(preservation, reading.UNREADABLE,
                             "without a reader this is unread, and unread is not preserved")


    def test_every_question_is_declared_to_answer_in_exactly_one_vocabulary(self):
        """The three families have to cover the seven, once each.

        Not decoration: the families are what stops a row being compared against the wrong set
        of admissible answers. A row in none of them would be checked against nothing, and a row
        in two would be checked against a vocabulary it does not answer in -- which is the same
        borrowing this module refuses, arriving through the door marked housekeeping.
        """
        families = {"check.VALUES": CHECK_FIELD_ROWS,
                    "the completion cells": COMPLETION_CELL_ROWS,
                    "a shape of their own": OWN_SHAPE_ROWS}
        declared = [cell for members in families.values() for cell in members]

        self.assertEqual(sorted(declared), sorted(SEVEN),
                         "every question answers in one declared vocabulary, and only the seven"
                         " do: " + json.dumps(families))
        self.assertEqual(len(set(declared)), len(declared),
                         "a question in two vocabularies is checked against one it does not"
                         " answer in")

    def test_every_acceptance_row_reaches_its_success_answer(self):
        """The one that decides whether this suite is worth having.

        Every row here judges whether something worked. A row whose assertion accepts a refusal,
        an absence or a not_verified is a row that stays green when the thing it judges never
        happens -- and an install acceptance suite that passes while nothing installed is worse
        than no suite, because the next person reads that ground as covered.

        So one run is made in which all seven have something that worked to read, and each row
        is required to read its declared success answer. The skills are really linked, the
        registration really names the command the diagnosis is asked about, both components
        really import from this checkout, the relay really answers the doctor and every step of
        a trial, and the hook has really fired.
        """
        with tempfile.TemporaryDirectory() as temporary:
            payloads, host, registration = succeeding(Path(temporary).resolve())
            # Where the import resolved, captured before the directory goes: a verified import
            # that resolved in the checkout would be source availability answering a question
            # about an installation.
            resolved = {name: component.get("importedLocation")
                        for name, component in
                        (payloads["diagnose"].get("components") or {}).items()}
            candidate = host.candidate
        rows = table(payloads)

        self.assertEqual(sorted(SUCCESS_ANSWERS), sorted(SEVEN),
                         "every question declares the answer it reads when the thing worked")

        if HAS_READER:
            # The exposure row declares that it travels the registration this run wrote, so the
            # writing is required here. Reconstructing the command and handing it to diagnose
            # would read verified while register-mcp wrote nothing at all.
            self.assertEqual(registration.returncode, 0, registration.stderr[-300:])
            self.assertEqual(json.loads(registration.stdout)["outcome"], "CREATED",
                             "the registration the exposure row compares against was not"
                             " written by this run")

        # One host, not several. The hook row answers from the journal of the Codex home the
        # rest of this run installed into; answering from a home of its own would compose
        # readings about two different machines and still look like a composition.
        journal = rows["hookCallback"]["value"].get("journalRoot")
        self.assertIsNotNone(journal, "the hook cell names no journal it counted")
        self.assertTrue(inside(journal, candidate.parent.parent / "codex"),
                        "the callback was recorded under " + str(journal) + ", which is not the"
                        " Codex home this run installed into")

        self.assertTrue(resolved, "the diagnosis reported no component at all")
        for name, where in resolved.items():
            with self.subTest(name):
                self.assertIsNotNone(where, name + ": nothing was imported")
                self.assertTrue(inside(where, candidate),
                                name + " imported from " + str(where) + ", which is outside the"
                                " environment this run installed, so the row is answering about"
                                " available source rather than an installed runtime")

        for cell in SEVEN:
            wanted = SUCCESS_ANSWERS[cell]
            with self.subTest(cell):
                if cell in CONDITIONAL_SUCCESS and not HAS_READER:
                    # The declared condition, asserted rather than assumed: the success answer
                    # is excused only where the thing it names is genuinely absent. Where the
                    # row can still be read, what it reads is checked by the vocabulary case;
                    # what is not claimed here is that it succeeded.
                    self.assertFalse(HAS_READER,
                                     cell + ": excused from its success answer on an"
                                     " interpreter that does have the reader")
                    continue
                self.assertTrue(rows[cell]["readable"], str(rows[cell].get("detail")))
                found = rows[cell]["value"]
                if isinstance(wanted, dict):
                    self.assertEqual({key: found.get(key) for key in wanted}, wanted,
                                     cell + ": the listing does not read as a success")
                elif cell in OWN_SHAPE_ROWS:
                    self.assertEqual(found, wanted, cell + ": not its success answer")
                else:
                    self.assertEqual(found["value"], wanted,
                                     cell + ": read " + repr(found["value"]) + " where the thing it judges had worked, so this row would stay green if it never did")


    def test_every_place_that_settles_for_a_refusal_is_declared(self):
        """The inventory of accepted answers, derived by accounting for every occurrence.

        What this used to see was a refusal spelled as a literal, or as one of five constant
        names written into the check itself, inside a call whose name began with assert. Three
        forms, enumerated by hand -- and the enumerated half is the part that cannot grow: a
        constant added to REFUSAL_ANSWERS was invisible until somebody remembered to add its
        name here too, and a refusal bound to a name before the assertion, or required through
        assertIn against the answers themselves, was never looked at at all.

        So nothing in the spelling table is written down now. refusal_spellings asks the objects: the answers are
        REFUSAL_ANSWERS, and every way one can be written is derived from them -- an attribute an
        imported module binds to one, a global here bound to one, a collection here or on an
        imported module that contains one, and an attribute a class binds one to for its own
        methods to read. Then EVERY occurrence of any of those is accounted for: it belongs to a
        place that settles for a refusal, or to one that names a refusal without settling, or the
        module fails naming the place, the line and the spelling. The place is the innermost
        function the occurrence sits in, named by the chain around it, so a nested helper or a
        lambda inside an already declared method is its own place rather than something the
        declaration absorbs. A helper that hands a refusal BACK rather than returning a container
        holding one carries its callers in with it, through a bare call or through self.

        The point is not that refusals are forbidden -- this repository is built on absence being
        a real answer. It is that a place settling for one has to say why the refusal is the
        right answer to the question it asks, so that no acceptance row quietly accepts its own
        failure. What is still out of reach is in REFUSAL_OUT_OF_REACH with a control for each.
        """
        found, spellings = refusals_reached(HERE.read_text(encoding="utf-8"))

        unaccounted = [row for row in found
                       if (row[1] not in REFUSAL_AT_MODULE_LEVEL if row[0]
                           else row[1] not in ACCEPTS_A_REFUSAL
                           and row[1] not in NAMES_A_REFUSAL_WITHOUT_SETTLING)]
        self.assertEqual(unaccounted, [],
                         "a place names an answer that means the question was not settled, and"
                         " nobody wrote down what it is doing there. Say why the refusal is the"
                         " right answer here, or why this place only names one: "
                         + json.dumps(unaccounted))

        reached = {row[1] for row in found}
        stale = sorted((set(ACCEPTS_A_REFUSAL) | set(NAMES_A_REFUSAL_WITHOUT_SETTLING)
                        | set(REFUSAL_AT_MODULE_LEVEL)) - reached)
        self.assertEqual(stale, [],
                         "a declaration outlived the place it described: " + json.dumps(stale))

        for place, why in sorted(ACCEPTS_A_REFUSAL.items()):
            with self.subTest(place):
                self.assertTrue(why.strip(), place + " settles for a refusal without a reason")
        for place, why in sorted(NAMES_A_REFUSAL_WITHOUT_SETTLING.items()):
            with self.subTest(place):
                self.assertTrue(why.strip(),
                                place + " names a refusal without saying why that is not"
                                " settling for one")
        for kind, spelled in sorted(spellings.items()):
            with self.subTest(kind):
                self.assertTrue(spelled, kind + " came back empty, so the sweep stopped matching")


    def test_a_text_reading_place_arriving_in_a_form_the_old_sweep_missed_is_still_caught(self):
        """The regression: a place that concludes from source text some other way is red.

        The forms are planted rather than argued about. Each is one the sweep this replaces could
        not see -- it looked for a call to ast.parse and nothing else -- and each is measured by
        running the check over a module written here and requiring it to refuse, naming the place,
        the line and the spelling. The control is the same module without the plant: whatever the
        check says about a synthetic module, it must not already be saying it about the planted
        place, or the refusal would be evidence of nothing.
        """
        for planted, lines, spelling in (
                ("a_place_that_reads_the_source_of_an_object",
                 ['self.assertIn("a phrase", inspect.getsource(read))'], "inspect.getsource"),
                ("a_place_that_compares_this_file_text_directly",
                 ['self.assertIn("a phrase", HERE.read_text(encoding="utf-8"))'], "HERE")):
            with self.subTest(planted):
                check = self.test_every_text_reading_place_is_declared
                control = asked_over(check, planted_source(TEXT_EVIDENCE, "ast.parse(text)", None, (),
                                                           BESIDE_TEXT))
                self.assertNotIn(planted, control or "",
                                 "the synthetic module already complains about the planted place"
                                 " before it is planted, so refusing it would prove nothing")
                built = planted_source(TEXT_EVIDENCE, "ast.parse(text)", planted, lines,
                                       BESIDE_TEXT)
                at = built.splitlines().index("    " + lines[0]) + 1
                said = asked_over(check, built)
                self.assertIsNotNone(said,
                                     planted + ": the check accepted a module in which a place"
                                     " concludes from source text without being declared")
                self.assertIn(planted, said, planted + ": refused, but not for this place")
                self.assertIn(spelling, said, planted + ": refused without naming what it saw")
                self.assertIn(str(at), said,
                              planted + ": refused without naming the line it was on")

    def test_a_place_that_settles_for_a_refusal_in_a_form_the_old_sweep_missed_is_still_caught(self):
        """The regression: a place that requires a refusal some other way is red.

        Two forms the sweep this replaces could not see. It looked inside calls whose name begins
        with assert, for a literal, one of five constant names written into the check, or the
        name CHANGED -- so a refusal required through the answers themselves, and a refusal bound
        to a name before the assertion, both passed. Same measurement, same control.
        """
        spelling = "self.assertEqual(row, " + repr(sorted(REFUSAL_ANSWERS)[0]) + ")"
        for planted, lines in (
                ("a_place_that_requires_one_of_the_answers",
                 ["self.assertIn(row, REFUSAL_ANSWERS)"]),
                ("a_place_that_names_the_refusal_before_it_asserts",
                 ["settled = reading.UNREADABLE", "self.assertEqual(row, settled)"])):
            with self.subTest(planted):
                check = self.test_every_place_that_settles_for_a_refusal_is_declared
                control = asked_over(check, planted_source(ACCEPTS_A_REFUSAL, spelling, None, (),
                                                           BESIDE_REFUSAL))
                self.assertNotIn(planted, control or "",
                                 "the synthetic module already complains about the planted place"
                                 " before it is planted, so refusing it would prove nothing")
                said = asked_over(check, planted_source(ACCEPTS_A_REFUSAL, spelling, planted,
                                                        lines, BESIDE_REFUSAL))
                self.assertIsNotNone(said,
                                     planted + ": the check accepted a module in which a place"
                                     " settles for a refusal without being declared")
                self.assertIn(planted, said, planted + ": refused, but not for this place")

    def test_the_reach_of_each_derivation_is_the_declared_one(self):
        """Support, not a regression: a derivation kind that stopped resolving reads as green.

        Every kind is derived, so nothing here is the reach itself. What is written down is the
        spellings this module actually relies on, and the check is that each is still derived.
        A kind may reach further than this and that is not a failure; a kind that quietly stopped
        resolving -- an import that moved, a constant that was renamed -- loses one of these and
        fails, which the occurrence counts alone would not catch because a spelling nobody writes
        has no occurrence to lose.
        """
        spellings = refusal_spellings()
        for kind, wanted in sorted(REFUSAL_REACH_INCLUDES.items()):
            with self.subTest(kind):
                self.assertLessEqual(set(wanted), set(spellings[kind]),
                                     kind + ": " + json.dumps(sorted(set(wanted)
                                                                     - set(spellings[kind])))
                                     + " is no longer derived")
        _found, reach, _undecided, _called = source_text_reached(
            HERE.read_text(encoding="utf-8"))
        for kind, wanted in sorted(SOURCE_REACH_INCLUDES.items()):
            with self.subTest(kind):
                self.assertLessEqual(set(wanted), set(reach[kind]),
                                     kind + ": " + json.dumps(sorted(set(wanted)
                                                                     - set(reach[kind])))
                                     + " is no longer derived")

    def test_the_forms_outside_each_derivations_reach_are_the_declared_ones(self):
        """Support: the list of what cannot be reached is checked in both directions.

        A sentence saying a derivation cannot see something rots quietly. So each declared form is
        planted and the derivation has to NOT see it. A form that gets covered later makes its own
        control fail, which is the prompt to delete the entry rather than leave it claiming a
        limitation that no longer exists.
        """
        for form, (sample, why) in sorted(SOURCE_OUT_OF_REACH.items()):
            with self.subTest(form):
                self.assertTrue(why.strip(), form + " is declared without a reason")
                found, _reach, _undecided, _called = source_text_reached(
                    planted_source((), "", "a_place_outside_the_reach", [sample]))
                self.assertEqual([row for row in found if row[1] == "a_place_outside_the_reach"],
                                 [], form + " is inside the reach now, so delete the entry")
        for form, (sample, why) in sorted(REFUSAL_OUT_OF_REACH.items()):
            with self.subTest(form):
                self.assertTrue(why.strip(), form + " is declared without a reason")
                found, _spellings = refusals_reached(
                    planted_source((), "", "a_place_outside_the_reach", [sample]))
                self.assertEqual([row for row in found if row[1] == "a_place_outside_the_reach"],
                                 [], form + " is inside the reach now, so delete the entry")

    def test_every_argument_form_outside_the_seating_grammar_is_declared(self):
        """The boundary of argument seating, held closed rather than claimed away.

        Ground 1 asks that a member arriving in a form the detector cannot see goes RED, not
        that the detector resolve every form. So outside the seating grammar this reader gives
        neither an answer nor silence: nothing is seated, and the occurrence is emitted. An
        undeclared one fails here.

        Four things are asserted, because any one of them alone would rot. Each declared form
        is planted and has to be reported, so an entry cannot outlive its form. The ordered
        forms are planted and have to be reported as NOTHING, so the boundary cannot creep over
        what is supported. Every occurrence in this module is declared, which is the gate going
        red on an arrival nobody wrote down. And the reach matches the claim: with a set
        unpacking in the call the argument is not seated, so the inventory does not quietly
        answer a question it just said it cannot.
        """
        for form, (sample, why) in sorted(ARGUMENT_FORM_NOT_SEATED.items()):
            with self.subTest(form):
                self.assertTrue(why.strip(), form + " is declared without a reason")
                self.assertIn(form, _seating_not_established(ast.parse(sample)),
                              form + ": its own sample is not reported, so this entry describes"
                              " a form the derivation no longer meets")
        for supported in ("helper(*(ONE, TWO))", "helper(*[ONE, TWO])",
                          "helper(**{\"answer\": ONE})", "helper(ONE, answer=TWO)"):
            with self.subTest(supported):
                self.assertEqual(_seating_not_established(ast.parse(supported)), {},
                                 supported + " is inside the grammar and was reported as"
                                 " outside it, so the boundary has crept over supported work")
        arrived = sorted(set(_seating_not_established(ast.parse(
            HERE.read_text(encoding="utf-8")))) - set(ARGUMENT_FORM_NOT_SEATED))
        self.assertEqual(arrived, [],
                         "an argument form outside the seating grammar arrived in this module"
                         " and nobody wrote it down, so what it hands over is nobody's answer: "
                         + json.dumps(arrived))
        # And the claim equals the reach: the unordered form really is left unseated.
        self.assertNotIn("consumer", places_reached(REFUSAL, "\n".join((
            "def identity(first, second):",
            "    return first",
            "",
            "def helper():",
            "    return identity(*{\"fine\", reading.UNREADABLE})",
            "",
            "def consumer():",
            "    return helper()"))),
            "a set unpacking is declared unseatable and the seating answered anyway")

    def test_a_refusal_nested_in_a_collection_is_still_collected(self):
        """The reach over written containers, asked of the objects rather than read off them.

        A refusal one container deep and a refusal two containers deep are spelled the same
        way at the use site, so a reach that stopped at the direct members let a row settle for
        one through the nested form without ever being inventoried. Nothing in the source says
        which it is: the value is a tuple either way, and only asking the object distinguishes
        them, which is why this is a behavioural control rather than another matcher case.

        Each shape is bound on this module, the derivation is re-run and has to answer, and the
        binding is removed again. The negatives are bound the same way -- a collection holding
        nothing of the kind, and one that refers to itself -- so the descent cannot pass by
        collecting everything, and cannot hang on a cycle either.
        """
        circular = []
        circular.append(circular)
        for shape, value, collected in (
                ("one container deep", (reading.UNREADABLE,), True),
                ("two containers deep", ((reading.UNREADABLE,),), True),
                ("three containers deep", (((reading.UNREADABLE,),),), True),
                ("held as a mapping value", {"key": [reading.UNREADABLE]}, True),
                ("held as a mapping key", {(reading.UNREADABLE,): "label"}, True),
                ("a collection of nothing of the kind", (("fine",),), False),
                ("a collection that holds itself", circular, False)):
            with self.subTest(shape):
                here = sys.modules[__name__]
                setattr(here, "A_COLLECTION_UNDER_TEST", value)
                try:
                    spelled = refusal_spellings()["collection"]
                finally:
                    delattr(here, "A_COLLECTION_UNDER_TEST")
                self.assertEqual("A_COLLECTION_UNDER_TEST" in spelled, collected,
                                 shape + ": the derivation and the object disagree about"
                                 " whether this collection holds a refusal, so a use of the"
                                 " name is inventoried on the wrong answer")

    def test_each_transformation_boundary_is_still_where_it_says(self):
        """Support: the declared transformation boundary, held at both ends.

        The two lists above declare forms this reader never sees at all, and their control is
        that the planted place is absent. This boundary is different in shape: the place naming
        the spelling IS found, and what is lost is the caller behind it. A control that only
        checked the absence would pass just as well if the derivation had stopped seeing
        anything, so both ends are asserted -- the named place present, the caller absent.

        A boundary that closes makes this fail and asks for the entry to be deleted, which is
        the whole reason it is data rather than a sentence.
        """
        self.assertTrue(HANDED_ON_THROUGH_A_TRANSFORMATION,
                        "the declared transformation boundary is empty, so this check would"
                        " pass by asserting nothing")
        for form, (which, lines, named, behind, why) in sorted(
                HANDED_ON_THROUGH_A_TRANSFORMATION.items()):
            with self.subTest(form):
                self.assertTrue(why.strip(), form + " is declared without a reason")
                places = places_reached(which, "\n".join(lines))
                self.assertIn(named, places,
                              form + ": the place that writes the spelling is not found at all,"
                              " so this boundary is not where the entry says it is: "
                              + json.dumps(places))
                self.assertNotIn(behind, places,
                                 form + ": the caller is inside the reach now, so delete the"
                                 " entry rather than leave it claiming a limit that is gone: "
                                 + json.dumps(places))

    def test_each_name_this_reader_resolves_lands_where_python_lands(self):
        """A name reaches what Python reaches, through classes as much as through scopes.

        Every form here is one review found this reader got wrong, or the pair that form's fix
        must not break, and the two are kept together deliberately: most of the errors below
        were introduced by the fix above them. A shadow that stops a lookup is correct when the
        binding really happens, and the fix for a loop that never runs sits one line from
        disabling shadowing altogether.

        How many there are is not written down here, because a count in a sentence is the thing
        this module keeps getting wrong: it was last true at eleven entries and stayed on the
        page long after. The table is the statement. Each entry carries the sample, the place,
        the answer Python gives and why, and adding a form means adding its pair rather than
        editing a number.
        """
        for form, (which, lines, place, reaches, why) in sorted(RESOLVES_LIKE_PYTHON.items()):
            with self.subTest(form):
                self.assertTrue(why.strip(), form + " is declared without a reason")
                places = places_reached(which, "\n".join(lines))
                if reaches:
                    self.assertIn(place, places,
                                  form + ": " + place + " reaches the declared thing and this"
                                  " reader lost it: " + json.dumps(places))
                else:
                    self.assertNotIn(place, places,
                                     form + ": " + place + " cannot reach the declared thing and"
                                     " this reader named it anyway: " + json.dumps(places))

    def test_a_decorator_alias_resolves_in_the_scope_that_imported_it(self):
        """The lexical rule, checked against what Python really binds rather than against this
        reader's own lookup.

        The sweep beside this one establishes that a lookup HAS a visibility predicate. It
        cannot establish that the predicate answers correctly, and here the whole defect was a
        guard that existed and answered wrongly, so structure alone would have passed it.

        So the expectation is observed rather than computed: each sample is run, and the class
        it builds is asked whether a property descriptor was installed. Where the alias names a
        module with no such attribute, Python does not build the class at all -- which is the
        same answer, reached by the language rather than by agreeing with this file.
        """
        for form, (lines, maker, place) in sorted(DECORATOR_ALIAS_CONTROLS.items()):
            with self.subTest(form):
                written = "\n".join(lines)
                namespace = {}
                exec(compile(written, "<control>", "exec"), namespace)
                try:
                    installed = isinstance(vars(namespace[maker]()).get("carrier"), property)
                except AttributeError:
                    # The module the alias really names has no attribute of that spelling, so
                    # the class is never built: no descriptor, and no consumer reading one.
                    installed = False
                self.assertEqual(place in places_reached(REFUSAL, written), installed,
                                 form + ": this reader and Python disagree about whether the"
                                 " decorator installed a descriptor, so the alias was resolved"
                                 " in a scope that cannot reach the use site")

    def test_every_owner_deciding_site_here_measures_how_near_the_binding_is(self):
        """Support: coverage over the sites that decide which binding a name refers to.

        Five findings in a row were one rule applied at some of those sites and not others, so
        the set is swept rather than the instances. The predicate is stated in
        DECIDES_AN_OWNER_WITHOUT_DISTANCE and read off this file: a site is recognised by
        calling _bound_around or _nearest, which are the only helpers that consult a
        scope-keyed binding map by name.

        This establishes COVERAGE, not correctness. That every site asks the question is a
        different claim from the answers being right, and the answers are held by the paired
        cases in RESOLVES_LIKE_PYTHON whose expectations come from Python rather than from this
        reader. A derived inventory that was allowed to stand for both would be this module's
        own defect wearing the tooling's clothes.

        The boundary is measured too: _visible_from decides an owner by reading a scope-keyed
        map directly, and the predicate does not recognise it. That absence is asserted, so the
        list cannot quietly claim to see every shape.
        """
        source = HERE.read_text(encoding="utf-8")
        parsed = ast.parse(source)
        owner = {}
        for top in parsed.body:
            if isinstance(top, (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef)):
                for inner in ast.walk(top):
                    owner[id(inner)] = top.name
        asks, measures = {}, set()
        for node in ast.walk(parsed):
            if not isinstance(node, ast.Call):
                continue
            named = getattr(node.func, "id", None)
            if named == "_bound_around":
                asks.setdefault(owner.get(id(node), MODULE_LEVEL), []).append(node.lineno)
            if named == "_nearest":
                measures.add(owner.get(id(node), MODULE_LEVEL))
        self.assertTrue(asks or measures,
                        "no owner-deciding site was recognised at all, so this check would pass"
                        " by sweeping nothing rather than by finding nothing")
        # The comparison itself, shown rather than asserted away.
        without = sorted(set(asks) - measures)
        undeclared = sorted(set(without) - set(DECIDES_AN_OWNER_WITHOUT_DISTANCE))
        self.assertEqual(undeclared, [],
                         "a site decides which binding a name refers to by asking only whether"
                         " one exists outward, and nobody wrote down why that is safe there: "
                         + json.dumps({name: asks[name] for name in undeclared}))
        stale = sorted(set(DECIDES_AN_OWNER_WITHOUT_DISTANCE) - set(without))
        self.assertEqual(stale, [],
                         "declared as deciding without distance, but it measures now, so delete"
                         " the entry: " + json.dumps(stale))
        for name, why in sorted(DECIDES_AN_OWNER_WITHOUT_DISTANCE.items()):
            with self.subTest(name):
                self.assertTrue(why.strip(), name + " is declared without a reason")
        # The boundary, exercised: a site that decides an owner by reading a map directly is
        # NOT recognised by this predicate, and saying so is the whole point of writing the
        # predicate down.
        self.assertNotIn("_visible_from", set(asks) | measures,
                         "_visible_from decides an owner without calling either helper, so the"
                         " predicate should not see it -- if it does, the boundary written"
                         " above is no longer true and the wording has to change")
        # The fourth boundary, measured the same way. _default_applies decides which definition
        # a name refers to by building its own map rather than consulting a scope-keyed one, so
        # this predicate is blind to it -- which is how a file-wide key of that shape reached a
        # reviewer after this inventory existed. The blindness is recorded rather than implied.
        self.assertNotIn("_default_applies", set(asks) | measures,
                         "_default_applies builds its own binding map instead of consulting a"
                         " scope-keyed one, so the predicate should not see it. If it does, the"
                         " fourth boundary above has stopped being true and the honest gap this"
        " list declares has changed shape")

    def test_every_statement_reader_here_pairs_an_unpacking_off(self):
        """Support: coverage over the sites that read a binding statement.

        _assigned answers with a statement's TARGETS and its VALUE, which is the right shape
        for asking what a statement binds and the wrong one for asking what each name is
        given. answers, ignored = reading, None hands the module to one name and None to the
        other, and a site pairing the whole value against every target either taints both or,
        where it accepts only a top-level Name, sees neither.

        Three sites were handed exactly that, one at a time -- the refusal owner, the opener
        and the class alias -- so the set is swept rather than the instances. The predicate is
        read off this file and it is deliberately blunt: ANY use of _assigned has to be
        declared with the reason the statement shape is right there.

        Blunt because the obvious predicate was not honest. Treating a site as safe when its
        function also calls _bindings passed both remaining sites for the wrong reason -- these
        are long functions that call _bindings elsewhere for unrelated work, so the check would
        have been measuring co-occurrence rather than pairing, and it would have gone on
        passing while a new _assigned site inside one of them mis-paired.

        COVERAGE, not correctness. That a use is declared says nothing about the pairing being
        right where _bindings IS used; the paired cases in RESOLVES_LIKE_PYTHON hold that, and
        their expectations come from Python rather than from this reader.
        """
        source = HERE.read_text(encoding="utf-8")
        parsed = ast.parse(source)
        owner = {}
        for top in parsed.body:
            if isinstance(top, (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef)):
                for inner in ast.walk(top):
                    owner[id(inner)] = top.name
        reads_statements = {}
        for node in ast.walk(parsed):
            named = getattr(getattr(node, "func", None), "id", None)
            here = owner.get(id(node), MODULE_LEVEL)
            if named == "_assigned":
                reads_statements.setdefault(here, []).append(node.lineno)
        self.assertTrue(reads_statements,
                        "no statement reader was recognised at all, so this check would pass by"
                        " sweeping nothing rather than by finding nothing")
        # The comparison itself, shown rather than asserted away.
        without = sorted(reads_statements)
        undeclared = sorted(set(without) - set(READS_A_STATEMENT_WITHOUT_PAIRING_IT))
        self.assertEqual(undeclared, [],
                         "a site reads a binding statement without pairing an unpacking off,"
                         " and nobody wrote down why that is safe there: "
                         + json.dumps({name: reads_statements[name] for name in undeclared}))
        stale = sorted(set(READS_A_STATEMENT_WITHOUT_PAIRING_IT) - set(without))
        self.assertEqual(stale, [],
                         "declared as reading a statement without pairing it, but it pairs now,"
                         " so delete the entry: " + json.dumps(stale))
        for name, why in sorted(READS_A_STATEMENT_WITHOUT_PAIRING_IT.items()):
            with self.subTest(name):
                self.assertTrue(why.strip(), name + " is declared without a reason")

    def test_every_placement_site_here_honours_a_declared_owner(self):
        """Support: coverage over the sites that decide WHERE a binding lives.

        A different layer from the sweep above. That one is about LOOKUP -- which binding a
        name refers to from a use site. This one is about PLACEMENT -- which scope a binding
        lives in at all, which is the question those lookups are then correct about. Three
        findings in a row were placement, and they landed on three different sites, so the set
        is swept rather than the instances.

        The predicate is read off this file: a placement site is recognised by calling
        _binds_locally or _bound_names, the two helpers that answer what a statement or a scope
        binds. It honours the rule if it consults _bound_names, which applies the declaration
        itself, or reads BOTH ast.Global and ast.Nonlocal in its own right. Both, because
        honouring one was exactly the defect: the shadow table exempted global and not nonlocal,
        and a predicate satisfied by either would have passed that site unchanged.

        COVERAGE, not correctness, and the distinction matters more here than above: that a
        site consults the declaration says nothing about the declaration being resolved to the
        right scope. The answers are held by the paired cases in RESOLVES_LIKE_PYTHON, whose
        expectations come from Python rather than from this reader.

        The boundary is measured: _places decides placement for a comprehension's walrus
        target without calling either helper, so this predicate does not recognise it. That
        absence is asserted, because a derived list that cannot say what it misses is the
        defect this module was opened against.
        """
        source = HERE.read_text(encoding="utf-8")
        parsed = ast.parse(source)
        owner = {}
        for top in parsed.body:
            if isinstance(top, (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef)):
                for inner in ast.walk(top):
                    owner[id(inner)] = top.name
        places_a_name, reads_global, reads_nonlocal = {}, set(), set()
        for node in ast.walk(parsed):
            named = getattr(getattr(node, "func", None), "id", None)
            here = owner.get(id(node), MODULE_LEVEL)
            if named in ("_binds_locally", "_bound_names"):
                places_a_name.setdefault(here, []).append(node.lineno)
            if named == "_bound_names":
                reads_global.add(here)
                reads_nonlocal.add(here)
            if isinstance(node, ast.Attribute) and node.attr == "Global":
                reads_global.add(here)
            if isinstance(node, ast.Attribute) and node.attr == "Nonlocal":
                reads_nonlocal.add(here)
        asks_where_it_lives = reads_global & reads_nonlocal
        self.assertTrue(places_a_name,
                        "no placement site was recognised at all, so this check would pass by"
                        " sweeping nothing rather than by finding nothing")
        # The comparison itself, shown rather than asserted away.
        without = sorted(set(places_a_name) - asks_where_it_lives)
        undeclared = sorted(set(without) - set(PLACES_A_NAME_WITHOUT_ASKING_WHERE_IT_LIVES))
        self.assertEqual(undeclared, [],
                         "a site records which scope binds a name without honouring a global or"
                         " nonlocal declaration, and nobody wrote down why that is safe there: "
                         + json.dumps({name: places_a_name[name] for name in undeclared}))
        stale = sorted(set(PLACES_A_NAME_WITHOUT_ASKING_WHERE_IT_LIVES) - set(without))
        self.assertEqual(stale, [],
                         "declared as placing a name without asking where it lives, but it asks"
                         " now, so delete the entry: " + json.dumps(stale))
        for name, why in sorted(PLACES_A_NAME_WITHOUT_ASKING_WHERE_IT_LIVES.items()):
            with self.subTest(name):
                self.assertTrue(why.strip(), name + " is declared without a reason")
        # The boundary, exercised. _places moves a comprehension's walrus out to the scope
        # around it, which is a placement decision, and it reaches neither helper -- so the
        # predicate cannot see it and this list does not cover it.
        self.assertNotIn("_places", set(places_a_name),
                         "_places decides placement without calling either helper, so the"
                         " predicate should not see it -- if it does, the boundary written"
                         " above is no longer true and the wording has to change")

    def test_no_class_body_binding_shadows_without_asking_whether_it_happened(self):
        """The certainty rule, swept over every place that decides it rather than one branch.

        A name bound under a branch that may not run does not shadow, so a body that really does
        reach the enclosing name keeps reaching it. Getting that wrong loses the place: the
        inventory then accepts something that concludes from source text and was never declared,
        which is a miss rather than a narrow claim.

        The sweep predicate is every binding yielded in bound_in, and the requirement is that a
        certainty test guards it. Both are read off bound_in's own text, keyed by the node kinds
        its branch tests, so a branch added later without asking fails here. A declared
        exception that stops being unguarded fails too rather than sitting on the page.
        """
        source = HERE.read_text(encoding="utf-8")
        deciding = [node for node in ast.walk(ast.parse(source))
                    if isinstance(node, ast.FunctionDef) and node.name == "bound_in"]
        self.assertEqual(len(deciding), 1,
                         "the certainty rule is decided in bound_in, and this file no longer"
                         " has exactly one of those to sweep")
        above = {}
        for parent in ast.walk(deciding[0]):
            for child in ast.iter_child_nodes(parent):
                above[id(child)] = parent
        asking, yielded = {}, 0
        for node in ast.walk(deciding[0]):
            if not isinstance(node, ast.Yield) or node.value is None:
                continue
            yielded += 1
            guarded, kinds, here = False, (), node
            while id(here) in above:
                parent = above[id(here)]
                if isinstance(parent, ast.If):
                    if (any(isinstance(inner, ast.Name) and inner.id == "certain"
                            for inner in ast.walk(parent.test))
                            and any(here is step or any(here is inner
                                                        for inner in ast.walk(step))
                                    for step in parent.body)):
                        guarded = True
                    named = tuple(sorted(
                        inner.attr for inner in ast.walk(parent.test)
                        if isinstance(inner, ast.Attribute)
                        and isinstance(inner.value, ast.Name) and inner.value.id == "ast"))
                    if named and not kinds:
                        kinds = named
                here = parent
            if not guarded:
                asking[", ".join(kinds) or "a binding under no branch at all"] = node.lineno
        self.assertTrue(yielded,
                        "no binding is yielded in bound_in at all, so this check would pass by"
                        " sweeping nothing rather than by finding nothing")
        undeclared = sorted(set(asking) - set(BINDS_WITHOUT_ASKING_IF_IT_HAPPENED))
        self.assertEqual(undeclared, [],
                         "a class-body binding is treated as definite without asking whether it"
                         " happened, and nobody wrote down why it may: " + json.dumps(undeclared))
        stale = sorted(set(BINDS_WITHOUT_ASKING_IF_IT_HAPPENED) - set(asking))
        self.assertEqual(stale, [],
                         "declared as binding without asking, but it asks now, so delete the"
                         " entry: " + json.dumps(stale))
        for kinds, why in sorted(BINDS_WITHOUT_ASKING_IF_IT_HAPPENED.items()):
            with self.subTest(kinds):
                self.assertTrue(why.strip(), kinds + " is declared without a reason")

    def test_each_binding_fixpoint_here_halts_on_a_name_bound_twice(self):
        """The derivations halt, measured on the input that makes them spin.

        A loop that writes its answer and runs again while the answer differs is not a
        fixpoint. Two classes bound to one name flip it forever, so the module hangs rather
        than answering narrowly -- and a derivation that does not halt is worse than one that
        answers less, because nothing is reported at all. Both tables now grow the SET of what
        a name has ever been bound to, and a set only gains members, so each is bounded by the
        number of distinct classes.

        Measured rather than argued, and measured as a failure rather than as a hang: each
        derivation runs under a real-time bound on the input that used to spin, and one that
        does not come back fails here with the reason.
        """
        spun = ("class First:", "    pass", "", "class Second:", "    pass", "",
                "Alias = First", "Alias = Second", "", "def consumer():",
                "    holder = First()", "    holder = Second()", "    return holder")
        tree = ast.parse("\n".join(spun))
        for name, derive in (("_class_aliases", _class_aliases),
                             ("_instance_classes", _instance_classes)):
            with self.subTest(name):
                def rang(_number, _frame):
                    raise TimeoutError(name)
                before = signal.signal(signal.SIGALRM, rang)
                signal.setitimer(signal.ITIMER_REAL, 10.0)
                try:
                    derive(tree)
                except TimeoutError:
                    self.fail(name + ": did not halt on a name bound to two classes, so it"
                              " writes its answer and runs again rather than growing a set that"
                              " can only gain members")
                finally:
                    signal.setitimer(signal.ITIMER_REAL, 0)
                    signal.signal(signal.SIGALRM, before)

    def test_every_value_follower_here_reads_the_whole_pass_through_vocabulary(self):
        """Support: the reach over value forms, derived from this file and then measured.

        Not a regression test for one defect. It is the property the issue asks for, applied to
        the one thing this reader kept getting wrong: a resolver that follows a value either
        reads the pass-through forms or does not, and reading ast.Name alone is a narrower reach
        than the sentence beside it claims.

        The vocabulary is taken from _passed_through's own text and the coverage from each
        resolver's, so neither is remembered. A resolver taking values from _bindings without
        covering the vocabulary -- directly, or through _passed_through or _reachable -- is
        emitted and must be declared. Every declared entry is then RUN: the place it names has
        to stay unreported, so an entry that gets covered later fails here and asks to be
        deleted, and one that was never real fails immediately.
        """
        source = HERE.read_text(encoding="utf-8")
        parsed = ast.parse(source)

        def forms_named(where):
            """Which ast node types these nodes name, read off their own text."""
            return {inner.attr for node in where for inner in ast.walk(node)
                    if isinstance(inner, ast.Attribute)
                    and isinstance(inner.value, ast.Name) and inner.value.id == "ast"}

        written_as = {node.name: node for node in parsed.body
                      if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef))}
        self.assertIn("_passed_through", written_as,
                      "the vocabulary is derived from _passed_through, which is no longer here")
        vocabulary = forms_named([written_as["_passed_through"]]) - {"Name"}
        self.assertTrue(vocabulary,
                        "the pass-through vocabulary derived as empty, so this check would pass"
                        " by seeing nothing rather than by finding nothing")
        # Asked of each binding loop rather than of the function around it. A resolver like
        # _hands_on is many resolvers in one definition, and one of them reading ast.Name alone
        # is invisible if the whole function is excused because some other part of it follows a
        # value properly. That coarseness is the same fallacy one level up.
        owner = {}
        for top in parsed.body:
            if isinstance(top, (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef)):
                for inner in ast.walk(top):
                    owner[id(inner)] = top.name
        narrow = {}
        for node in ast.walk(parsed):
            if not isinstance(node, ast.For):
                continue
            # A loop that takes a binding's value, however it takes it: straight from
            # _bindings, or by unpacking what _assigned answered. Anchoring on the first shape
            # alone is how an opener alias written with a branch reached a reviewer before it
            # reached this check -- the loop was a value follower and this sweep did not see it.
            spelled = []
            if (isinstance(node.iter, ast.Call)
                    and getattr(node.iter.func, "id", None) == "_bindings"):
                spelled = [part.id for part in getattr(node.target, "elts", [])
                           if isinstance(part, ast.Name)]
            else:
                for statement in ast.walk(node):
                    if (isinstance(statement, ast.Assign)
                            and isinstance(statement.targets[0], ast.Tuple)
                            and getattr(statement.value, "id", None) == "binding"):
                        spelled = [part.id for part in statement.targets[0].elts
                                   if isinstance(part, ast.Name)]
                        break
            if not spelled:
                continue
            value = spelled[1] if len(spelled) > 1 else spelled[0]
            decided, routed = set(), False
            for call in (inner for statement in node.body for inner in ast.walk(statement)
                         if isinstance(inner, ast.Call)):
                named = getattr(call.func, "id", None)
                first = getattr(call.args[0], "id", None) if call.args else None
                if named == "_passed_through" and first == value:
                    routed = True
                if named == "_dotted" and first == value:
                    # Deciding the form by asking for a dotted spelling is deciding it: _dotted
                    # answers None for every pass-through form, so a loop that reads only its
                    # answer sees none of them.
                    decided |= {"Name", "Attribute"}
                if named != "isinstance" or first != value:
                    continue
                decided |= forms_named(call.args[1:])
            # A loop that hands the value to a helper is not deciding its reach; one that tests
            # the value's type itself is, and what it does not test it cannot see.
            if routed or not decided:
                continue
            if vocabulary - decided:
                narrow.setdefault(owner.get(id(node), MODULE_LEVEL), set()).update(
                    vocabulary - decided)
        narrow = {name: sorted(missing) for name, missing in narrow.items()}
        undeclared = sorted(set(narrow) - set(FOLLOWS_FEWER_VALUE_FORMS))
        self.assertEqual(undeclared, [],
                         "a resolver here follows values through fewer forms than the"
                         " vocabulary and nobody wrote down which: "
                         + json.dumps({name: narrow[name] for name in undeclared}))
        stale = sorted(set(FOLLOWS_FEWER_VALUE_FORMS) - set(narrow))
        self.assertEqual(stale, [],
                         "declared as reading fewer forms, but it no longer does, so delete the"
                         " entry: " + json.dumps(stale))
        for name, (lines, place, why) in sorted(FOLLOWS_FEWER_VALUE_FORMS.items()):
            with self.subTest(name):
                self.assertTrue(why.strip(), name + " is declared without a reason")
                self.assertNotIn(place, places_reached(REFUSAL, "\n".join(lines)),
                                 name + ": the declared blind spot is inside the reach now, so"
                                 " delete the entry rather than leave it claiming a limitation"
                                 " that has gone")

    def test_no_reader_here_sees_only_the_unannotated_binding(self):
        """Support: a drift guard, derived from this file rather than written as a list.

        Not a regression test for any one defect. It is the shape four of them shared: a table
        reading ast.Assign alone while the table beside it reads both, so that
        `carrier: property = property(getter)` binds for one and not the other. The set is
        computed from this file's own text, so a new reader of the bare form fails here instead
        of arriving later as a finding, and a declared exception that stops being true fails
        too rather than sitting on the page claiming something that has moved.
        """
        source = HERE.read_text(encoding="utf-8")
        bare = set()
        for node in ast.parse(source).body:
            if not isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef, ast.ClassDef)):
                continue
            written = ast.get_source_segment(source, node) or ""
            if "ast.Assign" not in written:
                continue
            if "ast.AnnAssign" in written or "_assigned(" in written:
                continue
            bare.add(node.name)
        undeclared = sorted(bare - set(ASSIGN_ONLY_ON_PURPOSE))
        self.assertEqual(undeclared, [],
                         "a reader here sees only the unannotated binding and nobody wrote down"
                         " why it may: " + json.dumps(undeclared))
        stale = sorted(set(ASSIGN_ONLY_ON_PURPOSE) - bare)
        self.assertEqual(stale, [],
                         "declared as reading only the bare binding, but it no longer does, so"
                         " delete the entry: " + json.dumps(stale))

    def test_no_two_places_this_module_declares_can_share_a_name(self):
        """Support: the declarations are keyed by name, so two places sharing one would merge.

        Counted from the parsed file, which keeps both, rather than from _functions_here(), whose
        set has already discarded the second by the time anyone could look. The names counted are
        the ones the derivation computes, so a nested helper is kept apart by the chain around it
        while two classes with a method of the same name collide here, which is the case a flat
        key cannot survive.
        """
        tree = ast.parse(HERE.read_text(encoding="utf-8"))
        places = _places(tree)
        named = [places[id(node)][0] for node in ast.walk(tree)
                 if isinstance(node, (ast.FunctionDef, ast.AsyncFunctionDef, ast.Lambda))]
        repeated = sorted({name for name in named if named.count(name) > 1})
        self.assertEqual(repeated, [],
                         "two places share a name, so one declaration now speaks for both: "
                         + json.dumps(repeated))

    def test_every_reading_declares_the_path_it_travels(self):
        """Reaching a success answer and reaching it down the right path are two questions.

        A reading can pass through a source-level shortcut and answer exactly as it would have
        through the installed thing -- that is what makes the substitution quiet. This issue
        delivers an acceptance test for installation and hook REGISTRATION, so a row that skips
        the registration is not a residual detail, it is the claim missing its subject.

        And the path is only half of it. A row can travel the registered command and still be
        reading a second Codex home this suite made for it, which composes readings about two
        machines and looks exactly like a composition. So each row names the host it looks at
        as well as the path it takes.

        The paths are not derivable from the source: what an executed command reaches is a fact
        about the run, not about the call graph. So each is declared, and what this check holds
        is that no row is silent about which one it travels.
        """
        self.assertEqual(sorted(READING_PATHS), sorted(SEVEN),
                         "a reading that does not say which path it travels can move onto a"
                         " shortcut without anything here changing")
        for cell, path in READING_PATHS.items():
            with self.subTest(cell):
                self.assertTrue(path.strip(), cell + " declares no path")
                self.assertTrue(path.startswith(("installed,", "registered,", "stand-in,")),
                                cell + " names a path this suite has no word for: " + path[:40])
                self.assertIn("on the run's own", path,
                              cell + " names a path but not the host it reads, and a row can"
                              " travel the right path on the wrong machine")
    def test_a_row_that_cannot_require_success_says_why_absence_is_normal(self):
        """The only exception, and it has to carry its reason.

        Absence is a real state in this repository and an answer set has to be able to express
        it. What it must not become is a place to file a row that simply never succeeds, so a
        conditional row names the condition rather than the fact that it is conditional.
        """
        self.assertEqual(set(CONDITIONAL_SUCCESS) - set(SEVEN), set(),
                         "a condition is declared for a question nobody asks")
        for cell, why in CONDITIONAL_SUCCESS.items():
            with self.subTest(cell):
                self.assertTrue(why.strip(), cell + " is excepted without a reason")
                self.assertIn(cell, SUCCESS_ANSWERS,
                              cell + " has no success answer to fall back from")
    def test_each_row_reads_its_own_path_and_no_other(self):
        """The property, stated as a change rather than as a declaration.

        One reading is moved at a time and the whole table is rebuilt. Exactly the row that
        declared that path may move with it. Any other row that moves is a row reading something
        it did not declare, which is the borrowed answer this table exists to prevent -- and no
        amount of careful declaration would have caught it.
        """
        sentinel = "a-value-no-reading-produces"
        with tempfile.TemporaryDirectory() as temporary:
            payloads, _host = observe_all(Path(temporary))
        baseline = table(payloads)

        for cell in SEVEN:
            _declared, source, path, _producer = _row(cell)
            with self.subTest(cell):
                moved = copy.deepcopy(payloads)
                target = moved[source]
                for key in path[:-1]:
                    target = target[key]
                target[path[-1]] = sentinel
                rebuilt = table(moved)
                self.assertEqual(rebuilt[cell]["value"], sentinel,
                                 cell + ": the row does not read the path it declared")
                for other in SEVEN:
                    if other == cell:
                        continue
                    self.assertEqual(rebuilt[other], baseline[other],
                                     other + " moved when only " + cell + " was changed")

    def test_withholding_a_source_leaves_only_its_own_rows_unreadable(self):
        """A command that did not run and a command that answered are never the same row."""
        with tempfile.TemporaryDirectory() as temporary:
            payloads, _host = observe_all(Path(temporary))
        baseline = table(payloads)

        for source in ("diagnose", "hook-status", ACCEPTANCE):
            with self.subTest(source):
                withheld = {k: v for k, v in payloads.items() if k != source}
                rows = table(withheld)
                for cell in SEVEN:
                    if _row(cell)[1] == source:
                        self.assertEqual(rows[cell]["value"], reading.UNREADABLE,
                                         cell + ": a reading that was not made is unreadable,"
                                         " never False and never absent")
                        self.assertFalse(rows[cell]["readable"])
                    else:
                        self.assertEqual(rows[cell], baseline[cell],
                                         cell + " lost its answer when another source did not"
                                         " run, so it was never answering on its own")

    def test_the_hook_row_counts_an_invocation_that_really_happened(self):
        """Criterion 3 asks for a real callback, and a file count is not one.

        The journal cell counts records. A directory somebody pre-populated would answer the
        same way, so the row is established by the transition: the journal is absent before, and
        names exactly one invocation after the Stop hook has actually run. Absent is an answer
        here, not a failure to look.

        And the invocation goes through the registration, not through this checkout's helper.
        The command is read out of the hook file the installer wrote and executed as a program,
        so the entry point, the settings argument it carries and the stdin contract are all on
        the path the row reports. Calling completion.run() here would have left every one of
        them untested while the row still reached "1".
        """
        with tempfile.TemporaryDirectory() as temporary:
            before, after, registered, done = fire_the_hook(Path(temporary))

        self.assertEqual(len(registered), 1,
                         "the installer registered " + str(len(registered)) + " commands for"
                         " this adapter, so what fired is not settled")
        self.assertIn(completion.ENTRY_POINT_NAME, registered[0]["command"],
                      "the registered command names the adapter entry point")
        self.assertIsNotNone(registered[0]["settings"],
                             "the registration carries the settings path the install chose,"
                             " rather than leaving it to be resolved again at every Stop")
        self.assertEqual(done.returncode, 0, done.stderr[-400:])
        self.assertEqual(done.stdout, b"", "an observing hook holds no turn")

        self.assertEqual(read("hookCallback", {"hook-status": before})["value"]["value"],
                         reading.ABSENT,
                         "before any invocation the journal is established absent")
        self.assertEqual(read("hookCallback", {"hook-status": after})["value"]["value"], "1",
                         "and afterwards it names the one invocation that happened")

    def test_the_model_and_permission_row_is_not_taken_from_a_neighbouring_verdict(self):
        """The row most likely to be filled by something that sounds close enough.

        settingsPreserved answers whether a command that writes nothing left config.toml alone.
        It is true of a diagnosis no matter what the model keys say, so it cannot answer this.
        The case changes the model and permission keys underneath and requires this row to move
        while that verdict does not.
        """
        earlier = MODEL_PERMISSION_BLOCK
        later = earlier.replace('approval_policy = "never"', 'approval_policy = "on-request"')
        # Through read() like every other cell: the claim is about the row, not about the
        # helper, and a row that only its own test can reach is not the row the table fills.
        moved = read("modelPermissionPreservation",
                     {ACCEPTANCE: _model_permission_delta(earlier, later)})
        still = read("modelPermissionPreservation",
                     {ACCEPTANCE: _model_permission_delta(earlier, earlier)})

        if not HAS_READER:
            self.assertEqual(moved["value"], reading.UNREADABLE)
            self.assertEqual(still["value"], reading.UNREADABLE)
            return
        self.assertEqual(still["value"], PRESERVED)
        self.assertEqual(moved["value"], CHANGED,
                         "a permission posture that moved is not a preserved one")



    def test_without_a_reader_the_row_reports_itself_unread_rather_than_preserved(self):
        """Checked on both jobs rather than only on the one where it bites.

        The supported floor has no TOML reader, so on that job this row cannot be taken. The
        failure worth preventing is not the missing reader: it is a row that reports itself
        readable anyway, which would be visible on exactly one interpreter and green on the
        other. Simulating the absence here states the property on both.
        """
        with mock.patch.object(sys.modules[__name__], "HAS_READER", False):
            row = read("modelPermissionPreservation",
                       {ACCEPTANCE: _model_permission_delta(MODEL_PERMISSION_BLOCK,
                                                            MODEL_PERMISSION_BLOCK)})

        self.assertFalse(row["readable"])
        self.assertEqual(row["value"], reading.UNREADABLE)
        self.assertNotEqual(row["value"], PRESERVED,
                            "two configurations that happen to be identical are still not a"
                            " preservation anybody read")
        self.assertIn("tomllib", row["detail"])
    def test_changing_one_condition_moves_only_the_reading_that_asked_about_it(self):
        """Independence at the input, which the accessor cases cannot reach.

        Moving a value inside a payload proves this table reads the path it declared. It does
        not prove the command computed those values separately: a diagnosis deriving exposure
        from the import result would still write both keys, and every accessor case would stay
        green. Varying an input is not enough either if only the EVIDENCE moves -- a verdict
        copied from a neighbour survives that too.

        So each direction moves a judgement. Registering the bridge and observing its identity
        tool takes exposure from not_verified to verified; asking for a trial takes delivery
        from not_applicable to not_verified. In each direction the answers that were not asked
        about must come back identical in value AND evidence, which a borrowed verdict cannot do.

        The exposure direction needs a configuration reader, because reaching verified means
        comparing the registered command, and the supported floor has none. That direction is
        therefore taken only where it can be taken. The delivery direction needs no reader and
        runs everywhere, so the orthogonality claim is exercised on both jobs rather than only
        on the newer one -- and where exposure cannot rise, the case still requires it to hold
        still while delivery moves.
        """
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            _payloads, host = observe_all(root)
            bridge = base.component_of_for_test(host.data, runtime_install.MCP_NAME)
            registered = str(host.pointer_path / "bin" / bridge["consoleScript"])
            plain = diagnose_for(root, host)
            exposed = diagnose_for(root, host, "--bridge-command", registered,
                                   "--observed-tool", "get_capabilities")
            tried = diagnose_for(root, host, "--relay-command",
                                 str(delivering_relay(root, doctor=False)),
                                 "--trial", *trial_inputs(root),
                                 # The copy INSIDE the candidate, not the working tree. The
                                 # preflight asks the relay's own reader and predicate, so it
                                 # needs the package importable -- and handing it the checkout
                                 # would answer a question about an installation with source
                                 # that was never installed.
                                 import_path=installed_components(host))
            reached = diagnose_for(root, host, "--relay-command",
                                   str(answering_relay(root)))
            # Last, because it rebuilds the supplied interpreter: the readings above are taken
            # against the runtime that cannot import, which is what makes the move a move.
            able = diagnose_for(root, host, import_path=importable_runtime(host, root))

        # The exact transition, not merely a different value: not_applicable becoming verified,
        # or a verdict falling to unknown, would satisfy "it moved" while meaning the opposite.
        #
        # All four move, each in a direction of its own, so every one of them is checked against
        # neighbours that are moving elsewhere. A cell that never moves cannot catch a neighbour
        # copying into it, and it cannot tell a live reading from a command that quietly stopped
        # attempting one: both look like the same answer forever.
        directions = [
            # The last field says which neighbours are compared on their evidence as well as
            # their value. A completing trial needs a runtime that has the relay in it, and the
            # import row reads where that same runtime finds things -- so that one direction
            # moves the relay's import EVIDENCE by construction while its verdict stays put.
            # Naming the coupling is honest; comparing evidence there anyway would be asserting
            # that two questions with a shared input never touch, which is not true.
            ("deliveryAcceptance", tried, "not_applicable", "verified", False),
            ("appServerConnection", reached, "unknown", "verified", True),
            ("runtimeImport", able, "not_verified", "verified", True),
        ]
        if HAS_READER:
            directions.append(("mcpToolExposure", exposed, "not_verified", "verified", True))
        else:
            without = read("mcpToolExposure", {"diagnose": exposed})["value"]
            self.assertEqual(without["value"], "not_verified",
                             "with no reader for the configuration the registered command cannot"
                             " be compared, so exposure stays unverified -- and it must stay so"
                             " for that reason rather than rise on a tool list alone")

        for moved_cell, varied, was_expected, now_expected, compare_evidence in directions:
            with self.subTest(moved_cell):
                before = read(moved_cell, {"diagnose": plain})["value"]
                after = read(moved_cell, {"diagnose": varied})["value"]
                self.assertEqual((before["value"], after["value"]),
                                 (was_expected, now_expected),
                                 moved_cell + ": the transition is the claim, and a verdict that"
                                 " moved somewhere else is not the claim being made")
                for other in CHECK_FIELD_ROWS:
                    if other == moved_cell:
                        continue
                    with self.subTest(other):
                        was = read(other, {"diagnose": plain})["value"]
                        now = read(other, {"diagnose": varied})["value"]
                        compared = ("value", "evidence") if compare_evidence else ("value",)
                        self.assertEqual(tuple(now[key] for key in compared),
                                         tuple(was[key] for key in compared),
                                         other + " moved when only " + moved_cell + " was asked"
                                         " about, so it is not answering on its own reading")
    def test_the_diagnosis_reads_nothing_outside_the_directory_it_was_given(self):
        """Pointing --state somewhere temporary is not isolation, so this checks the result.

        The survey runs discovery of its own and the filesystem side reads HOME and
        XDG_STATE_HOME, so a run that only redirected --state would still report a path on the
        host that ran it. Every path this diagnosis reports has to be inside the directory it
        was handed, which is also what keeps this suite away from an installed runtime.
        """
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            payloads, _host = observe_all(root)
        scope = payloads["diagnose"].get("scope") or {}
        reported = [scope.get("stateDirectory"), scope.get("databasePath"),
                    payloads["diagnose"].get("codexHome"),
                    (payloads["diagnose"].get("mcpRegistration") or {}).get("path")]

        for where in reported:
            if where is None:
                continue
            with self.subTest(where):
                self.assertTrue(inside(where, root),
                                where + " is outside the temporary directory")

    def test_the_diagnosis_does_not_import_or_run_what_the_host_has_installed(self):
        """The other way out, which redirecting paths does not close.

        Resolving where a module lives imports it, and the bridge smoke script starts a server,
        both under whatever interpreter the record names. Clearing PYTHONPATH and the user site
        does not reach a system site directory, and checking the resolved location afterwards is
        too late: by then the import has happened. So the assertion is on the interpreter, which
        is decided before any of it runs. Every component has to be asked through the runtime
        this suite supplied; if one is asked through the interpreter running these tests, the
        probe could reach this machine and that fails here whatever it happened to find.
        """
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary).resolve()
            payloads, host = observe_all(root)
            supplied = host.candidate / "bin" / "python"
            present = supplied.is_file()
            answers, refusal = hermetic_answers(supplied, root) if present else (None, "absent")
            # The console scripts are run as programs by the survey, so their first line is what
            # decides the interpreter for those invocations. The record does not govern that
            # one: a shebang changed back to this machine's interpreter would leave every
            # recorded path reading correctly while the survey ran outside the supplied runtime.
            shebangs = {c["consoleScript"]:
                        (host.candidate / "bin" / c["consoleScript"]).read_text(
                            encoding="utf-8").splitlines()[0]
                        for c in host.data["components"]}
        components = payloads["diagnose"].get("components") or {}

        self.assertTrue(components, "the diagnosis reported no component at all")
        self.assertTrue(present, "the supplied runtime is not there at all")
        self.assertIsNone(refusal, str(refusal))
        for flag, wanted in HERMETIC_FLAGS.items():
            self.assertEqual(answers.get(flag), wanted,
                             "the supplied interpreter answered " + repr(answers.get(flag))
                             + " for " + flag + ", so it is not hermetic and every path this"
                             " case reads could be right while the probe ran with this"
                             " machine's site directory and import path")
        self.assertEqual(answers.get("smuggled"), [],
                         "an import path planted in the environment reached the interpreter the"
                         " probes run, which is the thing the flags were supposed to prevent")
        for script, first in shebangs.items():
            with self.subTest(script):
                self.assertEqual(first, "#!" + str(supplied),
                                 script + " is run as a program, and its first line sends it to "
                                 + first + " rather than to the supplied runtime")
        for name, component in components.items():
            with self.subTest(name):
                asked = component.get("interpreterPath")
                self.assertIsNotNone(asked, name + ": no interpreter was named at all")
                self.assertEqual(Path(asked), supplied,
                                 name + " was probed through " + str(asked) + " rather than the"
                                 " runtime this suite supplied")
                self.assertNotEqual(Path(asked), Path(sys.executable).resolve())
                where = component.get("importedLocation")
                if where is None:
                    continue
                self.assertTrue(inside(where, root),
                                name + " was imported from " + where + ", which is installed on"
                                " this host rather than in the destination under test")


class DistinctionTests(unittest.TestCase):
    """Five conditions that must not collapse into each other.

    Each is read by the thing that answers it, and the case asserts that the five answers are
    five different values rather than five spellings of "something is wrong". A host told only
    that an install refused cannot tell whether it is looking at somebody else runtime, a
    registration nobody wrote, a version that moved, a configuration already claimed, or a store
    it could not open -- and those five need five different actions.
    """

    def signals(self, **overrides):
        declared = dict(entry_point_recorded=True, commit_matches=True, tree_matches=True,
                        working_tree_clean=True, digest_matches=True, has_point=True)
        declared.update(overrides)
        return ownership.Signals(**declared)

    def test_an_external_installation_and_a_fork_are_not_the_same_answer(self):
        """An entry point nothing recorded is somebody else. A dirty checkout is ours, changed."""
        foreign = ownership.classify(self.signals(entry_point_recorded=False))[0]
        fork = ownership.classify(self.signals(working_tree_clean=False))[0]

        self.assertEqual(foreign, ownership.FOREIGN)
        self.assertEqual(fork, ownership.FORK)
        self.assertNotEqual(foreign, fork)
        self.assertFalse(ownership.reusable(foreign))
        self.assertFalse(ownership.reusable(fork))

    @base.needs_reader
    def test_a_missing_registration_and_a_claimed_one_are_not_the_same_answer(self):
        """Not registered and registered to somebody else are two answers, not one refusal.

        What "not registered" is called depends on what was asked. Asked with a command, the
        reading answers what registering would do -- CREATED, and it says so by also answering
        that it would write. Asked without one, there is nothing to compare and the answer is
        the absence itself. Neither is the answer a configuration already naming another
        command gives, and that is the distinction a host acts on.
        """
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            home.joinpath("config.toml").write_text(FOREIGN_TABLE, encoding="utf-8")
            missing = runtime_install.registration_state(home, "/opt/bridge", [])
            unasked = runtime_install.registration_state(home, None, [])
            home.joinpath("config.toml").write_text(
                FOREIGN_TABLE + "\n[mcp_servers." + runtime_install.MCP_NAME + "]\n"
                'command = "/somebody/elses/bridge"\n', encoding="utf-8")
            claimed = runtime_install.registration_state(home, "/opt/bridge", [])

        self.assertEqual(missing["outcome"], codexconfig.CREATED,
                         "nothing is registered, so registering would create one")
        self.assertTrue(missing["wouldWrite"],
                        "and the reading says it would write, which is how CREATED is told"
                        " apart from a registration that is already there")
        self.assertEqual(unasked["outcome"], "ABSENT",
                         "asked without a command there is nothing to compare, and the answer"
                         " is the absence rather than a plan")
        self.assertEqual(claimed["outcome"], codexconfig.CONFLICT,
                         "a registration naming another command is a conflict, not an absence")
        self.assertFalse(claimed["wouldWrite"],
                         "a conflict is not something this command writes through")
        self.assertEqual(len({missing["outcome"], unasked["outcome"], claimed["outcome"]}), 3,
                         "three readings, three answers")

    def test_a_version_that_moved_is_not_a_source_that_moved(self):
        """The two drift findings a reader would otherwise have to tell apart by guessing."""
        self.assertEqual(definition.verify(ROOT), [],
                         "the committed definition describes this checkout, or nothing below"
                         " is a controlled comparison")

        moved = definition.load()
        moved["components"][0]["version"] = "0.0.0-never-released"
        findings = definition.verify(ROOT, definition=moved)

        self.assertTrue(any("version recorded" in finding for finding in findings), findings)
        self.assertFalse(any("sourceDigest" in finding for finding in findings),
                         "only the version moved, so only the version may be reported: a digest"
                         " finding here would mean the two conditions were read as one")

    def test_a_store_that_is_not_there_is_not_a_store_nobody_could_read(self):
        """The distinction the clean-host install depends on, kept here as a distinction."""
        schema = {"relationships": "CREATE TABLE relationships (relationship_id TEXT)"}
        candidate = {"readable": True, "tables": dict(schema)}
        absent = swapgate.tables_cell(
            {"readable": True, "present": False, "dbPath": "/nowhere/relay.sqlite3"}, candidate)
        unopenable = swapgate.tables_cell(
            {"readable": False, "present": None, "detail": "permission denied"}, candidate)

        self.assertEqual(absent["answer"], swapgate.NO_STORE)
        self.assertTrue(absent["readable"], "absence is an answer that was read")
        self.assertFalse(unopenable["readable"],
                         "a store that could not be opened established nothing")
        self.assertNotEqual(absent["answer"], unopenable["answer"])

    def test_the_five_conditions_are_five_different_answers(self):
        """Stated once, over all five, so that two of them collapsing is a failure here."""
        schema = {"relationships": "CREATE TABLE relationships (relationship_id TEXT)"}
        answers = {
            "external": ownership.classify(self.signals(entry_point_recorded=False))[0],
            "fork": ownership.classify(self.signals(working_tree_clean=False))[0],
            "configuration claimed": ownership.classify(
                self.signals(registration_conflict="registered with another command"))[0],
            "store unreadable": swapgate.tables_cell(
                {"readable": False, "present": None}, {"readable": True, "tables": schema}
            )["answer"],
            "store absent": swapgate.tables_cell(
                {"readable": True, "present": False}, {"readable": True, "tables": schema}
            )["answer"],
        }
        self.assertEqual(len(set(answers.values())), len(answers),
                         "two conditions answering the same value cannot be told apart: "
                         + json.dumps(answers))

    @base.needs_reader
    def test_a_registration_that_writes_preserves_every_other_setting(self):
        """The half a classifier call cannot establish: a command that actually writes."""
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary) / "codex"
            home.mkdir()
            original = MODEL_PERMISSION_BLOCK + "\n" + FOREIGN_TABLE
            (home / "config.toml").write_text(original, encoding="utf-8")
            arguments = ["register-mcp", "--codex-home", str(home),
                         "--bridge-command", "/opt/bridge", "--apply"]
            first = base.run(*arguments)
            after_first = (home / "config.toml").read_text(encoding="utf-8")
            second = base.run(*arguments)
            after_second = (home / "config.toml").read_text(encoding="utf-8")

        self.assertEqual(first.returncode, 0, first.stdout + first.stderr)
        self.assertEqual(json.loads(first.stdout)["outcome"], "CREATED")
        self.assertTrue(after_first.startswith(original),
                        "the settings it was handed are still there, unchanged, first")
        self.assertEqual(json.loads(second.stdout)["outcome"], "LINKED",
                         "a second registration recognises its own rather than appending")
        self.assertEqual(after_second, after_first,
                         "and writes nothing the second time")

    def test_a_hook_installation_leaves_a_hook_that_was_already_there(self):
        """Runtime hooks live in their own file, which no other snapshot in this suite reads."""
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            (home / "hooks.json").write_text(json.dumps(FOREIGN_HOOKS), encoding="utf-8")
            arguments = argparse_namespace = base.argparse.Namespace(
                codex_home=str(home), event=None, hook_command=None, adapter="completion",
                dest=None, relay_command=str(home / "codex-session-relay"),
                marker_root=str(home / "marker"), db_path=None,
                journal_root=str(home / "journal"), python=sys.executable,
                mode=completion.OBSERVE, guard_timeout=5, timeout=10, issue="CRW-69",
                apply=True)
            with mock.patch.object(runtime_install, "emit"):
                runtime_install.cmd_hook(arguments)
            document = json.loads((home / "hooks.json").read_text(encoding="utf-8"))

        groups = document["hooks"]["Stop"]
        commands = [entry.get("command")
                    for group in groups for entry in group.get("hooks", [])]
        self.assertIn("/opt/cxc/stop", commands,
                      "the hook that was already registered is still registered")
        self.assertGreater(len(commands), 1, "and the new one was appended beside it")


class DaemonStateTests(unittest.TestCase):
    """A daemon that was running, one that was not, and a handover that was in progress.

    The swap is refused while a daemon is running and while attempts are open, and those are two
    different readings that happen to produce the same refusal. Reading one from the other would
    be invisible, because both stop the update. So each case asserts the cell that answered, and
    the case that could not read the daemon at all asserts the third answer: a reading that
    failed is not a daemon that is running, and it is not one that is stopped either.

    Nothing in this class starts, stops, enables or registers anything. The installer does not
    either, which is why a successful install is never reported as an activation.
    """

    def _update_with(self, **injected):
        with tempfile.TemporaryDirectory() as temporary:
            host = base._Host(temporary)
            clean(host)
            install(host)
            installed = host.candidate
            settled = host.snapshot()
            arriving_source(host)
            loaded, verified = updating(host)
            with loaded, verified:
                code, payload = install(host, **injected)
            after = host.snapshot()
            reached = pointer.read(host.pointer_path).get("target")
        return code, payload, settled, after, installed, reached

    def test_a_daemon_that_was_not_running_does_not_stop_the_update(self):
        code, payload, _settled, _after, _installed, reached = self._update_with()
        cells = (payload.get("swapGate") or {}).get("cells") or {}

        self.assertEqual(code, 0, json.dumps(payload)[:800])
        self.assertTrue(payload["promoted"])
        self.assertEqual(cells["daemon"]["answer"], scope.STOPPED)
        self.assertEqual(reached, str(payload["environment"]),
                         "the update that was allowed to happen did happen")

    def test_a_daemon_that_was_running_stops_it_and_keeps_what_was_there(self):
        code, payload, settled, after, installed, reached = self._update_with(
            gate="running daemon")
        cells = (payload.get("swapGate") or {}).get("cells") or {}

        self.assertEqual(code, 1)
        self.assertEqual(payload["failedStep"], GATE_REFUSAL)
        self.assertEqual(payload["swapGate"]["verdict"], swapgate.BLOCKED)
        self.assertEqual(cells["daemon"]["answer"], scope.RUNNING)
        self.assertEqual(cells["inFlight"]["answer"], swapgate.NO_ATTEMPTS,
                         "the running daemon refused this, and the in-flight cell answered its"
                         " own question rather than agreeing with it")
        self.assertEqual(after, settled)
        self.assertEqual(reached, str(installed))

    def test_a_handover_in_flight_is_preserved_rather_than_swapped_under(self):
        code, payload, settled, after, installed, reached = self._update_with(
            gate="handover in flight")
        cells = (payload.get("swapGate") or {}).get("cells") or {}

        self.assertEqual(code, 1)
        self.assertEqual(payload["swapGate"]["verdict"], swapgate.BLOCKED)
        self.assertEqual(cells["inFlight"]["answer"], 3,
                         "the open attempts are counted, not rounded to a refusal")
        self.assertEqual(cells["daemon"]["answer"], scope.STOPPED,
                         "and the daemon cell still answers its own question")
        self.assertEqual(after["storeRows"], settled["storeRows"],
                         "the attempts that were in flight are still in the store")
        self.assertEqual(after["storeInode"], settled["storeInode"])
        self.assertEqual(reached, str(installed))

    def test_a_daemon_that_could_not_be_read_is_neither_running_nor_stopped(self):
        code, payload, settled, after, _installed, _reached = self._update_with(
            gate="unreadable daemon")
        cells = (payload.get("swapGate") or {}).get("cells") or {}

        self.assertEqual(code, 1)
        self.assertEqual(payload["swapGate"]["verdict"], swapgate.UNESTABLISHED,
                         "a question that could not be answered did not answer no")
        self.assertEqual(cells["daemon"]["answer"], reading.ACCESS_ERROR)
        self.assertNotIn(cells["daemon"]["answer"], (scope.RUNNING, scope.STOPPED))
        self.assertFalse(cells["daemon"]["readable"])
        self.assertEqual(after, settled)

    def test_an_install_that_succeeded_is_not_reported_as_an_activation(self):
        """Installed, running and always-on are three claims, and this command makes one."""
        with tempfile.TemporaryDirectory() as temporary:
            payloads, _host = observe_all(Path(temporary))
        always = read("alwaysActive", payloads)

        self.assertTrue(always["readable"])
        self.assertEqual(always["value"]["value"], "not_verified",
                         "no supervised runtime was enabled and no host restart was observed,"
                         " so a successful install still does not establish this")
        self.assertNotEqual(always["value"]["value"], "verified")


# Every seam the update fixture stands in for, derived from the fixture rather than listed from
# memory: a stand-in added there has to be acknowledged here before this suite can describe what
# it exercised. That is the whole purpose of the declaration -- a provenance record that a later
# change can silently outgrow is worse than none.
FAKED_IN_FIXTURE = ("candidate_tables", "classify_component", "emit", "interpreter_version",
                    "load", "measure_candidate", "module_location", "names", "ops12_digest",
                    "place", "relay", "run", "store_presence", "store_tables", "verify")


def _stand_ins():
    """The names the update fixture replaces, observed while it is replacing them.

    Read out of the fixture's source this was a claim about how the fixture is written. A
    replacement applied some other way, or one applied conditionally and spelled differently,
    was simply not seen -- and the provenance record would then understate what stood in for a
    real thing while still looking derived.

    So the fixture is run and asked. _run calls interpose from inside the patched measurement,
    which is after every patch in that run has been entered, so the modules can be inspected
    for the attributes that are not themselves at that moment. The two pointer replacements are
    only applied for their own injected failures, so those runs are made too and the answers
    are taken together.
    """
    watched = ("subprocess", "definition", "scope", "pointer")
    found = set()

    def sample():
        for name, value in list(vars(runtime_install).items()):
            if isinstance(value, mock.NonCallableMock):
                found.add(name)
        for module_name in watched:
            module = getattr(runtime_install, module_name, None)
            for name, value in list(vars(module).items()) if module else []:
                if isinstance(value, mock.NonCallableMock):
                    found.add(name)

    for breaking in (None, "replace the owned pointer", "read the owned pointer back"):
        with tempfile.TemporaryDirectory() as temporary:
            host = base._Host(temporary)
            clean(host)
            # Inside the same context the composed lifecycle uses for its failed update. Run
            # without it, the sampling never sees the definition replacements that stage makes,
            # and the inventory would understate what stood in for a real thing while still
            # looking derived.
            loaded, verified = updating(host)
            with loaded, verified:
                install(host, breaking=breaking, interpose=sample)
    return found


# The switches the update fixture's run accepts, each with what handing it means here. DERIVED
# from the fixture's own signature, so a switch added there arrives unclassified rather than
# silently available.
#
# REFUSED_INJECTIONS names the one this module must never hand. clean_store tells the run the
# store is absent and its tables unknown, and the fixture has built a populated one at the same
# path -- so a regression that detects a store and then loses it passed, because the run under it
# had been told there was no store to lose.
REFUSED_INJECTIONS = ("clean_store",)
INJECTIONS = {
    "breaking": "a build step is made to fail at a named boundary, which is how every recovery"
                " claim here is exercised. It stands in for a build that really failed, and"
                " install's entry below says what that costs.",
    "gate": "a swap-gate reading is made to refuse: a running daemon, an open handover, a"
            " daemon nobody could read, or a schema the candidate would narrow.",
    "interpose": "a callback the fixture runs inside the patched measurement. It reads what is"
                 " patched at that moment and changes nothing.",
    "probes": "a list the store probe appends the interpreter it was asked through to. It"
              " records and changes nothing.",
    "dest": "the destination the run is invoked with, which is an argument the command really"
            " accepts and not a stand-in for anything. A retry after a failure can be aimed"
            " somewhere else, and that is the whole difference between the ordinary case and"
            " the one a pointer path recorded under an earlier destination is about.",
    "observe": "a list the stubbed classification appends what it was HANDED to: the"
               " registration and pointer readings it would have judged on. It records and"
               " changes nothing.",
    "issue": "the issue the run records as the evidence that it placed the owned pointer,"
             " which is an argument the command really accepts. It stands in for nothing: a"
             " blank one is a real invocation the command has to refuse rather than a state"
             " the fixture invents.",
    "clean_store": "REFUSED: it reports the store absent and empty to the run while the fixture"
                   " has built a populated one at that path, so what the run is told and what"
                   " the scenario built disagree.",
}

# What a function of this module hands the thing under test, and whether it is what this scenario
# built.
#
# Three findings were one finding wearing three faces -- a row read through a helper beside the
# registered command, a row read against a second Codex home, a run told its store was absent
# while the fixture had populated one. Each repair was the instance. The class is that something
# handed to the thing under test was not what the scenario built and the difference was nowhere
# in the claim, so the difference is written down here and the inventory is DERIVED by asking the
# module for its functions.
#
# What the derivation sees: every function defined in this file, whether or not anything calls
# it. That is how the one nothing called was found.
#
# What it does not see, and a reader should not believe it does: a value written inline inside a
# function body -- the granularity is the function, so a function declared BUILT that later hands
# a stand-in from its own body keeps a sentence that no longer fits; method names are flat, so
# the same name in two classes would be one entry; and the imported fixture's own replacements
# are not here at all, because FAKED_IN_FIXTURE covers those and observes them being applied.
BUILT = "the value this scenario built"
NOTHING = "nothing of its own: what it hands is classified elsewhere in this table"

HANDED = {
    # The composed scenarios, and the cases over them. Each hands what the helpers below hand
    # and what INJECTIONS classifies, and nothing of its own.
    "succeeding": NOTHING,
    "observe_all": NOTHING,
    "_stand_ins": NOTHING,
    "_fired": NOTHING,
    "_update_with": NOTHING,
    "setUp": NOTHING,
    "_functions_here": NOTHING,
    "test_no_reader_here_sees_only_the_unannotated_binding": NOTHING,

    # The derivations behind the two declared lists, and the synthetic modules that control them.
    # None of these reaches the thing under test: they read this file's own text and this
    # module's own objects.
    "_dotted": NOTHING,
    "_places": NOTHING,
    "_module_statements": NOTHING,
    "_reachable": NOTHING,
    "_bindings": NOTHING,
    "_binds_locally": NOTHING,
    "_linearised": NOTHING,
    "_class_aliases": NOTHING,
    "_base_named": NOTHING,
    "_bound_around": NOTHING,
    "_nearest": NOTHING,
    "_defined_in_scope": NOTHING,
    "_assigned": NOTHING,
    "_names_module": NOTHING,
    "_visible_from": NOTHING,
    "_passed_through": NOTHING,
    "_defaults": NOTHING,
    "_default_applies": NOTHING,
    "_declared_owner": NOTHING,
    "_seating_not_established": NOTHING,
    "_owner_names_a_class": NOTHING,
    "_bound_names": NOTHING,
    "_lambda_named": NOTHING,
    "test_every_value_follower_here_reads_the_whole_pass_through_vocabulary": NOTHING,
    "test_each_binding_fixpoint_here_halts_on_a_name_bound_twice": NOTHING,
    "test_a_decorator_alias_resolves_in_the_scope_that_imported_it": NOTHING,
    "test_no_class_body_binding_shadows_without_asking_whether_it_happened": NOTHING,
    "test_every_placement_site_here_honours_a_declared_owner": NOTHING,
    "test_every_statement_reader_here_pairs_an_unpacking_off": NOTHING,
    "test_each_transformation_boundary_is_still_where_it_says": NOTHING,
    "test_every_argument_form_outside_the_seating_grammar_is_declared": NOTHING,
    "test_a_refusal_nested_in_a_collection_is_still_collected": NOTHING,
    "test_every_owner_deciding_site_here_measures_how_near_the_binding_is": NOTHING,
    "_written_in": NOTHING,
    "_class_named": NOTHING,
    "_class_spellings": NOTHING,
    "_base_key": NOTHING,
    "_instance_classes": NOTHING,
    "_shadowing_names": NOTHING,
    "places_reached": NOTHING,
    "_owned_by_a_class": NOTHING,
    "_held_by_class": NOTHING,
    "_hands_on": NOTHING,
    "_occurrences": NOTHING,
    "_refusal_spelled": NOTHING,
    "_source_spelled": NOTHING,
    "_handle_names": NOTHING,
    "_opener_spellings": NOTHING,
    "_opener_call": NOTHING,
    "_opens_here": NOTHING,
    "refusal_spellings": NOTHING,
    "source_spellings": NOTHING,
    "refusals_reached": NOTHING,
    "source_text_reached": NOTHING,
    "planted_source": NOTHING,
    "asked_over": NOTHING,

    # The accessor, and readings taken after the fact. None of these reaches the thing under
    # test: they read a payload, a path or a file that already exists.
    "read": NOTHING,
    "table": NOTHING,
    "_row": NOTHING,
    "_unreadable": NOTHING,
    "inside": NOTHING,
    "hooks_path": NOTHING,
    "settings_bytes": NOTHING,
    "_configuration_keys": NOTHING,
    "_model_permission_delta": NOTHING,

    # State this scenario really builds, handed as itself.
    "clean": BUILT,
    "seed_settings": BUILT,
    "isolated": BUILT,
    "diagnose_for": BUILT,
    "linked_skills": BUILT,
    "installed_components": BUILT,

    # Stand-ins: what the scenario has instead, and what a row reading through it cannot say.
    "install": (
        "the update fixture's run, in which the two build steps, the relay, the measurement, the"
        " component classification and the store readings are replaced. FAKED_IN_FIXTURE names"
        " all fifteen and _stand_ins observes them while they are applied; the store readings"
        " now describe the store the fixture built, which the agreement case checks",
        "that a venv builds, that pip installs, that a live daemon or a live store answers, or"
        " that the interpreter version the record keeps is one a built environment reported"),
    "arriving_source": (
        "a source change, made by moving the digests on the fixture's own copy of the definition."
        " The fixture derives its measured digests from that copy, so moving only what load()"
        " returns makes the run refuse for a disagreement about the fixture instead of reaching"
        " the boundary under test",
        "that a real second checkout is what produces a different candidate directory, or that"
        " the committed definition detects one"),
    "updating": (
        "the arriving definition, for the length of one run: definition.load and definition.verify"
        " answer for it, because verify re-derives the component trees from git in THIS checkout"
        " and would refuse the moved digests",
        "that a real definition file and a real verification accept an arriving source"),
    "stand_in_runtime": (
        "the recorded interpreter, written here as a wrapper around this interpreter with -S -E."
        " The build step is a stand-in, so no built environment has an interpreter of its own,"
        " and a probe left to fall back would import and run whatever THIS machine has installed",
        "that an interpreter a real build produced resolves these components; what it does"
        " establish is asked of the process rather than read off the wrapper, in HERMETIC_FLAGS"),
    "importable_runtime": (
        "the same recorded interpreter rebuilt to keep -S and drop -E, so the import path this"
        " suite chooses is the only place it can look. Called with paths=None it also writes"
        " empty stub packages, which is a directory that can be imported and nothing more",
        "that a real installation is what the import resolved, unless the caller hands it the"
        " installed copy -- which succeeding() does and the stub direction does not"),
    "standin_relay": (
        "the relay the registered hook calls, written here to answer the guard. No relay is"
        " installed in a destination whose build steps were stand-ins",
        "that an installed relay answers the guard, or that its answer is this one"),
    "answering_relay": (
        "the relay whose doctor reports a socket it reached. No App Server runs here, and the"
        " connection question has no input of its own on the command line",
        "that a live App Server socket is reachable; READING_PATHS states the same narrowing"
        " for the row itself"),
    "delivering_relay": (
        "the relay that answers every step of a trial, so a delivery can complete at all",
        "that a live relay completes a delivery; READING_PATHS states the same narrowing for"
        " the row"),
    "trial_inputs": (
        "the inputs a trial requires, written here in full. On a host an operator supplies these,"
        " so there is nothing for a scenario to have built -- but they are supplied, and a row"
        " reading through them is reading an answer to input this suite chose",
        "that the values a host would supply are these, or that a delivery anybody else asked"
        " for would be accepted"),
    "fire_the_hook": (
        "the registration and the command line are the ones cmd_hook wrote into this Codex home,"
        " read back out of that file and executed as a program. What is supplied rather than"
        " built is the Stop payload on its stdin, which is a recorded one replayed because no"
        " Codex turn happens here, and the installer arguments an operator would choose",
        "that a live turn produces this payload, or that an operator's own arguments register"
        " this command line"),
    "hermetic_answers": (
        "a directory planted on PYTHONPATH that nothing ever creates, so the probe is whether an"
        " inherited path reaches the interpreter at all",
        "that a real importable package on an inherited path would be refused -- only that no"
        " inherited entry arrives"),
    "signals": (
        "the ownership signals, written here rather than gathered from a host. The real gatherer"
        " is classify_component, which the fixture replaces, so a scenario that built a foreign"
        " entry point on disk still could not reach the classifier through a run",
        "that a foreign installation on disk produces these signals; what the row establishes is"
        " that the classifier answers the five conditions differently"),

    # Cases that hand something themselves rather than through a helper.
    "test_a_store_that_is_not_there_is_not_a_store_nobody_could_read": (
        "the two presence readings, written here, because the distinction being drawn is between"
        " a store that is absent and one that could not be opened -- and a scenario cannot build"
        " the second without making a path unopenable for the whole run",
        "that a real absent store and a real unopenable one produce these readings"),
    "test_the_five_conditions_are_five_different_answers": (
        "the same written signals and presence readings as the two cases it generalises",
        "that five real host conditions produce these inputs; what it establishes is that the"
        " five answers do not collapse into each other"),
    "test_a_version_that_moved_is_not_a_source_that_moved": (
        "a version moved in a loaded definition, rather than a component actually released at a"
        " different version",
        "that a released version change is reported this way; the verification it is read"
        " through is the committed one, run against this checkout"),
    "test_the_model_and_permission_row_is_not_taken_from_a_neighbouring_verdict": (
        "two configuration texts handed straight to the producer, because the claim is that the"
        " row moves when the posture moves. The two sides of a real install are read in the"
        " success case instead",
        "that an install produces these two texts"),
    "test_without_a_reader_the_row_reports_itself_unread_rather_than_preserved": (
        "the absence of a configuration reader, simulated, so the property is stated on both"
        " supported interpreters rather than only on the one where it bites",
        "that the floor interpreter behaves this way -- which is why the vocabulary and success"
        " cases assert the same refusal unsimulated, and only on that job"),
    "test_a_hook_installation_leaves_a_hook_that_was_already_there": (
        "the foreign hook file is really written and cmd_hook really runs over it; what is"
        " supplied is the argument namespace an operator would choose",
        "that an operator's own arguments append beside a foreign hook the same way"),
    "test_a_missing_registration_and_a_claimed_one_are_not_the_same_answer": BUILT,
    "test_a_registration_that_writes_preserves_every_other_setting": BUILT,

    # Cases that hand nothing of their own: they run a scenario above, or mutate a RESULT to show
    # a reading was load-bearing, which is the opposite direction from handing a substitute in.
    "test_a_new_install_a_rerun_a_failed_update_and_the_recovery_of_what_it_replaced": NOTHING,
    "test_the_rows_seeded_before_the_first_install_survive_every_stage": NOTHING,
    "test_a_failed_update_that_never_reached_its_seam_is_not_a_recovery": NOTHING,
    "test_a_restoration_it_could_not_read_back_is_reported_rather_than_claimed": NOTHING,
    "test_the_store_the_run_is_told_about_is_the_store_the_fixture_built": NOTHING,
    "test_a_daemon_that_was_not_running_does_not_stop_the_update": NOTHING,
    "test_a_daemon_that_was_running_stops_it_and_keeps_what_was_there": NOTHING,
    "test_a_handover_in_flight_is_preserved_rather_than_swapped_under": NOTHING,
    "test_a_daemon_that_could_not_be_read_is_neither_running_nor_stopped": NOTHING,
    "test_an_install_that_succeeded_is_not_reported_as_an_activation": NOTHING,
    "test_an_external_installation_and_a_fork_are_not_the_same_answer": NOTHING,
    "test_every_question_has_exactly_one_declared_reading": NOTHING,
    "test_no_two_cells_read_the_same_answer": NOTHING,
    "test_every_declared_producer_exists_where_it_is_declared": NOTHING,
    "test_every_text_reading_place_is_declared": NOTHING,
    "test_every_declared_path_is_one_its_source_actually_writes": NOTHING,
    "test_no_cell_is_filled_without_going_through_the_declared_reading": NOTHING,
    "test_every_row_is_readable_and_answers_in_its_own_vocabulary": NOTHING,
    "test_every_question_is_declared_to_answer_in_exactly_one_vocabulary": NOTHING,
    "test_every_acceptance_row_reaches_its_success_answer": NOTHING,
    "test_every_place_that_settles_for_a_refusal_is_declared": NOTHING,
    "test_every_reading_declares_the_path_it_travels": NOTHING,
    "test_a_row_that_cannot_require_success_says_why_absence_is_normal": NOTHING,
    "test_each_row_reads_its_own_path_and_no_other": NOTHING,
    "test_withholding_a_source_leaves_only_its_own_rows_unreadable": NOTHING,
    "test_the_hook_row_counts_an_invocation_that_really_happened": NOTHING,
    "test_changing_one_condition_moves_only_the_reading_that_asked_about_it": NOTHING,
    "test_the_diagnosis_reads_nothing_outside_the_directory_it_was_given": NOTHING,
    "test_the_diagnosis_does_not_import_or_run_what_the_host_has_installed": NOTHING,
    "test_the_stand_ins_are_the_ones_the_fixture_actually_uses": NOTHING,
    "test_no_row_this_suite_fills_can_claim_a_host": NOTHING,
    "test_the_destination_this_suite_measured_is_recorded_as_temporary": NOTHING,
    "test_every_function_here_says_what_it_hands": NOTHING,
    "test_every_switch_the_fixture_accepts_is_classified": NOTHING,
    "test_no_call_site_hands_the_fixture_an_injection_this_module_refuses": NOTHING,
    "test_a_text_reading_place_arriving_in_a_form_the_old_sweep_missed_is_still_caught": NOTHING,
    "test_a_place_that_settles_for_a_refusal_in_a_form_the_old_sweep_missed_is_still_caught":
        NOTHING,
    "test_the_reach_of_each_derivation_is_the_declared_one": NOTHING,
    "test_the_forms_outside_each_derivations_reach_are_the_declared_ones": NOTHING,
    "test_each_name_this_reader_resolves_lands_where_python_lands": NOTHING,
    "test_no_two_places_this_module_declares_can_share_a_name": NOTHING,
}


def _functions_here():
    """Every function defined in this module, asked of the module rather than read out of it.

    Including the ones nothing calls: a helper left behind after its last caller went is still a
    thing this file offers, and the one that had been left behind was found this way.
    """
    module = sys.modules[__name__]
    found = set()

    def mine(value):
        return isinstance(value, types.FunctionType) and value.__module__ == __name__

    for name, value in vars(module).items():
        if mine(value):
            found.add(name)
        if isinstance(value, type) and value.__module__ == __name__:
            found.update(inner for inner, member in vars(value).items() if mine(member))
    return found


class ProvenanceTests(unittest.TestCase):
    """What this suite exercised, and what it is therefore not evidence about.

    A composed run that could describe itself as a host measurement would be worse than no
    composed run, because the record it produced would read like one nobody can reproduce. So
    the provenance is fixture, the set of things standing in for real ones is derived from the
    fixture instead of remembered, and no row this suite commits can claim to have been
    performed on a host.
    """

    def test_the_stand_ins_are_the_ones_the_fixture_actually_uses(self):
        self.assertEqual(_stand_ins(), set(FAKED_IN_FIXTURE),
                         "the fixture replaces something this record does not acknowledge, so"
                         " the record understates what was simulated")

    def test_no_row_this_suite_fills_can_claim_a_host(self):
        with tempfile.TemporaryDirectory() as temporary:
            payloads, _host = observe_all(Path(temporary))
        rows = table(payloads)

        self.assertIn(COMMITTED_PROVENANCE, PROVENANCE)
        self.assertNotEqual(COMMITTED_PROVENANCE, "host")
        for cell in SEVEN:
            with self.subTest(cell):
                self.assertEqual(rows[cell]["performedAgainst"], COMMITTED_PROVENANCE)

    def test_the_destination_this_suite_measured_is_recorded_as_temporary(self):
        """The record own word for it, so a reader is not relying on this docstring."""
        with tempfile.TemporaryDirectory() as temporary:
            payloads, _host = observe_all(Path(temporary))
        kind = read("destinationKind", payloads)

        self.assertTrue(kind["readable"])
        self.assertEqual(kind["value"], "temporary")
        self.assertIn("definitionVersion", payloads["diagnose"])
        self.assertIn("repositoryCommit", payloads["diagnose"],
                      "the revision a reader would need to reproduce this is in the payload")

    def test_every_function_here_says_what_it_hands(self):
        """The inventory, derived from the module rather than remembered.

        Asked of the module object, so a function nothing calls is still in it -- a stand-in
        whose last caller went is still a stand-in this file offers, and the one that had been
        left behind was found exactly this way.
        """
        self.assertEqual(_functions_here(), set(HANDED),
                         "a function here does not say what it hands the thing under test, or a"
                         " declaration outlived the function it described: "
                         + json.dumps(sorted(_functions_here() ^ set(HANDED))))

        for name, handed in sorted(HANDED.items()):
            with self.subTest(name):
                if handed in (BUILT, NOTHING):
                    continue
                self.assertIsInstance(handed, tuple,
                                      name + ": a stand-in is declared as what the scenario has"
                                      " instead and what a row through it cannot say")
                self.assertEqual(len(handed), 2, name + ": both halves are required")
                instead, cannot_say = handed
                self.assertTrue(instead.strip(),
                                name + " stands in for something without saying what the"
                                " scenario has instead")
                self.assertTrue(cannot_say.strip(),
                                name + " stands in for something without saying what a row"
                                " reading through it therefore does not prove")

    def test_every_switch_the_fixture_accepts_is_classified(self):
        """Asked of the fixture's own signature, so a switch added there arrives unclassified."""
        parameters = inspect.signature(base.UpdateRecoveryTests._run).parameters
        accepted = {name for name, parameter in parameters.items()
                    if parameter.kind is inspect.Parameter.KEYWORD_ONLY}

        self.assertEqual(accepted, set(INJECTIONS),
                         "the fixture accepts a switch nothing here classifies, or a"
                         " classification outlived the switch: "
                         + json.dumps(sorted(accepted ^ set(INJECTIONS))))
        for name in REFUSED_INJECTIONS:
            with self.subTest(name):
                self.assertIn(name, accepted, name + " is refused but nothing accepts it")
                self.assertTrue(INJECTIONS[name].startswith("REFUSED"),
                                name + " is refused without the table saying so")

    def test_no_call_site_hands_the_fixture_an_injection_this_module_refuses(self):
        """Every call in this file, because one green case cannot speak for the others.

        The agreement case below reads what a run was actually told, which is the behavioural
        half. It can only say that about its own run: the same switch handed at another call
        site would leave it green. So the call sites are taken together, and the subject here IS
        this file's own calls -- which is why reading it is the reading to take. Observing them
        all instead would mean running every scenario inside one case.
        """
        tree = ast.parse(HERE.read_text(encoding="utf-8"))
        refused = []
        to_the_fixture = set()
        for node in ast.walk(tree):
            if not isinstance(node, ast.Call):
                continue
            named = [keyword.arg for keyword in node.keywords if keyword.arg]
            refused.extend(name for name in named if name in REFUSED_INJECTIONS)
            if isinstance(node.func, ast.Name) and node.func.id == "install":
                to_the_fixture.update(named)

        self.assertEqual(refused, [],
                         "a call here hands the fixture a switch this module refuses: "
                         + repr(sorted(set(refused))))
        self.assertEqual(to_the_fixture - set(INJECTIONS), set(),
                         "a run is handed a switch nothing classifies: "
                         + repr(sorted(to_the_fixture - set(INJECTIONS))))
        self.assertTrue(to_the_fixture,
                        "no call hands the fixture anything, so this check is watching a door"
                        " nobody uses")
