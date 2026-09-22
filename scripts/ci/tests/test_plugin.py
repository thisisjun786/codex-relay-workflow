"""Check the package validator against the shapes that install silently wrong."""

import importlib.util
import hashlib
import json
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest

ROOT = Path(__file__).resolve().parents[3]
SCRIPT = ROOT / "scripts/ci/plugin.py"
NAMES = ("crw-check", "crw-define", "crw-logic", "crw-loop", "crw-next", "crw-plan",
         "crw-refactor", "crw-run",
         "crw-status", "crw-tidy")

_spec = importlib.util.spec_from_file_location("crw_plugin_check", SCRIPT)
plugin = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(plugin)


def manifest(**overrides):
    base = {
        "name": "crw",
        "version": "0.1.0",
        "description": "d",
        "author": {"name": "a"},
        "repository": "https://example.invalid/repo",
        "license": "MIT",
        "keywords": ["codex"],
        "skills": "./skills/",
        "interface": {
            "displayName": "CRW", "shortDescription": "s", "longDescription": "l",
            "developerName": "a", "category": "Developer Tools",
            "capabilities": ["Skills"], "defaultPrompt": ["p"],
        },
    }
    base.update(overrides)
    return base


def catalog(**overrides):
    base = {
        "name": "crw",
        "interface": {"displayName": "CRW"},
        "plugins": [{
            "name": "crw",
            "source": {"source": "local", "path": "./plugins/crw"},
            "policy": {"installation": "AVAILABLE", "authentication": "ON_USE"},
            "category": "Developer Tools",
        }],
    }
    base.update(overrides)
    return base


def payload(files):
    return {name: ("100644", data.encode()) for name, data in files.items()}


def recorded(files):
    """A fixture whose manifest names its own payload, the way a release manifest has to."""
    declared = json.loads(files[plugin.MANIFEST])
    version = plugin.payload_version(payload(files), declared["version"])
    return dict(files, **{plugin.MANIFEST: json.dumps(dict(declared, version=version))})


SKILL = "---\nname: crw-run\ndescription: d\n---\n"


def unframed(payload):
    """The serialisation this digest used before length framing, kept as the counterexample."""
    lines = [mode + " " + hashlib.sha256(data).hexdigest() + " " + name
             for name, (mode, data) in sorted(payload.items())]
    return hashlib.sha256("\n".join(lines).encode()).hexdigest()

INTERFACE = (
    "interface:\n"
    '  display_name: "A skill"\n'
    '  short_description: "What it does"\n'
    '  default_prompt: "$%s do the thing"\n'
)

GOOD = recorded({
    ".codex-plugin/plugin.json": json.dumps(manifest()),
    "skills/crw-run/SKILL.md": SKILL,
    "skills/crw-run/agents/openai.yaml": INTERFACE % "crw-run",
    "LICENSE": "MIT",
})


