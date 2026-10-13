package tracking

import (
	"context"
	"errors"
)

// ErrNoToken is the error of a token source that has no token.
var ErrNoToken = errors.New("tracking: no token")

// TokenSource gives the token the GitHub implementation sends. It is asked once per request, so a
// source may rotate its token.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// TokenFunc makes a TokenSource of a function.
type TokenFunc func(ctx context.Context) (string, error)

func (f TokenFunc) Token(ctx context.Context) (string, error) { return "", errNotImplemented }

// StaticToken is a TokenSource for one fixed token.
func StaticToken(token string) TokenSource { return nil }

// EnvToken reads GH_TOKEN, then GITHUB_TOKEN, through getenv.
func EnvToken(getenv func(string) string) TokenSource { return nil }

// GHAuthToken runs "<gh> auth token".
func GHAuthToken(gh string) TokenSource { return nil }

// FirstToken asks each source in order and gives the first token; it skips a source whose error is
// ErrNoToken and stops at any other error.
func FirstToken(sources ...TokenSource) TokenSource { return nil }

// CachedToken asks src until it gives a token, then keeps that token.
func CachedToken(src TokenSource) TokenSource { return nil }

// DefaultTokenSource is the production source: GH_TOKEN or GITHUB_TOKEN, else "gh auth token".
func DefaultTokenSource() TokenSource { return nil }
