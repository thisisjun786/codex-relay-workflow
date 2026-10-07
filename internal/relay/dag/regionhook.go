package dag

import (
	"context"

	"github.com/thisisjun786/codex-relay-workflow/internal/relay/store"
)

// RegionOwnerCheck is the packet region-owner rule, asked by the writer on the plan a revision would
// produce (CRW-839 d7, pre-merge d3). The edit-region vocabulary (what a place is, how a grade folds, what
// overlaps what) lives in internal/relay/dagsched, which imports this package; the rule therefore travels
// as a hook the scheduler installs at start, and a build without it writes no plan differently.
//
// It returns the violations of a plan whose packets of one feature issue take a shared edit region without
// exactly one declared owner. It reads dag_node_regions and dag_node_region_grades through the caller's
// connection, so it sees the declarations the revision does not change and the ones it does.
var RegionOwnerCheck func(ctx context.Context, q store.Querier, plan string, snap Snapshot) []Violation
