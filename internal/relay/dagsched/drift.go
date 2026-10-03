package dagsched

import (
	"path"
	"sort"
)

// DriftMark is a node that conflicted on a path none of its declared regions covers (CRW-410): the declaration did not describe the work.
type DriftMark struct{ Node, Path string }

// covers is whether a declaration has a region over a path of the observed checkout: a tree region covers what lies under it, a file or symbol region its file. A region is matched under the names the
// checkout is known by (repositoryNames), because a region is declared with one of them. A symbol region covers its file: a conflict is a file, and which clause of it is not known.
func covers(regions []Region, names []string, p string) bool {
	p = path.Clean(p)
	for _, r := range regions {
		if !hasName(names, r.Repository) {
			continue
		}
		rp := path.Clean(r.Path)
		if r.Kind == "tree" {
			if rp == "." || within(p, rp) {
				return true
			}
		} else if p == rp {
			return true
		}
	}
	return false
}

func hasName(names []string, name string) bool {
	for _, n := range names {
		if n == name {
			return true
		}
	}
	return false
}

// driftOf is, for every conflicting file, the nodes among those that conflicted whose latest declaration has no region over it, sorted by path and node. A node that declared nothing is
// undeclared everywhere (it drifted on every file it conflicted on); a path no node declared is drift for every node that edited it.
func driftOf(files, nodes []string, declarations map[string][]Region, names []string) []DriftMark {
	var out []DriftMark
	for _, file := range files {
		for _, node := range nodes {
			if !covers(declarations[node], names, file) {
				out = append(out, DriftMark{Node: node, Path: file})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return out[i].Node < out[j].Node
	})
	return out
}
