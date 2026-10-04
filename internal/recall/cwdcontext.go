// CXC v0.2.40 (3c1459ac) recall/src/cwd-context.ts: read-only session context.
package recall

import (
	"encoding/json"
	"fmt"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/source"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf16"
)

// CwdSession carries an opening user excerpt and the indexed rollout identity.
type CwdSession struct {
	Path        string  `json:"path"`
	ThreadID    *string `json:"threadId"`
	Date        string  `json:"date"`
	Excerpt     string  `json:"excerpt"`
	excerptClip *cwdExcerptClip
}

// Go callers see valid UTF-8; JSON retains the oracle's split UTF-16 code unit.
type cwdExcerptClip struct {
	original, prefix string
	lone             uint16
}

func (s CwdSession) MarshalJSON() ([]byte, error) {
	type plain CwdSession
	clip := s.excerptClip
	if clip == nil || s.Excerpt != clip.original {
		return json.Marshal(plain(s))
	}
	quoted, err := json.Marshal(clip.prefix)
	if err != nil {
		return nil, err
	}
	raw := string(quoted[:len(quoted)-1]) + fmt.Sprintf(`\u%04x..."`, clip.lone)
	return json.Marshal(struct {
		plain
		Excerpt json.RawMessage `json:"excerpt"`
	}{plain(s), json.RawMessage(raw)})
}

// ExcerptChars is nullable so an explicit zero keeps JavaScript's slice behavior.
type CwdSessionOptions struct {
	IndexPath     string
	Home          string
	ExcerptChars  *int
	ReadOriginUrl ReadOriginUrl
}

type SummaryEntry struct {
	Relpath string `json:"relpath"`
	Title   string `json:"title"`
}

const maxRepoThreadIDs = 5000
const summaryHeadBytes = 1200

func extraHarnessPrefixes() []string {
	return []string{"<recommended_plugins>", "<hook_prompt", "<realtime_delegation>",
		"<codex_internal_context", "<in-app-browser-context", "<send_user_message_question_reply>",
		"# Files mentioned by the user:", "# Files pasted by the user:", "# Browser comments:", "## Referenced chats with Codex:"}
}

func isHarnessText(s string) bool {
	head := strings.TrimLeftFunc(s, isJSSpace)
	if head == "" || IsSyntheticUserText(head) {
		return true
	}
	for _, prefix := range extraHarnessPrefixes() {
		if strings.HasPrefix(head, prefix) {
			return true
		}
	}
	return false
}

func flatten(s string) string { return strings.Join(strings.FieldsFunc(s, isJSSpace), " ") }

func cwdExcerpt(s string, limit int) (string, *cwdExcerptClip) {
	u := utf16.Encode([]rune(s))
	if len(u) <= limit {
		return s, nil
	}
	// String.slice's negative end counts from the end, even for budgets below 3.
	end := 0
	if limit >= 3 {
		end = limit - 3
	} else if limit > 3-len(u) {
		end = len(u) + limit - 3
	}
	u = u[:end]
	rendered := string(utf16.Decode(u)) + "..."
	if end > 0 && u[end-1] >= 0xD800 && u[end-1] <= 0xDBFF {
		return rendered, &cwdExcerptClip{rendered, string(utf16.Decode(u[:end-1])), u[end-1]}
	}
	return rendered, nil
}

