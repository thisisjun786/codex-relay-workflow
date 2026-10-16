package crwconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/tmploc/tmploctest"
)

// CRW-1341: the tracking section of the configuration file names the repository that holds the
// work tracking, apart from the code repository (manage.repository).

// trackingFile loads a document under a fresh home.
func trackingFile(t *testing.T, document string) *File {
	t.Helper()
	_, env := rootsHome(t)
	file, err := Load(env, configWrite(t, document))
	if err != nil {
		t.Fatalf("Load(%s): %v", document, err)
	}
	return file
}

// A tracking section written with WithTracking is read back as written, beside the other
// sections of the document, which keep their values; the setting is apart from manage.repository.
func TestTrackingRoundTrip(t *testing.T) {
	for _, want := range []Tracking{
		{Kind: TrackingGitHub, Repo: "thisisjun786/crw-work"},
		{Kind: TrackingLinear},
	} {
		doc, err := WithTracking([]byte(`{"schema":"crw-config/1","manage":{"repository":"thisisjun786/codex-relay-workflow","parents":["p"]},"paths":{}}`), want)
		if err != nil {
			t.Fatalf("WithTracking(%+v): %v", want, err)
		}
		file := trackingFile(t, string(doc))
		got, err := file.Tracking()
		if err != nil || got != want {
			t.Errorf("round trip of %+v = %+v, %v\n%s", want, got, err, doc)
		}
		var manage struct {
			Repository string   `json:"repository"`
			Parents    []string `json:"parents"`
		}
		if err := file.Section("manage", &manage); err != nil || manage.Repository != "thisisjun786/codex-relay-workflow" || len(manage.Parents) != 1 {
			t.Errorf("the manage section changed: %+v, %v\n%s", manage, err, doc)
		}
		if want.Repo == manage.Repository {
			t.Errorf("tracking repo and code repository are one: %q", want.Repo)
		}
	}
	// no document yet: the setting is written into a new object
	doc, err := WithTracking(nil, Tracking{Kind: TrackingGitHub, Repo: "o/n"})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := trackingFile(t, string(doc)).Tracking(); err != nil || got.Repo != "o/n" {
		t.Errorf("new document: %+v, %v\n%s", got, err, doc)
	}
	// a document that is not an object is refused, and an invalid setting is not written
	if _, err := WithTracking([]byte(`[1]`), Tracking{Kind: TrackingGitHub, Repo: "o/n"}); err == nil {
		t.Error("WithTracking wrote into a document that is not an object")
	}
	if _, err := WithTracking([]byte(`{}`), Tracking{Kind: TrackingGitHub, Repo: "not a repo"}); err == nil {
		t.Error("WithTracking wrote an invalid repo")
	}
}

// Defaults: a file without the section has no tracking, with the message that says what to set; a
// section that names a repo and no kind is github; linear needs no repo.
func TestTrackingDefaults(t *testing.T) {
	file := trackingFile(t, `{}`)
	_, err := file.Tracking()
	if !errors.Is(err, ErrTrackingMissing) {
		t.Fatalf("a file without the section: %v", err)
	}
	for _, want := range []string{file.path, "crw config tracking --repo OWNER/NAME --config ", "manage.repository", "crw config check --config "} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the missing message lacks %q: %s", want, err)
		}
	}
	got, err := trackingFile(t, `{"tracking":{"repo":"o/n"}}`).Tracking()
	if err != nil || got != (Tracking{Kind: TrackingGitHub, Repo: "o/n"}) {
		t.Errorf("repo without a kind: %+v, %v", got, err)
	}
	got, err = trackingFile(t, `{"tracking":{"kind":"linear"}}`).Tracking()
	if err != nil || got != (Tracking{Kind: TrackingLinear}) {
		t.Errorf("linear: %+v, %v", got, err)
	}
	// the code repository is not the tracking repository
	if _, err := trackingFile(t, `{"manage":{"repository":"o/code"}}`).Tracking(); !errors.Is(err, ErrTrackingMissing) {
		t.Errorf("manage.repository stood in for the tracking repo: %v", err)
	}
}

