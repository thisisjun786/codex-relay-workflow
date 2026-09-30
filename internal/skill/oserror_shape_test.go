package skill

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// osErrorShapeCases make a skill command's file read fail with ENOENT, EACCES
// or EISDIR under a directory whose name repr() spells otherwise than '%s':
// a quote (repr picks the other quote, or escapes it), a control, a backslash,
// or bytes outside UTF-8 (each a lone surrogate after os.fsdecode). Python
// prints str(OSError), which spells the filename as repr(str(Path(name))).
func osErrorShapeCases() []skillShapeCase {
	dirs := []struct{ tag, name string }{
		{"quote", "it's"},
		{"both-quotes", `it's "both"`},
		{"tab", "tab\tdir"},
		{"backslash", `back\slash`},
		{"surrogate-escape", "dir\xff\xed\xa0\x80"},
	}
	var cases []skillShapeCase
	add := func(name, family string, files map[string]any, args ...string) {
		cases = append(cases, skillShapeCase{name: "oserror/" + name, family: family, args: args, files: files})
	}
	for _, dir := range dirs {
		inside := dir.name + "/a.json"
		asDirectory := map[string]any{inside: shapeDirectory{}}
		unreadable := map[string]any{inside: shapeUnreadableFile("{}")}
		at := func(rest string) string { return "$TMP/" + dir.name + "/" + rest }

		add(fmt.Sprintf("title-replay/is-a-directory/%s", dir.tag), "parent-title", asDirectory, "replay", "--fixtures", at(""), "--allow-unreached")
		add(fmt.Sprintf("title-replay/permission/%s", dir.tag), "parent-title", unreadable, "replay", "--fixtures", at("."), "--allow-unreached")
		add(fmt.Sprintf("probe-decide/missing/%s", dir.tag), "hook-probe", nil, "decide", at("missing.json"))
		add(fmt.Sprintf("probe-decide/permission/%s", dir.tag), "hook-probe", unreadable, "decide", at("a.json"))
		add(fmt.Sprintf("probe-decide/is-a-directory/%s", dir.tag), "hook-probe", asDirectory, "decide", at("a.json")+"/")
		add(fmt.Sprintf("policy-check/missing/%s", dir.tag), "start-policy", nil, "check", at("missing.md"))
		add(fmt.Sprintf("policy-check/permission/%s", dir.tag), "start-policy", unreadable, "check", at("a.json"))
		add(fmt.Sprintf("policy-check/is-a-directory/%s", dir.tag), "start-policy", asDirectory, "check", at("./a.json"))
		// hook-probe replay opens its three paths through os.DirFS, which
		// refuses a name outside UTF-8 before any read, and globs each fixture
		// directory with the directory inside the pattern, where a backslash
		// escapes. Neither is this spelling, so those names stay out.
		if !utf8.ValidString(dir.name) || strings.ContainsAny(dir.name, `\*?[`) {
			continue
		}
		add(fmt.Sprintf("probe-replay/fixtures-is-a-directory/%s", dir.tag), "hook-probe", asDirectory, "replay", "--fixtures", at(""), "--allow-unreached")
		add(fmt.Sprintf("probe-replay/contract-missing/%s", dir.tag), "hook-probe", nil, "replay", "--contract", at("missing.md"), "--allow-unreached")
		add(fmt.Sprintf("probe-replay/contract-is-a-directory/%s", dir.tag), "hook-probe", asDirectory, "replay", "--contract", at("a.json"), "--allow-unreached")
		add(fmt.Sprintf("probe-replay/host-is-a-directory/%s", dir.tag), "hook-probe",
			map[string]any{dir.name + "/host-observation-codex-0.1.0.json": shapeDirectory{}},
			"replay", "--host-fixtures", at(""), "--allow-unreached")
	}
	// A relative --contract is named as str(Path) holds it: "./" and repeated
	// separators go, ".." stays.
	add("probe-replay/contract-missing/relative", "hook-probe", nil, "replay", "--contract", ".//nowhere/../missing'.md", "--allow-unreached")
	return cases
}