class ManifestTests(unittest.TestCase):
    def test_required_fields_types_and_semver(self):
        self.assertEqual(plugin.manifest_errors(manifest(), "crw", "t"), [])
        for bad, expected in (({"version": "1.0"}, "semantic version"),
                              ({"version": "01.0.0"}, "semantic version"),
                              ({"author": {"url": "u"}}, "author.name"),
                              ({"keywords": []}, "keywords"),
                              ({"repository": 5}, "repository"),
                              ({"skills": ""}, "skills")):
            with self.subTest(bad=bad):
                errors = plugin.manifest_errors(manifest(**bad), "crw", "t")
                self.assertTrue(any(expected in e for e in errors), errors)

    def test_name_must_match_the_plugin_directory(self):
        errors = plugin.manifest_errors(manifest(), "other", "t")
        self.assertTrue(any("must match the plugin directory" in e for e in errors), errors)

    def test_field_types_are_checked_not_just_presence(self):
        broken = manifest(author={"name": 1}, keywords=[1])
        broken["interface"]["displayName"] = 1
        broken["interface"]["capabilities"] = [1]
        broken["interface"]["defaultPrompt"] = [""]
        errors = plugin.manifest_errors(broken, "crw", "t")
        for expected in ("author.name", "keywords", "interface.displayName",
                         "interface.capabilities", "interface.defaultPrompt"):
            self.assertTrue(any(expected in e for e in errors), (expected, errors))

    def test_whitespace_only_values_are_treated_as_absent(self):
        blank = manifest(description="   ", author={"name": " "}, keywords=[" "])
        blank["interface"]["displayName"] = "  "
        blank["interface"]["capabilities"] = ["  "]
        errors = plugin.manifest_errors(blank, "crw", "t")
        for expected in ("description", "author.name", "keywords", "interface.displayName",
                         "interface.capabilities"):
            self.assertTrue(any(expected in e for e in errors), (expected, errors))

    def test_prerelease_identifiers_follow_the_specification(self):
        for good in ("1.0.0", "0.1.0-alpha.1", "1.2.3+build.5", "1.0.0-rc.1+exp.sha.5114f85"):
            with self.subTest(good=good):
                self.assertEqual(plugin.manifest_errors(manifest(version=good), "crw", "t"), [])
        for bad in ("1.0.0-01", "1.0.0-..", "1.0.0-", "1.0", "01.0.0", "1.0.0+"):
            with self.subTest(bad=bad):
                errors = plugin.manifest_errors(manifest(version=bad), "crw", "t")
                self.assertTrue(any("semantic version" in e for e in errors), (bad, errors))

    def test_installed_tree_is_not_checked_against_its_version_directory(self):
        # An installed payload lives in a directory named by version, not by plugin.
        self.assertEqual(plugin.manifest_errors(manifest(), None, "t"), [])

    def test_missing_interface_field_is_refused(self):
        broken = manifest()
        del broken["interface"]["defaultPrompt"]
        errors = plugin.manifest_errors(broken, "crw", "t")
        self.assertTrue(any("interface.defaultPrompt" in e for e in errors), errors)

    def test_a_declared_component_has_to_be_shipped(self):
        # A declaration replaces default discovery, so a component the manifest names and the
        # package does not carry loads nothing and reports nothing.
        for field in ("hooks", "mcpServers"):
            with self.subTest(field=field):
                errors = plugin.manifest_errors(manifest(**{field: "./x.json"}), "crw", "t",
                                                payload(GOOD))
                self.assertTrue(any("does not ship" in e for e in errors), errors)

    def test_apps_stays_undeclared(self):
        errors = plugin.manifest_errors(manifest(apps="./x.json"), "crw", "t")
        self.assertTrue(any("apps" in e for e in errors), errors)

    def test_unsupported_keys_are_refused(self):
        # The bundled ingestion validator rejects keys it does not know, so a manifest
        # that passes here but carries a stray key would be refused when published.
        errors = plugin.manifest_errors(manifest(unsupported="x"), "crw", "t")
        self.assertTrue(any("not a supported manifest key" in e for e in errors), errors)
        nested = manifest()
        nested["interface"]["unsupported"] = "x"
        errors = plugin.manifest_errors(nested, "crw", "t")
        self.assertTrue(any("not a supported interface key" in e for e in errors), errors)
        author = manifest(author={"name": "a", "unsupported": "x"})
        errors = plugin.manifest_errors(author, "crw", "t")
        self.assertTrue(any("unsupported keys" in e for e in errors), errors)

    def test_optional_interface_values_are_checked_when_present(self):
        good = manifest()
        good["interface"]["websiteURL"] = "https://example.invalid"
        good["interface"]["screenshots"] = ["./assets/one.png"]
        self.assertEqual(plugin.manifest_errors(good, "crw", "t"), [])
        for field, value in (("websiteURL", 5), ("screenshots", []), ("screenshots", [""])):
            with self.subTest(field=field, value=value):
                broken = manifest()
                broken["interface"][field] = value
                errors = plugin.manifest_errors(broken, "crw", "t")
                self.assertTrue(any("not a usable value" in e for e in errors), errors)

    def test_optional_interface_fields_follow_the_ingestion_rules(self):
        good = manifest()
        good["interface"].update({"websiteURL": "https://example.invalid", "brandColor": "#D7010F",
                                  "logo": "./assets/logo.png"})
        files = recorded({**GOOD, plugin.MANIFEST: json.dumps(good), "assets/logo.png": "png"})
        good = json.loads(files[plugin.MANIFEST])
        shipped = payload(files)
        self.assertEqual(plugin.manifest_errors(good, "crw", "t", shipped), [])
        for field, value, expected in (("websiteURL", "http://insecure.invalid", "https URL"),
                                       ("brandColor", "red", "#RRGGBB"),
                                       ("logo", "./missing.png", "does not ship"),
                                       ("logo", "assets/logo.png", "./ relative path")):
            with self.subTest(field=field, value=value):
                broken = manifest()
                broken["interface"][field] = value
                errors = plugin.manifest_errors(broken, "crw", "t", shipped)
                self.assertTrue(any(expected in e for e in errors), (field, errors))

    def test_urls_are_parsed_rather_than_prefix_matched(self):
        for value in ("https://", "https:///missing-host", "http://example.invalid",
                      "example.invalid", "https://@", "https://[", "https://:443"):
            with self.subTest(value=value):
                broken = manifest()
                broken["interface"]["websiteURL"] = value
                errors = plugin.manifest_errors(broken, "crw", "t")
                self.assertTrue(any("https URL" in e for e in errors), (value, errors))
                author = plugin.manifest_errors(manifest(author={"name": "a", "url": value}), "crw", "t")
                self.assertTrue(any("author.url" in e for e in author), (value, author))

    def test_optional_author_values_are_checked_when_present(self):
        self.assertEqual(plugin.manifest_errors(
            manifest(author={"name": "a", "email": "a@example.invalid",
                             "url": "https://example.invalid"}), "crw", "t"), [])
        errors = plugin.manifest_errors(manifest(author={"name": "a", "email": 5}), "crw", "t")
        self.assertTrue(any("author.email" in e for e in errors), errors)

    def test_ssh_keys_and_credential_dotfiles_are_refused(self):
        for name in ("skills/id_ed25519", "skills/id_ecdsa", "skills/.netrc",
                     "skills/.npmrc", "skills/authorized_keys"):
            with self.subTest(name=name):
                errors = plugin.hygiene(payload({**GOOD, name: "x"}), manifest(), "t")
                self.assertTrue(any("may not ship" in e for e in errors), errors)

    def test_declared_path_may_not_leave_the_plugin_root(self):
        for bad in ("../../skills/", "/abs/skills/", "skills/"):
            with self.subTest(bad=bad):
                with self.assertRaises(ValueError):
                    plugin.declared_skills_path(manifest(skills=bad))


