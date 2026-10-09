package shellir

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// TestFunctionCannotShadowChdir: chdir is a modelled directory change (the zsh synonym of cd), so a function of that name would
// hide the builtin the shell runs past it. The reader refuses the definition (CRW-1064 d2, P0).
func TestFunctionCannotShadowChdir(t *testing.T) {
	for _, cmd := range []string{
		"chdir() { :; }; chdir sub",
		"chdir() { :; }; for i in 1; do builtin chdir sub; done; bash prog.sh",
	} {
		if _, err := Analyze(cmd, "/work"); !isUnreadable(err) {
			t.Errorf("%q: err = %v, want unreadable", cmd, err)
		}
	}
}

// TestPrescanWorkIsBounded: each function calls the next one twice. Judging the loop without memoised function effects visits
// the bottom function 2^30 times before the ordinary depth check can run (CRW-1064 d3, P1). The analysis must finish.
func TestPrescanWorkIsBounded(t *testing.T) {
	const depth = 30
	var b strings.Builder
	for i := depth; i >= 1; i-- {
		fmt.Fprintf(&b, "f%d() { f%d; f%d; }; ", i, i-1, i-1)
	}
	b.WriteString("f0() { :; }; ")
	for _, cmd := range []string{
		b.String() + fmt.Sprintf("for i in 1; do f%d; done; bash prog.sh", depth),
		b.String() + fmt.Sprintf("case x in x) f%d ;& y) : ;; esac; bash prog.sh", depth),
	} {
		done := make(chan struct{})
		go func() {
			defer close(done)
			_, _ = Analyze(cmd, "/work")
		}()
		select {
		case <-done:
		case <-time.After(20 * time.Second):
			t.Fatalf("analysis of a %d-byte command did not finish in 20s: the prescan repeats function calls", len(cmd))
		}
	}
}
