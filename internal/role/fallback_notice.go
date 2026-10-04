package role

import (
	"context"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"io"
)

const DispatchGuidance = ""

func SessionFallbackNotice(env host.LookupEnv) (string, error) { return "", nil }
func fallbackSessionNotice(env host.LookupEnv, render func() (string, error)) (string, error) {
	return "", nil
}
func RunFallbackNoticeHook(context.Context, io.Reader, io.Writer, host.LookupEnv) int { return 0 }