class MarketplaceTests(unittest.TestCase):
    def test_entry_is_pinned_to_the_manifest(self):
        self.assertEqual(plugin.marketplace_errors(catalog(), manifest(), "plugins/crw"), [])
        cases = (
            (catalog(name="other"), "name"),
            (catalog(interface={"displayName": "Other"}), "displayName"),
        )
        for broken, expected in cases:
            with self.subTest(expected=expected):
                errors = plugin.marketplace_errors(broken, manifest(), "plugins/crw")
                self.assertTrue(any(expected in e for e in errors), errors)

    def test_entry_fields_are_required(self):
        for mutate, expected in (
            (lambda e: e["source"].update({"path": "./plugins/other"}), "source.path"),
            (lambda e: e["source"].update({"source": "git"}), "source.source"),
            (lambda e: e["policy"].update({"authentication": "ON_INSTALL"}), "ON_USE"),
            (lambda e: e["policy"].update({"installation": "NOT_AVAILABLE"}), "AVAILABLE"),
            (lambda e: e.pop("category"), "category"),
        ):
            with self.subTest(expected=expected):
                broken = catalog()
                mutate(broken["plugins"][0])
                errors = plugin.marketplace_errors(broken, manifest(), "plugins/crw")
                self.assertTrue(any(expected in e for e in errors), errors)

    def test_source_path_is_compared_exactly(self):
        # A traversal or absolute spelling would install a different directory.
        for spelling in ("../../plugins/crw", "/plugins/crw", "plugins/crw",
                         "./plugins/crw/", "./plugins/./crw"):
            with self.subTest(spelling=spelling):
                broken = catalog()
                broken["plugins"][0]["source"]["path"] = spelling
                errors = plugin.marketplace_errors(broken, manifest(), "plugins/crw")
                self.assertTrue(any("source.path" in e for e in errors), (spelling, errors))

    def test_malformed_entry_objects_report_instead_of_raising(self):
        for field in ("source", "policy"):
            with self.subTest(field=field):
                broken = catalog()
                broken["plugins"][0][field] = "string"
                errors = plugin.marketplace_errors(broken, manifest(), "plugins/crw")
                self.assertTrue(any("must be an object" in e for e in errors), errors)


