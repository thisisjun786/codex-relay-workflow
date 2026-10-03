package agy

import "strings"

// passEnv names the variables agy takes from the caller's environment, by exact name. Nothing else passes, and the caller cannot add to the list.
var passEnv = map[string]bool{
	"PATH": true, "HOME": true, "USER": true, "LOGNAME": true, "TZ": true, "TERM": true, "TMPDIR": true,
	"XDG_CONFIG_HOME": true, "XDG_DATA_HOME": true, "XDG_STATE_HOME": true, "XDG_CACHE_HOME": true, "XDG_RUNTIME_DIR": true, "XDG_CONFIG_DIRS": true, "XDG_DATA_DIRS": true,
	"LANG": true, "LANGUAGE": true, "LC_ALL": true, "LC_CTYPE": true, "LC_MESSAGES": true, "LC_TIME": true, "LC_COLLATE": true, "LC_NUMERIC": true, "LC_MONETARY": true,
	"SSL_CERT_FILE": true, "SSL_CERT_DIR": true,
	"HTTP_PROXY": true, "HTTPS_PROXY": true, "ALL_PROXY": true, "NO_PROXY": true, "http_proxy": true, "https_proxy": true, "all_proxy": true, "no_proxy": true,
}

// scrubEnv is environ cut down to the allowlist.
func scrubEnv(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if passEnv[name] {
			out = append(out, kv)
		}
	}
	return out
}