// ListCwdSessions returns nil for fallback, and a nonnil [] for a usable empty index.
// The query is exact cwd OR same repository; child directories are not prefix matches.
func ListCwdSessions(cwd string, topN int, options ...CwdSessionOptions) []CwdSession {
	if cwd == "" || topN <= 0 {
		return nil
	}
	opts := CwdSessionOptions{}
	if len(options) != 0 {
		opts = options[0]
	}
	limit := 100
	if opts.ExcerptChars != nil {
		limit = *opts.ExcerptChars
	}
	path := opts.IndexPath
	if path == "" {
		var err error
		path, err = indexPath()
		if err != nil {
			return nil
		}
	}
	db, err := openIndexReadOnly(path)
	if err != nil {
		return nil
	}
	defer db.Close()
	col := CanonicalCwdSQL("cwd")
	condition := col + " = ?"
	if FoldCwdCase() {
		condition = "lower(" + col + ") = lower(?)"
	}
	conditions := []string{condition}
	params := []any{NormalizeCwd(cwd)}
	if key := repoKeyForCwd(cwd, opts.ReadOriginUrl); key != "" {
		if filesHasColumn(db, "repo_key") {
			conditions = append(conditions, "(repo_key IS NOT NULL AND repo_key = ?)")
			params = append(params, key)
		}
		home := opts.Home
		if home == "" {
			home, err = codexHome()
			if err != nil {
				return nil
			}
		}
		if ids := sameOriginThreadIDs(home, key); len(ids) > 0 {
			conditions = append(conditions, "thread_id IN ("+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+")")
			for _, id := range ids {
				params = append(params, id)
			}
		}
	}
	stmt, err := db.Prepare("SELECT path, thread_id, date FROM files WHERE (" + strings.Join(conditions, " OR ") + ") AND source = 'main' ORDER BY date DESC, path DESC LIMIT ?")
	if err != nil {
		return nil
	}
	params = append(params, float64(topN)*2)
	rows, err := stmt.All(params...)
	if err != nil {
		return nil
	}
	sessions := []CwdSession{}
	for _, row := range rows {
		if len(sessions) >= topN {
			break
		}
		path, _ := row["path"].(string) // files.path is TEXT PRIMARY KEY.
		stmt, err := db.Prepare("SELECT text FROM msgs WHERE path = ? AND role = 'user' AND synthetic = 0 ORDER BY ord LIMIT 4")
		if err != nil {
			return nil
		}
		messages, err := stmt.All(path)
		if err != nil {
			return nil
		} // The oracle discards the whole list on a row failure.
		session := CwdSession{Path: path, ThreadID: rolloutStringPointer(row["thread_id"])}
		session.Date, _ = row["date"].(string)
		for _, message := range messages {
			s, _ := message["text"].(string)
			if isHarnessText(s) {
				continue
			}
			flat := flatten(s)
			if flat == "" {
				continue
			}
			session.Excerpt, session.excerptClip = cwdExcerpt(flat, limit)
			break
		}
		sessions = append(sessions, session)
	}
	return sessions
}

func sameOriginThreadIDs(home, repoKey string) []string {
	ids := []string{}
	path, err := stateDbPath(home)
	if err != nil {
		return ids
	}
	meta := loadThreadMeta(path)
	for _, id := range meta.IDs {
		thread := meta.ByID[id]
		origin := ""
		if thread.GitOriginURL != nil {
			origin = *thread.GitOriginURL
		}
		if repoKeysEqual(repoKey, normalizeRepoKey(origin)) {
			ids = append(ids, id)
			if len(ids) >= maxRepoThreadIDs {
				break
			}
		}
	}
	return ids
}

// JS regexp whitespace, including BOM and excluding U+0085. Regexps are built on use.
const cwdSummarySpace = `\t\n\x0B\f\r \x{00A0}\x{1680}\x{2000}-\x{200A}\x{2028}\x{2029}\x{202F}\x{205F}\x{3000}\x{FEFF}`

// LoadSummaryIndex reads the first heading in each markdown head, joined by thread id.
func LoadSummaryIndex(homes ...string) map[string]SummaryEntry {
	out := map[string]SummaryEntry{}
	home := ""
	if len(homes) != 0 {
		home = homes[0]
	} else {
		var err error
		home, err = codexHome()
		if err != nil {
			return out
		}
	}
	dir := filepath.Join(memoriesDir(home), "rollout_summaries")
	names, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	threadID := regexp.MustCompile(`(?m)^thread_id:[` + cwdSummarySpace + `]*([^` + cwdSummarySpace + `]+)[` + cwdSummarySpace + `]*$`)
	title := regexp.MustCompile(`(?m)^#[` + cwdSummarySpace + `]+(.+)$`)
	// Mapping each terminator individually preserves JS ^/$/dot positions, even CRLF.
	lines := strings.NewReplacer("\r", "\n", "\u2028", "\n", "\u2029", "\n")
	for _, entry := range names {
		if !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		f, err := os.Open(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		buf := make([]byte, summaryHeadBytes)
		n, err := f.ReadAt(buf, 0)
		_ = f.Close()
		if err != nil && err != io.EOF {
			continue
		}
		head := lines.Replace(source.DecodeUTF8(buf[:n]))
		id, heading := threadID.FindStringSubmatch(head), title.FindStringSubmatch(head)
		if id == nil || heading == nil {
			continue
		}
		out[id[1]] = SummaryEntry{Relpath: entry.Name(), Title: text.Trim(heading[1])}
	}
	return out
}