class HygieneTests(unittest.TestCase):
    def test_clean_payload_passes(self):
        self.assertEqual(plugin.hygiene(payload(GOOD), manifest(), "t"), [])

    def test_required_files_must_ship(self):
        for missing in (".codex-plugin/plugin.json", "LICENSE"):
            with self.subTest(missing=missing):
                files = {k: v for k, v in GOOD.items() if k != missing}
                errors = plugin.hygiene(payload(files), manifest(), "t")
                self.assertTrue(any(missing in e for e in errors), errors)

    def test_top_level_allowlist(self):
        errors = plugin.hygiene(payload({**GOOD, "packages/relay.py": "x"}), manifest(), "t")
        self.assertTrue(any("may ship in the package" in e for e in errors), errors)

    def test_operational_state_and_credentials(self):
        for name in (".codexclaw/sessions/s.json", "skills/crw-run/relay.sqlite3",
                     ".env", ".env.local", "skills/id_rsa", "skills/aws-credentials",
                     "skills/client.key", "skills/crw-run/client-secrets.json",
                     "skills/secret.yaml", "skills/service-credential.json"):
            with self.subTest(name=name):
                errors = plugin.hygiene(payload({**GOOD, name: "x"}), manifest(), "t")
                self.assertTrue(any("may not ship" in e for e in errors), errors)

    def test_ordinary_words_are_not_mistaken_for_credentials(self):
        # The rule matches a credential name, not any word containing one.
        for name in ("skills/crw-run/secretary.md", "skills/crw-run/credentialing.md",
                     "skills/crw-run/keyboard.md", "skills/crw-run/database.md"):
            with self.subTest(name=name):
                self.assertEqual(plugin.hygiene(payload({**GOOD, name: "x"}), manifest(), "t"), [])

    def test_personal_paths_are_refused_and_placeholders_are_not(self):
        bad = plugin.hygiene(payload({**GOOD, "skills/crw-run/SKILL.md":
                                      "put it in /home/someone/code/x"}),
                             manifest(), "t")
        self.assertTrue(any("personal path" in e for e in bad), bad)
        placeholder = plugin.hygiene(payload({**GOOD, "skills/crw-run/SKILL.md":
                                              "use <worktree-root>/<project> or"
                                              " /example/home/x"}), manifest(), "t")
        self.assertEqual(placeholder, [])


class SkillSetTests(unittest.TestCase):
    def test_declared_directory_is_the_skill_set(self):
        errors, found = plugin.skills(payload(GOOD), manifest(), "t")
        self.assertEqual(errors, [])
        self.assertEqual(sorted(found), ["crw-run"])

    def test_a_nested_declared_path_is_read_the_same_way(self):
        # declared_skills_path accepts a nested path, so the skill set must follow it.
        files = {".codex-plugin/plugin.json": json.dumps(manifest(skills="./skills/current/")),
                 "skills/current/crw-run/SKILL.md": SKILL,
                 "skills/current/crw-run/agents/openai.yaml": INTERFACE % "crw-run",
                 "LICENSE": "MIT"}
        errors, found = plugin.skills(payload(files), manifest(skills="./skills/current/"), "t")
        self.assertEqual(errors, [])
        self.assertEqual(sorted(found), ["crw-run"])

    def test_files_outside_the_declared_path_are_refused(self):
        # Everything under the plugin root ships, so an undeclared tree would install
        # without ever being validated.
        files = {".codex-plugin/plugin.json": json.dumps(manifest(skills="./skills/current/")),
                 "skills/current/crw-run/SKILL.md": SKILL,
                 "skills/current/crw-run/agents/openai.yaml": INTERFACE % "crw-run",
                 "skills/old/crw-check/SKILL.md": SKILL,
                 "LICENSE": "MIT"}
        errors, _ = plugin.skills(payload(files), manifest(skills="./skills/current/"), "t")
        self.assertTrue(any("ships outside every declared component path" in e for e in errors), errors)

    def test_interface_metadata_is_required_per_skill(self):
        files = {k: v for k, v in GOOD.items() if not k.endswith("openai.yaml")}
        errors, _ = plugin.skills(payload(files), manifest(), "t")
        self.assertTrue(any("agents/openai.yaml" in e for e in errors), errors)

    def test_empty_declaration_is_refused(self):
        errors, _ = plugin.skills(payload({"LICENSE": "MIT"}), manifest(), "t")
        self.assertTrue(any("ships no skill" in e for e in errors), errors)


