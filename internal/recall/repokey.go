package recall

type ReadOriginUrl func(string) string

const GIT_TIMEOUT_MS = 1500

func normalizeRepoKey(raw string) string                     { return "" }
func pack(host, path string) string                          { return "" }
func readOriginUrl(cwd string) string                        { return "" }
func repoKeyForCwd(cwd string, read ...ReadOriginUrl) string { return "" }
func repoKeysEqual(a, b string) bool                         { return false }
