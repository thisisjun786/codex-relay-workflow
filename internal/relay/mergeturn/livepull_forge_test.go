package mergeturn

import (
	"context"
	"os"
	"strings"
	"testing"
)

// CRW-538: TargetReader reads a pull request's head with one gh api GET and trusts nothing the answer does not name.
func TestLivePullTargetReaderReadsOnePullRequestHead(t *testing.T) {
	sha := strings.Repeat("a", 40)
	good := "printf '%s\\n' '{\"number\":500,\"state\":\"open\",\"head\":{\"sha\":\"" + sha + "\"}}'"
	gh, log := fakeGH(t, good)
	got, err := (TargetReader{GH: gh}).PullRequestHead(context.Background(), "owner/repo", 500)
	if err != nil || got.SHA != sha || got.Number != 500 || got.Repository != "owner/repo" || got.Source != "github:github.com" {
		t.Fatalf("%+v %v", got, err)
	}
	raw, err := os.ReadFile(log)
	if err != nil || string(raw) != "api\n--method\nGET\n-H\nAccept: application/vnd.github+json\nrepos/owner/repo/pulls/500\n" {
		t.Fatalf("the read was not one GET of the pull request: %q %v", raw, err)
	}
}

func TestLivePullTargetReaderRefusesAnswersThatDoNotNameThePullRequestHead(t *testing.T) {
	sha := strings.Repeat("b", 40)
	for _, tc := range []struct {
		name, script, want string
	}{
		{"another pull request", "printf '%s\\n' '{\"number\":501,\"head\":{\"sha\":\"" + sha + "\"}}'", "does not name pull request 500"},
		{"a short object name", "printf '%s\\n' '{\"number\":500,\"head\":{\"sha\":\"abc123\"}}'", "does not name a full commit"},
		{"an upper-case object name", "printf '%s\\n' '{\"number\":500,\"head\":{\"sha\":\"" + strings.ToUpper(sha) + "\"}}'", "does not name a full commit"},
		{"no head", "printf '%s\\n' '{\"number\":500}'", "does not name a full commit"},
		{"a list", "printf '%s\\n' '[]'", "not one pull request"},
		{"not JSON", "printf '%s\\n' 'oops'", "not JSON"},
		{"a missing pull request", "printf 'gh: HTTP 404\\n' >&2; exit 1", "pull request 500 does not exist in owner/repo"},
		{"any other failure", "printf 'gh: HTTP 502\\n' >&2; exit 1", "reading pull request 500 failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gh, _ := fakeGH(t, tc.script)
			got, err := (TargetReader{GH: gh}).PullRequestHead(context.Background(), "owner/repo", 500)
			if err == nil || !strings.Contains(err.Error(), tc.want) || got.SHA != "" {
				t.Fatalf("%+v %v, want an error saying %q", got, err, tc.want)
			}
			if _, ok := err.(*TargetUnreadable); !ok {
				t.Fatalf("%T is not a TargetUnreadable", err)
			}
		})
	}
	// a repository that is no owner/name, and a number that is no pull request, are refused before the forge is asked
	gh, log := fakeGH(t, "printf '%s\\n' '{\"number\":500,\"head\":{\"sha\":\""+sha+"\"}}'")
	for _, tc := range []struct {
		repository string
		number     int64
	}{{"/srv/local/R.git", 500}, {"owner", 500}, {"owner/repo/extra", 500}, {"-owner/repo", 500}, {"owner/repo", 0}, {"owner/repo", -3}} {
		if _, err := (TargetReader{GH: gh}).PullRequestHead(context.Background(), tc.repository, tc.number); err == nil {
			t.Fatalf("%s#%d has no pull request this can read", tc.repository, tc.number)
		}
	}
	if _, err := os.Stat(log); !os.IsNotExist(err) {
		t.Fatalf("the forge was asked for a pull request that cannot exist: %v", err)
	}
}
