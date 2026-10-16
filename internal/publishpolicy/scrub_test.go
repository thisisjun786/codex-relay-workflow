package publishpolicy

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-1549: the publication scrub over each category, on text nothing else of the host is in (an
// empty Context, so only the fixed roots and the supplied literals are judged).
func TestScrubReplacesThePrivateAndKeepsTheOrdinary(t *testing.T) {
	ctx := Context{Paths: []string{"/home/me", "/work"}, Hostnames: []string{"lina-os"}, Identifiers: []string{"job-4471"}}
	for name, c := range map[string]struct{ in, want string }{
		"home":       {"ran in /home/me/build", "ran in [path]"},
		"fixed root": {"log /tmp/x.txt and /scratch/y", "log [path] and [path]"},
		"var tmp":    {"x /var/tmp/z", "x [path]"},
		"users":      {"in /Users/me/doc", "in [path]"},
		"tilde":      {"see ~/work", "see [path]"},
		"supplied":   {"under /work/private", "under [path]"},
		"windows":    {`at C:\Users\me\doc`, "at [path]"},
		"relative":   {"edited internal/flow/foo.go and docs/state/x.md", "edited internal/flow/foo.go and docs/state/x.md"},
		"in-word":    {"a/home/b and url https://example.com/home/x", "a/home/b and url https://example.com/home/x"},
		"hostname":   {"on lina-os but lina-os-2 stays", "on [hostname] but lina-os-2 stays"},
		"host case":  {"on LINA-OS", "on [hostname]"},
		"ipv4 priv":  {"hit 10.2.3.4 then", "hit [ip] then"},
		"loopback":   {"hit 127.0.0.1:8080", "hit [ip]:8080"},
		"ipv6 local": {"hit [fd00::1] here", "hit [[ip]] here"},
		"v4 mapped":  {"hit ::ffff:10.0.0.1", "hit [ip]"},
		"ipv4 pub":   {"hit 8.8.8.8", "hit 8.8.8.8"},
		"ipv6 pub":   {"hit 2001:db8::1", "hit 2001:db8::1"},
		"not ip":     {"at 12:30 and owner:name:n", "at 12:30 and owner:name:n"},
		"email":      {"mail dev@private.example", "mail [email]"},
		"uuid":       {"id 123e4567-e89b-42d3-a456-426614174000", "id [thread_id:__PSEU__]"},
		"thread":     {"of thread-a1b2c3d4", "of [thread_id:__PSEU__]"},
		"call":       {"of call_0123456789abcdef and c_0123456789ab", "of [call_id:__PSEU__] and [call_id:__PSEU__]"},
		"toolu":      {"of toolu_0123456789abcdef", "of [call_id:__PSEU__]"},
		"flow op":    {"of crw-flow-op:9f3eab and crwh1:77aa", "of [call_id:__PSEU__] and [call_id:__PSEU__]"},
		"run":        {"of run-0123456789ab and req_0123456789abcdef01234567", "of [run_id:__PSEU__] and [run_id:__PSEU__]"},
		"session":    {"of session-a1b2c3d4", "of [session_id:__PSEU__]"},
		"literal id": {"of job-4471", "of [session_id:__PSEU__]"},
		"sha40":      {"commit " + strings.Repeat("a", 40), "commit " + strings.Repeat("a", 40)},
		"sha12":      {"commit " + strings.Repeat("b", 12), "commit " + strings.Repeat("b", 12)},
		"issue ref":  {"CRW-1549 and o/r#10 and owner:name:n", "CRW-1549 and o/r#10 and owner:name:n"},
		"adjacent":   {"/home/me/x /home/me/y", "[path] [path]"},
		"nested":     {"/home/me/session-a1b2c3d4", "[path]"},
	} {
		t.Run(name, func(t *testing.T) {
			got := Scrub(c.in, ctx, Policy{})
			if strings.Contains(c.want, "__PSEU__") {
				kind := c.want[strings.Index(c.want, "[")+1 : strings.Index(c.want, ":")]
				if !strings.Contains(got.Text, "["+kind+":") || strings.Contains(got.Text, strings.TrimPrefix(c.in, "of ")) {
					t.Errorf("Scrub(%q) = %q, want a [%s:hex] pseudonym and no original", c.in, got.Text, kind)
				}
			} else if got.Text != c.want {
				t.Errorf("Scrub(%q) = %q, want %q", c.in, got.Text, c.want)
			}
			if got.Blocked {
				t.Errorf("%q blocked", c.in)
			}
		})
	}
}

// A token shape blocks the text wherever it sits — inside a path it would hide under too — and a
// credential shape that is left after token replacement collapses the whole text to [secret].
func TestScrubBlocksSecretsEverywhere(t *testing.T) {
	for name, in := range map[string]string{
		"token":         "key ghp_" + strings.Repeat("A", 24) + " here",
		"token in path": "see /home/me/ghp_" + strings.Repeat("A", 24) + "/x",
		"openai":        "key sk-" + strings.Repeat("k", 20),
		"privkey":       "-----BEGIN RSA " + "PRIVATE KEY-----",
		"assignment":    "API_KEY=abc123\nPORT=8080",
		"fenced json":   "look:\n```json\n{\"dump\": \"API_KEY=abc123\"}\n```\nend",
	} {
		t.Run(name, func(t *testing.T) {
			r := Scrub(in, Context{}, Policy{})
			if !r.Blocked {
				t.Fatalf("%q not blocked: %q", in, r.Text)
			}
			if strings.Contains(r.Text, strings.Repeat("A", 24)) || strings.Contains(r.Text, "abc123") {
				t.Errorf("the result text echoes the secret: %q", r.Text)
			}
		})
	}
	// a token alone: replaced by [token] in the text for diagnostics, still blocked
	r := Scrub("token ghp_"+strings.Repeat("A", 24), Context{}, Policy{})
	if !r.Blocked || !strings.Contains(r.Text, "[token]") {
		t.Errorf("token: %+v", r)
	}
	// a private repository may publish its own internals, never a secret
	if r := Scrub("token ghp_"+strings.Repeat("A", 24)+" in /home/me/x", Context{}, Policy{Disabled: true}); !r.Blocked {
		t.Error("disabled policy still publishes a secret")
	}
}

// The Redact list is exactly the kinds it names: nil is all, an explicit empty list is none, and a
// subset keeps the other kinds untouched.
func TestScrubPolicySelectsTheKinds(t *testing.T) {
	in := "/home/me/x on lina-os"
	ctx := Context{Hostnames: []string{"lina-os"}}
	if r := Scrub(in, ctx, Policy{Redact: []string{"path"}}); !strings.Contains(r.Text, "[path]") || !strings.Contains(r.Text, "lina-os") {
		t.Errorf("path only: %q", r.Text)
	}
	if r := Scrub(in, ctx, Policy{Redact: []string{}}); r.Text != in || len(r.Changes) != 0 {
		t.Errorf("explicit empty redact: %+v", r)
	}
	if r := Scrub(in, ctx, Policy{Disabled: true}); r.Text != in {
		t.Errorf("disabled: %q", r.Text)
	}
	// Changes is [] not nil, one entry per kind with its count
	r := Scrub("clean text", ctx, Policy{})
	if r.Changes == nil || len(r.Changes) != 0 {
		t.Errorf("changes %v, want empty not nil", r.Changes)
	}
	r = Scrub("/home/me/a /tmp/b on lina-os", ctx, Policy{})
	want := map[string]int{"path": 2, "hostname": 1}
	if len(r.Changes) != len(want) {
		t.Fatalf("changes %+v", r.Changes)
	}
	for _, ch := range r.Changes {
		if want[ch.Kind] != ch.Count {
			t.Errorf("change %+v", ch)
		}
	}
	if r.Changes[0].Kind != "path" {
		t.Errorf("changes not in kind order: %+v", r.Changes)
	}
}

// The scrub is deterministic and byte-exact outside the spans it covers.
func TestScrubIsStableAndPreservesTheRest(t *testing.T) {
	in := "unicode 한국어 and **markdown** /home/me/x — unchanged."
	a, b := Scrub(in, Context{}, Policy{}), Scrub(in, Context{}, Policy{})
	if a.Text != b.Text || a.Blocked != b.Blocked {
		t.Errorf("not deterministic: %q / %q", a.Text, b.Text)
	}
	if !strings.HasPrefix(a.Text, "unicode 한국어 and **markdown** [path]") || !strings.HasSuffix(a.Text, " — unchanged.") {
		t.Errorf("surrounding text changed: %q", a.Text)
	}
	if got := Scrub("plain text", Context{}, Policy{}); got.Text != "plain text" {
		t.Errorf("unchanged text: %q", got.Text)
	}
	// the same identifier twice is one pseudonym; two identifiers do not collapse
	r := Scrub("session-a1b2c3d4 and session-a1b2c3d4 and session-ffffffff", Context{}, Policy{})
	if strings.Contains(r.Text, "a1b2c3d4") || strings.Contains(r.Text, "ffffffff") {
		t.Fatalf("identifiers left: %q", r.Text)
	}
	parts := strings.Split(r.Text, " and ")
	if len(parts) != 3 || parts[0] != parts[1] || parts[0] == parts[2] {
		t.Errorf("pseudonyms %q", r.Text)
	}
}

// Validate refuses an unknown kind, a blank literal, a relative path root and "/" itself; a Windows
// drive root is a path root.
func TestPolicyValidate(t *testing.T) {
	for name, p := range map[string]Policy{
		"unknown kind":   {Redact: []string{"path", "nope"}},
		"blank path":     {Paths: []string{" "}},
		"root path":      {Paths: []string{"/"}},
		"relative path":  {Paths: []string{"work/private"}},
		"blank hostname": {Hostnames: []string{""}},
		"blank ident":    {Identifiers: []string{"\t"}},
	} {
		t.Run(name, func(t *testing.T) {
			if err := p.Validate(); err == nil {
				t.Errorf("%+v accepted", p)
			}
		})
	}
	for _, p := range []Policy{
		{},
		{Disabled: true},
		{Redact: []string{"path", "hostname", "ip", "thread_id", "call_id", "run_id", "session_id", "email"}},
		{Paths: []string{`/work`, `D:\work\private`, `D:/work`}, Hostnames: []string{"build-box", "localhost"}, Identifiers: []string{"session-local"}},
	} {
		if err := p.Validate(); err != nil {
			t.Errorf("%+v refused: %v", p, err)
		}
	}
}

// HostContext carries the machine's home and name.
func TestHostContext(t *testing.T) {
	c := HostContext()
	if len(c.Hostnames) == 0 {
		t.Error("no hostname")
	}
	if len(c.Paths) == 0 {
		t.Error("no home path")
	}
}

// CI can put verification records outside the well-known private path roots.
// Their labels and public source references must survive, while the temporary
// directory selected by the host is private regardless of its spelling.
func TestHostContextScrubsConfiguredTemporaryDirectory(t *testing.T) {
	root := filepath.Join(filepath.VolumeName(os.TempDir())+string(filepath.Separator), "ci-private", "job-1549")
	for _, key := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(key, root)
	}
	if os.TempDir() != root {
		t.Fatalf("temporary root = %q, want %q", os.TempDir(), root)
	}
	ctx := HostContext()
	for _, field := range []string{"verification-record", "review"} {
		text := field + ": " + filepath.Join(root, "run", "record.json")
		if got := Scrub(text, ctx, Policy{}); got.Blocked || got.Text != field+": [path]" {
			t.Errorf("private record was not scrubbed: %+v", got)
		}
	}
	for _, text := range []string{"docs/verification-record.md", root + "-public/example.json"} {
		if got := Scrub(text, ctx, Policy{}); got.Blocked || got.Text != text {
			t.Errorf("unrelated reference changed: %q -> %+v", text, got)
		}
	}
}