// The repo spelling is OWNER/NAME, as GitHub spells it.
func TestTrackingRepoSpelling(t *testing.T) {
	for _, repo := range []string{"thisisjun786/crw-work", "a/b", "o-1/n.a_m-e", "Owner/Name.js", "o/.github", "o/a..b"} {
		if err := ValidateRepo(repo); err != nil {
			t.Errorf("ValidateRepo(%q) = %v", repo, err)
		}
	}
	for _, repo := range []string{
		"", "crw-work", "/crw-work", "o/", "o/n/x", "https://github.com/o/n", "github.com/o/n", "git@github.com:o/n.git",
		"o/n.git", "o/.", "o/..", "-o/n", "o-/n", "o o/n", "o/n m", "o/n\n", "o/n?x", "o_x/n", strings.Repeat("a", 40) + "/n", "o/" + strings.Repeat("n", 101),
	} {
		err := ValidateRepo(repo)
		if err == nil {
			t.Errorf("ValidateRepo(%q) accepted it", repo)
			continue
		}
		if !strings.Contains(err.Error(), "OWNER/NAME") {
			t.Errorf("the refusal of %q does not show the spelling: %v", repo, err)
		}
		doc := fmt.Sprintf(`{"tracking":{"kind":"github","repo":%q}}`, repo)
		if _, err := trackingFile(t, doc).Tracking(); err == nil || errors.Is(err, ErrTrackingMissing) {
			t.Errorf("the section with repo %q was read: %v", repo, err)
		}
	}
}

