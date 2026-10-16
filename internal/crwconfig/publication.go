package crwconfig

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/publishpolicy"
)

// The publication section holds the per-repository scrub policy for text bound to a public forge
// (CRW-1549):
//
//	"publication": {"repositories": {"OWNER/NAME": {"redact": [...], "paths": [...],
//	  "hostnames": [...], "identifiers": [...], "disabled": false}}}
//
// A repository not named publishes with the default policy: every privacy kind replaced, secrets
// always blocked. The section is read here and nowhere in load, so a section another reader does not
// use never stops it; it is told to the caller that asks for it.

// PublicationPolicy is the publication policy of repository repo (OWNER/NAME, matched without
// regard to case): the defaults when the file or its publication.repositories has no entry for it.
// Every repository key is judged with ValidateRepo, two keys that differ only in case are refused,
// and the chosen policy is decoded strictly (an unknown key or wrong type is an error) and
// validated.
func (f *File) PublicationPolicy(repo string) (publishpolicy.Policy, error) {
	if err := ValidateRepo(repo); err != nil {
		return publishpolicy.Policy{}, fmt.Errorf("%s: publication.repositories: %v", f.path, err)
	}
	var section struct {
		Repositories map[string]json.RawMessage `json:"repositories"`
	}
	if err := f.Section("publication", &section); err != nil {
		return publishpolicy.Policy{}, fmt.Errorf("%s: publication is not an object with a repositories map: %v", f.path, err)
	}
	if len(section.Repositories) == 0 {
		return publishpolicy.Policy{}, nil
	}
	var found json.RawMessage
	seen := map[string]string{}
	for key, raw := range section.Repositories {
		if err := ValidateRepo(key); err != nil {
			return publishpolicy.Policy{}, fmt.Errorf("%s: publication.repositories: %v", f.path, err)
		}
		lower := strings.ToLower(key)
		if first, ok := seen[lower]; ok {
			return publishpolicy.Policy{}, fmt.Errorf("%s: publication.repositories: %q and %q are the same repository", f.path, first, key)
		}
		seen[lower] = key
		if lower == strings.ToLower(repo) {
			found = raw
		}
	}
	if found == nil {
		return publishpolicy.Policy{}, nil
	}
	trimmed := bytes.TrimSpace(found)
	if len(trimmed) == 0 || trimmed[0] != '{' || trimmed[len(trimmed)-1] != '}' {
		return publishpolicy.Policy{}, fmt.Errorf("%s: publication.repositories.%s is not a policy object", f.path, repo)
	}
	var p publishpolicy.Policy
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return publishpolicy.Policy{}, fmt.Errorf("%s: publication.repositories.%s: %v", f.path, repo, err)
	}
	if err := p.Validate(); err != nil {
		return publishpolicy.Policy{}, fmt.Errorf("%s: publication.repositories.%s: %v", f.path, repo, err)
	}
	return p, nil
}

// LoadPublicationPolicy is the publication policy of repo in the loaded configuration file, the
// defaults when the host cannot name a configuration or the file is not there (loadOptional). It
// reads only; nothing is written.
func LoadPublicationPolicy(getenv func(string) string, repo string) (publishpolicy.Policy, error) {
	if repo != "" {
		if err := ValidateRepo(repo); err != nil {
			return publishpolicy.Policy{}, err
		}
	}
	file, err := loadOptional(getenv, "")
	if err != nil || file == nil {
		return publishpolicy.Policy{}, err
	}
	p, err := file.PublicationPolicy(repo)
	if err != nil {
		return publishpolicy.Policy{}, err
	}
	// the roots the configuration itself declares are private too (CRW-1549): a work tree or relay
	// state directory outside the fixed roots still may not appear in public text
	for _, name := range RootNames() {
		if r := file.roots[name]; r.Source == SourceConfig && len(r.Path) > 1 {
			p.Paths = append(p.Paths, r.Path)
		}
	}
	return p, nil
}
