//go:build dev

package ci

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CRW-964 pre-merge finding (P2, credential exposure): a remote whose password holds a character url.Parse
// rejects must still lose its userinfo. Every case here is a remote git accepts verbatim.
func TestLocal_remote_userinfo_is_removed_whether_or_not_the_url_parses(t *testing.T) {
	cases := []struct {
		name    string
		remote  string
		want    string
		secrets []string
	}{
		{"percent", "https://user:100%@gitlab.example/owner/repo.git", "https://gitlab.example/owner/repo.git", []string{"100%", "user:"}},
		{"space", "https://user:pa ss@gitlab.example/owner/repo.git", "https://gitlab.example/owner/repo.git", []string{"pa ss", "user:"}},
		{"hash", "https://user:pa#ss@gitlab.example/owner/repo.git", "https://gitlab.example/owner/repo.git", []string{"pa#ss", "user:"}},
		{"backslash", "https://user:pa\\ss@gitlab.example/owner/repo.git", "https://gitlab.example/owner/repo.git", []string{"pa\\ss", "user:"}},
		{"ssh-port", "ssh://user:pw@host.example:22/owner/repo.git", "ssh://host.example:22/owner/repo.git", []string{"pw", "user:"}},
		{"query-after-unparsed", "https://user:100%@gitlab.example/owner/repo.git?token=abc", "https://gitlab.example/owner/repo.git", []string{"100%", "abc"}},
		{"slash-in-password", "https://user:pa/ss@gitlab.example/owner/repo.git", "", []string{"pa/ss", "user:"}},
		{"scp-user", "git@github.com:owner/repo.git", "github.com:owner/repo.git", []string{"git@"}},
		{"plain", "https://gitlab.example/owner/repo.git", "https://gitlab.example/owner/repo.git", nil},
	}
	for _, c := range cases {
		got := localWithoutUserinfo(c.remote)
		if got != c.want {
			t.Errorf("%s: localWithoutUserinfo(%q) = %q, want %q", c.name, c.remote, got, c.want)
		}
		for _, secret := range c.secrets {
			if strings.Contains(got, secret) {
				t.Errorf("%s: the repository name still carries %q", c.name, secret)
			}
		}
	}
}

// CRW-964 pre-merge finding (P3): a remote without the colon git requires for an scp-style address is a
// local path, so its @ stays and two such paths name two repositories.
func TestLocal_remotes_without_a_colon_keep_their_name(t *testing.T) {
	a, b := localWithoutUserinfo("a@r.git"), localWithoutUserinfo("b@r.git")
	if a != "a@r.git" || b != "b@r.git" {
		t.Errorf("a@r.git -> %q and b@r.git -> %q, want both unchanged", a, b)
	}
	if a == b {
		t.Errorf("two different remotes name the same repository %q", a)
	}
}

// The record and its canonical sidecar carry the repository name, so neither may hold the password of a
// remote whose URL git accepts but url.Parse rejects.
func TestLocal_the_record_and_its_sidecar_drop_an_unparseable_password(t *testing.T) {
	repo := newLocalFixture(t)
	runGit(repo.root, "remote", "add", "origin", "https://user:100%@gitlab.example/owner/repo.git")
	record := filepath.Join(t.TempDir(), "record.json")
	made, _, err := localVerify(localRunOptions(repo, localFixturePlan("echo hello"), record), "", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writeRecord(record, made); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{record, record + ".canonical"} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"100%", "user:"} {
			if strings.Contains(string(data), secret) {
				t.Errorf("%s carries the origin password fragment %q", filepath.Base(path), secret)
			}
		}
	}
}
