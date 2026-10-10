package manage

import (
	"slices"
	"strings"
	"testing"
)

// CRW-1135: the evaluation's git runs on gitprobe's common base, so every inherited routing variable is gone
// (the object directories and the discovery boundary too, which the old list kept), the evaluation's own index
// and identity are applied once after it, and the transport variables a fetch needs stay.
func TestPremergeGitEnvAppliesItsOverridesAfterTheCommonBase(t *testing.T) {
	for name, value := range map[string]string{
		"GIT_DIR": "/elsewhere/.git", "GIT_INDEX_FILE": "/elsewhere/index", "GIT_OBJECT_DIRECTORY": "/missing",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES": "/missing-alternates", "GIT_CEILING_DIRECTORIES": "/", "GIT_NAMESPACE": "ns",
		"GIT_CONFIG_PARAMETERS": "'core.hooksPath'='/x'", "GIT_SSH_COMMAND": "ssh -o BatchMode=yes", "GIT_AUTHOR_NAME": "inherited",
	} {
		t.Setenv(name, value)
	}
	env := premergeGitEnv("GIT_INDEX_FILE=/evaluation/index", "GIT_AUTHOR_NAME=premerge")
	var gitEntries []string // only these are reported: the rest of the environment may hold credentials
	for _, entry := range env {
		if strings.HasPrefix(entry, "GIT_") {
			gitEntries = append(gitEntries, entry)
		}
	}
	count := func(name string) (n int) {
		for _, entry := range env {
			if strings.HasPrefix(entry, name+"=") {
				n++
			}
		}
		return n
	}
	for _, name := range []string{"GIT_DIR", "GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES", "GIT_CEILING_DIRECTORIES", "GIT_NAMESPACE", "GIT_CONFIG_PARAMETERS"} {
		if count(name) != 0 {
			t.Errorf("%s inherited: %q", name, gitEntries)
		}
	}
	if count("GIT_INDEX_FILE") != 1 || count("GIT_AUTHOR_NAME") != 1 || !slices.Contains(env, "GIT_INDEX_FILE=/evaluation/index") || !slices.Contains(env, "GIT_AUTHOR_NAME=premerge") {
		t.Errorf("overrides not applied once: %q", gitEntries)
	}
	if !slices.Contains(env, "GIT_SSH_COMMAND=ssh -o BatchMode=yes") {
		t.Errorf("the transport variable was dropped: %q", gitEntries)
	}
}
