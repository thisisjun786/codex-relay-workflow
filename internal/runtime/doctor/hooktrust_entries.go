// Hook trust entry listing, ported from CXC v0.2.40 hook-trust.ts:48-57 and :140-216 (stub: the
// port follows in the next commit).
package doctor

import "errors"

// HookTrustEntry is one trust-relevant hook of a plugin.
type HookTrustEntry struct {
	Key, Hash, FileSha256 string
}

// ListHookTrustEntries lists the trust-relevant hooks of the plugin at pluginRoot.
func ListHookTrustEntries(pluginRoot, pluginKey string) ([]HookTrustEntry, error) {
	return nil, errors.New("not implemented")
}
