package pullrequest

import (
	"strings"
	"testing"
)

// TestPushEnvironmentHasNoRoutingAndOnePromptSetting (CRW-1355 fix 1): the push's git environment drops the routing variables and
// sets GIT_TERMINAL_PROMPT=0 in place of a value the environment already had, never beside it.
func TestPushEnvironmentHasNoRoutingAndOnePromptSetting(t *testing.T) {
	env := pushEnv([]string{"PATH=/bin", "GIT_TERMINAL_PROMPT=1", "GIT_DIR=/x/.git", "GIT_WORK_TREE=/x", "GIT_OBJECT_DIRECTORY=/x/o", "GIT_NAMESPACE=n", "GIT_AUTHOR_NAME=a"})
	prompts := 0
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		switch k {
		case "GIT_DIR", "GIT_WORK_TREE", "GIT_OBJECT_DIRECTORY", "GIT_NAMESPACE":
			t.Errorf("%s is inherited", kv)
		case "GIT_TERMINAL_PROMPT":
			prompts++
			if v != "0" {
				t.Errorf("GIT_TERMINAL_PROMPT=%s", v)
			}
		}
	}
	if prompts != 1 || !slicesContains(env, "PATH=/bin") || !slicesContains(env, "GIT_AUTHOR_NAME=a") {
		t.Errorf("env %q", env)
	}
	t.Setenv("GIT_DIR", "/y/.git")
	t.Setenv("GIT_TERMINAL_PROMPT", "1")
	for _, kv := range pushEnv(nil) {
		if kv == "GIT_DIR=/y/.git" || kv == "GIT_TERMINAL_PROMPT=1" {
			t.Errorf("the process environment's %s is inherited", kv)
		}
	}
}

func slicesContains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}