// Boundary reuse: a value twice in a row is matched twice — the trailing boundary of the first match
// is the leading boundary of the second.
func TestScrubAdjacentValues(t *testing.T) {
	ctx := Context{Hostnames: []string{"lina-os"}, Identifiers: []string{"job-4471"}}
	r := Scrub("on lina-os lina-os", ctx, Policy{})
	if strings.Count(r.Text, "[hostname]") != 2 || strings.Contains(r.Text, "lina-os") {
		t.Errorf("adjacent hostnames: %q", r.Text)
	}
	r = Scrub("at /home /home again", Context{}, Policy{})
	if strings.Count(r.Text, "[path]") != 2 || strings.Contains(r.Text, "/home") {
		t.Errorf("adjacent roots: %q", r.Text)
	}
	r = Scrub("jobs job-4471 job-4471", ctx, Policy{})
	if strings.Count(r.Text, "[session_id:") != 2 || strings.Contains(r.Text, "job-4471") {
		t.Errorf("adjacent identifiers: %q", r.Text)
	}
}

// IPv6 with punctuation, a zone and brackets: the address is replaced, the punctuation is not
// swallowed, and a public address is left alone.
func TestScrubIPv6Edges(t *testing.T) {
	for in, want := range map[string]string{
		"hit fd00::1.":              "hit [ip].",
		"at fe80::1%eth0 now":       "at [ip] now",
		"open [fd00::1]:8080":       "open [[ip]]:8080",
		"hit ::ffff:10.0.0.1.":      "hit [ip].",
		"hit 2001:4860:4860::8888.": "hit 2001:4860:4860::8888.",
	} {
		r := Scrub(in, Context{}, Policy{})
		if r.Text != want {
			t.Errorf("Scrub(%q) = %q, want %q", in, r.Text, want)
		}
		if r.Blocked {
			t.Errorf("%q blocked", in)
		}
	}
}

