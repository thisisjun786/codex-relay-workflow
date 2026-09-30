package managed

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/pyjson"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// RequestIdentity is managed.py:183's physical selector and complete request fingerprint.
// The original selector spellings are part of replay identity even when they resolve to
// the same physical path. The observed bridge ledger identity is mandatory.
func RequestIdentity(ctx context.Context, request map[string]any, s *store.Store, socket, markerRoot, stateSelector string, ledger map[string]any) (Identity, error) {
	loc, err := s.Locate(ctx)
	if err != nil {
		return Identity{}, err
	}
	if loc.Device == 0 || loc.Inode == 0 {
		return Identity{}, fmt.Errorf("the selected store has no observed physical identity")
	}
	root, err := canonicalMarkerPath(markerRoot)
	if err != nil {
		return Identity{}, err
	}
	socketPath, err := canonicalMarkerPath(socket)
	if err != nil {
		return Identity{}, err
	}
	workspace, err := canonicalMarkerPath(request["child"].(map[string]any)["settings"].(map[string]any)["cwd"].(string))
	if err != nil {
		return Identity{}, err
	}
	roots := make([]any, 0)
	for _, r := range request["artifactRoots"].([]any) {
		resolved, e := canonicalMarkerPath(r.(string))
		if e != nil {
			return Identity{}, e
		}
		roots = append(roots, resolved)
	}
	if stateSelector == "" {
		stateSelector = store.PathlibParent(s.Path)
	}
	selectors := map[string]any{
		"original": map[string]any{"state": stateSelector, "socket": socket, "markerRoot": markerRoot},
		"db":       map[string]any{"storeId": loc.StoreID, "device": loc.Device, "inode": loc.Inode, "realPath": loc.RealPath},
		"socket":   socketPath, "markerRoot": root, "workspace": workspace, "ledger": ledger, "artifactRoots": roots, "bootstrapVersion": 1,
	}
	canonical, err := compactPythonJSON(map[string]any{"request": request, "selectors": selectors})
	if err != nil {
		return Identity{}, err
	}
	sum := sha256.Sum256(canonical)
	create, dispatch := OperationIDs(request["requestId"].(string))
	return Identity{RequestID: request["requestId"].(string), IssueKey: request["issueKey"].(string), Fingerprint: hex.EncodeToString(sum[:]), Version: Schema, Workspace: workspace, MarkerRoot: root, SocketIdentity: socketPath, CreateRequestID: create, DispatchRequestID: dispatch}, nil
}
func canonicalMarkerPath(name string) (string, error) {
	if strings.TrimSpace(name) == "" || !filepath.IsAbs(name) {
		return "", fmt.Errorf("path must be absolute")
	}
	absolute, err := filepath.Abs(name)
	if err != nil {
		return "", err
	}
	// Python Path.resolve(strict=False) follows an existing prefix and normalises the rest.
	prefix := absolute
	var suffix []string
	for {
		resolved, e := filepath.EvalSymlinks(prefix)
		if e == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return resolved, nil
		}
		if !os.IsNotExist(e) {
			return "", e
		}
		parent := filepath.Dir(prefix)
		if parent == prefix {
			return "", e
		}
		suffix = append(suffix, filepath.Base(prefix))
		prefix = parent
	}
}

// compactPythonJSON is the request fingerprint's canonical text: sorted keys and compact
// separators, every scalar as encoding/json's json.Marshal writes it with its HTML escapes put
// back (pyjson Marshal, UnescapeHTML), the bytes every stored fingerprint was taken over.
func compactPythonJSON(value any) ([]byte, error) {
	return pyjson.Encode(value, pyjson.Options{Compact: true, SortKeys: true, Marshal: true, UnescapeHTML: true})
}
