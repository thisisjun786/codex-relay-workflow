package hook

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProtectionStaysWithOriginalCheckout(t *testing.T) {
	r := newDelRig(t)
	r.allowed(t, "cd "+r.other+" && rm -rf .", "git -C "+r.other+" clean -fdx", "(cd "+r.other+"; rmdir .)")
	for _, target := range []string{r.checkout, r.slotRoot, filepath.Dir(r.slotRoot), filepath.Dir(r.worktrees)} {
		for _, cmd := range []string{
			"cd " + r.other + " && rm -rf " + target,
			"git -C " + r.other + " worktree remove " + target,
			"(cd " + r.other + "; rm -rf " + target + ")",
			"cd " + r.other + " && mv " + target + " ./gone",
			"cd " + r.other + " && find " + target + " -delete",
			"cd " + r.other + " && printf '%s\\n' " + target + " | xargs rm -rf",
		} {
			r.denied(t, cmd)
		}
	}
	wtWrite(t, filepath.Join(r.other, "wipe.sh"), "rm -rf "+r.worktrees+"\n")
	r.denied(t, "cd "+r.other+" && bash wipe.sh")
	wtWrite(t, filepath.Join(r.other, "clean.sh"), "rm -rf .\n")
	r.allowed(t, "cd "+r.other+" && bash clean.sh")
}

func TestFixedProtectionPathBoundariesAndFallback(t *testing.T) {
	r := newDelRig(t)
	r.allowed(t, "rm -rf ./build", "rm -rf "+r.checkout+"2")
	r.denied(t, "cd \"$UNKNOWN\"; rm -rf .", "rm -rf \"$TARGET\"")
	alias := filepath.Join(r.home, "alias")
	if err := os.Symlink(r.slotRoot, alias); err != nil {
		t.Fatal(err)
	}
	r.denied(t, "cd "+r.other+" && rm -rf "+alias)
	r.allowed(t, "cd "+alias+"/repo && rm -rf ./build")
	id := r.id()
	id.CheckoutRoot = ""
	if !evaluateCommand("cd "+r.other+" && rm -rf "+r.checkout, r.checkout, id).Deny {
		t.Fatal("original cwd fallback lost")
	}
	if evaluateCommand("cd "+r.other+" && rm -rf .", r.checkout, id).Deny {
		t.Fatal("fallback follows cd")
	}
}

func TestUnreadableWorktreeCommandGivesAnalysisRemedy(t *testing.T) {
	r := newDelRig(t)
	v := r.verdict("python3 -m unknown_module")
	if !v.Deny || !strings.Contains(v.Reason, "WORKTREE-GUARD-03") || !strings.Contains(v.Reason, "cannot analyze") || strings.Contains(v.Reason, "teardown") || strings.Contains(v.Reason, "it deletes") || strings.Contains(v.Reason, "archive") {
		t.Fatalf("unreadable verdict: %+v", v)
	}
}
