package hook

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestRefusedCommandsHarmlessRewrites is the false-refusal differential (CRW-1028, criterion c2c). Every refused removal
// of the rule audit is rewritten to a harmless command (the removal becomes echo, find -delete becomes -print) and run by
// each real shell present, in a throwaway home with no network and PATH limited to the system directories. A rewrite the
// shell runs to success that the reader still refuses is a false refusal; the test reports each one and does not fail on
// them, because refusing by rule is the design. It also checks that the real home listings do not change.
func TestRefusedCommandsHarmlessRewrites(t *testing.T) {
	if testing.Short() {
		t.Skip("false-refusal differential skipped in -short")
	}
	var shells []string
	for _, s := range []string{"bash", "zsh"} {
		if p, err := exec.LookPath(s); err == nil {
			shells = append(shells, p)
		}
	}
	if len(shells) == 0 {
		t.Skip("no bash or zsh on this host")
	}
	home := t.TempDir()
	realHome, _ := os.UserHomeDir()
	before := refusedHomeListing(realHome)
	r := newDelRig(t)
	fill := strings.NewReplacer("{CHECKOUT}", r.checkout, "{SLOT}", r.slotRoot, "{OTHER}", r.other, "{HOME}", r.home)
	rewrite := strings.NewReplacer("rm -rf", "echo", "rm ", "echo ", "-delete", "-print")
	refusedRows, falseRefusals := 0, 0
	for _, row := range auditRows() {
		if row.worktree != "deny" || !strings.Contains(row.cmd, "rm") && !strings.Contains(row.cmd, "-delete") {
			continue
		}
		harmless := rewrite.Replace(row.cmd)
		if harmless == row.cmd {
			continue
		}
		refusedRows++
		cmd := fill.Replace(harmless)
		ran := false
		for _, sh := range shells {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			c := exec.CommandContext(ctx, sh, "-c", cmd)
			c.Dir = r.other
			c.Env = []string{"HOME=" + home, "PATH=/usr/bin:/bin", "LANG=C"}
			out, err := c.CombinedOutput()
			cancel()
			if err != nil {
				continue
			}
			ran = true
			_ = out
		}
		if !ran {
			continue
		}
		if v := r.verdict(cmd); v.Deny {
			falseRefusals++
			t.Logf("FALSE REFUSAL %s: %q (%s)", row.id, cmd, v.Reason)
		}
	}
	t.Logf("refused rows rewritten: %d, harmless under a shell but refused by the reader: %d", refusedRows, falseRefusals)
	after := refusedHomeListing(realHome)
	if before != after {
		t.Errorf("the real home listing changed during the differential")
	}
	r.intact(t)
}

// refusedHomeListing lists the names under the real ~/.codex and ~/.crw, so a run that touched them is visible.
func refusedHomeListing(home string) string {
	if home == "" {
		return ""
	}
	var b strings.Builder
	for _, d := range []string{".codex", ".crw"} {
		entries, _ := filepath.Glob(filepath.Join(home, d, "*"))
		for _, e := range entries {
			b.WriteString(e + "\n")
		}
	}
	return b.String()
}
