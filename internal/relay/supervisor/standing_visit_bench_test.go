package supervisor

import (
	"fmt"
	"testing"
)

// CRW-299 benchmarks: what the standing read costs as a project grows in relationships and in the
// events of each. Both report the statements a call sent as well as its time, because the time on
// a shared host is noisy and the statements are not.
//
//	go test ./internal/relay/supervisor -run '^$' -bench 'Standing|ProjectVisit' -benchtime 5x
var (
	benchRelationships = []int{8, 32, 128}
	benchEvents        = []int{4, 16, 64}
)

func benchStatements(b *testing.B, w *standingWorld, f func()) {
	b.Helper()
	counter := countStatements(b, w.s)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f()
	}
	b.StopTimer()
	b.ReportMetric(float64(counter.count())/float64(b.N), "stmts/op")
}

// BenchmarkStanding is the project's whole standing answer (what supervisor-standing reads): every
// relationship is active and owes one completion, and each has the given number of events.
func BenchmarkStanding(b *testing.B) {
	for _, relationships := range benchRelationships {
		for _, events := range benchEvents {
			w := newStandingWorld(b, relationships, events)
			b.Run(fmt.Sprintf("rel=%d/events=%d", relationships, events), func(b *testing.B) {
				benchStatements(b, w, func() {
					if _, err := w.c.Standing(w.ctx, worldProject, nil); err != nil {
						b.Fatal(err)
					}
				})
			})
		}
	}
}

// BenchmarkProjectVisit is the daemon's visit to a project whose reports have all gone up (the
// state of nearly every visit: it reads and writes nothing): the standing read and the staging
// pass over it, in a project where three relationships in four are archived.
func BenchmarkProjectVisit(b *testing.B) {
	for _, relationships := range benchRelationships {
		for _, events := range benchEvents {
			w := newStandingWorld(b, relationships, events)
			w.settle()
			for _, rid := range w.rels[:relationships*3/4] {
				w.archive(rid)
			}
			b.Run(fmt.Sprintf("rel=%d/events=%d", relationships, events), func(b *testing.B) {
				benchStatements(b, w, func() { w.visit() })
			})
		}
	}
}
