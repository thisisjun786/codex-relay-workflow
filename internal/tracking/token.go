package tracking

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
)

// ErrNoToken is the error of a token source that has no token.
var ErrNoToken = errors.New("tracking: no token")

// TokenSource gives the token the GitHub implementation sends. It is asked once per request, so a
// source may rotate its token. A source never writes the token to a log or an error.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// TokenFunc makes a TokenSource of a function.
type TokenFunc func(ctx context.Context) (string, error)

// Token calls f.
func (f TokenFunc) Token(ctx context.Context) (string, error) { return f(ctx) }

// StaticToken is a TokenSource for one fixed token; a blank token is ErrNoToken.
func StaticToken(token string) TokenSource {
	return TokenFunc(func(context.Context) (string, error) {
		if t := strings.TrimSpace(token); t != "" {
			return t, nil
		}
		return "", ErrNoToken
	})
}

// EnvToken reads GH_TOKEN, then GITHUB_TOKEN, through getenv.
func EnvToken(getenv func(string) string) TokenSource {
	return TokenFunc(func(context.Context) (string, error) {
		for _, key := range []string{"GH_TOKEN", "GITHUB_TOKEN"} {
			if t := strings.TrimSpace(getenv(key)); t != "" {
				return t, nil
			}
		}
		return "", ErrNoToken
	})
}

// GHAuthToken runs "<gh> auth token". A gh that is missing, fails or prints nothing is ErrNoToken;
// the error carries gh's exit status and none of its output.
func GHAuthToken(gh string) TokenSource {
	return TokenFunc(func(ctx context.Context) (string, error) {
		out, err := exec.CommandContext(ctx, gh, "auth", "token").Output()
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if err != nil {
			var exit *exec.ExitError
			if errors.As(err, &exit) {
				return "", fmt.Errorf("%w: gh auth token: %s", ErrNoToken, exit.ProcessState)
			}
			return "", fmt.Errorf("%w: gh auth token: %v", ErrNoToken, err)
		}
		if t := strings.TrimSpace(string(out)); t != "" {
			return t, nil
		}
		return "", fmt.Errorf("%w: gh auth token printed nothing", ErrNoToken)
	})
}

// FirstToken asks each source in order and gives the first token; it skips a source whose error is
// ErrNoToken and stops at any other error.
func FirstToken(sources ...TokenSource) TokenSource {
	return TokenFunc(func(ctx context.Context) (string, error) {
		for _, s := range sources {
			t, err := s.Token(ctx)
			switch {
			case err == nil:
				return t, nil
			case !errors.Is(err, ErrNoToken):
				return "", err
			}
		}
		return "", ErrNoToken
	})
}

// CachedToken asks src until it gives a token, then keeps that token.
func CachedToken(src TokenSource) TokenSource {
	var mu sync.Mutex
	var token string
	return TokenFunc(func(ctx context.Context) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		if token != "" {
			return token, nil
		}
		t, err := src.Token(ctx)
		if err != nil {
			return "", err
		}
		token = t
		return t, nil
	})
}

// DefaultTokenSource is the production source: GH_TOKEN or GITHUB_TOKEN, else "gh auth token",
// kept after the first success.
func DefaultTokenSource() TokenSource {
	return CachedToken(FirstToken(EnvToken(os.Getenv), GHAuthToken("gh")))
}
