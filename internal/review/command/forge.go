package command

import (
	"context"
	"errors"
	"io"
)

type prComment struct {
	ID   int64  `json:"id"`
	Body string `json:"body"`
	URL  string `json:"html_url"`
}

type forge interface {
	List(ctx context.Context, pr int) ([]prComment, error)
	Create(ctx context.Context, pr int, body string) (prComment, error)
	Update(ctx context.Context, id int64, body string) (prComment, error)
}

type ghForge struct{ binary, dir string }

var errStub = errors.New("not implemented")

func (ghForge) List(context.Context, int) ([]prComment, error)           { return nil, errStub }
func (ghForge) Create(context.Context, int, string) (prComment, error)   { return prComment{}, errStub }
func (ghForge) Update(context.Context, int64, string) (prComment, error) { return prComment{}, errStub }

func postSummary(ctx context.Context, cfg Config, f forge, sum *Summary, stderr io.Writer) error {
	return errStub
}
