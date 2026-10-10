package hook

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDeletionGuardDoesNotAssumeFailedCDSucceeded(t *testing.T) {
	r := newDelRig(t)
	missing := filepath.Join(r.other, "missing")
	for _, destination := range []string{missing, r.other} {
		for _, command := range []string{
			"cd " + destination + " || find . -delete",
			"cd " + destination + "; find . -delete",
			"! cd " + destination + " && find . -delete",
			"cd " + destination + " && true; find . -delete",
			"{ cd " + destination + "; true; } && find . -delete",
			"cd " + destination + " || rm -rf .",
			"bash -c 'cd " + destination + "; find . -delete' && true",
		} {
			r.denied(t, command)
		}
	}
	r.allowed(t, "cd "+r.other+" && find . -delete", "cd "+r.other+" && rm -rf .", "(cd "+r.other+" && rmdir .)")
	r.allowed(t, "cd "+r.other+" && true && find . -delete")
	r.allowed(t, "cd "+missing+" || rm -rf "+r.other)
	r.denied(t, "cd "+missing+" || rm -rf "+r.checkout)
	wtWrite(t, filepath.Join(r.other, "failed-cd.sh"), "cd "+missing+" || find . -delete\n")
	r.denied(t, "bash "+filepath.Join(r.other, "failed-cd.sh"))
}

func TestDeletionGuardRefusesFindLinkFollowingTraversal(t *testing.T) {
	r := newDelRig(t)
	if err := os.Symlink(r.checkout, filepath.Join(r.other, "checkout-link")); err != nil {
		t.Fatal(err)
	}
	r.denied(t,
		"cd "+r.other+" && find -L . -delete",
		"find -L "+r.other+" -name '*' -delete",
		"find -L "+r.other+" -exec rm -rf {} \\;",
		"find -L "+r.other+" -execdir rmdir {} \\;",
		"find -P -L "+r.other+" -exec git worktree remove {} \\;",
	)
	r.allowed(t, "cd "+r.other+" && find -P . -delete", "find -L -P "+r.other+" -delete", "find -L "+r.other+" -print")
}
