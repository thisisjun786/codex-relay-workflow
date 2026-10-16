package crwconfig

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/quote"
)

// The tracking section names where work tracking lives, apart from the code repository
// (manage.repository):
//
//	"tracking": {"kind": "github", "repo": "OWNER/NAME"}
//
// kind "github" is the 1.0 path: the issues of that repository. kind "linear" is a read-only
// archive setting for looking the old record up and is never used to write. The section is read
// here and nowhere in load, so a section that is missing or malformed never stops another reader
// of the shared file (the state selection, the tool roots, crw config paths and show): it is told
// to the reader that asks for it, and warned about by crw config check and the storage report of
// crw doctor.
const (
	TrackingGitHub = "github"
	TrackingLinear = "linear"
)

// The tracking checks, each the stable key of one finding. They are reported on the root "tracking".
const (
	CheckTrackingMissing = "tracking_missing"
	CheckTrackingInvalid = "tracking_invalid"
	CheckTrackingAuth    = "tracking_auth"
	CheckTrackingAccess  = "tracking_access"
	CheckTrackingIssues  = "tracking_issues"
	// CheckTrackingPermissions is a warning: the repository answer carried no permissions at all, so
	// whether issues can be read and written is not known.
	CheckTrackingPermissions = "tracking_permissions"
)

// trackingRoot is the Root of a tracking finding.
const trackingRoot = "tracking"

// Tracking is the tracking section: the backend and, for github, the OWNER/NAME of the repository.
type Tracking struct {
	Kind string `json:"kind"`
	Repo string `json:"repo,omitempty"`
}

// ErrTrackingMissing is the error of a file that has no tracking section.
var ErrTrackingMissing = errors.New("tracking is not set")

const trackingExample = `"tracking": {"kind": "github", "repo": "OWNER/NAME"}`

var (
	ownerSpelling = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)
	nameSpelling  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)
)

// ValidateRepo reports whether repo is spelled OWNER/NAME the way GitHub spells a repository: an
// owner of letters, digits and inner hyphens (39 at most), a slash, and a name of letters, digits,
// dots, underscores and hyphens (100 at most) that is neither "." nor ".." nor ends in ".git".
func ValidateRepo(repo string) error {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || !ownerSpelling.MatchString(owner) || !nameSpelling.MatchString(name) || name == "." || name == ".." || strings.HasSuffix(name, ".git") {
		return fmt.Errorf("repo %q is not spelled OWNER/NAME (an owner and a repository name, no URL, no .git)", repo)
	}
	return nil
}

// normalize applies the defaults (a repo without a kind is github) and judges a setting.
func (t Tracking) normalize() (Tracking, error) {
	if t.Kind == "" {
		t.Kind = TrackingGitHub
	}
	switch t.Kind {
	case TrackingGitHub:
		if err := ValidateRepo(t.Repo); err != nil {
			return Tracking{}, err
		}
	case TrackingLinear:
		if t.Repo != "" {
			return Tracking{}, fmt.Errorf(`kind "linear" is a read-only archive setting and takes no repo (got %q)`, t.Repo)
		}
	default:
		return Tracking{}, fmt.Errorf(`kind %q is not "github" or "linear"`, t.Kind)
	}
	return t, nil
}

// Tracking is the tracking section of this file. A file without it gives ErrTrackingMissing, with
// what to set; a section that is not usable gives an error that says what to set instead. The
// section is judged here, never when the file is loaded.
func (f *File) Tracking() (Tracking, error) {
	raw, ok := f.doc["tracking"]
	if !ok {
		return Tracking{}, fmt.Errorf("%s: %w; fix: crw config tracking --repo OWNER/NAME --config %s (replace OWNER/NAME with the work-tracking repository, apart from manage.repository); then run crw config check --config %s", f.path, ErrTrackingMissing, quote.Shell(f.path), quote.Shell(f.path))
	}
	invalid := func(reason string) (Tracking, error) {
		return Tracking{}, fmt.Errorf(`%s: tracking is not usable: %s; set %s (kind "linear" is a read-only archive setting and takes no repo), then run crw config check`, f.path, reason, trackingExample)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return invalid("it is not an object")
	}
	var t Tracking
	for key, into := range map[string]*string{"kind": &t.Kind, "repo": &t.Repo} {
		if value, ok := obj[key]; ok {
			if err := json.Unmarshal(value, into); err != nil {
				return invalid(key + " is not a string")
			}
		}
	}
	t, err := t.normalize()
	if err != nil {
		return invalid(err.Error())
	}
	return t, nil
}

