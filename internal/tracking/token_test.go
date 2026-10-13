package tracking

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tokenOf(t *testing.T, s TokenSource) (string, error) {
	t.Helper()
	if s == nil {
		t.Fatal("nil TokenSource")
	}
	return s.Token(context.Background())
}

func TestStaticTokenGivesItsTokenTrimmed(t *testing.T) {
	if got, err := tokenOf(t, StaticToken(" abc\n")); err != nil || got != "abc" {
		t.Errorf("StaticToken = %q, %v; want abc", got, err)
	}
	if _, err := tokenOf(t, StaticToken(" \n")); !errors.Is(err, ErrNoToken) {
		t.Errorf("StaticToken(blank) error = %v; want ErrNoToken", err)
	}
}

func TestTokenFuncIsATokenSource(t *testing.T) {
	var s TokenSource = TokenFunc(func(context.Context) (string, error) { return "f", nil })
	if got, err := tokenOf(t, s); err != nil || got != "f" {
		t.Errorf("TokenFunc = %q, %v", got, err)
	}
}

func TestEnvTokenReadsGHTokenThenGithubToken(t *testing.T) {
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	for _, c := range []struct {
		name string
		env  map[string]string
		want string
	}{
		{"both", map[string]string{"GH_TOKEN": "a", "GITHUB_TOKEN": "b"}, "a"},
		{"github only", map[string]string{"GITHUB_TOKEN": "b"}, "b"},
		{"blank GH_TOKEN", map[string]string{"GH_TOKEN": " ", "GITHUB_TOKEN": "b"}, "b"},
	} {
		if got, err := tokenOf(t, EnvToken(env(c.env))); err != nil || got != c.want {
			t.Errorf("%s: EnvToken = %q, %v; want %q", c.name, got, err, c.want)
		}
	}
	if _, err := tokenOf(t, EnvToken(env(nil))); !errors.Is(err, ErrNoToken) {
		t.Errorf("EnvToken(none) error = %v; want ErrNoToken", err)
	}
}

func fakeGH(t *testing.T, script string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "gh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestGHAuthTokenRunsGhAuthToken(t *testing.T) {
	gh := fakeGH(t, `[ "$1 $2" = "auth token" ] || exit 2; echo gho_fake`)
	if got, err := tokenOf(t, GHAuthToken(gh)); err != nil || got != "gho_fake" {
		t.Errorf("GHAuthToken = %q, %v; want gho_fake", got, err)
	}
}

func TestGHAuthTokenFailureIsNoTokenAndLeaksNothing(t *testing.T) {
	failing := fakeGH(t, `echo "stderr-secret-ghp_zzz" >&2; exit 1`)
	_, err := tokenOf(t, GHAuthToken(failing))
	if !errors.Is(err, ErrNoToken) || strings.Contains(err.Error(), "stderr-secret") {
		t.Errorf("failing gh error = %v; want ErrNoToken without stderr", err)
	}
	empty := fakeGH(t, `exit 0`)
	if _, err := tokenOf(t, GHAuthToken(empty)); !errors.Is(err, ErrNoToken) {
		t.Errorf("silent gh error = %v; want ErrNoToken", err)
	}
	if _, err := tokenOf(t, GHAuthToken(filepath.Join(t.TempDir(), "absent"))); !errors.Is(err, ErrNoToken) {
		t.Errorf("missing gh error = %v; want ErrNoToken", err)
	}
}

func TestFirstTokenSkipsMissingStopsAtFailure(t *testing.T) {
	none := TokenFunc(func(context.Context) (string, error) { return "", ErrNoToken })
	boom := errors.New("boom")
	failing := TokenFunc(func(context.Context) (string, error) { return "", boom })
	good := StaticToken("g")
	if got, err := tokenOf(t, FirstToken(none, good, failing)); err != nil || got != "g" {
		t.Errorf("FirstToken(none, good) = %q, %v", got, err)
	}
	if _, err := tokenOf(t, FirstToken(none, failing, good)); !errors.Is(err, boom) {
		t.Errorf("FirstToken(none, failing) error = %v; want boom", err)
	}
	if _, err := tokenOf(t, FirstToken(none, none)); !errors.Is(err, ErrNoToken) {
		t.Errorf("FirstToken(none, none) error = %v; want ErrNoToken", err)
	}
	if _, err := tokenOf(t, FirstToken()); !errors.Is(err, ErrNoToken) {
		t.Errorf("FirstToken() error = %v; want ErrNoToken", err)
	}
}

func TestCachedTokenAsksOnceAfterSuccessAndRetriesAfterFailure(t *testing.T) {
	calls := 0
	src := CachedToken(TokenFunc(func(context.Context) (string, error) {
		calls++
		if calls == 1 {
			return "", ErrNoToken
		}
		return "t", nil
	}))
	if _, err := tokenOf(t, src); !errors.Is(err, ErrNoToken) {
		t.Fatalf("first = %v; want ErrNoToken", err)
	}
	for i := 0; i < 3; i++ {
		if got, err := tokenOf(t, src); err != nil || got != "t" {
			t.Fatalf("later = %q, %v", got, err)
		}
	}
	if calls != 2 {
		t.Errorf("underlying source asked %d times; want 2", calls)
	}
}

func TestDefaultTokenSourceReadsTheEnvironmentThenGh(t *testing.T) {
	t.Setenv("GH_TOKEN", "from-env")
	t.Setenv("GITHUB_TOKEN", "")
	if got, err := tokenOf(t, DefaultTokenSource()); err != nil || got != "from-env" {
		t.Errorf("DefaultTokenSource = %q, %v; want from-env", got, err)
	}
	t.Setenv("GH_TOKEN", "")
	t.Setenv("PATH", fakeGHDir(t, `echo gho_from_gh`))
	if got, err := tokenOf(t, DefaultTokenSource()); err != nil || got != "gho_from_gh" {
		t.Errorf("DefaultTokenSource via gh = %q, %v", got, err)
	}
	t.Setenv("PATH", t.TempDir())
	if _, err := tokenOf(t, DefaultTokenSource()); !errors.Is(err, ErrNoToken) {
		t.Errorf("DefaultTokenSource with nothing = %v; want ErrNoToken", err)
	}
}

func fakeGHDir(t *testing.T, script string) string { return filepath.Dir(fakeGH(t, script)) }
