package dagsched

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

const pinnedInputDirectory = "dag-input-snapshots"

const pinnedInputReadInstruction = " Volatile snapshot_uri paths in this manifest are retained relay copies authorized as read-only inputs. Consume those exact paths instead of original source paths mentioned in earlier instructions; do not modify the copies."

// Test seam after publication and before temporary-name cleanup; nil in production.
var pinnedInputAfterPublish func(string)

func (s *Scheduler) pinnedInputRoot(plan string) string {
	state, err := filepath.Abs(filepath.Dir(s.Store.Path))
	if err != nil {
		return "" // No retained path can be authorized or published from an unreadable cwd.
	}
	return filepath.Join(state, pinnedInputDirectory, shaOf([]byte(plan)))
}

func pinnedInputPath(root, digest string) string {
	if root == "" || !digestPattern.MatchString(digest) {
		return ""
	}
	return filepath.Join(root, digest)
}

func (s *Scheduler) pinnedInputHasCopies(plan string, body map[string]any) bool {
	list, _ := body["volatile"].([]any)
	for _, item := range list {
		v, _ := item.(map[string]any)
		if path := pinnedInputPath(s.pinnedInputRoot(plan), textOf(v["sha256"])); path != "" && path == textOf(v["snapshot_uri"]) {
			return true
		}
	}
	return false
}

// pinnedInputKeep completes every copy before changing the manifest. Source paths remain
// available to refusals; no request or stored manifest can name a partially copied input.
func (s *Scheduler) pinnedInputKeep(ctx context.Context, plan string, body map[string]any, opts VerifyOptions) error {
	list, _ := body["volatile"].([]any)
	paths := make([]string, len(list))
	root := s.pinnedInputRoot(plan)
	for i, item := range list {
		v := item.(map[string]any)
		paths[i] = pinnedInputPath(root, textOf(v["sha256"]))
		source := textOf(v["snapshot_uri"])
		if paths[i] == "" {
			return refuse(contract.RefusalMalformedReceipt, "volatile snapshot %s cannot name a retained copy under %q", source, root)
		}
		if source == paths[i] {
			continue // BuildManifest already verified this retained input.
		}
		if err := pinnedInputPublish(ctx, source, textOf(v["sha256"]), root, opts.ArtifactRoots); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			finding := BlockedFinding{"B-03", BlockedInputMissing, source + ": " + err.Error()}
			var refused *store.RefusedError
			if errors.As(err, &refused) {
				switch refused.Reason {
				case store.ReasonScopeEscape, store.ReasonPathRelocated, store.ReasonSymlinkComponent:
					finding.Code, finding.Reason = "B-05", BlockedInputOutOfScope
				case store.ReasonArtifactMutated, store.ReasonArtifactLeaseBroken, store.ReasonManifestUnverified:
					finding.Code, finding.Reason = "B-04", BlockedInputHashMismatch
				}
			}
			return refusalOfFinding(finding)
		}
		if finding, _, err := hashEntry(ctx, consumed{Path: paths[i], SHA256: textOf(v["sha256"])}, []string{root}); err != nil {
			return err
		} else if finding != nil {
			return refusalOfFinding(*finding)
		}
	}
	for i, item := range list {
		item.(map[string]any)["snapshot_uri"] = paths[i]
	}
	if len(list) != 0 {
		body["manifest_digest"] = dag.ManifestDigest(body)
	}
	return nil
}

// A changed source is still refused. Recovery is advertised only after hashing the
// retained copy; a missing or corrupted copy is never offered as a restore source.
func pinnedInputRestoreHint(ctx context.Context, root string, v Volatile) string {
	path := pinnedInputPath(root, v.SHA256)
	if path == "" || path == v.SnapshotURI {
		return ""
	}
	if finding, _, err := hashEntry(ctx, consumed{Path: path, SHA256: v.SHA256}, []string{root}); err != nil || finding != nil {
		return ""
	}
	return fmt.Sprintf("; the retained copy at %q holds the pinned bytes: restore by copying it to %q, then recheck sha256 %s; or explicitly use the retained path as snapshot_uri", path, v.SnapshotURI, v.SHA256)
}