// WithTracking returns doc, a configuration document (nil or empty for a new one), with its tracking
// section set to t after the defaults and the spelling are judged. Every other section keeps its
// value and its place: an existing tracking section is replaced where it stands, a new one goes last.
// The document is written indented, and <, > and & are written as they are, not as \u003c, \u003e
// and \u0026, because a user reads and edits this file.
func WithTracking(doc []byte, t Tracking) ([]byte, error) {
	t, err := t.normalize()
	if err != nil {
		return nil, err
	}
	var keys []string
	sections := map[string]json.RawMessage{}
	if len(bytes.TrimSpace(doc)) > 0 {
		if keys, sections, err = orderedSections(doc); err != nil {
			return nil, err
		}
	}
	value, err := marshalLiteral(t)
	if err != nil {
		return nil, err
	}
	if _, ok := sections["tracking"]; !ok {
		keys = append(keys, "tracking")
	}
	sections["tracking"] = value
	var compact bytes.Buffer
	compact.WriteByte('{')
	for i, key := range keys {
		if i > 0 {
			compact.WriteByte(',')
		}
		name, err := marshalLiteral(key)
		if err != nil {
			return nil, err
		}
		compact.Write(name)
		compact.WriteByte(':')
		compact.Write(sections[key])
	}
	compact.WriteByte('}')
	var out bytes.Buffer
	if err := json.Indent(&out, compact.Bytes(), "", "  "); err != nil {
		return nil, err
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}

var errNotAnObject = errors.New("the configuration is not a JSON object")

// orderedSections reads doc, one JSON object, into the keys of its top level in the order they are
// written (a repeated key keeps its first place and its last value) and the raw value of each.
func orderedSections(doc []byte) ([]string, map[string]json.RawMessage, error) {
	dec := json.NewDecoder(bytes.NewReader(doc))
	if token, err := dec.Token(); err != nil || token != json.Delim('{') {
		return nil, nil, errNotAnObject
	}
	var keys []string
	sections := map[string]json.RawMessage{}
	for dec.More() {
		token, err := dec.Token()
		key, ok := token.(string)
		if err != nil || !ok {
			return nil, nil, errNotAnObject
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, nil, errNotAnObject
		}
		if _, seen := sections[key]; !seen {
			keys = append(keys, key)
		}
		sections[key] = raw
	}
	if token, err := dec.Token(); err != nil || token != json.Delim('}') {
		return nil, nil, errNotAnObject
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, nil, errNotAnObject
	}
	return keys, sections, nil
}

// marshalLiteral is v as compact JSON with <, > and & left as they are.
func marshalLiteral(v any) (json.RawMessage, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// TrackingPermissions are the repository permissions GitHub reports for the signed-in login.
type TrackingPermissions struct {
	Admin    bool `json:"admin"`
	Maintain bool `json:"maintain"`
	Push     bool `json:"push"`
	Triage   bool `json:"triage"`
	Pull     bool `json:"pull"`
}

// TrackingAccess is what a probe learned about the tracking repository: whether the host is signed
// in, and the repository's permissions, has_issues and archived flags.
type TrackingAccess struct {
	Authenticated bool                `json:"authenticated"`
	Private       bool                `json:"private"`
	Archived      bool                `json:"archived"`
	HasIssues     bool                `json:"has_issues"`
	Permissions   TrackingPermissions `json:"permissions"`
	// PermissionsUnreported: the answer had no permissions object (a fine-grained token, an app
	// token or a token without the scope gets none), which is not the same as every permission false.
	PermissionsUnreported bool `json:"permissions_unreported,omitempty"`
}

// CanReadIssues reports whether the login can read the repository's issues: Issues are on and the
// login has any permission on the repository.
func (a TrackingAccess) CanReadIssues() bool {
	p := a.Permissions
	return a.Authenticated && a.HasIssues && (p.Pull || p.Triage || p.Push || p.Maintain || p.Admin)
}

// CanWriteIssues reports whether the login can create and change issues: it can read them, the
// repository is not archived, and the login has triage permission or more.
func (a TrackingAccess) CanWriteIssues() bool {
	p := a.Permissions
	return a.CanReadIssues() && !a.Archived && (p.Triage || p.Push || p.Maintain || p.Admin)
}

// TrackingProbe reads the access to the tracking repository repo (OWNER/NAME). It writes nothing. An
// error says why the access could not be read: ErrGHMissing, ErrGHUnauthenticated, ErrRepoUnreachable
// or another.
type TrackingProbe func(repo string) (TrackingAccess, error)

var (
	// ErrGHMissing: the gh command is not installed.
	ErrGHMissing = errors.New("gh is not installed")
	// ErrGHUnauthenticated: gh is not signed in to the host it talks to (GH_HOST, else github.com);
	// the error that wraps it names the host.
	ErrGHUnauthenticated = errors.New("gh is not signed in")
	// ErrRepoUnreachable: the repository does not exist or this login cannot see it.
	ErrRepoUnreachable = errors.New("the repository is not found or not visible to this login")
)

// TrackingAccessReport is the access a check read, with its two verdicts.
type TrackingAccessReport struct {
	TrackingAccess
	IssuesRead  bool `json:"issues_read"`
	IssuesWrite bool `json:"issues_write"`
}

// TrackingStatus is what the check saw of the tracking section: whether the file has one, what it
// names and, when a probe read it, the access.
type TrackingStatus struct {
	Set    bool                  `json:"set"`
	Kind   string                `json:"kind,omitempty"`
	Repo   string                `json:"repo,omitempty"`
	Access *TrackingAccessReport `json:"access,omitempty"`
}

// runTrackingProbe is the probe crw config check reads GitHub with; a test replaces it.
var runTrackingProbe TrackingProbe = ghProbe

// CheckTracking judges the tracking section of a loaded file. A section that is not usable is a
// warning that says what to set; a missing one is the same warning, with the repair command, when
// reportMissing is set (crw config check, and the storage report that crw doctor and crw install
// make on every start, CRW-1577) and no finding otherwise. A github section is judged through probe
// when there is one: gh signed in, the
// repository visible, Issues on, and issues readable and writable for the login; each way it is not
// is an error. Nothing is written anywhere. The status is nil when the file has no section and the
// caller did not ask for it to be reported.
func CheckTracking(file *File, probe TrackingProbe, reportMissing bool) (*TrackingStatus, []Finding) {
	tracking, err := file.Tracking()
	finding := func(level Level, check, format string, args ...any) []Finding {
		return []Finding{{Level: level, Root: trackingRoot, Check: check, Message: fmt.Sprintf(format, args...)}}
	}
	switch {
	case errors.Is(err, ErrTrackingMissing):
		if !reportMissing {
			return nil, nil
		}
		return &TrackingStatus{}, finding(LevelWarning, CheckTrackingMissing, "%v", err)
	case err != nil:
		return &TrackingStatus{Set: true}, finding(LevelWarning, CheckTrackingInvalid, "%v", err)
	}
	status := &TrackingStatus{Set: true, Kind: tracking.Kind, Repo: tracking.Repo}
	if tracking.Kind != TrackingGitHub || probe == nil {
		return status, nil
	}
	access, err := probe(tracking.Repo)
	switch {
	case errors.Is(err, ErrGHMissing):
		return status, finding(LevelError, CheckTrackingAuth, "%v: install the GitHub CLI (gh) and run gh auth login", err)
	case errors.Is(err, ErrGHUnauthenticated):
		return status, finding(LevelError, CheckTrackingAuth, "%v: run gh auth login", err)
	case errors.Is(err, ErrRepoUnreachable):
		return status, finding(LevelError, CheckTrackingAccess, "%s: %v; check tracking.repo or ask for access", tracking.Repo, err)
	case err != nil:
		return status, finding(LevelError, CheckTrackingAccess, "the access to %s could not be read: %v", tracking.Repo, err)
	}
	status.Access = &TrackingAccessReport{TrackingAccess: access, IssuesRead: access.CanReadIssues(), IssuesWrite: access.CanWriteIssues()}
	switch {
	case !access.Authenticated:
		return status, finding(LevelError, CheckTrackingAuth, "gh did not read %s as a signed-in login: run gh auth login", tracking.Repo)
	case !access.HasIssues:
		return status, finding(LevelError, CheckTrackingIssues, "%s has Issues turned off (has_issues is false): turn Issues on in the repository settings", tracking.Repo)
	case access.PermissionsUnreported && access.Archived:
		return status, finding(LevelError, CheckTrackingIssues, "%s is archived, so its issues are read-only: unarchive it or point tracking.repo at another repository", tracking.Repo)
	case access.PermissionsUnreported:
		return status, finding(LevelWarning, CheckTrackingPermissions, "GitHub did not report this login's permissions on %s, so whether it can read and write issues is not known (not a failure). Likely causes: a fine-grained personal access token, a GitHub app token (installation token) or a token without the repo scope does not get a permissions object; check that the token has Issues read and write access to the repository", tracking.Repo)
	case !access.CanReadIssues():
		return status, finding(LevelError, CheckTrackingIssues, "this gh login has no permission to read issues of %s: ask for read access", tracking.Repo)
	case access.Archived:
		return status, finding(LevelError, CheckTrackingIssues, "%s is archived, so its issues are read-only: unarchive it or point tracking.repo at another repository", tracking.Repo)
	case !access.CanWriteIssues():
		return status, finding(LevelError, CheckTrackingIssues, "this gh login can read issues of %s but has no permission to write them: ask for triage or write access", tracking.Repo)
	}
	return status, nil
}

// runTracking is crw config tracking: it writes the tracking section into the configuration file
// through the file's own writer (writeSection), which preserves every other section, writes through
// a symbolic link and creates a file that is not there yet. It probes nothing: the check of the
// repository stays crw config check's (CRW-1577).
func runTracking(getenv func(string) string, flagPath string, args []string, stdout, stderr io.Writer) int {
	flags := flag.NewFlagSet("crw config tracking", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	repo := flags.String("repo", "", "the work-tracking repository, OWNER/NAME")
	if err := flags.Parse(args); err != nil || flags.NArg() > 0 {
		if err == nil {
			err = fmt.Errorf("unexpected argument %q", flags.Arg(0))
		}
		fmt.Fprintf(stderr, "crw config tracking: error: %v\n", err)
		return usageExit
	}
	tracking, err := (Tracking{Kind: TrackingGitHub, Repo: *repo}).normalize()
	if err != nil {
		fmt.Fprintf(stderr, "crw config tracking: error: %v\n", err)
		return usageExit
	}
	file, err := Load(getenv, flagPath)
	if err != nil {
		fmt.Fprintf(stderr, "crw config tracking: error: %v\n", err)
		return usageExit
	}
	section := map[string]json.RawMessage{}
	if raw, ok := file.doc["tracking"]; ok {
		if json.Unmarshal(raw, &section) != nil || section == nil {
			section = map[string]json.RawMessage{}
		}
	}
	section["kind"], _ = json.Marshal(tracking.Kind)
	section["repo"], _ = json.Marshal(tracking.Repo)
	if err := file.writeSection("tracking", section); err != nil {
		fmt.Fprintf(stderr, "crw config tracking: error: %v\n", err)
		return 1
	}
	return writeJSON(stdout, stderr, struct {
		Config   string   `json:"config"`
		Tracking Tracking `json:"tracking"`
	}{file.path, tracking})
}

// ghError is a failed gh run: its standard error.
type ghError struct{ message string }

func (e *ghError) Error() string { return e.message }

// runGH runs gh with args and returns its standard output; a test replaces it. A gh that is not
// installed is an error that is exec.ErrNotFound; a failed run is a *ghError.
var runGH = runGHHost

func runGHHost(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "gh", args...)
	cmd.Env = append(os.Environ(), "GH_PROMPT_DISABLED=1", "GH_NO_UPDATE_NOTIFIER=1", "NO_COLOR=1")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, err
		}
		message := strings.TrimSpace(stderr.String())
		if message == "" {
			message = err.Error()
		}
		return nil, &ghError{message: message}
	}
	return stdout.Bytes(), nil
}

