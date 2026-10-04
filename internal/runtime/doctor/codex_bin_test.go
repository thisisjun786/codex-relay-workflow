package doctor

import (
	"reflect"
	"testing"
)

func TestCodexInvocationPOSIX(t *testing.T) {
	for _, tc := range []struct {
		env  []string
		want string
	}{
		{nil, "codex"}, {[]string{"CODEX_BIN=/opt/codex/bin/codex"}, "/opt/codex/bin/codex"},
		{[]string{"CODEX_BIN= \ufeff /opt/bin/codex \n"}, "/opt/bin/codex"}, {[]string{"CODEX_BIN= \t"}, "codex"},
		{[]string{"codex_bin=first", "CoDeX_BiN=second"}, "first"}, {[]string{"codex_bin=lower", "CODEX_BIN="}, "codex"},
	} {
		args := []string{"features", "list"}
		got := ResolveCodexInvocation("codex", args, tc.env)
		if got.File != tc.want || !reflect.DeepEqual(got.Args, args) || got.Options == nil || len(got.Options) != 0 {
			t.Fatalf("%v: %+v", tc.env, got)
		}
		args[0] = "changed"
		if got.Args[0] != "features" {
			t.Fatal("arguments alias caller")
		}
	}
	if got := ResolveCodexInvocation("relative/bin", nil, nil); got.File != "relative/bin" || got.Args == nil {
		t.Fatalf("passthrough: %+v", got)
	}
}
