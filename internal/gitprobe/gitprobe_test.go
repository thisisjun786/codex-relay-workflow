package gitprobe

import (
	"context"
	"os"
	"slices"
	"testing"
)

func TestProbeEnvRemovesEveryGitVariableAndAppliesOverridesOnce(t *testing.T) {
	base := []string{"PATH=/bin", "GIT_DIR=/x", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_SSH_COMMAND=ssh", "LC_ALL=ko_KR.UTF-8", "HOME=/h"}
	got := ProbeEnv(base, "LC_ALL=C", "GIT_INDEX_FILE=/i")
	want := []string{"GIT_TERMINAL_PROMPT=0", "GIT_NO_LAZY_FETCH=1", "PATH=/bin", "HOME=/h", "LC_ALL=C", "GIT_INDEX_FILE=/i"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	if got := ProbeEnv(base, "GIT_TERMINAL_PROMPT=1"); slices.Contains(got, "GIT_TERMINAL_PROMPT=0") || got[len(got)-1] != "GIT_TERMINAL_PROMPT=1" {
		t.Fatalf("an override of a pin is applied once: %q", got)
	}
	if !slices.Equal(base, []string{"PATH=/bin", "GIT_DIR=/x", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_SSH_COMMAND=ssh", "LC_ALL=ko_KR.UTF-8", "HOME=/h"}) {
		t.Fatalf("base changed: %q", base)
	}
}

func TestSanitizeRemovesRoutingAndKeepsTransportAndIdentity(t *testing.T) {
	var base []string
	for _, name := range append(slices.Clone(routing), "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0") {
		base = append(base, name+"=v")
	}
	kept := []string{"GIT_SSH_COMMAND=ssh -i k", "GIT_ASKPASS=/a", "GIT_AUTHOR_NAME=a", "GIT_CONFIG_GLOBAL=/g", "GIT_CONFIG_NOSYSTEM=1", "PATH=/bin"}
	base = append(base, kept...)
	if got := Sanitize(base); !slices.Equal(got, kept) {
		t.Fatalf("got %q\nwant %q", got, kept)
	}
	got := Sanitize(base, "GIT_INDEX_FILE=/i", "GIT_AUTHOR_NAME=b")
	want := []string{"GIT_SSH_COMMAND=ssh -i k", "GIT_ASKPASS=/a", "GIT_CONFIG_GLOBAL=/g", "GIT_CONFIG_NOSYSTEM=1", "PATH=/bin", "GIT_INDEX_FILE=/i", "GIT_AUTHOR_NAME=b"}
	if !slices.Equal(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestCommandCarriesTheProbePolicy(t *testing.T) {
	t.Setenv("GIT_OBJECT_DIRECTORY", "/nonexistent")
	cmd := Command(context.Background(), "/repo", []string{"LC_ALL=C"}, "rev-parse", "--git-dir")
	want := []string{"git", "--no-optional-locks", "--no-replace-objects", "-c", "core.hooksPath=" + os.DevNull, "-c", "submodule.recurse=false", "-c", "core.fsmonitor=false", "-C", "/repo", "rev-parse", "--git-dir"}
	if !slices.Equal(cmd.Args, want) {
		t.Fatalf("args %q", cmd.Args)
	}
	if slices.Contains(cmd.Env, "GIT_OBJECT_DIRECTORY=/nonexistent") || !slices.Contains(cmd.Env, "GIT_TERMINAL_PROMPT=0") || cmd.Env[len(cmd.Env)-1] != "LC_ALL=C" || cmd.WaitDelay == 0 {
		t.Fatalf("env (last %q, %d entries), wait delay %v", cmd.Env[len(cmd.Env)-1], len(cmd.Env), cmd.WaitDelay)
	}
}
