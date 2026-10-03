package bundle

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ErrSizeLimit means an indivisible file group or the repeated preamble cannot fit.
var ErrSizeLimit = errors.New("review bundle size limit")

// Options accepts only numeric rendering configuration. Zero values use defaults.
type Options struct{ Context, MaxFileBytes, MaxChunkBytes, MaxRuleBytes int }

// FileMetadata describes one change; byte sizes distinguish source and rendered text.
type FileMetadata struct {
	Path, OldPath, Status, Mode string
	Binary, Truncated           bool
	Additions, Deletions        int
	HeadLines, HeadBytes        int
	TextBytes, DiffBytes        int
}

// RuleMetadata records present, missing and truncated fixed rule documents.
type RuleMetadata struct {
	Path               string
	Missing, Truncated bool
	SourceBytes, Bytes int
}

// Metadata records resolved identities and exact output sizes, without timestamps.
type Metadata struct {
	Base, Head, MergeBase, PatchID   string
	Files                            []FileMetadata
	Rules                            []RuleMetadata
	Additions, Deletions, FileCount  int
	ChunkCount, DiffBytes, HeadBytes int
	RuleBytes, TotalBytes            int
	ChunkSizes                       []int
	TruncatedFiles                   []string
}

// Chunk is one complete prompt-material group, in path order.
type Chunk struct {
	Paths []string
	Text  string
}

// Bundle contains material for one review pass. Pipeline callers add their prompts.
type Bundle struct {
	Metadata Metadata
	Chunks   []Chunk
}

type file struct {
	FileMetadata
	patch, text string
}

func defaults(o Options) (Options, error) {
	values := []*int{&o.Context, &o.MaxFileBytes, &o.MaxChunkBytes, &o.MaxRuleBytes}
	for i, v := range values {
		if *v < 0 {
			return o, fmt.Errorf("negative bundle option %d", i)
		}
		if *v == 0 {
			*v = []int{25, 64 << 10, 256 << 10, 32 << 10}[i]
		}
	}
	return o, nil
}

// Build reads committed Git objects only; repo, base and head identify the source.
// It accepts no authoring context and executes neither code nor document content.
func Build(ctx context.Context, repo, base, head string, options Options) (*Bundle, error) {
	o, err := defaults(options)
	if err != nil {
		return nil, err
	}
	g := gitRepo{ctx: ctx, path: repo}
	m := Metadata{}
	if m.Base, err = g.resolve(base); err != nil {
		return nil, err
	}
	if m.Head, err = g.resolve(head); err != nil {
		return nil, err
	}
	g.head = m.Head
	mb, err := g.run(nil, "merge-base", "--all", m.Base, m.Head)
	if err != nil {
		return nil, err
	}
	bases := strings.Fields(string(mb))
	if len(bases) != 1 {
		return nil, fmt.Errorf("review bundle requires one merge base, got %d", len(bases))
	}
	m.MergeBase = bases[0]
	files, err := g.changes(m.MergeBase, m.Head, o.Context)
	if err != nil {
		return nil, err
	}
	patch, err := g.diff(m.MergeBase, m.Head, 3, "-p")
	if err != nil {
		return nil, err
	}
	if len(patch) > 0 {
		id, err := g.run(patch, "patch-id", "--stable")
		if err != nil {
			return nil, err
		}
		fields := strings.Fields(string(id))
		if len(fields) == 0 {
			return nil, errors.New("git produced no patch-id for a nonempty diff")
		}
		m.PatchID = fields[0]
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	for i := range files {
		f := &files[i]
		if err := g.headText(f, o); err != nil {
			return nil, err
		}
		m.Files = append(m.Files, f.FileMetadata)
		m.Additions += f.Additions
		m.Deletions += f.Deletions
		m.DiffBytes += f.DiffBytes
		m.HeadBytes += f.TextBytes
		if f.Truncated {
			m.TruncatedFiles = append(m.TruncatedFiles, f.Path)
		}
	}
	m.FileCount = len(files)
	rules, err := g.rules(o.MaxRuleBytes, &m)
	if err != nil {
		return nil, err
	}
	chunks, err := group(files, preamble(m)+rules, o.MaxChunkBytes)
	if err != nil {
		return nil, err
	}
	m.ChunkCount = len(chunks)
	for _, c := range chunks {
		m.ChunkSizes = append(m.ChunkSizes, len(c.Text))
		m.TotalBytes += len(c.Text)
	}
	return &Bundle{Metadata: m, Chunks: chunks}, nil
}
