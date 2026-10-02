package dagsched

import (
	"context"
	"database/sql"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/contract"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/dag"
	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// Region is one place in a repository a node expects to edit (contract 7.2). Kind is tree, file or symbol; Change is edit, rename or delete.
// Exclusive regions conflict with every other region of the same repository.
type Region struct {
	Repository, Path, Kind, Key, Change string
	Exclusive                           bool
}

var hotspotNames = map[string]bool{
	"go.mod": true, "go.sum": true, "package-lock.json": true, "yarn.lock": true, "pnpm-lock.yaml": true, "Cargo.lock": true,
	"poetry.lock": true, "Pipfile.lock": true, "uv.lock": true, "Makefile": true, "Dockerfile": true,
}

// Classify says whether a region is exclusive and why: a rename or a delete, or a hotspot (a lockfile, anything under .github/, a Makefile or a
// Dockerfile, a .sql file, a path with a schema or migrations segment), is the kind of change two parallel branches cannot both make.
func Classify(p, change string) (exclusive bool, why string) {
	if change == "rename" || change == "delete" {
		return true, change
	}
	clean := strings.TrimPrefix(path.Clean(p), "/")
	if hotspotNames[path.Base(clean)] {
		return true, "hotspot"
	}
	if clean == ".github" || strings.HasPrefix(clean, ".github/") || strings.HasSuffix(clean, ".sql") {
		return true, "hotspot"
	}
	for _, segment := range strings.Split(clean, "/") {
		if segment == "schema" || segment == "migrations" {
			return true, "hotspot"
		}
	}
	return false, ""
}

func within(p, dir string) bool { return p == dir || strings.HasPrefix(p, dir+"/") }

// Overlaps is whether two regions can be touched by the same change. Different repositories never overlap; an exclusive region overlaps everything in its repository; a tree
// overlaps whatever lies under it; two symbols overlap only when they are the same symbol; any other pair on one path overlaps.
func Overlaps(a, b Region) bool {
	if a.Repository != b.Repository {
		return false
	}
	if a.Exclusive || b.Exclusive {
		return true
	}
	ap, bp := path.Clean(a.Path), path.Clean(b.Path)
	if (a.Kind == "tree" && within(bp, ap)) || (b.Kind == "tree" && within(ap, bp)) {
		return true
	}
	if ap != bp {
		return false
	}
	if a.Kind == "symbol" && b.Kind == "symbol" {
		return a.Key == b.Key
	}
	return true
}

// loadDeclarations is the latest declaration of every node of a plan (the rows with the highest declaration_seq per node). A node absent from the map has none: its
// regions are unknown.
func loadDeclarations(ctx context.Context, q store.Querier, plan string) (map[string][]Region, error) {
	rows, err := q.QueryContext(ctx, "SELECT r.node_id, r.repository, r.path, r.region_kind, r.region_key, r.change, r.exclusive FROM dag_node_regions r"+
		" WHERE r.plan_id = ? AND r.declaration_seq = (SELECT MAX(declaration_seq) FROM dag_node_regions m WHERE m.plan_id = r.plan_id AND m.node_id = r.node_id)"+
		" ORDER BY r.node_id, r.repository, r.path, r.region_kind, r.region_key", plan)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]Region{}
	for rows.Next() {
		var node string
		var r Region
		var exclusive int
		if err := rows.Scan(&node, &r.Repository, &r.Path, &r.Kind, &r.Key, &r.Change, &exclusive); err != nil {
			return nil, err
		}
		r.Exclusive = exclusive == 1
		out[node] = append(out[node], r)
	}
	return out, rows.Err()
}

// A holder is a node whose edit regions are in use or claimed in this pass: its declaration, or none (Unknown).
type holder struct {
	NodeID  string
	Regions []Region
	Unknown bool
}

// overlapsAny is the edit-region rule (contract 7.2) between one node and everything that already holds regions. An undeclared node counts as
// overlapping every other: it conflicts with any holder, and any undeclared holder conflicts with every candidate (the safe side of not knowing).
// Declared against declared uses Overlaps.
func overlapsAny(c holder, holders []holder) bool {
	if len(holders) == 0 {
		return false
	}
	if c.Unknown {
		return true
	}
	for _, h := range holders {
		if h.Unknown {
			return true
		}
		for _, a := range c.Regions {
			for _, b := range h.Regions {
				if Overlaps(a, b) {
					return true
				}
			}
		}
	}
	return false
}

