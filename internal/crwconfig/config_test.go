package crwconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// configRun runs the command with a getenv and returns the exit status and both streams.
func configRun(t *testing.T, getenv func(string) string, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut strings.Builder
	code := Run(args, &out, &errOut, getenv)
	return code, out.String(), errOut.String()
}

// configWrite writes a configuration document to a fresh file and returns its path.
func configWrite(t *testing.T, document string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// configWriteAt writes a document to a path, creating its directories.
func configWriteAt(t *testing.T, path, document string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// configDefaultPath is where the default location resolves to under one home.
func configDefaultPath(home string) string {
	return filepath.Join(home, ".config", "crw", "config.json")
}

// C1: a file that is not there leaves the defaults in place, whichever way the path was
// reached, and reports no error.
func TestAMissingFileIsTheDefaults(t *testing.T) {
	home, env := rootsHome(t)
	missing := filepath.Join(home, "absent.json")
	for _, test := range []struct {
		name string
		env  func(string) string
		flag string
	}{
		{"the flag", env, missing},
		{"CRW_CONFIG", rootsEnv("HOME", home, "CRW_CONFIG", missing), ""},
		{"the default location", env, ""},
	} {
		file, err := Load(test.env, test.flag)
		if err != nil {
			t.Errorf("%s: %v", test.name, err)
			continue
		}
		rootsCheck(t, file.Roots(), rootsDefaults(home), SourceDefault)
	}
}

// C1: the flag names the file ahead of CRW_CONFIG, and each names where the path came from.
func TestTheFlagBeatsTheEnvironmentAndEachNamesItsSource(t *testing.T) {
	home := t.TempDir()
	fromEnv := configWrite(t, `{"paths": {"tools_root": "/from/env"}}`)
	env := rootsEnv("HOME", home, "CRW_CONFIG", fromEnv)
	file, err := Load(env, configWrite(t, `{"paths": {"tools_root": "/from/flag"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := file.Roots()[RootTools]; got.Path != "/from/flag" || got.Source != SourceConfig {
		t.Errorf("tools_root = %+v, want the flag's file", got)
	}
	if _, source := file.Path(); source != SourceConfig {
		t.Errorf("Path() source = %q, want config for a file named with --config", source)
	}
	file, err = Load(env, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := file.Roots()[RootTools]; got.Path != "/from/env" || got.Source != SourceConfig {
		t.Errorf("tools_root = %+v, want CRW_CONFIG's file", got)
	}
	if path, source := file.Path(); path != fromEnv || source != SourceEnv {
		t.Errorf("Path() = %q %q, want CRW_CONFIG's path from env", path, source)
	}
}

// C1: a file at the default location is read, and XDG_CONFIG_HOME moves that location and
// names the source env.
func TestTheDefaultLocationIsRead(t *testing.T) {
	home, env := rootsHome(t)
	path := configWriteAt(t, configDefaultPath(home), `{"paths": {"tools_root": "/from/default"}}`)
	file, err := Load(env, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := file.Roots()[RootTools]; got.Path != "/from/default" || got.Source != SourceConfig {
		t.Errorf("tools_root = %+v, want the default location's file", got)
	}
	if gotPath, source := file.Path(); gotPath != path || source != SourceDefault {
		t.Errorf("Path() = %q %q, want the default location from default", gotPath, source)
	}
	xdg := t.TempDir()
	env = rootsEnv("HOME", home, "XDG_CONFIG_HOME", xdg)
	moved := configWriteAt(t, filepath.Join(xdg, "crw", "config.json"), `{"paths": {"tools_root": "/from/xdg"}}`)
	file, err = Load(env, "")
	if err != nil {
		t.Fatal(err)
	}
	if got := file.Roots()[RootTools]; got.Path != "/from/xdg" || got.Source != SourceConfig {
		t.Errorf("tools_root = %+v, want XDG_CONFIG_HOME's file", got)
	}
	if gotPath, source := file.Path(); gotPath != moved || source != SourceEnv {
		t.Errorf("Path() = %q %q, want XDG_CONFIG_HOME's path from env", gotPath, source)
	}
}

// C1: CRW_CONFIG beats a file that is present at the default location.
func TestTheEnvironmentBeatsTheDefaultLocation(t *testing.T) {
	home := t.TempDir()
	configWriteAt(t, configDefaultPath(home), `{"paths": {"tools_root": "/from/default"}}`)
	fromEnv := configWrite(t, `{"paths": {"tools_root": "/from/env"}}`)
	file, err := Load(rootsEnv("HOME", home, "CRW_CONFIG", fromEnv), "")
	if err != nil {
		t.Fatal(err)
	}
	if got := file.Roots()[RootTools]; got.Path != "/from/env" || got.Source != SourceConfig {
		t.Errorf("tools_root = %+v, want CRW_CONFIG's file", got)
	}
	if path, source := file.Path(); path != fromEnv || source != SourceEnv {
		t.Errorf("Path() = %q %q, want CRW_CONFIG's path from env", path, source)
	}
}

// C1: a file that is there must be a JSON object.
func TestANonObjectDocumentIsRefused(t *testing.T) {
	_, env := rootsHome(t)
	for _, document := range []string{"[]", `"a string"`, "null", "42"} {
		if _, err := Load(env, configWrite(t, document)); err == nil {
			t.Errorf("%s: accepted as a configuration", document)
		}
	}
}

// C1: a file's schema, when it carries one, must be this package's; a document without a
// schema key is accepted.
func TestTheSchemaGate(t *testing.T) {
	home, env := rootsHome(t)
	file, err := Load(env, configWrite(t, `{"schema": "crw-config/1"}`))
	if err != nil {
		t.Fatalf("the schema this package declares: %v", err)
	}
	rootsCheck(t, file.Roots(), rootsDefaults(home), SourceDefault)
	if _, err := Load(env, configWrite(t, `{"paths": {"tools_root": "/no/schema"}}`)); err != nil {
		t.Errorf("a document without a schema key: %v", err)
	}
	for _, document := range []string{`{"schema": "crw-config/2"}`, `{"schema": 2}`, `{"schema": null}`, `{"schema": {}}`} {
		if _, err := Load(env, configWrite(t, document)); err == nil {
			t.Errorf("%s: accepted a foreign schema", document)
		}
	}
}

// C4: Section decodes a raw key of the document into a struct; a key the document does not
// carry leaves the value untouched and reports no error, and so does a session with no file
// at all.
func TestSectionDecodesARawKeyAndLeavesAMissingOne(t *testing.T) {
	_, env := rootsHome(t)
	file, err := Load(env, configWrite(t, `{"custom": {"answer": 42, "name": "x"}}`))
	if err != nil {
		t.Fatal(err)
	}
	var section struct {
		Answer int    `json:"answer"`
		Name   string `json:"name"`
	}
	if err := file.Section("custom", &section); err != nil {
		t.Fatal(err)
	}
	if section.Answer != 42 || section.Name != "x" {
		t.Errorf("Section read %+v", section)
	}
	untouched := struct {
		Answer int `json:"answer"`
	}{Answer: 7}
	if err := file.Section("absent", &untouched); err != nil {
		t.Fatal(err)
	}
	if untouched.Answer != 7 {
		t.Errorf("Section changed a value for an absent key: %d", untouched.Answer)
	}
	empty, err := Load(env, filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := empty.Section("custom", &untouched); err != nil || untouched.Answer != 7 {
		t.Errorf("a session with no file: %v %d", err, untouched.Answer)
	}
}

// C2: crw config paths prints every root, in the declared order, with its source.
func TestRunPathsPrintsEveryRoot(t *testing.T) {
	home, env := rootsHome(t)
	code, out, errOut := configRun(t, env, "paths")
	if code != 0 || errOut != "" {
		t.Fatalf("crw config paths: exit %d stderr %q", code, errOut)
	}
	var rows []struct {
		Name   string `json:"name"`
		Path   string `json:"path"`
		Source string `json:"source"`
	}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("the report is not JSON: %v", err)
	}
	defaults := rootsDefaults(home)
	if len(rows) != len(RootNames()) {
		t.Fatalf("the report has %d rows, want %d", len(rows), len(RootNames()))
	}
	for i, row := range rows {
		if row.Name != RootNames()[i] {
			t.Errorf("row %d is %q, want %q", i, row.Name, RootNames()[i])
		}
		if defaults[row.Name] != row.Path || row.Source != string(SourceDefault) {
			t.Errorf("row %q = %+v, want %q from default", row.Name, row, defaults[row.Name])
		}
	}
	code, out, errOut = configRun(t, env, "paths", "--config", configWrite(t, `{"paths": {"cache_root": "/paths/cache"}}`))
	if code != 0 || errOut != "" || !strings.Contains(out, `"/paths/cache"`) || !strings.Contains(out, `"config"`) {
		t.Errorf("crw config paths --config: exit %d stdout %q stderr %q", code, out, errOut)
	}
}

// C2: crw config show prints the file's path, where it came from and the document; with no
// file the document is null.
func TestRunShowPrintsTheFileAndItsDocument(t *testing.T) {
	home, env := rootsHome(t)
	path := configWrite(t, `{"schema": "crw-config/1", "paths": {"cache_root": "/shown/cache"}}`)
	code, out, errOut := configRun(t, env, "show", "--config", path)
	if code != 0 || errOut != "" {
		t.Fatalf("crw config show: exit %d stderr %q", code, errOut)
	}
	var report struct {
		Path   string         `json:"path"`
		Source string         `json:"source"`
		Config map[string]any `json:"config"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("the report is not JSON: %v", err)
	}
	if report.Path != path || report.Source != string(SourceConfig) || report.Config["schema"] != Schema {
		t.Errorf("the report = %+v, want the file, its source and its document", report)
	}
	code, out, errOut = configRun(t, env, "show", "--config", filepath.Join(home, "absent.json"))
	if code != 0 || errOut != "" || !strings.Contains(out, `"config": null`) {
		t.Errorf("show with no file: exit %d stdout %q stderr %q", code, out, errOut)
	}
}

// C2: the command's usage contract: no verb, an unknown verb, an unknown flag and a missing
// --config value print the usage and exit 2; help prints it and exits 0.
func TestRunUsageAndErrors(t *testing.T) {
	_, env := rootsHome(t)
	for _, test := range []struct {
		name   string
		args   []string
		code   int
		stdout string
		stderr string
	}{
		{"no verb", nil, usageExit, "", "usage: crw config"},
		{"unknown verb", []string{"bogus"}, usageExit, "", `invalid command "bogus"`},
		{"unknown flag", []string{"paths", "--nope"}, usageExit, "", "unexpected argument"},
		{"missing flag value", []string{"paths", "--config"}, usageExit, "", "expected one argument"},
		{"help", []string{"--help"}, 0, "usage: crw config", ""},
		{"word help", []string{"help"}, 0, "usage: crw config", ""},
	} {
		code, out, errOut := configRun(t, env, test.args...)
		if code != test.code || !strings.Contains(out, test.stdout) || !strings.Contains(errOut, test.stderr) {
			t.Errorf("%s: exit %d stdout %q stderr %q", test.name, code, out, errOut)
		}
	}
}

// C1/C2: a configuration the command cannot use is a usage error, not a report: a relative
// path, a document that is not an object and a foreign schema all exit 2.
func TestRunRefusesAnInvalidConfiguration(t *testing.T) {
	_, env := rootsHome(t)
	for _, test := range []struct {
		name     string
		document string
		want     string
	}{
		{"a relative path", `{"paths": {"tools_root": "relative"}}`, "absolute"},
		{"not an object", `[1, 2]`, "not a JSON object"},
		{"a foreign schema", `{"schema": "crw-config/2"}`, "schema"},
	} {
		code, out, errOut := configRun(t, env, "paths", "--config", configWrite(t, test.document))
		if code != usageExit || out != "" || !strings.Contains(errOut, test.want) {
			t.Errorf("%s: exit %d stdout %q stderr %q, want exit %d and %q", test.name, code, out, errOut, usageExit, test.want)
		}
	}
}

// C2: the --config=<path> spelling is the same flag.
func TestRunAcceptsTheEqualsSpelling(t *testing.T) {
	_, env := rootsHome(t)
	path := configWrite(t, `{"paths": {"cache_root": "/equals/cache"}}`)
	code, out, errOut := configRun(t, env, "paths", "--config="+path)
	if code != 0 || errOut != "" || !strings.Contains(out, "/equals/cache") {
		t.Errorf("crw config paths --config=: exit %d stdout %q stderr %q", code, out, errOut)
	}
}

// C2: an explicitly empty --config value is refused rather than silently read as no flag,
// which would report another file's configuration.
func TestRunRefusesAnEmptyConfigValue(t *testing.T) {
	_, env := rootsHome(t)
	for _, args := range [][]string{{"paths", "--config", ""}, {"paths", "--config="}} {
		code, out, errOut := configRun(t, env, args...)
		if code != usageExit || out != "" || !strings.Contains(errOut, "non-empty path") {
			t.Errorf("%v: exit %d stdout %q stderr %q", args, code, out, errOut)
		}
	}
}

// C1: a present paths key that is JSON null is refused as a section that is not an object
// of paths, while an absent key and an empty object both keep the defaults.
func TestANullPathsSectionIsRefused(t *testing.T) {
	home, env := rootsHome(t)
	code, out, errOut := configRun(t, env, "paths", "--config", configWrite(t, `{"paths": null}`))
	if code != usageExit || out != "" || !strings.Contains(errOut, "paths is not an object of paths") {
		t.Errorf("a null paths section: exit %d stdout %q stderr %q", code, out, errOut)
	}
	for _, document := range []string{`{}`, `{"paths": {}}`} {
		code, out, errOut = configRun(t, env, "paths", "--config", configWrite(t, document))
		if code != 0 || errOut != "" {
			t.Errorf("%s: exit %d stderr %q", document, code, errOut)
		}
		if !strings.Contains(out, rootsDefaults(home)[RootTools]) {
			t.Errorf("%s: the report does not carry the default tools root", document)
		}
	}
}

// C12: the default location is the configuration home's raw text plus /crw/config.json,
// joined the way the roots are, so a base that mixes a symbolic link and ".." keeps the
// meaning the filesystem gives that spelling instead of the directory filepath.Clean names.
func TestTheDefaultLocationKeepsTheRawConfigurationHome(t *testing.T) {
	home := t.TempDir()
	real := t.TempDir()
	link := filepath.Join(home, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	// The raw text mixes the link and "..", so cleaning it would name a different tree.
	raw := link + "/../link"
	env := rootsEnv("HOME", home, "XDG_CONFIG_HOME", raw)
	file, err := Load(env, "")
	if err != nil {
		t.Fatal(err)
	}
	path, source := file.Path()
	if want := raw + "/crw/config.json"; path != want {
		t.Errorf("the default location is %q, want the raw %q", path, want)
	}
	if source != SourceEnv {
		t.Errorf("the source is %q, want env", source)
	}
}
