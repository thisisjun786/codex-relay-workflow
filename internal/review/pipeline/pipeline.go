package pipeline

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/review"
	"github.com/thisisjun786/codex-relay-workflow/internal/review/agy"
	"github.com/thisisjun786/codex-relay-workflow/internal/review/bundle"
)

// Runner is agy.Run's shape; callers must inject it explicitly.
type Runner func(context.Context, agy.Config, agy.Request) (agy.Result, error)

// HeadReader supplies contiguous, clipped 1-based lines at the bundle's immutable
// head. Both methods must read that same head. ReadLines includes both endpoints.
type HeadReader interface {
	review.HeadReader
	ReadLines(string, int, int) ([]string, error)
}
type Config struct {
	Agy                             agy.Config
	Head                            HeadReader
	Thresholds                      [3]int
	Perspectives                    []Perspective
	ToolVersion, Effort, AgyVersion string // artifact identity only, never prompt text
}

var serial = make(chan struct{}, 1)

type runState struct {
	ctx      context.Context
	b        *bundle.Bundle
	runner   Runner
	cfg      Config
	a        *review.Artifact
	rules    review.Rules
	problems []string
}

// Run assembles an artifact in memory; it writes nothing and provides no repository
// tools. Cancellation returns no artifact. See doc.go for fail-open semantics.
func Run(ctx context.Context, b *bundle.Bundle, runner Runner, cfg Config) (*review.Artifact, error) {
	if b == nil || runner == nil || cfg.Head == nil || len(b.Chunks) == 0 || b.Metadata.FileCount == 0 {
		return nil, errors.New("pipeline: bundle, runner, head reader and a nonempty diff are required")
	}
	lenses, err := reviewers(b.Metadata, &cfg)
	if err != nil {
		return nil, err
	}
	if cfg.Agy.Model == "" {
		cfg.Agy.Model = agy.DefaultModel
	}
	if cfg.ToolVersion == "" {
		cfg.ToolVersion = "unknown"
	}
	if cfg.Effort == "" {
		cfg.Effort = "default"
	}
	if cfg.AgyVersion == "" {
		cfg.AgyVersion = "unknown"
	}
	select {
	case serial <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { <-serial }()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	m := b.Metadata
	a := &review.Artifact{Schema: review.SchemaV1, Tool: "crw review", ToolVersion: cfg.ToolVersion, Model: cfg.Agy.Model, Effort: cfg.Effort, AgyVersion: cfg.AgyVersion,
		Base: m.Base, Head: m.Head, PatchID: m.PatchID, Diff: review.DiffStats{Files: m.FileCount, Additions: m.Additions, Deletions: m.Deletions},
		Status: review.StatusUnavailable, Reason: "not started", StartedAt: now, FinishedAt: now, Findings: []review.Finding{}, Dropped: []review.Drop{}}
	if err := a.Validate(m.Head); err != nil {
		return nil, err
	}
	s := &runState{ctx: ctx, b: b, runner: runner, cfg: cfg, a: a, rules: review.Rules{Changed: map[string]bool{}, Head: cfg.Head}}
	for _, f := range m.Files {
		s.rules.Changed[f.Path] = true
	}
	a.Reviewers.Run = len(lenses)
	var raw []review.Finding
	for id, lens := range lenses {
		for chunk, c := range b.Chunks {
			var out struct {
				Findings []review.Finding `json:"findings"`
			}
			ok, err := s.call(review.CallRecord{Stage: "review", Reviewer: id, Chunk: chunk, Perspective: string(lens)}, map[string]any{"chunk": c.Text}, reviewSchema, &out)
			if err != nil {
				return nil, err
			}
			if !ok {
				continue
			}
			if slices.ContainsFunc(out.Findings, func(f review.Finding) bool {
				return strings.TrimSpace(f.Title) == "" || strings.TrimSpace(f.Explanation) == ""
			}) {
				s.invalidLast("blank_finding_text")
				continue
			}
			for _, f := range out.Findings {
				f.Grade, f.Security, _ = review.NormalizeGrade(f.Severity)
				f.Perspective, f.Reviewers, f.Support, f.Verdict = string(lens), []int{id}, 1, review.VerdictUnverified
				raw = append(raw, f)
			}
		}
	}
	grouped, ok, err := s.group(raw)
	if err != nil {
		return nil, err
	}
	if ok {
		if err = s.verify(grouped); err != nil {
			return nil, err
		}
	}
	res, err := s.rules.Apply(grouped)
	if canceled := ctx.Err(); canceled != nil {
		return nil, canceled
	}
	if err != nil {
		return nil, err
	}
	a.Findings, a.Dropped = res.Findings, res.Dropped
	s.assemble()
	if err = a.Validate(m.Head); err != nil {
		return nil, err
	}
	return a, nil
}

func (s *runState) call(c review.CallRecord, data any, schema string, out any) (bool, error) {
	if err := s.ctx.Err(); err != nil {
		return false, err
	}
	p, err := prompt(c.Stage, c.Perspective, data)
	if err != nil {
		return false, err
	}
	r, err := s.runner(s.ctx, s.cfg.Agy, agy.Request{Prompt: p, Schema: []byte(schema)})
	if s.ctx.Err() != nil {
		return false, s.ctx.Err()
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false, err
	}
	c.Model, c.Class, c.Reason, c.ElapsedMillis = r.Model, string(r.Class), string(r.Reason), r.Elapsed.Milliseconds()
	c.Tokens = review.TokenUsage{Input: r.Usage.InputTokens, Output: r.Usage.OutputTokens, Thinking: r.Usage.ThinkingTokens, CacheRead: r.Usage.CacheReadTokens, Total: r.Usage.TotalTokens}
	if err != nil {
		c.Error = err.Error()
	}
	if r.Class != agy.ClassNormal && r.Class != agy.ClassInvalid && r.Class != agy.ClassUnavailable {
		c.Class, c.Reason = "invalid", "invalid_class"
		if err != nil {
			c.Class, c.Reason = "unavailable", "runner_error"
		}
	}
	if c.Class == "normal" {
		if err := parse(r.StructuredOutput, schema, out); err != nil {
			c.Class, c.Reason = "invalid", "invalid_output: "+err.Error()
		}
	} else if strings.TrimSpace(c.Reason) == "" {
		c.Reason = "unspecified_failure"
	}
	s.a.Calls = append(s.a.Calls, c)
	return c.Class == "normal", nil
}

func (s *runState) invalidLast(reason string) {
	c := &s.a.Calls[len(s.a.Calls)-1]
	c.Class, c.Reason = "invalid", reason
}