var tokenSpelling = regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}`)

// scrub is text with anything that spells a GitHub token removed and cut to one reasonable length.
func scrub(text string) string {
	text = strings.Join(strings.Fields(tokenSpelling.ReplaceAllString(text, "[token]")), " ")
	if len(text) > 300 {
		text = text[:300] + "..."
	}
	return text
}

// ghHost is the host gh talks to: GH_HOST, else github.com. The sign-in and the repository are read
// on this one host.
func ghHost() string {
	if host := strings.TrimSpace(os.Getenv("GH_HOST")); host != "" {
		return host
	}
	return "github.com"
}

// ghProbe is the host probe: gh must be signed in to its host (GH_HOST, else github.com; both gh
// calls carry that --hostname), and gh api reads the repository (a GET; no method, field or input
// flag is ever passed) for its permissions and has_issues. gh auth status exits non-zero when any
// login of the host is broken, even when the active one works, so a failed auth status is not
// final: the repository read decides, and only an answer that proves the active login (it carries
// permissions) overrides it.
func ghProbe(repo string) (TrackingAccess, error) {
	host := ghHost()
	var authErr error
	if _, err := runGH("auth", "status", "--hostname", host); err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return TrackingAccess{}, ErrGHMissing
		}
		authErr = fmt.Errorf("%w to %s (%s)", ErrGHUnauthenticated, host, scrub(err.Error()))
	}
	out, err := runGH("api", "--hostname", host, "repos/"+repo)
	if err != nil {
		text := scrub(err.Error())
		switch {
		case errors.Is(err, exec.ErrNotFound):
			return TrackingAccess{}, ErrGHMissing
		case authErr != nil:
			return TrackingAccess{}, authErr
		case strings.Contains(text, "HTTP 404"):
			return TrackingAccess{}, fmt.Errorf("%w (%s)", ErrRepoUnreachable, text)
		case strings.Contains(text, "HTTP 401"):
			return TrackingAccess{}, fmt.Errorf("%w to %s (%s)", ErrGHUnauthenticated, host, text)
		}
		return TrackingAccess{}, fmt.Errorf("gh api repos/%s on %s: %s", repo, host, text)
	}
	var doc struct {
		Private     bool                 `json:"private"`
		Archived    bool                 `json:"archived"`
		HasIssues   bool                 `json:"has_issues"`
		Permissions *TrackingPermissions `json:"permissions"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		if authErr != nil {
			return TrackingAccess{}, authErr
		}
		return TrackingAccess{}, fmt.Errorf("gh api repos/%s returned output that is not JSON: %v", repo, err)
	}
	if authErr != nil && doc.Permissions == nil {
		return TrackingAccess{}, authErr
	}
	access := TrackingAccess{Authenticated: true, Private: doc.Private, Archived: doc.Archived, HasIssues: doc.HasIssues}
	if doc.Permissions != nil {
		access.Permissions = *doc.Permissions
	} else {
		access.PermissionsUnreported = true
	}
	return access, nil
}
