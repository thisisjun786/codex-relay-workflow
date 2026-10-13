package doctor

import (
	"context"
	"strconv"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/role"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/record"
	"github.com/thisisjun786/codex-relay-workflow/internal/runtime/scope"
)

// ModelCatalogReading is what a model catalogue probe answers: the models a provider or the Codex home lists, and which catalogue
// that was. State is the role package's catalogue state (ocx-active, native-catalog, unsupported-ocx-catalog, unavailable); only the
// first two are lists that can condemn a model.
type ModelCatalogReading struct {
	State  string
	Source string
	Models []string
	Detail string
}

// readsModels is whether the reading is a list a model can be missing from.
func (r ModelCatalogReading) readsModels() bool {
	return r.State == string(role.CatalogOcx) || r.State == string(role.CatalogNative)
}

// readModelCatalog is the production probe: the same discovery the role helper and the GUI use (OCX's live catalogue, else the
// native catalogue), without the cache file that reader keeps, because the doctor writes nothing.
func readModelCatalog(_ context.Context, env scope.Env) ModelCatalogReading {
	live := role.ReadCatalogReadOnly(role.CatalogOptions{Environ: []string(env)})
	reading := ModelCatalogReading{State: string(live.State), Source: string(live.Source), Detail: live.Message}
	for _, entry := range live.Entries {
		reading.Models = append(reading.Models, entry.ID)
	}
	return reading
}

// RoleModels is the doctor's reading of the models the helper roles route to (CRW-1324, item 9): each model a role (or its
// fallback) names, whether a known model catalogue lists it, and a warning for each one no catalogue lists, naming the catalogue
// that was read. A catalogue is read only when some role names a model, and one that could not be read condemns nothing: its state
// is recorded. Nothing here blocks anything.
func RoleModels(ctx context.Context, o Options) Object {
	lookup := host.LookupEnv(func(key string) (string, bool) {
		value, set := environ(o.Env)(key)
		return value, set
	})
	report := Object{}
	storePath, err := role.StorePath(lookup)
	var store any
	if err == nil {
		store = storePath
	}
	snapshot := role.ReadSettingsSnapshot(lookup)

	type candidate struct{ role, field, model string }
	var candidates []candidate
	unusable := []any{}
	for _, name := range role.Roles() {
		config, err := snapshot.Role(name)
		if err != nil {
			unusable = append(unusable, Object{{Key: "role", Value: string(name)}, {Key: "detail", Value: err.Error()}})
			continue
		}
		if config.Mode == role.ModeModel && config.Model != nil && *config.Model != "" {
			candidates = append(candidates, candidate{string(name), "model", *config.Model})
		}
		if config.Fallback != nil && config.Fallback.Model != "" {
			candidates = append(candidates, candidate{string(name), "fallback.model", config.Fallback.Model})
		}
	}

	catalogue := Object{{Key: "state", Value: "not_read"}, {Key: "source", Value: nil}, {Key: "models", Value: nil}, {Key: "detail", Value: "no helper role names a model, so no catalogue was read"}}
	roles, warnings := []any{}, []any{}
	if len(candidates) > 0 {
		probe := o.ModelCatalog
		if probe == nil {
			probe = readModelCatalog
		}
		reading := probe(ctx, o.Env)
		var count any
		listed := map[string]bool{}
		if reading.readsModels() {
			count = int64(len(reading.Models))
			for _, id := range reading.Models {
				listed[id] = true
			}
		}
		catalogue = Object{{Key: "state", Value: reading.State}, {Key: "source", Value: nullable(reading.Source)}, {Key: "models", Value: count}, {Key: "detail", Value: nullable(reading.Detail)}}
		for _, c := range candidates {
			entry := Object{{Key: "role", Value: c.role}, {Key: "field", Value: c.field}, {Key: "model", Value: c.model}}
			switch {
			case !reading.readsModels():
				entry = append(entry, record.Object{{Key: "listed", Value: nil}}...)
			case listed[c.model]:
				entry = append(entry, record.Object{{Key: "listed", Value: true}}...)
			default:
				entry = append(entry, record.Object{{Key: "listed", Value: false}}...)
				warnings = append(warnings, "helper role "+c.role+" routes to "+c.model+" ("+c.field+"), which the "+reading.State+" model catalogue ("+reading.Source+", "+strconv.Itoa(len(reading.Models))+" models) does not list, so a spawn on it can fail; point the role at a listed model with crw role helper")
			}
			roles = append(roles, entry)
		}
	}
	report = append(report, record.Object{
		{Key: "store", Value: store}, {Key: "catalogue", Value: catalogue}, {Key: "roles", Value: roles},
		{Key: "unusableRoles", Value: unusable}, {Key: "warnings", Value: warnings},
	}...)
	return report
}