class DigestTests(unittest.TestCase):
    def test_digest_is_order_independent_and_content_sensitive(self):
        first = plugin.digest(payload(GOOD))
        reordered = plugin.digest(payload(dict(reversed(list(GOOD.items())))))
        self.assertEqual(first, reordered)
        self.assertNotEqual(first, plugin.digest(payload(dict(GOOD, LICENSE="MIT "))))

    def test_digest_covers_the_file_mode(self):
        executable = dict(payload(GOOD))
        executable["LICENSE"] = ("100755", b"MIT")
        self.assertNotEqual(plugin.digest(payload(GOOD)), plugin.digest(executable))

    def test_names_cannot_be_smuggled_across_the_field_boundary(self):
        # Without length framing a newline in a file name lets two different
        # payloads serialise identically, and the digest stops being evidence.
        third = hashlib.sha256(b"three").hexdigest()
        smuggled = "b\n100644 " + third + " x"
        left = {"a": ("100644", b"one"), smuggled: ("100644", b"two")}
        right = {"a": ("100644", b"one"), "b": ("100644", b"two"), "x": ("100644", b"three")}
        self.assertEqual(unframed(left), unframed(right))
        self.assertNotEqual(plugin.digest(left), plugin.digest(right))


class PayloadVersionTests(unittest.TestCase):
    """The version is the only payload identity an operator sees, so it has to be one."""

    def version_of(self, files):
        return json.loads(files[plugin.MANIFEST])["version"]

    def test_one_changed_shipped_file_changes_the_version(self):
        self.assertNotEqual(self.version_of(GOOD),
                            self.version_of(recorded(dict(GOOD, LICENSE="MIT\n"))))

    def test_an_unchanged_payload_derives_the_same_version(self):
        self.assertEqual(recorded(GOOD), GOOD)

    def test_the_recorded_suffix_is_elided_before_the_digest_is_taken(self):
        # The manifest ships inside the payload it names. Without the elision, recording
        # the digest would change the digest, and the value would never settle.
        version = self.version_of(GOOD)
        self.assertRegex(version, r"^0\.1\.0\+[0-9a-f]{12}$")
        self.assertEqual(plugin.payload_version(payload(GOOD), version), version)
        self.assertEqual(plugin.version_payload(payload(GOOD), version)[plugin.MANIFEST][1],
                         json.dumps(manifest()).encode())

    def test_a_suffix_recorded_for_other_bytes_is_refused_and_the_right_one_named(self):
        stale = dict(json.loads(GOOD[plugin.MANIFEST]), version="0.1.0+000000000000")
        files = dict(GOOD, **{plugin.MANIFEST: json.dumps(stale)})
        errors = plugin.manifest_errors(stale, "crw", "t", payload(files))
        self.assertTrue(any("does not name this payload" in e for e in errors), errors)
        self.assertTrue(any(self.version_of(GOOD) in e for e in errors), errors)

    def test_a_shipped_file_may_not_repeat_the_suffix(self):
        # Two places holding the same derived value cannot both be updated to agree.
        suffix = self.version_of(GOOD).partition("+")[2]
        files = dict(GOOD, **{"skills/crw-run/references/built.md": "built from " + suffix})
        errors = plugin.manifest_errors(json.loads(GOOD[plugin.MANIFEST]), "crw", "t",
                                        payload(files))
        self.assertTrue(any("repeats the payload suffix" in e for e in errors), errors)

    def test_a_manifest_that_spells_its_version_twice_cannot_be_derived_from(self):
        declared = json.loads(GOOD[plugin.MANIFEST])
        declared["description"] = declared["version"]
        files = dict(GOOD, **{plugin.MANIFEST: json.dumps(declared)})
        errors = plugin.manifest_errors(declared, "crw", "t", payload(files))
        self.assertTrue(any("elided" in e for e in errors), errors)

    def test_a_version_without_a_suffix_is_refused(self):
        plain = manifest()
        files = dict(GOOD, **{plugin.MANIFEST: json.dumps(plain)})
        errors = plugin.manifest_errors(plain, "crw", "t", payload(files))
        self.assertTrue(any("does not name this payload" in e for e in errors), errors)


