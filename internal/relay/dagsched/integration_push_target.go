package dagsched

import (
	"context"
	"strings"
)

// remoteURLForPush is the address a push goes to: the remote's push URL when one is configured, else the remote named.
// The remote branch is read from the same address the push writes to, so a separate push URL cannot hide a divergence
// (CRW-965 review).
func remoteURLForPush(ctx context.Context, checkout, remote string) string {
	out, err := runGit(ctx, checkout, nil, "remote", "get-url", "--push", remote)
	if err != nil {
		return remote
	}
	if url := strings.TrimSpace(out); url != "" {
		return url
	}
	return remote
}
