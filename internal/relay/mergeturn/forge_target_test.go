package mergeturn

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func fakeGH(t *testing.T, body string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	log := filepath.Join(dir, "argv")
	cmd := filepath.Join(dir, "gh")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > '" + log + "'\n" + body + "\n"
	if err := os.WriteFile(cmd, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	return cmd, log
}

// The gh shim only forwards the request to the in-process forge. This exercises
// the real HTTP status/body boundary without accessing an external service.
func Test26_MTG_4_forge_HTTP_status_and_branch_response(t *testing.T) {
	sha := strings.Repeat("a", 40)
	good := fmt.Sprintf(`{"ref":"refs/heads/dev","object":{"type":"commit","sha":"%s"}}`, sha)
	responses := make(chan struct {
		status int
		body   string
	}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		answer := <-responses
		if r.Method != http.MethodGet || r.URL.Path != "/repos/owner/repo/git/ref/heads/dev" || r.Header.Get("Accept") != "application/vnd.github+json" {
			t.Errorf("unexpected forge request: %s %s %s", r.Method, r.URL, r.Header.Get("Accept"))
		}
		w.WriteHeader(answer.status)
		_, _ = fmt.Fprint(w, answer.body)
	}))
	defer server.Close()
	gh, log := fakeGH(t, `curl -sS -H 'Accept: application/vnd.github+json' -w '%{http_code}' `+server.URL+`/"$6" > "${0}.response"
code=$(tail -c 3 "${0}.response")
if [ "$code" = 200 ]; then head -c -3 "${0}.response"; else printf 'gh: HTTP %s\n' "$code" >&2; exit 1; fi`)
	for _, tc := range []struct {
		status int
		body   string
		want   string
	}{
		{http.StatusOK, good, ""},
		{http.StatusNotFound, "not found", "branch 'dev' does not exist in owner/repo"},
		{http.StatusConflict, "empty", "owner/repo is an empty repository"},
		{http.StatusOK, `[{"ref":"refs/heads/dev"}]`, "not one ref"},
	} {
		responses <- struct {
			status int
			body   string
		}{tc.status, tc.body}
		got, err := (TargetReader{GH: gh}).Tip(context.Background(), "owner/repo", "dev")
		if tc.want == "" {
			if err != nil || got.SHA != sha {
				t.Fatalf("status %d: %+v %v", tc.status, got, err)
			}
		} else if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("status %d: %+v %v", tc.status, got, err)
		}
	}
	raw, err := os.ReadFile(log)
	if err != nil || !strings.HasSuffix(string(raw), "repos/owner/repo/git/ref/heads/dev\n") {
		t.Fatal(string(raw), err)
	}
}

func Test26_MTG_4_forge_one_GET_and_branch_commit_only(t *testing.T) {
	sha := strings.Repeat("a", 40)
	good := `printf '%s\n' '{"ref":"refs/heads/dev","object":{"type":"commit","sha":"` + sha + `"}}'`
	gh, log := fakeGH(t, good)
	reader := TargetReader{GH: gh}
	tip, err := reader.Tip(context.Background(), "owner/repo", "dev")
	if err != nil || tip.SHA != sha || tip.Source != "github:github.com" || tip.Reference != "refs/heads/dev" {
		t.Fatal(tip, err)
	}
	raw, err := os.ReadFile(log)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != "api\n--method\nGET\n-H\nAccept: application/vnd.github+json\nrepos/owner/repo/git/ref/heads/dev\n" {
		t.Fatal(string(raw))
	}
	for _, answer := range []string{`[]`, `{"ref":"refs/heads/dev-2","object":{"type":"commit","sha":"` + sha + `"}}`, `{"ref":"refs/heads/dev","object":{"type":"tag","sha":"` + sha + `"}}`, `{"ref":"refs/heads/dev","object":{"type":"commit","sha":"` + strings.ToUpper(sha) + `"}}`} {
		other, _ := fakeGH(t, "printf '%s\\n' '"+answer+"'")
		if _, err := (TargetReader{GH: other}).Tip(context.Background(), "owner/repo", "dev"); err == nil {
			t.Fatal(answer)
		}
	}
	for _, invalid := range []struct{ repo, branch string }{{"owner/repo", "topic#42"}, {"-owner/repo", "dev"}, {"owner", "dev"}, {"owner/repo/extra", "dev"}} {
		if _, err := reader.Tip(context.Background(), invalid.repo, invalid.branch); err == nil {
			t.Fatal(invalid)
		}
	}
}
