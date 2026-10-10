// CXC v0.2.40 (3c1459ac) recall/src/chat-search.ts:151-268.
// Dispatch and index lifecycle; scan, ranking, ingest and storage keep their owners.
package recall

import (
	"fmt"
	"math"
	"os"
	"time"
)

// SearchChat chooses the sidecar index unless Scan or an empty match plan selects
// the rollout scan. The optional clock mirrors the scan's deterministic seam;
// NowMs remains a separate override for index relevance scoring only.
func SearchChat(query string, opts ChatSearchOptions, clock ...time.Time) (ChatSearchResult, error) {
	now := time.Now
	if len(clock) != 0 {
		now = func() time.Time { return clock[0] }
	}
	home := ""
	if opts.Home != nil {
		home = *opts.Home
	} else {
		var err error
		home, err = codexHome()
		if err != nil {
			return ChatSearchResult{}, err
		}
	}
	shared := chatScanShared{
		Home: home, Days: chatSearchNumber(opts.Days, DefaultDays),
		Limit:    math.Min(math.Max(chatSearchNumber(opts.Limit, DefaultLimit), 1), MaxLimit),
		ContextN: math.Max(chatSearchNumber(opts.Context, 0), 0),
		Plan:     ChatMatchPlan(query, opts.Any, opts.Synonyms), Source: RolloutMain,
	}
	if opts.Source != nil {
		shared.Source = *opts.Source
	}
	// The entry computes this even for empty plans and forced scans, before git.
	cutoff, err := chatScanCutoff(now(), shared.Days)
	if err != nil {
		return ChatSearchResult{}, err
	}
	if cwd := scanString(opts.Cwd); cwd != "" {
		shared.RepoKey = repoKeyForCwd(cwd, opts.ReadOriginUrl)
	}
	if !opts.Scan && !PlanIsEmpty(shared.Plan) {
		indexed, err := chatSearchViaIndex(opts, shared, cutoff, now)
		if err == nil {
			return indexed, nil
		}
		scan, scanErr := searchViaScan(query, opts, shared, clock...)
		if scanErr != nil {
			return ChatSearchResult{}, scanErr
		}
		scan.Warnings = append([]string{fmt.Sprintf("index unavailable (%s) — served by scan", err)}, scan.Warnings...)
		return scan, nil
	}
	return searchViaScan(query, opts, shared, clock...)
}

func chatSearchNumber(value *float64, fallback float64) float64 {
	if value != nil {
		return *value
	}
	return fallback
}

func chatSearchViaIndex(opts ChatSearchOptions, shared chatScanShared, cutoff string, now func() time.Time) (result ChatSearchResult, err error) {
	started := now().UnixMilli()
	path := ""
	if opts.IndexPath != nil {
		path = *opts.IndexPath
	} else {
		path, err = indexPath()
		if err != nil {
			return ChatSearchResult{}, err
		}
	}
	var db *RwDb
	readOnly, roWarning := false, ""
	if opts.NoRefresh {
		db, err = openIndexReadOnly(path)
		readOnly = true
	} else {
		db, err = openIndex(path)
		if err != nil {
			roWarning = fmt.Sprintf("index opened read-only (%s) — refresh skipped", err)
			db, err = openIndexReadOnly(path)
			readOnly = true
		}
	}
	if err != nil {
		return ChatSearchResult{}, err
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			err = closeErr
		}
	}()
	refreshed := 0
	if !opts.NoRefresh && !readOnly {
		stmt, err := db.Prepare("SELECT COUNT(*) AS n FROM files")
		if err != nil {
			return ChatSearchResult{}, err
		}
		row, err := stmt.Get()
		if err != nil {
			return ChatSearchResult{}, err
		}
		if row["n"] == float64(0) {
			fmt.Fprintln(os.Stderr, "recall: building the sidecar index for the first time — subsequent queries are instant")
		}
		r, err := ingest(shared.Home, db, 0)
		if err != nil {
			return ChatSearchResult{}, err
		}
		refreshed = int(r.Ingested + r.Appended)
	}
	result, err = queryIndex(db, IndexQueryOptions{
		Plan: shared.Plan, Limit: shared.Limit, ContextN: shared.ContextN, CutoffISO: cutoff,
		Role: scanString(opts.Role), Cwd: scanString(opts.Cwd), Source: shared.Source,
		IncludeSynthetic: opts.IncludeSynthetic, IncludeTools: opts.IncludeTools == nil || *opts.IncludeTools,
		Home: shared.Home, Order: opts.Order, RepoKey: shared.RepoKey, NowMs: opts.NowMs,
	})
	if err != nil {
		return ChatSearchResult{}, err
	}
	if roWarning != "" {
		result.Warnings = append(result.Warnings, roWarning)
	}
	status, err := indexStatus(db, path)
	if err != nil {
		return ChatSearchResult{}, err
	}
	budget := BannerFreshnessBudget()
	fresh, err := measureIndexFreshness(shared.Home, db, 0, &budget)
	if err != nil {
		return ChatSearchResult{}, err
	}
	if fresh.UnreadDirs > 0 {
		result.Warnings = append(result.Warnings, fmt.Sprintf("%d rollout directories could not be listed — their rollouts are not covered, so the result and the stale count may be incomplete", int(fresh.UnreadDirs)))
	}
	result.Index = &ChatIndexInfo{LastIngestAt: status.LastIngestAt, Files: int(status.Files), SourceFiles: int(fresh.SourceFiles), StaleFiles: int(fresh.StaleFiles), ReadOnly: readOnly}
	result.ScannedFiles = refreshed
	result.ElapsedMs = now().UnixMilli() - started
	return result, nil
}
