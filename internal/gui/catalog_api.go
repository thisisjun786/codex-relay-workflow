package gui

import (
	"net/http"
	"os"

	"github.com/thisisjun786/codex-relay-workflow/internal/role"
)

// catalogReader is the process-wide model-catalog reader. The oracle keeps one pending map per
// process, so the server shares one reader across its requests rather than building one per call.
var catalogReader role.CatalogReader

// catalogHandler answers GET /api/catalog[?refresh=1] with the model catalog. The reader owns the
// judgement, and this route returns its answer unchanged: unsupported (this host's OCX does not
// read the live catalog), a read failure, and stale (a failed refresh behind a cached list) are
// three states the reader tells apart, and re-deciding any of them here would be a second reader.
func catalogHandler(_ *Env, r *http.Request) (Response, error) {
	force := r.URL.Query().Get("refresh") == "1"
	catalog, err := catalogReader.ReadCatalog(role.CatalogOptions{
		ForceRefresh: force,
		Environ:      os.Environ(),
	})
	if err != nil {
		return Response{Status: http.StatusOK, Body: map[string]any{
			"status":  "unavailable",
			"state":   string(role.CatalogUnavailable),
			"entries": []role.CatalogEntry{},
			"message": err.Error(),
		}}, nil
	}
	return Response{Status: http.StatusOK, Body: catalog}, nil
}

func init() {
	Register(http.MethodGet, "/api/catalog", catalogHandler)
}
