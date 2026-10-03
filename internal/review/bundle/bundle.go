package bundle

import "context"

type Options struct{ Context, MaxFileBytes, MaxChunkBytes, MaxRuleBytes int }
type Chunk struct {
	Paths []string
	Text  string
}
type Bundle struct{ Chunks []Chunk }

func Build(context.Context, string, string, string, Options) (*Bundle, error) {
	return &Bundle{}, nil
}
