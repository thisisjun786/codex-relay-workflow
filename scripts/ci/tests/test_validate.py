import importlib.util
from pathlib import Path
import tempfile
import unittest

SCRIPT = Path(__file__).resolve().parents[1] / "validate.py"
spec = importlib.util.spec_from_file_location("validate", SCRIPT)
validate = importlib.util.module_from_spec(spec)
spec.loader.exec_module(validate)


class StructureTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.skill = self.root / "skills/example"
        (self.skill / "agents").mkdir(parents=True)
        self.source = self.skill / "SKILL.md"
        self.source.write_text('---\nname: example\ndescription: "Do useful work"\n---\n')
        self.ui = self.skill / "agents/openai.yaml"
        self.ui.write_text('interface:\n  display_name: "Example"\n'
                           '  short_description: "Do useful work"\n'
                           '  default_prompt: "$example work"\n')

    def test_metadata_matches_own_skill(self):
        validate.metadata(self.source)
        self.source.write_text('---\nname: another\ndescription: "Do work"\n---\n')
        with self.assertRaises(ValueError):
            validate.metadata(self.source)

    def test_missing_description_duplicate_field_and_wrong_prompt(self):
        for header in ('name: example', 'name: example\ndescription: ""',
                       'name: example\nname: example\ndescription: "Do work"'):
            with self.subTest(header=header):
                self.source.write_text('---\n' + header + '\n---\n')
                with self.assertRaises(ValueError):
                    validate.metadata(self.source)
        self.source.write_text('---\nname: example\ndescription: "Do work"\n---\n')
        self.ui.write_text(self.ui.read_text().replace('$example', '$another'))
        with self.assertRaises(ValueError):
            validate.metadata(self.source)

    def test_links_reject_missing_or_escaping_paths(self):
        doc = self.root / "README.md"
        (self.root / "has space.md").write_text("# Existing\n")
        doc.write_text('[ok](has%20space.md#existing)\n[ok](<has space.md>)\n'
                       '[remote](https://example.invalid/no-network)\n'
                       '```md\n[example](missing-in-example.md)\n```\n'
                       '[bad](missing.md)\n[escape](../outside.md)\n')
        errors = validate.link_errors(doc, self.root)
        self.assertEqual(len(errors), 2)
        self.assertIn('missing.md', errors[0])
        self.assertIn('../outside.md', errors[1])


if __name__ == "__main__":
    unittest.main()