// MaxRegions bounds one declaration.
const MaxRegions = 64

// RegionDeclaration is the answer of DeclareRegions: the sequence number of the declaration now in force and the regions it holds (as stored: sorted,
// with the exclusive flag the classifier and the caller agreed on).
type RegionDeclaration struct {
	PlanID, NodeID string
	Seq            int64
	Replayed       bool
	Regions        []Region
}

// DeclareRegions records the edit regions of an implementation node before it is released (contract 7.2). A declaration replaces the node's earlier one
// as a whole; one identical to the latest is a replay and writes nothing. The classifier marks a rename, a delete and the hotspots exclusive whatever
// the caller said; a caller can only make a region more exclusive.
func (s *Scheduler) DeclareRegions(ctx context.Context, plan, node, actor string, regions []Region) (RegionDeclaration, error) {
	normal, err := normalizeRegions(regions)
	if err != nil {
		return RegionDeclaration{}, err
	}
	out := RegionDeclaration{PlanID: plan, NodeID: node, Regions: normal}
	err = s.Store.Transaction(ctx, func(txCtx context.Context, _ *sql.Conn) error {
		q := s.Store.Q(txCtx)
		if err := s.fence(txCtx, q, plan, actor); err != nil {
			return err
		}
		snap, _, err := dag.SnapshotAt(txCtx, q, plan, 0)
		if err != nil {
			return err
		}
		n, ok := nodeOf(snap, node)
		if !ok {
			return refuse(contract.RefusalUnregisteredScope, "plan %s has no live node %s", plan, node)
		}
		if n.Kind != dag.NodeImplementation {
			return refuse(contract.RefusalDispositionConflict, "node %s is a %s node: it edits no repository, so it has no regions to declare", node, n.Kind)
		}
		current, err := loadDeclarations(txCtx, q, plan)
		if err != nil {
			return err
		}
		latest, declared := current[node]
		replay := declared && slices.Equal(latest, normal)
		if !replay {
			// regions are held until the node lands (contract 7.2): once a release or an execution holds them a different declaration, which could only free
			// them for another node early, is refused. Declare before releasing.
			state, err := s.stateOf(txCtx, q, plan, snap, n)
			if err != nil {
				return err
			}
			if state.Holds {
				return refuse(contract.RefusalDispositionConflict, "node %s is %s and holds its regions until its head lands; a declaration is made before the release", node, state.State)
			}
		}
		if replay {
			var seq sql.NullInt64
			if err := q.QueryRowContext(txCtx, "SELECT MAX(declaration_seq) FROM dag_node_regions WHERE plan_id = ? AND node_id = ?", plan, node).Scan(&seq); err != nil {
				return err
			}
			out.Seq, out.Replayed = seq.Int64, true
			return nil
		}
		var last sql.NullInt64
		if err := q.QueryRowContext(txCtx, "SELECT MAX(declaration_seq) FROM dag_node_regions WHERE plan_id = ? AND node_id = ?", plan, node).Scan(&last); err != nil {
			return err
		}
		out.Seq = last.Int64 + 1
		at := s.now()
		for _, r := range normal {
			exclusive := 0
			if r.Exclusive {
				exclusive = 1
			}
			if _, err := q.ExecContext(txCtx, "INSERT INTO dag_node_regions (plan_id, node_id, declaration_seq, repository, path, region_kind, region_key, change, exclusive, declared_by, declared_at)"+
				" VALUES (?,?,?,?,?,?,?,?,?,?,?)", plan, node, out.Seq, r.Repository, r.Path, r.Kind, r.Key, r.Change, exclusive, actor, at); err != nil {
				return err
			}
		}
		return nil
	})
	return out, err
}

