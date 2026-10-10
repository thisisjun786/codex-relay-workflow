// Injection history storage from CXC v0.2.40 recall/src/index-db.ts:176-205.
package recall

import (
	"math"
	"math/big"
	"strconv"
	"strings"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

func hitCountRef(threadID, file string) string {
	if threadID != "" {
		return "thread:" + threadID
	}
	return "file:" + file
}

func readHitCounts(db *RwDb, refs []string) map[string]float64 {
	counts := map[string]float64{}
	if len(refs) == 0 {
		return counts
	}
	holes := strings.TrimSuffix(strings.Repeat("?,", len(refs)), ",")
	stmt, err := db.Prepare("SELECT ref, hit_count FROM recall_hit_counts WHERE ref IN (" + holes + ")")
	if err != nil {
		return counts
	}
	params := make([]any, len(refs))
	for i, ref := range refs {
		params[i] = ref
	}
	rows, err := stmt.All(params...)
	if err != nil {
		return counts
	}
	for _, row := range rows {
		counts[row["ref"].(string)] = hitCountNumber(row["hit_count"])
	}
	return counts
}

const hitCountUpsert = `INSERT INTO recall_hit_counts (ref, hit_count, last_hit_at) VALUES (?, 1, ?)
     ON CONFLICT(ref) DO UPDATE SET hit_count = hit_count + 1, last_hit_at = excluded.last_hit_at`

// bumpHitCounts counts each ref once per occurrence, all in one transaction: a failure part way leaves
// the history as it was.
func bumpHitCounts(db *RwDb, refs []string, atISO string) error {
	if len(refs) == 0 {
		return nil
	}
	return ingestTransaction(db, func() error { return bumpHitRefs(db, refs, atISO) })
}

func bumpHitRefs(db *RwDb, refs []string, atISO string) error {
	stmt, err := db.Prepare(hitCountUpsert)
	if err != nil {
		return err
	}
	for _, ref := range refs {
		if _, err = stmt.Run(ref, atISO); err != nil {
			return err
		}
	}
	return nil
}

// The events table keeps the identity of recent counting events, so that counting one event twice (a
// retry after an unknown outcome) changes nothing. It is bounded in age and in number.
const (
	hitEventTTL  = 24 * time.Hour
	hitEventKeep = 1000
)

// recordHitEvent counts refs for one event, once: the event is recorded and the counts are raised in
// the same transaction, so either both happen or neither, and a second call with the event does nothing.
func recordHitEvent(db *RwDb, event string, refs []string, atISO string) error {
	if len(refs) == 0 {
		return nil
	}
	return ingestTransaction(db, func() error {
		if err := db.Exec("CREATE TABLE IF NOT EXISTS recall_hit_events (event TEXT PRIMARY KEY, at TEXT NOT NULL)"); err != nil {
			return err
		}
		stmt, err := db.Prepare("INSERT OR IGNORE INTO recall_hit_events (event, at) VALUES (?, ?)")
		if err != nil {
			return err
		}
		recorded, err := stmt.Run(event, atISO)
		if err != nil {
			return err
		}
		if recorded.Changes == 0 {
			return nil
		}
		if err = bumpHitRefs(db, refs, atISO); err != nil {
			return err
		}
		prune, err := db.Prepare("DELETE FROM recall_hit_events WHERE at < ? OR event NOT IN (SELECT event FROM recall_hit_events ORDER BY at DESC, rowid DESC LIMIT ?)")
		if err != nil {
			return err
		}
		_, err = prune.Run(hitEventCutoff(atISO), hitEventKeep)
		return err
	})
}

// forgetHitEvent drops a counted event once nothing can retry it any more; events of an invocation that
// died first are removed by the age and number bounds above.
func forgetHitEvent(db *RwDb, event string) {
	if stmt, err := db.Prepare("DELETE FROM recall_hit_events WHERE event = ?"); err == nil {
		_, _ = stmt.Run(event)
	}
}

func hitEventCutoff(atISO string) string {
	at, err := time.Parse("2006-01-02T15:04:05.000Z", atISO)
	if err != nil {
		return ""
	}
	return at.Add(-hitEventTTL).Format("2006-01-02T15:04:05.000Z")
}

// Number(row.hit_count) over SQLite's result domain. INTEGER affinity permits TEXT/BLOB.
func hitCountNumber(value any) float64 {
	switch v := value.(type) {
	case nil:
		return 0
	case float64:
		return v
	case []byte:
		if len(v) == 0 {
			return 0
		}
		if len(v) == 1 {
			return float64(v[0])
		}
		return math.NaN()
	case string:
		s := text.Trim(v)
		if s == "" {
			return 0
		}
		if s == "Infinity" || s == "+Infinity" {
			return math.Inf(1)
		}
		if s == "-Infinity" {
			return math.Inf(-1)
		}
		if len(s) > 2 && s[0] == '0' {
			base := 0
			switch s[1] {
			case 'x', 'X':
				base = 16
			case 'b', 'B':
				base = 2
			case 'o', 'O':
				base = 8
			}
			if base != 0 {
				digits := s[2:]
				allowed := "0123456789abcdef"[:base]
				if base == 16 {
					digits = strings.ToLower(digits)
				}
				if strings.Trim(digits, allowed) != "" {
					return math.NaN()
				}
				n, ok := new(big.Int).SetString(digits, base)
				if !ok {
					return math.NaN()
				}
				f, _ := n.Float64()
				return f
			}
		}
		// ParseFloat accepts Go-only inf, underscores and hexadecimal exponents.
		if strings.ContainsAny(s, "_xXpP") || strings.Contains(strings.ToLower(s), "inf") {
			return math.NaN()
		}
		n, err := strconv.ParseFloat(s, 64)
		if err != nil && !math.IsInf(n, 0) {
			return math.NaN()
		}
		return n
	}
	return math.NaN()
}