class SyntheticRepositoryTests(unittest.TestCase):
    """The release payload must come from the revision, not from the working tree."""

    def build(self, folder, files=None, link="plugins/crw/skills", commit=True):
        root = Path(folder)
        (root / "scripts/ci").mkdir(parents=True)
        shutil.copy(SCRIPT, root / "scripts/ci/plugin.py")
        for name, data in (files or GOOD).items():
            path = root / "plugins/crw" / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(data, encoding="utf-8")
        (root / "LICENSE").write_text("MIT", encoding="utf-8")
        marketplace = root / ".agents/plugins/marketplace.json"
        marketplace.parent.mkdir(parents=True, exist_ok=True)
        marketplace.write_text(json.dumps(catalog()), encoding="utf-8")
        if link:
            (root / "skills").symlink_to(link)
        subprocess.run(["git", "init", "-q", "-b", "main", str(root)], check=True)
        if commit:
            subprocess.run(["git", "-C", str(root), "add", "-A"], check=True)
            subprocess.run(["git", "-C", str(root), "-c", "user.email=t@t", "-c", "user.name=t",
                            "commit", "-q", "-m", "package"], check=True)
        return root

    def run_in(self, root, *args):
        return subprocess.run([sys.executable, str(root / "scripts/ci/plugin.py"), *args],
                              cwd=root, capture_output=True, text=True)

    def test_healthy_synthetic_package_passes(self):
        with tempfile.TemporaryDirectory() as folder:
            result = self.run_in(self.build(folder))
            self.assertEqual(result.returncode, 0, result.stderr)

    def test_committed_manifest_must_be_readable(self):
        with tempfile.TemporaryDirectory() as folder:
            files = dict(GOOD, **{".codex-plugin/plugin.json": "not-json"})
            result = self.run_in(self.build(folder, files))
            self.assertEqual(result.returncode, 1)
            self.assertIn("plugin.json", result.stderr)

    def test_committed_license_must_ship_and_match(self):
        with tempfile.TemporaryDirectory() as folder:
            files = {k: v for k, v in GOOD.items() if k != "LICENSE"}
            result = self.run_in(self.build(folder, files))
            self.assertEqual(result.returncode, 1)
            self.assertIn("LICENSE", result.stderr)
        with tempfile.TemporaryDirectory() as folder:
            result = self.run_in(self.build(folder, dict(GOOD, LICENSE="Apache")))
            self.assertEqual(result.returncode, 1)
            self.assertIn("repository license", result.stderr)

    def test_marketplace_entry_is_read_from_the_revision(self):
        with tempfile.TemporaryDirectory() as folder:
            root = self.build(folder)
            broken = catalog()
            broken["plugins"][0]["source"]["path"] = "./plugins/elsewhere"
            (root / ".agents/plugins/marketplace.json").write_text(json.dumps(broken), encoding="utf-8")
            subprocess.run(["git", "-C", str(root), "add", "-A"], check=True)
            subprocess.run(["git", "-C", str(root), "-c", "user.email=t@t", "-c", "user.name=t",
                            "commit", "-q", "-m", "break"], check=True)
            result = self.run_in(root)
            self.assertEqual(result.returncode, 1)
            self.assertIn("source.path", result.stderr)

    def test_compatibility_link_must_point_at_the_packaged_skills(self):
        for link, expected in ((None, "must keep a link"), ("plugins/crw", "root link points at")):
            with self.subTest(link=link), tempfile.TemporaryDirectory() as folder:
                result = self.run_in(self.build(folder, link=link))
                self.assertEqual(result.returncode, 1)
                self.assertIn(expected, result.stderr)

    def test_untracked_file_in_the_plugin_root_is_refused(self):
        with tempfile.TemporaryDirectory() as folder:
            root = self.build(folder)
            (root / "plugins/crw/notes.txt").write_text("local", encoding="utf-8")
            result = self.run_in(root)
            self.assertEqual(result.returncode, 1)
            self.assertIn("untracked", result.stderr)

    def test_working_tree_manifest_is_checked_too(self):
        # A local marketplace installs the working tree, so a broken uncommitted
        # manifest would be copied into the cache even though the revision is clean.
        with tempfile.TemporaryDirectory() as folder:
            root = self.build(folder)
            (root / "plugins/crw/.codex-plugin/plugin.json").write_text("not-json", encoding="utf-8")
            result = self.run_in(root)
            self.assertEqual(result.returncode, 1)
            self.assertIn("working tree", result.stderr)

    def test_working_tree_must_ship_the_committed_skill_set(self):
        files = dict(GOOD)
        files["skills/crw-plan/SKILL.md"] = "---\nname: crw-plan\ndescription: d\n---\n"
        files["skills/crw-plan/agents/openai.yaml"] = INTERFACE % "crw-plan"
        with tempfile.TemporaryDirectory() as folder:
            root = self.build(folder, files)
            shutil.rmtree(root / "plugins/crw/skills/crw-plan")
            result = self.run_in(root)
            self.assertEqual(result.returncode, 1)
            self.assertIn("crw-plan", result.stderr)

    def test_root_link_target_is_compared_exactly(self):
        # A committed link target with trailing whitespace is a broken link.
        with tempfile.TemporaryDirectory() as folder:
            root = self.build(folder, link="plugins/crw/skills ")
            result = self.run_in(root)
            self.assertEqual(result.returncode, 1)
            self.assertIn("root link points at", result.stderr)

    def test_declared_license_must_match_the_repository_license(self):
        with tempfile.TemporaryDirectory() as folder:
            files = dict(GOOD, **{".codex-plugin/plugin.json":
                                  json.dumps(manifest(license="Apache-2.0"))})
            result = self.run_in(self.build(folder, files))
            self.assertEqual(result.returncode, 1)
            self.assertIn("license", result.stderr)

    def test_empty_directories_are_refused(self):
        # Git cannot record one, but the installer copies it out of a working tree.
        with tempfile.TemporaryDirectory() as folder:
            root = self.build(folder)
            (root / "plugins/crw/extra-empty").mkdir()
            result = self.run_in(root)
            self.assertEqual(result.returncode, 1)
            self.assertIn("empty directory", result.stderr)

    def test_version_may_not_carry_trailing_whitespace(self):
        with tempfile.TemporaryDirectory() as folder:
            files = dict(GOOD, **{".codex-plugin/plugin.json":
                                  json.dumps(manifest(version="1.0.0\n"))})
            result = self.run_in(self.build(folder, files))
            self.assertEqual(result.returncode, 1)
            self.assertIn("semantic version", result.stderr)

    def test_a_release_payload_must_declare_its_own_digest(self):
        # One shipped file edited after the version was recorded. Both trees would install
        # under one cache directory and `codex plugin list` would show one version.
        files = dict(GOOD)
        files["skills/crw-run/SKILL.md"] = SKILL + "\nAn extra paragraph.\n"
        with tempfile.TemporaryDirectory() as folder:
            result = self.run_in(self.build(folder, files))
            self.assertEqual(result.returncode, 1)
            self.assertIn("does not name this payload", result.stderr)

    def test_record_version_writes_the_derived_suffix_and_then_settles(self):
        files = dict(GOOD, **{".codex-plugin/plugin.json": json.dumps(manifest())})
        with tempfile.TemporaryDirectory() as folder:
            root = self.build(folder, files)
            written = root / "plugins/crw/.codex-plugin/plugin.json"
            first = self.run_in(root, "--record-version")
            self.assertEqual(first.returncode, 0, first.stderr)
            self.assertRegex(json.loads(written.read_text())["version"],
                             r"^0\.1\.0\+[0-9a-f]{12}$")
            settled = written.read_text(encoding="utf-8")
            again = self.run_in(root, "--record-version")
            self.assertEqual(again.returncode, 0, again.stderr)
            self.assertIn("already recorded", again.stdout)
            self.assertEqual(written.read_text(encoding="utf-8"), settled)

    def test_record_version_reports_a_manifest_it_cannot_derive_from(self):
        # Another field holding the same string leaves no single spelling to elide. Deriving is
        # what this command does, so it has to say that and stop, not end in a traceback.
        declared = json.loads(GOOD[plugin.MANIFEST])
        declared["description"] = declared["version"]
        files = dict(GOOD, **{plugin.MANIFEST: json.dumps(declared)})
        with tempfile.TemporaryDirectory() as folder:
            result = self.run_in(self.build(folder, files), "--record-version")
            self.assertEqual(result.returncode, 1)
            self.assertIn("elided", result.stderr)
            self.assertNotIn("Traceback", result.stderr)


