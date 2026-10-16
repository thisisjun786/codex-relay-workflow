package crwconfig

import (
	"reflect"
	"strings"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/publishpolicy"
)

// CRW-1549: publication.repositories holds a scrub policy per repository, read case-insensitively;
// a file without the section, or a repository it does not name, gets the defaults.
func TestPublicationPolicyDefaultsAndLookup(t *testing.T) {
	// no section, no file
	if p, err := trackingFile(t, `{}`).PublicationPolicy("o/r"); err != nil || !reflect.DeepEqual(p, publishpolicy.Policy{}) {
		t.Fatalf("no section: %+v, %v", p, err)
	}
	file := trackingFile(t, `{"publication":{"repositories":{"O/R":{"paths":["/work"]},"other/x":{"disabled":true}}}}`)
	p, err := file.PublicationPolicy("o/r")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Paths) != 1 || p.Paths[0] != "/work" {
		t.Errorf("case-insensitive lookup: %+v", p)
	}
	if p, err := file.PublicationPolicy("O/R"); err != nil || len(p.Paths) != 1 {
		t.Errorf("lookup the other case: %+v, %v", p, err)
	}
	if p, err := file.PublicationPolicy("o/else"); err != nil || !reflect.DeepEqual(p, publishpolicy.Policy{}) {
		t.Errorf("a repository not named: %+v, %v", p, err)
	}
}

// The policy decodes strictly: the kinds it names, custom literals, disabled. Disabled keeps secret
// blocking on; an explicit empty redact turns the privacy replacements off.
func TestPublicationPolicyDecodesStrictly(t *testing.T) {
	file := trackingFile(t, `{"publication":{"repositories":{"o/r":{"disabled":false,"redact":["path","hostname","ip","thread_id","call_id","run_id","session_id","email"],"paths":["/work/private"],"hostnames":["build-box"],"identifiers":["session-local"]},"o/empty":{"redact":[]},"o/off":{"disabled":true}}}}`)
	p, err := file.PublicationPolicy("o/r")
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Redact) != 8 || p.Paths[0] != "/work/private" || p.Hostnames[0] != "build-box" || p.Identifiers[0] != "session-local" {
		t.Errorf("decoded policy %+v", p)
	}
	if p, err := file.PublicationPolicy("o/empty"); err != nil || p.Redact == nil || len(p.Redact) != 0 {
		t.Errorf("explicit empty redact: %+v, %v", p, err)
	}
	p, err = file.PublicationPolicy("o/off")
	if err != nil || !p.Disabled {
		t.Fatalf("disabled: %+v, %v", p, err)
	}
	// disabled never publishes a secret
	if r := publishpolicy.Scrub("ghp_"+strings.Repeat("A", 24), publishpolicy.Context{}, p); !r.Blocked {
		t.Error("a disabled policy published a secret")
	}
	for name, doc := range map[string]string{
		"unknown key":    `{"o/r":{"nope":true}}`,
		"wrong type":     `{"o/r":{"disabled":"yes"}}`,
		"unknown kind":   `{"o/r":{"redact":["everything"]}}`,
		"bad repo key":   `{"not a repo":{}}`,
		"duplicate case": `{"o/r":{},"O/R":{}}`,
		"bad repo arg":   ``,
	} {
		t.Run(name, func(t *testing.T) {
			if name == "bad repo arg" {
				if _, err := trackingFile(t, `{}`).PublicationPolicy("nope"); err == nil {
					t.Error("an invalid repository argument accepted")
				}
				return
			}
			if _, err := trackingFile(t, `{"publication":{"repositories":`+doc+`}}`).PublicationPolicy("o/r"); err == nil {
				t.Errorf("%s accepted: %s", name, doc)
			}
		})
	}
}

// LoadPublicationPolicy is the policy of the configured file, defaults when no file is read; it
// reads only.
func TestLoadPublicationPolicy(t *testing.T) {
	home, env := rootsHome(t)
	path := configWriteAt(t, home+`/x/cfg.json`, `{"publication":{"repositories":{"o/r":{"hostnames":["build-box"]}}}}`)
	envC := func(k string) string {
		if k == "CRW_CONFIG" {
			return path
		}
		return env(k)
	}
	p, err := LoadPublicationPolicy(envC, "O/R")
	if err != nil || len(p.Hostnames) != 1 {
		t.Fatalf("LoadPublicationPolicy: %+v, %v", p, err)
	}
	// no configuration at all is the defaults, not an error
	empty := func(string) string { return "/nonexistent-home/crw-1549-test" }
	if p, err := LoadPublicationPolicy(empty, "o/r"); err != nil || !reflect.DeepEqual(p, publishpolicy.Policy{}) {
		t.Errorf("no home: %+v, %v", p, err)
	}
}