// A private key is a secret whole: header, body and trailer collapse to [secret] — the payload is
// never in the diagnostic text.
func TestScrubPrivateKeyIsWholeBlock(t *testing.T) {
	pem := "-----BEGIN PRIVATE KEY-----\nMIIabc123SECRETpayloadKEYDATA456789==\n-----END PRIVATE KEY-----"
	r := Scrub("the key is:\n"+pem+"\ndone", Context{}, Policy{})
	if !r.Blocked || r.Text != "[secret]" {
		t.Fatalf("private key: blocked=%v text=%q", r.Blocked, r.Text)
	}
	if strings.Contains(r.Text, "SECRETpayload") {
		t.Error("the payload survived into the diagnostic")
	}
}

// Named credentials and bearer tokens block even when they match no provider pattern; an ordinary
// JSON key does not.
func TestScrubNamedCredentials(t *testing.T) {
	for in := range map[string]struct{}{
		`{"password":"private-value"}`:                     {},
		`token: private-value`:                             {},
		`{"client_secret":"xyz123abc"} ` + "x":             {},
		"Authorization: Bearer " + strings.Repeat("b", 20): {},
	} {
		r := Scrub(in, Context{}, Policy{})
		if !r.Blocked {
			t.Errorf("%q not blocked", in)
		}
		if strings.Contains(r.Text, "private-value") || strings.Contains(r.Text, strings.Repeat("b", 20)) {
			t.Errorf("the diagnostic echoes the value: %q", r.Text)
		}
	}
	if r := Scrub(`{"key":"CRW-1549"}`, Context{}, Policy{}); r.Blocked || r.Text != `{"key":"CRW-1549"}` {
		t.Errorf("ordinary JSON blocked/changed: %+v", r)
	}
}