class CommandTests(unittest.TestCase):
    def run_script(self, *args):
        return subprocess.run([sys.executable, str(SCRIPT), *args], capture_output=True, text=True)

    def write_payload(self, root, files):
        for name, data in files.items():
            path = root / name
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(data, encoding="utf-8")

    def test_repository_package_reports_the_namespaced_skill_names(self):
        result = self.run_script("--json")
        self.assertEqual(result.returncode, 0, result.stderr)
        report = json.loads(result.stdout)
        self.assertEqual(report["expectedSkillNames"], [f"crw:{name}" for name in NAMES])
        self.assertEqual(report["skills"], list(NAMES))
        head = subprocess.run(["git", "rev-parse", "HEAD"], cwd=ROOT,
                              capture_output=True, text=True, check=True).stdout.strip()
        self.assertEqual(report["resolved"], head)
        self.assertEqual(report["digest"], json.loads(self.run_script("--json").stdout)["digest"])

    def test_installed_payload_is_validated_with_the_same_rules(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder) / "crw"
            self.write_payload(root, GOOD)
            good = self.run_script("--payload", str(root), "--json")
            self.assertEqual(good.returncode, 0, good.stderr)
            self.assertEqual(json.loads(good.stdout)["expectedSkillNames"], ["crw:crw-run"])

    def test_installed_payload_missing_a_required_file_is_refused(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder) / "crw"
            self.write_payload(root, {k: v for k, v in GOOD.items() if k != "LICENSE"})
            result = self.run_script("--payload", str(root))
            self.assertEqual(result.returncode, 1)
            self.assertIn("LICENSE", result.stderr)

    def test_installed_payload_with_a_symlink_is_refused(self):
        # The installer drops symlinks, so a package that relies on one loses those files.
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder) / "crw"
            self.write_payload(root, GOOD)
            (root / "skills/crw-plan").symlink_to(root / "skills/crw-run", target_is_directory=True)
            result = self.run_script("--payload", str(root))
            self.assertEqual(result.returncode, 1)
            self.assertIn("symlink", result.stderr)

    def test_installed_payload_with_an_empty_directory_is_refused(self):
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder) / "crw"
            self.write_payload(root, GOOD)
            (root / "leftover").mkdir()
            result = self.run_script("--payload", str(root))
            self.assertEqual(result.returncode, 1)
            self.assertIn("empty directory", result.stderr)

    def test_installed_payload_must_declare_the_version_it_is_filed_under(self):
        # The cache directory is the version, so this is how an operator asks which bytes
        # the directory in front of them actually holds.
        with tempfile.TemporaryDirectory() as folder:
            root = Path(folder) / "crw"
            self.write_payload(root, dict(GOOD, LICENSE="MIT, with a later edit"))
            result = self.run_script("--payload", str(root))
            self.assertEqual(result.returncode, 1)
            self.assertIn("does not name this payload", result.stderr)
            # Nothing writes to an installed cache, so the remedy offered has to be one that
            # exists for it rather than a command that edits a working tree somewhere else.
            self.assertNotIn("--record-version", result.stderr)
            self.assertIn("install the package again", result.stderr)


if __name__ == "__main__":
    unittest.main()
