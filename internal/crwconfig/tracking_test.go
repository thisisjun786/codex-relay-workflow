package crwconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
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
	for _, want := range []string{file.path, `"tracking"`, `"kind": "github"`, `"repo": "OWNER/NAME"`, "manage.repository", "crw config check"} {
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
		// a store socket path must stay under the bind limit, so the relay state root is short
		relay, err := os.MkdirTemp("/tmp", "rs")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(relay) })
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
func TestMissingTrackingWarnsInConfigCheckOnly(t *testing.T) {
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
	for _, want := range []string{path, `"kind": "github"`, "OWNER/NAME"} {
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
	if err != nil || find(pre, CheckTrackingMissing, "tracking") != nil || pre.Tracking != nil {
		t.Errorf("Preflight on a file without the section: %v, %+v", err, pre.Findings)
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
	if len(g.calls) != 2 || strings.Join(g.calls[0], " ") != "auth status --hostname github.com" || strings.Join(g.calls[1], " ") != "api repos/thisisjun786/crw-work" {
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
