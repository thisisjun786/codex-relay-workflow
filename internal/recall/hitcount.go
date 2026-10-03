// Injection history storage from CXC v0.2.40 recall/src/index-db.ts:176-205.
package recall

import (
	"math"
	"math/big"
	"strconv"
	"strings"

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

func bumpHitCounts(db *RwDb, refs []string, atISO string) error {
	if len(refs) == 0 {
		return nil
	}
	stmt, err := db.Prepare(`INSERT INTO recall_hit_counts (ref, hit_count, last_hit_at) VALUES (?, 1, ?)
     ON CONFLICT(ref) DO UPDATE SET hit_count = hit_count + 1, last_hit_at = excluded.last_hit_at`)
	if err != nil {
		return err
	}
	// Each ref commits separately, including duplicates; errors stop the loop.
	for _, ref := range refs {
		if _, err = stmt.Run(ref, atISO); err != nil {
			return err
		}
	}
	return nil
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
