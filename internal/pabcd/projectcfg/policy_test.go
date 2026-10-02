package projectcfg

import (
	"os"
	"testing"
)

func env(vars map[string]string) func(string) string {
	return func(key string) string { return vars[key] }
}

// repoWith is a directory holding crw.json with the given contents, or none when there are none.
func repoWith(t *testing.T, contents ...string) string {
	t.Helper()
	dir := t.TempDir()
	if len(contents) > 0 {
		if err := os.WriteFile(ConfigPath(dir), []byte(contents[0]), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestDecideEntry(t *testing.T) {
	for name, c := range map[string]struct {
		in   EntryInput
		want bool
	}{
		"goal mode suppression beats always":      {EntryInput{"P", PolicyAlways, false, true}, false},
		"unreadable goal state takes the branch":  {EntryInput{"P", PolicyAlways, true, true}, false},
		"always advises even mid-cycle":           {EntryInput{"P", PolicyAlways, true, false}, true},
		"new-unit advises the first plan request": {EntryInput{"P", PolicyNewUnit, false, false}, true},
		"new-unit does not interrupt a cycle":     {EntryInput{"P", PolicyNewUnit, true, false}, false},
		"off keeps today's behavior":              {EntryInput{"P", PolicyOff, false, false}, false},
	} {
		if got := DecideEntry(c.in); got != (EntryDecision{"P", c.want}) {
			t.Errorf("%s: %+v", name, got)
		}
	}
	for _, policy := range []Policy{PolicyOff, PolicyNewUnit, PolicyAlways} {
		for _, trigger := range []string{"", "I", "A", "B", "C", "P"} {
			for _, active := range []bool{false, true} {
				for _, suppressed := range []bool{false, true} {
					got := DecideEntry(EntryInput{trigger, policy, active, suppressed})
					// The phase is always the raw trigger, so no session can be wedged in I; only P is ever promoted.
					if got.Phase != trigger || got.AdviseInterview && trigger != "P" {
						t.Errorf("%s/%q: %+v", policy, trigger, got)
					}
				}
			}
		}
	}
}

func TestReadPolicy(t *testing.T) {
	if got := ReadPolicy(repoWith(t)); got != PolicyNewUnit || DefaultPolicy != PolicyNewUnit {
		t.Errorf("no crw.json: %s", got)
	}
	for contents, want := range map[string]Policy{
		"{ not json": PolicyNewUnit, `{"interview":"off"}x`: PolicyNewUnit, `{"interview":"off"`: PolicyNewUnit, "[]": PolicyNewUnit, "null": PolicyNewUnit, `{"interview":"bogus"}`: PolicyNewUnit, `{"interview":42}`: PolicyNewUnit, "{}": PolicyNewUnit,
		`{"interview":"off"}`: PolicyOff, `{"interview":"always"}`: PolicyAlways, `{"interview":"new-unit"}`: PolicyNewUnit,
		`{"interview":"off","x":1e999}`: PolicyOff, // JSON.parse reads 1e999; json.Unmarshal into a float fails
	} {
		if got := ReadPolicy(repoWith(t, contents)); got != want {
			t.Errorf("%s: %s, want %s", contents, got, want)
		}
	}
	for value, want := range map[string]bool{"off": true, "new-unit": true, "always": true, "Off": false, "": false} {
		if IsPolicy(value) != want {
			t.Errorf("IsPolicy(%q) = %v", value, !want)
		}
	}
}

// A recognized CRW_PABCD overrides the project file; any other state of the file enables PABCD.
func TestPabcdEnabled(t *testing.T) {
	disabled, enabled, absent := repoWith(t, `{"pabcd":{"enabled":false}}`), repoWith(t, `{"pabcd":{"enabled":true}}`), repoWith(t)
	for value, want := range map[string]bool{"off": false, "  OFF ": false, "0": false, "false": false, " FALSE ": false, "on": true, " ON ": true, "1": true, "true": true, " TRUE ": true} {
		for _, dir := range []string{disabled, enabled, absent} {
			if got := PabcdEnabled(dir, env(map[string]string{PabcdEnv: value})); got != want {
				t.Errorf("%q in %s: %v", value, dir, got)
			}
		}
	}
	for _, vars := range []map[string]string{{PabcdEnv: ""}, {PabcdEnv: "other"}, {}} {
		if PabcdEnabled(disabled, env(vars)) || !PabcdEnabled(enabled, env(vars)) || !PabcdEnabled(absent, env(vars)) {
			t.Errorf("fallback to the file with %v", vars)
		}
	}
	for contents, want := range map[string]bool{"{ broken": true, "[]": true, "null": true, `{"pabcd":[]}`: true, `{"pabcd":{"enabled":"false"}}`: true, `{"pabcd":{"enabled":false}}x`: true, `{"pabcd":{"enabled":false},"x":1e999}`: false} {
		if PabcdEnabled(repoWith(t, contents), env(nil)) != want {
			t.Errorf("%s: want %v", contents, want)
		}
	}
}

func TestWritePolicyRoundTripAndFailure(t *testing.T) {
	dir := repoWith(t)
	if res, err := WritePolicy(dir, PolicyAlways); err != nil || res.ReplacedMalformed || res.Path != ConfigPath(dir) || ReadPolicy(dir) != PolicyAlways {
		t.Fatalf("round trip: %+v, %v", res, err)
	}
	dir = repoWith(t)
	if err := os.Mkdir(ConfigPath(dir), 0o755); err != nil {
		t.Fatal(err)
	}
	if res, err := WritePolicy(dir, PolicyOff); err == nil || res != (WriteResult{}) {
		t.Errorf("writing over a directory: %+v, %v", res, err)
	}
}

func TestWritePolicyRewritesTheFileAsJSONStringifyDoes(t *testing.T) {
	// Each pair is a crw.json and what the oracle's writeInterviewPolicy(cwd, "always") leaves in it, as
	// Node v24 printed it for JSON.stringify({...JSON.parse(text), interview: "always"}, null, 2).
	// REPLACED is a file that is not a JSON object: it is replaced and the caller is told.
	writeCases := []struct{ in, want string }{
		{"{\"somethingElse\":{\"nested\":1,\"list\":[1.0,\"<&>\",{}],\"big\":1e21,\"neg\":-0},\"interview\":\"off\",\"pabcd\":{\"enabled\":false}}", "{\n  \"somethingElse\": {\n    \"nested\": 1,\n    \"list\": [\n      1,\n      \"<&>\",\n      {}\n    ],\n    \"big\": 1e+21,\n    \"neg\": 0\n  },\n  \"interview\": \"always\",\n  \"pabcd\": {\n    \"enabled\": false\n  }\n}\n"},
		{"{\"b\":1,\"10\":2,\"2\":3,\"01\":4}", "{\n  \"2\": 3,\n  \"10\": 2,\n  \"b\": 1,\n  \"01\": 4,\n  \"interview\": \"always\"\n}\n"},
		{"{\"keep\":\"\\ud800\",\"interview\":\"off\"}", "{\n  \"keep\": \"\\ud800\",\n  \"interview\": \"always\"\n}\n"},
		{"{\"\\ud800\":1,\"\\ufffd\":2}", "{\n  \"\\ud800\": 1,\n  \"�\": 2,\n  \"interview\": \"always\"\n}\n"},
		{"{\"keep\":\"\u2028\u2029 \\u2028\"}", "{\n  \"keep\": \"\u2028\u2029 \u2028\",\n  \"interview\": \"always\"\n}\n"},
		{"{\"a\":[\"x\\\"y\",\"\",{\"b\":\"\\\\\"}],\"c\":\"\",\"d\":[],\"e\":{},\"f\":[[],[1,[2]]]}", "{\n  \"a\": [\n    \"x\\\"y\",\n    \"\",\n    {\n      \"b\": \"\\\\\"\n    }\n  ],\n  \"c\": \"\",\n  \"d\": [],\n  \"e\": {},\n  \"f\": [\n    [],\n    [\n      1,\n      [\n        2\n      ]\n    ]\n  ],\n  \"interview\": \"always\"\n}\n"},
		{"{\"ctl\":\"\\u0001\\b\\f\\n\\r\\t\\u001f\\u007f\",\"slash\":\"\\/\",\"esc\":\"\\u00e9\\ud83d\\ude00\"}", "{\n  \"ctl\": \"\\u0001\\b\\f\\n\\r\\t\\u001f\x7f\",\n  \"slash\": \"/\",\n  \"esc\": \"é😀\",\n  \"interview\": \"always\"\n}\n"},
		{"{\"x\":1e999,\"y\":-1e999,\"z\":1e-999,\"w\":123456789012345680000,\"v\":0.000001,\"u\":1e-7}", "{\n  \"x\": null,\n  \"y\": null,\n  \"z\": 0,\n  \"w\": 123456789012345680000,\n  \"v\": 0.000001,\n  \"u\": 1e-7,\n  \"interview\": \"always\"\n}\n"},
		{"{\"dup\":1,\"other\":2,\"dup\":3}", "{\n  \"dup\": 3,\n  \"other\": 2,\n  \"interview\": \"always\"\n}\n"},
		{"\"just a string\"", "REPLACED"},
		{"[1,2]", "REPLACED"},
		{"{ not json", "REPLACED"},
		{"", "REPLACED"},
		{"{\"\xed\xa0\x80\":1,\"\\ud800\":2}", "{\n  \"���\": 1,\n  \"\\ud800\": 2,\n  \"interview\": \"always\"\n}\n"},
		{"{\"\x80\":1,\"\x80\x80\":2}", "{\n  \"�\": 1,\n  \"��\": 2,\n  \"interview\": \"always\"\n}\n"},
		{"{\"k\":\"\xe2\x82\",\"j\":\"\xf0\x9f\x98\"}", "{\n  \"k\": \"�\",\n  \"j\": \"�\",\n  \"interview\": \"always\"\n}\n"},
		{"{\"k\":\"\xc0\xaf\xffx\xe2\x82A\"}", "{\n  \"k\": \"���x�A\",\n  \"interview\": \"always\"\n}\n"},
	}
	for _, c := range writeCases {
		want, replaced := c.want, c.want == "REPLACED"
		if replaced {
			want = "{\n  \"interview\": \"always\"\n}\n"
		}
		dir := repoWith(t, c.in)
		res, err := WritePolicy(dir, PolicyAlways)
		if got, _ := os.ReadFile(ConfigPath(dir)); err != nil || string(got) != want || res.ReplacedMalformed != replaced {
			t.Errorf("%q:\n got %q\nwant %q (replaced %v, err %v)", c.in, got, want, res.ReplacedMalformed, err)
		}
	}
}
