package hook

import (
	"context"
	"os/exec"
	"testing"
	"time"
)

func Test33LargeFactHookPython(t *testing.T) {
	home := hookHome(t, 5)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, python(t), "testdata/large_fact.py", binary(t), home)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	t.Logf("%s", out)
}
