package skill

import "fmt"

// osErrorShapeCases make a skill command's file read fail with ENOENT, EACCES or EISDIR: each
// command reports the system's error naming the file and exits as a read failure does.
func osErrorShapeCases() []skillShapeCase {
	var cases []skillShapeCase
	add := func(name, family string, files map[string]any, args ...string) {
		cases = append(cases, skillShapeCase{name: "oserror/" + name, family: family, args: args, files: files})
	}
	const dir = "dir"
	inside := dir + "/a.json"
	asDirectory := map[string]any{inside: shapeDirectory{}}
	unreadable := map[string]any{inside: shapeUnreadableFile("{}")}
	at := func(rest string) string { return "$TMP/" + dir + "/" + rest }
	add(fmt.Sprintf("title-replay/is-a-directory/%s", dir), "parent-title", asDirectory, "replay", "--fixtures", at(""), "--allow-unreached")
	add(fmt.Sprintf("title-replay/permission/%s", dir), "parent-title", unreadable, "replay", "--fixtures", at("."), "--allow-unreached")
	add(fmt.Sprintf("probe-decide/missing/%s", dir), "hook-probe", nil, "decide", at("missing.json"))
	add(fmt.Sprintf("probe-decide/permission/%s", dir), "hook-probe", unreadable, "decide", at("a.json"))
	add(fmt.Sprintf("probe-decide/is-a-directory/%s", dir), "hook-probe", asDirectory, "decide", at("a.json")+"/")
	add(fmt.Sprintf("policy-check/missing/%s", dir), "start-policy", nil, "check", at("missing.md"))
	add(fmt.Sprintf("policy-check/permission/%s", dir), "start-policy", unreadable, "check", at("a.json"))
	add(fmt.Sprintf("policy-check/is-a-directory/%s", dir), "start-policy", asDirectory, "check", at("./a.json"))
	add(fmt.Sprintf("probe-replay/fixtures-is-a-directory/%s", dir), "hook-probe", asDirectory, "replay", "--fixtures", at(""), "--allow-unreached")
	add(fmt.Sprintf("probe-replay/contract-missing/%s", dir), "hook-probe", nil, "replay", "--contract", at("missing.md"), "--allow-unreached")
	add(fmt.Sprintf("probe-replay/contract-is-a-directory/%s", dir), "hook-probe", asDirectory, "replay", "--contract", at("a.json"), "--allow-unreached")
	add(fmt.Sprintf("probe-replay/host-is-a-directory/%s", dir), "hook-probe",
		map[string]any{dir + "/host-observation-codex-0.1.0.json": shapeDirectory{}},
		"replay", "--host-fixtures", at(""), "--allow-unreached")
	return cases
}
