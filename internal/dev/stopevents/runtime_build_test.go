//go:build dev

package stopevents

import (
	"os"
	"testing"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/hook"
)

func TestRuntimeBuildOldAndNewJournalRows(t *testing.T) {
	identity := hook.Object{{Key: "build", Value: "recorded-build"}, {Key: "executable", Value: "/synthetic/bin/crw"}}
	for _, path := range []string{"event", "excluded", "prescan"} {
		for _, old := range []bool{false, true} {
			t.Run(path+map[bool]string{false: "/new", true: "/old"}[old], func(t *testing.T) {
				h := newHost(t, hook.Release)
				if path == "prescan" {
					if err := os.Remove(h.state + "/control.sock"); err != nil {
						t.Fatal(err)
					}
				}
				if path == "event" {
					h.twice(h.at(loadFixture(t), 0))
				} else {
					h.run(h.settings, looseStop)
				}
				for _, row := range h.rowPaths(h.journal) {
					change(t, row, func(o hook.Object) hook.Object {
						if old {
							return setAt(o, []string{"runtime"}, nil, true)
						}
						return o.Set("runtime", identity)
					})
				}
				code, answer := verify(t, roots(h.journal)...)
				wantCode, wantVerdict := 0, "TRUE"
				if path == "excluded" {
					wantCode, wantVerdict = 3, "UNREADABLE"
				}
				expectVerdict(t, code, answer, wantCode, wantVerdict)
				if path != "event" {
					exclusionReports(t, answer, 1)
				}
			})
		}
	}
}

func TestRuntimeBuildMalformedAttributionIsUnreadable(t *testing.T) {
	for _, value := range []any{nil, true, "build", hook.Object{}, hook.Object{{Key: "build", Value: ""}, {Key: "executable", Value: nil}},
		hook.Object{{Key: "build", Value: "build"}, {Key: "executable", Value: "relative"}},
		hook.Object{{Key: "build", Value: "build"}, {Key: "executable", Value: nil}, {Key: "extra", Value: true}}} {
		h := newHost(t, hook.Release)
		if err := os.Remove(h.state + "/control.sock"); err != nil {
			t.Fatal(err)
		}
		h.run(h.settings, looseStop)
		change(t, h.rowPaths(h.journal)[0], set("runtime", value))
		code, answer := verify(t, roots(h.journal)...)
		expectVerdict(t, code, answer, 3, "UNREADABLE")
	}
	h := newHost(t, hook.Release)
	if err := os.Remove(h.state + "/control.sock"); err != nil {
		t.Fatal(err)
	}
	h.run(h.settings, looseStop)
	change(t, h.rowPaths(h.journal)[0], set("runtime", hook.Object{{Key: "build", Value: "build"}, {Key: "executable", Value: nil}}))
	code, answer := verify(t, roots(h.journal)...)
	expectVerdict(t, code, answer, 0, "TRUE")
}
