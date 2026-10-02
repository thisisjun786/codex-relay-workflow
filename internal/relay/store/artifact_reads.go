package store

import (
	"context"
	"sync/atomic"
)

// ArtifactReads counts the artifacts one piece of work had HashArtifactContext read. It is a
// measurement: it never changes what a read answers. A test or a benchmark that wants to say "this tick
// read N artifacts" installs one with WithArtifactReads and hands the context to the code under test.
type ArtifactReads struct{ n atomic.Int64 }

type artifactReadsKey struct{}

// WithArtifactReads returns ctx and the counter for the artifact reads made under it, by this goroutine
// or by any code the context reaches (a store transaction keeps the values of the context it began on).
func WithArtifactReads(ctx context.Context) (context.Context, *ArtifactReads) {
	reads := &ArtifactReads{}
	return context.WithValue(ctx, artifactReadsKey{}, reads), reads
}

// Count is how many artifacts HashArtifactContext went on to read under the counter's context: one for
// each call that reads the bytes (a call refused before the read, or by a cancelled context, is not
// counted), whatever the number of passes it makes over them.
func (r *ArtifactReads) Count() int64 { return r.n.Load() }

func countArtifactRead(ctx context.Context) {
	if reads, ok := ctx.Value(artifactReadsKey{}).(*ArtifactReads); ok {
		reads.n.Add(1)
	}
}