// Scrubbing the answer again changes nothing more (the placeholders match no pattern).
func TestScrubIsIdempotent(t *testing.T) {
	ctx := Context{Hostnames: []string{"lina-os"}}
	in := "ran /home/me/x on lina-os from 10.0.0.9, dev@ex.com"
	r1 := Scrub(in, ctx, Policy{})
	r2 := Scrub(r1.Text, ctx, Policy{})
	if r2.Text != r1.Text || len(r2.Changes) != 0 {
		t.Errorf("not idempotent: %q -> %q (%v)", r1.Text, r2.Text, r2.Changes)
	}
}

// Public source filenames and issue references keep their identities: the automatic ID patterns must
// not redact a relative file name or an `owner/repo#N` carries reference (CRW-1549 v2).
func TestScrubPreservesSourceNamesAndIssueReferences(t *testing.T) {
	for _, in := range []string{
		"see session_evidence.go for the format",
		"check thread_status_test.go",
		"change internal/flow/engine/session_evidence.go here",
		"`thread_status_test.go:42` covers it",
		"edit ./src/session-a1b2c3d4.ts now",
		"notes/019a2b3c-4d5e-7f60-8a9b-0c1d2e3f4a5b/detail.md",
		"win path internal\\thread_status_test.go file",
		"session_evidence.go session_evidence.go",
		"Carries: acme/session-management#1",
		"Carries: session-maintainers/thread-status-tests#2, acme/req_0123456789abcdef01234567#3",
		"Carries: acme/123e4567-e89b-42d3-a456-426614174000#9",
		"Fixed in `internal/flow/engine/session_evidence.go`; see Carries: acme/session-management#1 for the link.",
	} {
		r := Scrub(in, Context{}, Policy{})
		if r.Blocked || r.Text != in || len(r.Changes) != 0 {
			t.Errorf("Scrub(%q) = %q blocked=%v changes=%v, want unchanged", in, r.Text, r.Blocked, r.Changes)
		}
	}
}