// normalizeRegions validates and canonicalises a declaration: 1 to 64 distinct regions, each with a repository, a relative clean path, a kind, a change
// and, for a symbol, its key. The result is sorted so two spellings of one declaration compare equal.
func normalizeRegions(in []Region) ([]Region, error) {
	if len(in) == 0 || len(in) > MaxRegions {
		return nil, refuse(contract.RefusalMalformedReceipt, "a declaration holds 1 to %d regions, not %d", MaxRegions, len(in))
	}
	type key struct{ repository, path, kind, key string }
	seen := map[key]Region{}
	out := make([]Region, 0, len(in))
	for _, r := range in {
		if r.Change == "" {
			r.Change = "edit"
		}
		repository, err := canonicalRepository(r.Repository)
		if err != nil {
			return nil, err
		}
		r.Repository = repository
		// A path is kept as written, spaces inside a name included: trimming would store another place than the one declared. Whitespace around the whole
		// path is refused rather than guessed at.
		if r.Path != strings.TrimSpace(r.Path) || hasControl(r.Path) {
			return nil, refuse(contract.RefusalMalformedReceipt, "region path %q has whitespace or a control character at its ends or inside it", r.Path)
		}
		clean := path.Clean(r.Path)
		switch {
		case clean == "." || strings.HasPrefix(clean, "/") || clean == ".." || strings.HasPrefix(clean, "../") || strings.ContainsRune(clean, 0):
			return nil, refuse(contract.RefusalMalformedReceipt, "region path %q is not a clean path inside the repository", r.Path)
		case r.Kind != "tree" && r.Kind != "file" && r.Kind != "symbol":
			return nil, refuse(contract.RefusalMalformedReceipt, "region kind %q is tree, file or symbol", r.Kind)
		case r.Change != "edit" && r.Change != "rename" && r.Change != "delete":
			return nil, refuse(contract.RefusalMalformedReceipt, "region change %q is edit, rename or delete", r.Change)
		case r.Kind == "symbol" && r.Key == "":
			return nil, refuse(contract.RefusalMalformedReceipt, "a symbol region names its symbol")
		case r.Kind != "symbol" && r.Key != "":
			return nil, refuse(contract.RefusalMalformedReceipt, "only a symbol region has a key")
		}
		r.Path = clean
		classified, _ := Classify(clean, r.Change)
		r.Exclusive = r.Exclusive || classified
		k := key{r.Repository, r.Path, r.Kind, r.Key}
		if earlier, dup := seen[k]; dup {
			if earlier != r {
				return nil, refuse(contract.RefusalMalformedReceipt, "region %s %s is declared twice with different changes", r.Repository, r.Path)
			}
			continue
		}
		seen[k] = r
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		switch {
		case a.Repository != b.Repository:
			return a.Repository < b.Repository
		case a.Path != b.Path:
			return a.Path < b.Path
		case a.Kind != b.Kind:
			return a.Kind < b.Kind
		}
		return a.Key < b.Key
	})
	return out, nil
}

// slugPattern is a forge repository, owner/name.
var slugPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)

// canonicalRepository is the one spelling of a repository a lock is keyed by: a local checkout is the directory its absolute path reaches, symbolic links resolved
// (so /repo, /repo/, /repo/. and a link to it are one repository; a path that does not resolve to an existing directory is refused), a forge repository is exactly owner/name. Anything else (a relative
// path among it) and any whitespace around or control character inside is refused: two spellings of one repository would be two locks. A forge slug and a
// checkout path of the same repository remain two keys; a plan names one spelling per repository, the target_repository of its edges.
func canonicalRepository(in string) (string, error) {
	switch {
	case in == "" || in != strings.TrimSpace(in) || hasControl(in):
		return "", refuse(contract.RefusalMalformedReceipt, "region repository %q is empty or has whitespace around it or a control character in it", in)
	case strings.HasPrefix(in, "/"):
		// The identity of a checkout is the directory the path reaches: links are resolved with the path as written (a ".." after a link means the link
		// target's parent, which cleaning first would get wrong). A path that cannot be resolved has no identity to lock under and is refused, so the same
		// string can never be stored as two keys because a link appeared between two declarations.
		resolved, err := filepath.EvalSymlinks(in)
		if err != nil {
			return "", refuse(contract.RefusalMalformedReceipt, "region repository %q cannot be resolved to a directory: %v", in, err)
		}
		if info, err := os.Stat(resolved); err != nil || !info.IsDir() {
			return "", refuse(contract.RefusalMalformedReceipt, "region repository %q is not a directory", in)
		}
		return resolved, nil
	}
	owner, name, _ := strings.Cut(in, "/")
	if !slugPattern.MatchString(in) || owner == "." || owner == ".." || name == "." || name == ".." {
		return "", refuse(contract.RefusalMalformedReceipt, "region repository %q is neither an absolute path nor owner/name", in)
	}
	return in, nil
}

func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}
