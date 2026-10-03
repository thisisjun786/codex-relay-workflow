package pipeline

import (
	"context"

	"github.com/thisisjun786/codex-relay-workflow/internal/review"
	"github.com/thisisjun786/codex-relay-workflow/internal/review/agy"
	"github.com/thisisjun786/codex-relay-workflow/internal/review/bundle"
)

type Runner func(context.Context, agy.Config, agy.Request) (agy.Result, error)
type HeadReader interface {
	review.HeadReader
	ReadLines(string, int, int) ([]string, error)
}
type Config struct {
	Agy  agy.Config
	Head HeadReader
}

func Run(context.Context, *bundle.Bundle, Runner, Config) (*review.Artifact, error) {
	return &review.Artifact{}, nil
}