// The exemption is syntactic context only: a standalone identifier still redacts, a private absolute
// path still masks, a secret shape inside a source-looking token still blocks, and a configured
// identifier still applies inside a filename.
func TestSourceReferencesDoNotHidePrivateIdentifiersOrSecrets(t *testing.T) {
	r := Scrub("ran session-a1b2c3d4 and thread-a1b2c3d4 on c_0123456789ab; see session_evidence.go", Context{}, Policy{})
	if r.Blocked {
		t.Fatalf("blocked: %q", r.Text)
	}
	if !strings.Contains(r.Text, "session_evidence.go") {
		t.Errorf("the file name was redacted: %q", r.Text)
	}
	for _, raw := range []string{"session-a1b2c3d4", "thread-a1b2c3d4", "c_0123456789ab"} {
		if strings.Contains(r.Text, raw) {
			t.Errorf("the standalone identifier %q survived: %q", raw, r.Text)
		}
	}
	r = Scrub("log at /home/me/session_evidence.go", Context{}, Policy{})
	if strings.Contains(r.Text, "session_evidence.go") && strings.Contains(r.Text, "/home/me") {
		t.Errorf("a private path leaked: %q", r.Text)
	}
	if !strings.Contains(r.Text, "[path]") {
		t.Errorf("the private path was not masked: %q", r.Text)
	}
	token := "ghp_" + strings.Repeat("A", 24)
	for _, in := range []string{"see src/" + token + ".go", "Carries: owner/" + token + "#1"} {
		r = Scrub(in, Context{}, Policy{})
		if !r.Blocked {
			t.Errorf("%q not blocked", in)
		}
		if strings.Contains(r.Text, token) {
			t.Errorf("the diagnostic echoes the token: %q", r.Text)
		}
	}
	r = Scrub("open session_evidence.go", Context{Identifiers: []string{"session_evidence"}}, Policy{})
	if strings.Contains(r.Text, "session_evidence") {
		t.Errorf("a configured identifier survived inside a file name: %q", r.Text)
	}
}