// A section that is not usable is told apart from a missing one, and says what to set.
func TestTrackingInvalidSections(t *testing.T) {
	for name, section := range map[string]string{
		"a string":          `"o/n"`,
		"null":              `null`,
		"an array":          `[]`,
		"an empty object":   `{}`,
		"github, no repo":   `{"kind":"github"}`,
		"unknown kind":      `{"kind":"gitlab","repo":"o/n"}`,
		"kind not a string": `{"kind":7,"repo":"o/n"}`,
		"repo not a string": `{"kind":"github","repo":{"a":1}}`,
		"linear with repo":  `{"kind":"linear","repo":"o/n"}`,
		"bad spelling":      `{"kind":"github","repo":"https://github.com/o/n"}`,
	} {
		_, err := trackingFile(t, `{"tracking":`+section+`}`).Tracking()
		if err == nil || errors.Is(err, ErrTrackingMissing) {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if !strings.Contains(err.Error(), `"kind": "github"`) || !strings.Contains(err.Error(), "OWNER/NAME") {
			t.Errorf("%s: the message does not say what to set: %v", name, err)
		}
	}
}

// A malformed tracking section never makes the shared configuration refuse for another reader: the
// state selection, the tool roots, crw config paths and show and the other sections read as before.
// It is a warning in crw config check and in the storage report of crw doctor, never a fault.
func TestMalformedTrackingNeverStopsAnotherReader(t *testing.T) {
	saved := runProbe
	runProbe = roomy()
	t.Cleanup(func() { runProbe = saved })
	savedTracking := runTrackingProbe
	runTrackingProbe = func(string) (TrackingAccess, error) {
		t.Error("a malformed section was probed")
		return TrackingAccess{}, nil
	}
	t.Cleanup(func() { runTrackingProbe = savedTracking })
	for name, section := range map[string]string{
		"string": `"o/n"`, "null": `null`, "array": `[]`, "empty": `{}`, "bad kind": `{"kind":"x"}`, "bad repo": `{"kind":"github","repo":"nope"}`,
		"kind type": `{"kind":[1]}`,
	} {
		home, env := rootsHome(t)
		build := home + "/build"
		// a store socket path must stay under the bind limit, so the relay state root is a socket
		// directory (never below TMPDIR)
		relay := tmploctest.SocketDir(t, "rs")
		path := configWriteAt(t, home+"/c/config.json", `{"tracking":`+section+`,"paths":{"build_cache":"`+build+`","relay_state":"`+relay+`"},"manage":{"repository":"o/code"},"sandbox":{"project_roots":["`+home+`/p"]}}`)
		envC := func(k string) string {
			if k == "CRW_CONFIG" {
				return path
			}
			return env(k)
		}
		if _, err := Load(envC, ""); err != nil {
			t.Errorf("%s: Load refused: %v", name, err)
		}
		if _, err := load(envC, "", false); err != nil {
			t.Errorf("%s: the non-strict load refused: %v", name, err)
		}
		tools, err := LoadToolRoots(envC, "")
		if err != nil || tools.BuildCache != build {
			t.Errorf("%s: tool roots %+v, %v", name, tools, err)
		}
		if root, err := ConfiguredRelayState(envC); err != nil || root != relay {
			t.Errorf("%s: relay state %q, %v", name, root, err)
		}
		if err := RelayStateSplit(envC); err != nil {
			t.Errorf("%s: relay split: %v", name, err)
		}
		for _, verb := range []string{"paths", "show"} {
			if code, _, errOut := configRun(t, envC, verb); code != 0 {
				t.Errorf("%s: crw config %s exit %d: %s", name, verb, code, errOut)
			}
		}
		code, out, errOut := configRun(t, envC, "check")
		if code != 0 {
			t.Errorf("%s: crw config check exit %d (stderr %q)\n%s", name, code, errOut, out)
		}
		var report Report
		if err := json.Unmarshal([]byte(out), &report); err != nil {
			t.Fatalf("%s: %v\n%s", name, err, out)
		}
		mustFind(t, report, CheckTrackingInvalid, "tracking", LevelWarning)
		if len(report.Faults()) != 0 {
			t.Errorf("%s: faults %+v", name, report.Faults())
		}
		// the storage report of crw doctor and crw install
		pre, err := Preflight(envC)
		if err != nil {
			t.Errorf("%s: Preflight refused: %v", name, err)
		}
		f := mustFind(t, pre, CheckTrackingInvalid, "tracking", LevelWarning)
		if !strings.Contains(f.Message, "OWNER/NAME") {
			t.Errorf("%s: the warning does not say what to set: %s", name, f.Message)
		}
		if !strings.Contains(fmt.Sprint(pre.FindingsOrdered()), CheckTrackingInvalid) {
			t.Errorf("%s: the ordered report lacks the finding", name)
		}
	}
}

// A missing section is a warning in crw config check and not in the storage report of crw doctor and
// crw install, which a host with no tracking setting yet runs on every start.
// CRW-1577: crw config tracking writes the section into the file it is pointed at (the default when
// no --config is given, an explicit path, or through a symbolic link), preserving every other
// section, and after it the preflight reports no missing tracking. It probes nothing: the repository
// check stays crw config check's.
func TestTrackingSetupCommandRepairsMissingConfiguration(t *testing.T) {
	saved := runTrackingProbe
	runTrackingProbe = func(string) (TrackingAccess, error) {
		t.Fatal("the setter must not probe")
		return TrackingAccess{}, nil
	}
	t.Cleanup(func() { runTrackingProbe = saved })
	home, env := rootsHome(t)
	path := configWriteAt(t, home+"/c/config.json", `{"manage":{"repository":"o/code"},"paths":{"build_cache":"`+home+`/b"},"custom":{"keep":"me"}}`)
	envC := func(k string) string {
		if k == "CRW_CONFIG" {
			return path
		}
		return env(k)
	}
	pre, err := Preflight(envC)
	if err != nil {
		t.Fatal(err)
	}
	miss := mustFind(t, pre, CheckTrackingMissing, "tracking", LevelWarning)
	if !strings.Contains(miss.Message, "crw config tracking --repo OWNER/NAME --config ") || !strings.Contains(miss.Message, path) {
		t.Fatalf("the missing warning lacks the repair command: %s", miss.Message)
	}
	code, out, errOut := configRun(t, envC, "tracking", "--repo", "o/work", "--config", path)
	if code != 0 {
		t.Fatalf("crw config tracking: exit %d\n%s%s", code, out, errOut)
	}
	file, err := Load(envC, path)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := file.Tracking(); err != nil || got != (Tracking{Kind: TrackingGitHub, Repo: "o/work"}) {
		t.Fatalf("tracking = %+v, %v", got, err)
	}
	if pre, err = Preflight(envC); err != nil || find(pre, CheckTrackingMissing, "tracking") != nil {
		t.Errorf("after the repair: %v %+v", err, pre.Findings)
	}
	raw, _ := os.ReadFile(path)
	for _, want := range []string{`"o/code"`, `"build_cache"`, `"keep"`, `"o/work"`, `"github"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("the file lost %q:\n%s", want, raw)
		}
	}

	// the file the command writes: the default (none yet), a name the shell would quote, a symlink
	decl, err := Load(env, "")
	if err != nil {
		t.Fatal(err)
	}
	weird := home + "/a b's $x.json"
	configWriteAt(t, weird, `{}`)
	real := configWriteAt(t, home+"/real.json", `{}`)
	link := home + "/link.json"
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		args []string
		read func() (*File, error)
	}{
		"the default file":         {[]string{"tracking", "--repo", "o/n"}, func() (*File, error) { return Load(env, "") }},
		"a path that quotes oddly": {[]string{"tracking", "--repo", "o/n", "--config", weird}, func() (*File, error) { return Load(env, weird) }},
		"a symbolic link":          {[]string{"tracking", "--repo", "o/n", "--config", link}, func() (*File, error) { return Load(env, real) }},
	} {
		t.Run(name, func(t *testing.T) {
			if code, out, errOut := configRun(t, env, c.args...); code != 0 {
				t.Fatalf("exit %d\n%s%s", code, out, errOut)
			}
			f, err := c.read()
			if err != nil {
				t.Fatal(err)
			}
			if got, err := f.Tracking(); err != nil || got.Repo != "o/n" || got.Kind != TrackingGitHub {
				t.Fatalf("tracking = %+v, %v", got, err)
			}
			if name == "a symbolic link" {
				if info, err := os.Lstat(link); err != nil || info.Mode()&os.ModeSymlink == 0 {
					t.Errorf("the symlink was replaced")
				}
			}
		})
	}
	_ = decl

	// a bad request writes nothing
	for name, args := range map[string][]string{
		"no repo":        {"tracking", "--config", path},
		"empty repo":     {"tracking", "--repo", "", "--config", path},
		"not OWNER/NAME": {"tracking", "--repo", "not a repo", "--config", path},
		"no slash":       {"tracking", "--repo", "work", "--config", path},
		"extra arg":      {"tracking", "--repo", "o/n", "--config", path, "x"},
		"unknown flag":   {"tracking", "--wat", "--config", path},
	} {
		t.Run(name, func(t *testing.T) {
			if code, _, _ := configRun(t, envC, args...); code == 0 {
				t.Errorf("%v exited 0", args)
			}
			if after, _ := os.ReadFile(path); string(after) != string(raw) {
				t.Errorf("%v rewrote the file", args)
			}
		})
	}
}

func TestMissingTrackingWarnsInConfigCheckAndPreflight(t *testing.T) {
	saved := runProbe
	runProbe = roomy()
	t.Cleanup(func() { runProbe = saved })
	_, env := rootsHome(t)
	path := configWrite(t, `{}`)
	code, out, _ := configRun(t, env, "check", "--config", path)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	var report Report
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatal(err)
	}
	f := mustFind(t, report, CheckTrackingMissing, "tracking", LevelWarning)
	for _, want := range []string{path, "crw config tracking --repo OWNER/NAME --config ", "OWNER/NAME"} {
		if !strings.Contains(f.Message, want) {
			t.Errorf("the warning lacks %q: %s", want, f.Message)
		}
	}
	envC := func(k string) string {
		if k == "CRW_CONFIG" {
			return path
		}
		return env(k)
	}
	pre, err := Preflight(envC)
	if err != nil || pre.Tracking == nil || pre.Tracking.Set {
		t.Errorf("Preflight on a file without the section: %v, %+v", err, pre.Tracking)
	}
	if f := mustFind(t, pre, CheckTrackingMissing, "tracking", LevelWarning); !strings.Contains(f.Message, "crw config tracking") {
		t.Errorf("the Preflight warning lacks the repair command: %s", f.Message)
	}
}

func accessOK() TrackingAccess {
	return TrackingAccess{Authenticated: true, HasIssues: true, Permissions: TrackingPermissions{Push: true, Triage: true, Pull: true}}
}

// crw config check judges the configured repository through the probe, never the host: a repository
// that allows issues to be read and written passes, and each way it cannot is a fault that says what
// to do. The probe is asked for the configured repository once.
func TestCheckTrackingThroughTheProbe(t *testing.T) {
	cases := []struct {
		name   string
		access TrackingAccess
		err    error
		check  string
		hint   string
	}{
		{name: "write access", access: accessOK()},
		{name: "triage is enough to write issues", access: TrackingAccess{Authenticated: true, HasIssues: true, Permissions: TrackingPermissions{Triage: true, Pull: true}}},
		{name: "admin", access: TrackingAccess{Authenticated: true, HasIssues: true, Permissions: TrackingPermissions{Admin: true, Maintain: true, Push: true, Triage: true, Pull: true}}},
		{name: "gh is not installed", err: ErrGHMissing, check: CheckTrackingAuth, hint: "gh"},
		{name: "gh is not signed in", err: ErrGHUnauthenticated, check: CheckTrackingAuth, hint: "gh auth login"},
		{name: "the probe saw no authentication", access: TrackingAccess{HasIssues: true}, check: CheckTrackingAuth, hint: "gh auth login"},
		{name: "repository not found", err: ErrRepoUnreachable, check: CheckTrackingAccess, hint: "o/work"},
		{name: "other probe failure", err: errors.New("dial tcp: i/o timeout"), check: CheckTrackingAccess, hint: "i/o timeout"},
		{name: "issues are disabled", access: TrackingAccess{Authenticated: true, Permissions: TrackingPermissions{Push: true, Pull: true}}, check: CheckTrackingIssues, hint: "has_issues"},
		{name: "read only", access: TrackingAccess{Authenticated: true, HasIssues: true, Permissions: TrackingPermissions{Pull: true}}, check: CheckTrackingIssues, hint: "write"},
		{name: "archived", access: TrackingAccess{Authenticated: true, HasIssues: true, Archived: true, Permissions: TrackingPermissions{Admin: true, Push: true, Triage: true, Pull: true}}, check: CheckTrackingIssues, hint: "archived"},
		{name: "no permission at all", access: TrackingAccess{Authenticated: true, HasIssues: true}, check: CheckTrackingIssues, hint: "read"},
	}
	for _, c := range cases {
		file := trackingFile(t, `{"tracking":{"kind":"github","repo":"o/work"},"manage":{"repository":"o/code"}}`)
		var asked []string
		probe := func(repo string) (TrackingAccess, error) {
			asked = append(asked, repo)
			return c.access, c.err
		}
		status, findings := CheckTracking(file, probe, true)
		if len(asked) != 1 || asked[0] != "o/work" {
			t.Errorf("%s: the probe was asked for %v", c.name, asked)
		}
		if status == nil || !status.Set || status.Kind != TrackingGitHub || status.Repo != "o/work" {
			t.Errorf("%s: status %+v", c.name, status)
		}
		if c.check == "" {
			if len(findings) != 0 {
				t.Errorf("%s: findings %+v", c.name, findings)
			}
			if status.Access == nil || !status.Access.IssuesRead || !status.Access.IssuesWrite {
				t.Errorf("%s: access %+v", c.name, status.Access)
			}
			continue
		}
		if len(findings) != 1 || findings[0].Check != c.check || findings[0].Level != LevelError || findings[0].Root != "tracking" {
			t.Errorf("%s: findings %+v, want one %s error", c.name, findings, c.check)
			continue
		}
		if !strings.Contains(findings[0].Message, c.hint) {
			t.Errorf("%s: the message lacks %q: %s", c.name, c.hint, findings[0].Message)
		}
	}
}

// kind linear is a read-only archive setting: nothing is probed and nothing is reported against it.
func TestCheckTrackingLinearIsNotProbed(t *testing.T) {
	file := trackingFile(t, `{"tracking":{"kind":"linear"}}`)
	status, findings := CheckTracking(file, func(string) (TrackingAccess, error) {
		t.Error("the linear setting was probed")
		return TrackingAccess{}, nil
	}, true)
	if len(findings) != 0 || status == nil || status.Kind != TrackingLinear || status.Access != nil {
		t.Errorf("linear: %+v, %+v", status, findings)
	}
}

// crw config check exits 0 on a passing repository and 1 on a fault, and prints the access it read.
func TestRunCheckTracking(t *testing.T) {
	saved, savedTracking := runProbe, runTrackingProbe
	runProbe = roomy()
	t.Cleanup(func() { runProbe, runTrackingProbe = saved, savedTracking })
	_, env := rootsHome(t)
	path := configWrite(t, `{"tracking":{"kind":"github","repo":"o/work"}}`)
	runTrackingProbe = func(repo string) (TrackingAccess, error) { return accessOK(), nil }
	code, out, errOut := configRun(t, env, "check", "--config", path)
	if code != 0 || errOut != "" {
		t.Fatalf("a passing repository: exit %d stderr %q\n%s", code, errOut, out)
	}
	var report struct {
		Tracking struct {
			Kind, Repo string
			Set        bool
			Access     map[string]any
		} `json:"tracking"`
		Findings []Finding `json:"findings"`
	}
	if err := json.Unmarshal([]byte(out), &report); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if report.Tracking.Kind != "github" || report.Tracking.Repo != "o/work" || !report.Tracking.Set || report.Tracking.Access["authenticated"] != true || report.Tracking.Access["issues_read"] != true || report.Tracking.Access["issues_write"] != true || len(report.Findings) != 0 {
		t.Errorf("report %+v", report)
	}
	runTrackingProbe = func(string) (TrackingAccess, error) { return TrackingAccess{}, ErrGHUnauthenticated }
	code, out, _ = configRun(t, env, "check", "--config", path)
	if code != 1 || !strings.Contains(out, CheckTrackingAuth) {
		t.Errorf("a failing repository: exit %d\n%s", code, out)
	}
}

// gh runner fakes: each call's arguments are kept, and a reply is chosen by the sub-command.
type fakeGH struct {
	calls   [][]string
	auth    error
	api     string
	apiErr  error
	missing bool
}

func (g *fakeGH) run(args ...string) ([]byte, error) {
	g.calls = append(g.calls, args)
	if g.missing {
		return nil, &exec.Error{Name: "gh", Err: exec.ErrNotFound}
	}
	if args[0] == "auth" {
		return []byte("github.com\n  Logged in as someone (GH_TOKEN)\n  Token: ghp_abcdefghijklmnopqrstuvwxyz0123456789\n"), g.auth
	}
	return []byte(g.api), g.apiErr
}

func useGH(t *testing.T, g *fakeGH) {
	t.Helper()
	t.Setenv("GH_HOST", "")
	saved := runGH
	runGH = g.run
	t.Cleanup(func() { runGH = saved })
}

const repoJSON = `{"name":"crw-work","full_name":"thisisjun786/crw-work","private":true,"archived":false,"has_issues":true,"permissions":{"admin":true,"maintain":true,"push":true,"triage":true,"pull":true}}`

// The host probe reads authentication and the repository's permissions and has_issues with two
// read-only gh calls, and writes nothing to GitHub: no method, field or input flag is ever passed.
func TestGHProbeReadsOnly(t *testing.T) {
	g := &fakeGH{api: repoJSON}
	useGH(t, g)
	access, err := ghProbe("thisisjun786/crw-work")
	if err != nil {
		t.Fatal(err)
	}
	want := TrackingAccess{Authenticated: true, Private: true, HasIssues: true, Permissions: TrackingPermissions{Admin: true, Maintain: true, Push: true, Triage: true, Pull: true}}
	if access != want {
		t.Errorf("access = %+v, want %+v", access, want)
	}
	if len(g.calls) != 2 || strings.Join(g.calls[0], " ") != "auth status --hostname github.com" || strings.Join(g.calls[1], " ") != "api --hostname github.com repos/thisisjun786/crw-work" {
		t.Errorf("gh calls: %q", g.calls)
	}
	for _, call := range g.calls {
		for _, arg := range call {
			switch arg {
			case "-X", "--method", "-f", "-F", "--field", "--raw-field", "--input", "-d", "--data":
				t.Errorf("a gh call carries the write flag %q: %q", arg, call)
			}
		}
	}
}

// Each way the host probe fails is told apart, and no token reaches a message.
func TestGHProbeFailures(t *testing.T) {
	apiFail := func(stderr string) error {
		return &ghError{message: stderr}
	}
	cases := []struct {
		name string
		g    *fakeGH
		want error
		text string
	}{
		{name: "gh missing", g: &fakeGH{missing: true}, want: ErrGHMissing},
		{name: "not signed in", g: &fakeGH{auth: apiFail("You are not logged into any GitHub hosts. To log in, run: gh auth login")}, want: ErrGHUnauthenticated},
		{name: "401", g: &fakeGH{apiErr: apiFail("gh: Bad credentials (HTTP 401)")}, want: ErrGHUnauthenticated},
		{name: "404", g: &fakeGH{apiErr: apiFail("gh: Not Found (HTTP 404)")}, want: ErrRepoUnreachable},
		{name: "other", g: &fakeGH{apiErr: apiFail("dial tcp: lookup api.github.com: no such host token ghp_abcdefghijklmnopqrstuvwxyz0123456789 github_pat_11ABCDEFG0abcdefghijklmnopqrstuvwxyz")}, text: "no such host"},
		{name: "not json", g: &fakeGH{api: "<html>"}, text: "not JSON"},
	}
	for _, c := range cases {
		useGH(t, c.g)
		_, err := ghProbe("o/n")
		if err == nil {
			t.Errorf("%s: no error", c.name)
			continue
		}
		if c.want != nil && !errors.Is(err, c.want) {
			t.Errorf("%s: %v is not %v", c.name, err, c.want)
		}
		if c.text != "" && !strings.Contains(err.Error(), c.text) {
			t.Errorf("%s: %v lacks %q", c.name, err, c.text)
		}
		if strings.Contains(err.Error(), "ghp_") || strings.Contains(err.Error(), "github_pat_") {
			t.Errorf("%s: the message carries a token: %v", c.name, err)
		}
	}
	// a repository the API returns without permissions was read without authentication
	useGH(t, &fakeGH{api: `{"has_issues":true,"private":false}`})
	if access, err := ghProbe("o/n"); err != nil || access.CanReadIssues() || access.CanWriteIssues() {
		t.Errorf("no permissions: %+v, %v", access, err)
	}
}

// the real runner: a gh that is not on PATH is exec.ErrNotFound, which the probe reports as ErrGHMissing
func TestRunGHMissingBinary(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if _, err := runGHHost("auth", "status"); !errors.Is(err, exec.ErrNotFound) {
		t.Errorf("runGHHost without gh on PATH: %v", err)
	}
}

// CRW-1359 d1: the sign-in is read on the host gh api goes to (GH_HOST, else github.com), so both
// calls carry the same --hostname; a blank GH_HOST is the default host.
func TestGHProbeAsksAuthAndAPIOnTheSameHost(t *testing.T) {
	for name, tc := range map[string]struct{ env, host string }{
		"unset": {"", "github.com"}, "blank": {"  \t", "github.com"}, "enterprise": {" ghe.example.com ", "ghe.example.com"},
	} {
		g := &fakeGH{api: repoJSON}
		useGH(t, g)
		t.Setenv("GH_HOST", tc.env)
		if _, err := ghProbe("o/n"); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want := []string{"auth status --hostname " + tc.host, "api --hostname " + tc.host + " repos/o/n"}
		if len(g.calls) != 2 || strings.Join(g.calls[0], " ") != want[0] || strings.Join(g.calls[1], " ") != want[1] {
			t.Errorf("%s: gh calls %q, want %q", name, g.calls, want)
		}
	}
}

// authSplitGH answers auth status with a failure (a second login of the host has a broken token,
// so gh auth status exits non-zero) while gh api, which uses the active login, works.
func authSplitGH(api string) *fakeGH {
	return &fakeGH{auth: &ghError{message: "github.com\n  X Failed to log in to github.com account second (keyring)\n  - The token in keyring is invalid."}, api: api}
}

// CRW-1359 d1: a broken second login of the host does not make tracking_auth wrong when the active
// login reads the repository; with no permissions in the answer, or no answer, the sign-in is told
// as the cause, with the host named.
func TestGHProbeBrokenSecondLoginDoesNotHideTheActiveOne(t *testing.T) {
	t.Setenv("GH_HOST", "")
	useGH(t, authSplitGH(repoJSON))
	access, err := ghProbe("o/n")
	if err != nil || !access.Authenticated || !access.CanWriteIssues() {
		t.Errorf("active login works: %+v, %v", access, err)
	}
	useGH(t, authSplitGH(`{"has_issues":true,"private":false}`))
	if _, err := ghProbe("o/n"); !errors.Is(err, ErrGHUnauthenticated) || !strings.Contains(err.Error(), "github.com") {
		t.Errorf("no permissions and a failed auth status: %v", err)
	}
	broken := authSplitGH("")
	broken.apiErr = &ghError{message: "gh: Bad credentials (HTTP 401)"}
	useGH(t, broken)
	if _, err := ghProbe("o/n"); !errors.Is(err, ErrGHUnauthenticated) {
		t.Errorf("a failed auth status and a failed api: %v", err)
	}
	useGH(t, &fakeGH{auth: &ghError{message: "not logged in"}})
	t.Setenv("GH_HOST", "ghe.example.com")
	if _, err := ghProbe("o/n"); !errors.Is(err, ErrGHUnauthenticated) || !strings.Contains(err.Error(), "ghe.example.com") {
		t.Errorf("the host is not named: %v", err)
	}
}

// CRW-1359 d3: an answer with no permissions at all (a fine-grained token, an app token, a token
// without the scope) is told apart from permissions that are all false, names those causes, and is a
// warning with the access recorded, never a failure.
func TestMissingPermissionsAreAWarningThatNamesTheCauses(t *testing.T) {
	useGH(t, &fakeGH{api: `{"has_issues":true,"private":true}`})
	t.Setenv("GH_HOST", "")
	access, err := ghProbe("o/work")
	if err != nil || !access.PermissionsUnreported {
		t.Fatalf("probe: %+v, %v", access, err)
	}
	file := trackingFile(t, `{"tracking":{"kind":"github","repo":"o/work"}}`)
	status, findings := CheckTracking(file, func(string) (TrackingAccess, error) { return access, nil }, true)
	if len(findings) != 1 || findings[0].Level != LevelWarning || findings[0].Check != CheckTrackingPermissions || findings[0].Root != "tracking" {
		t.Fatalf("findings %+v, want one %s warning", findings, CheckTrackingPermissions)
	}
	for _, cause := range []string{"fine-grained", "app token", "scope"} {
		if !strings.Contains(findings[0].Message, cause) {
			t.Errorf("the message lacks %q: %s", cause, findings[0].Message)
		}
	}
	if status.Access == nil || !status.Access.PermissionsUnreported || status.Access.IssuesWrite {
		t.Errorf("the state is not recorded: %+v", status.Access)
	}
	if report := (Report{Findings: findings}); len(report.Faults()) != 0 {
		t.Errorf("a failure: %+v", report.Faults())
	}
	// permissions that are reported and all false stay an error; an archived repository stays one
	_, findings = CheckTracking(file, func(string) (TrackingAccess, error) {
		return TrackingAccess{Authenticated: true, HasIssues: true}, nil
	}, true)
	if len(findings) != 1 || findings[0].Level != LevelError || findings[0].Check != CheckTrackingIssues {
		t.Errorf("all-false permissions: %+v", findings)
	}
	_, findings = CheckTracking(file, func(string) (TrackingAccess, error) {
		return TrackingAccess{Authenticated: true, HasIssues: true, Archived: true, PermissionsUnreported: true}, nil
	}, true)
	if len(findings) != 1 || findings[0].Level != LevelError || !strings.Contains(findings[0].Message, "archived") {
		t.Errorf("archived without permissions: %+v", findings)
	}
}

// CRW-1359 d2: WithTracking keeps the keys of the document in the order they have, puts a new
// tracking section last and replaces an old one in place, and writes <, > and & as they are.
func TestWithTrackingKeepsKeyOrderAndDoesNotEscapeHTML(t *testing.T) {
	doc := `{"zeta":{"note":"a <b> & c","list":[1,2]},"schema":"crw-config/1","alpha":"x>y","tracking":{"kind":"linear"},"mid":null}`
	out, err := WithTracking([]byte(doc), Tracking{Kind: TrackingGitHub, Repo: "o/n"})
	if err != nil {
		t.Fatal(err)
	}
	text := string(out)
	var order []string
	for _, key := range []string{`"zeta"`, `"schema"`, `"alpha"`, `"tracking"`, `"mid"`} {
		order = append(order, key)
		if i := strings.Index(text, key); i < 0 {
			t.Fatalf("%s is gone:\n%s", key, text)
		}
	}
	last := -1
	for _, key := range order {
		i := strings.Index(text, key+":")
		if i <= last {
			t.Errorf("%s is out of order (document order zeta, schema, alpha, tracking, mid):\n%s", key, text)
		}
		last = i
	}
	for _, literal := range []string{`a <b> & c`, `x>y`} {
		if !strings.Contains(text, literal) {
			t.Errorf("%q was escaped:\n%s", literal, text)
		}
	}
	if strings.Contains(text, `\u003c`) || strings.Contains(text, `\u0026`) || strings.Contains(text, `\u003e`) {
		t.Errorf("HTML escapes in the output:\n%s", text)
	}
	if !strings.HasSuffix(text, "}\n") {
		t.Errorf("no final newline:\n%s", text)
	}
	got, err := trackingFile(t, text).Tracking()
	if err != nil || got != (Tracking{Kind: TrackingGitHub, Repo: "o/n"}) {
		t.Errorf("round trip: %+v, %v", got, err)
	}
	// a new section goes last; writing twice is the same document
	added, err := WithTracking([]byte(`{"b":1,"a":"<&>"}`), Tracking{Kind: TrackingLinear})
	if err != nil {
		t.Fatal(err)
	}
	if b, a, tr := strings.Index(string(added), `"b"`), strings.Index(string(added), `"a"`), strings.Index(string(added), `"tracking"`); !(b < a && a < tr) || !strings.Contains(string(added), `"<&>"`) {
		t.Errorf("a new section:\n%s", added)
	}
	again, err := WithTracking(added, Tracking{Kind: TrackingLinear})
	if err != nil || string(again) != string(added) {
		t.Errorf("not idempotent: %v\n%s\n%s", err, added, again)
	}
}

// CRW-1359 d2: a document that is not one JSON object is refused, and nothing is written for it.
func TestWithTrackingRefusesADocumentThatIsNotAnObject(t *testing.T) {
	for _, doc := range []string{`null`, `[1]`, `"s"`, `7`, `{`, `{"a":1} {"b":2}`, `{"a":1,}`, `{"a"}`} {
		if out, err := WithTracking([]byte(doc), Tracking{Kind: TrackingGitHub, Repo: "o/n"}); err == nil || out != nil {
			t.Errorf("%q: written %q, %v", doc, out, err)
		}
	}
	for _, doc := range []string{``, " \n", `{}`} {
		out, err := WithTracking([]byte(doc), Tracking{Kind: TrackingGitHub, Repo: "o/n"})
		if err != nil {
			t.Errorf("%q: %v", doc, err)
			continue
		}
		if got, err := trackingFile(t, string(out)).Tracking(); err != nil || got.Repo != "o/n" {
			t.Errorf("%q: %+v, %v", doc, got, err)
		}
	}
}
