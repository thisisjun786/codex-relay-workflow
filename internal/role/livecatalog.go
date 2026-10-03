package role

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

const CatalogTTLMS = 30_000
const ocxBufferLimit = 4 * 1024 * 1024

// LiveCatalog is live-catalog.ts:12-17. A cached object's unvalidated/unknown
// members survive the oracle's fresh return and stale spread.
type LiveCatalog struct {
	Catalog
	Status    string      `json:"status"`
	Source    ModelSource `json:"source"`
	FetchedAt *string     `json:"fetchedAt"`
	Message   string      `json:"message,omitempty"`
	raw       object
	fetchedMS int64
}

func (c LiveCatalog) MarshalJSON() ([]byte, error) {
	if c.raw == nil {
		type plain LiveCatalog
		return Stringify(plain(c), "")
	}
	o := append(object{}, c.raw...)
	o.set("status", c.Status)
	if c.Message != "" {
		o.set("message", c.Message)
	}
	return Stringify(o, "")
}

type CatalogOptions struct {
	ForceRefresh bool
	Environ      []string // nil inherits os.Environ; an empty slice inherits nothing.
	Now          func() time.Time
	RunOcx       func([]string) (string, error)
	ReadNative   func(host.LookupEnv) []CatalogEntry
}

type catalogRequest struct {
	done    chan struct{}
	catalog LiveCatalog
}

// CatalogReader owns the oracle's process-local pending map (ts:25). Its zero
// value is ready; a server shares one reader across its requests. Homes remain
// isolated by path and discovery environment; project cwd is not a cache key.
type CatalogReader struct {
	mu      sync.Mutex
	pending map[string]*catalogRequest
}

func catalogEnv(environ []string) host.LookupEnv {
	m := map[string]string{}
	for _, e := range environ {
		if k, v, ok := strings.Cut(e, "="); ok {
			m[k] = v
		}
	}
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func sourceKey(env host.LookupEnv) string {
	p, ok := env("PATH")
	if !ok {
		p, _ = env("Path")
	}
	h, _ := env("CODEX_HOME")
	cache, _ := env("CODEX_MODELS_CACHE_PATH")
	ocx, _ := env("OPENCODEX_HOME")
	b, _ := Stringify([]string{h, cache, p, ocx}, "")
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// ReadCatalog ports ts:83-119. Only failure to resolve the user's home escapes
// as an error; discovery/cache failures return the oracle's labelled result.
func (r *CatalogReader) ReadCatalog(o CatalogOptions) (LiveCatalog, error) {
	environ := o.Environ
	if environ == nil {
		environ = os.Environ()
	}
	env := catalogEnv(environ)
	store, err := StorePath(env)
	if err != nil {
		return LiveCatalog{}, err
	}
	path, key := filepath.Join(filepath.Dir(store), "model-catalog.json"), sourceKey(env)
	now := o.Now
	if now == nil {
		now = time.Now
	}
	pendingKey := path + ":" + key
	r.mu.Lock()
	if q := r.pending[pendingKey]; q != nil {
		r.mu.Unlock()
		<-q.done
		return q.catalog, nil
	}
	cached := cachedCatalog(path, key, now().UnixMilli())
	if !o.ForceRefresh && cached != nil && now().UnixMilli()-cached.fetchedMS < CatalogTTLMS {
		r.mu.Unlock()
		return *cached, nil
	}
	q := &catalogRequest{done: make(chan struct{})}
	if r.pending == nil {
		r.pending = map[string]*catalogRequest{}
	}
	r.pending[pendingKey] = q
	r.mu.Unlock()
	c := queryCatalog(path, key, environ, env, now, o, cached)
	r.mu.Lock()
	q.catalog = c
	delete(r.pending, pendingKey)
	close(q.done)
	r.mu.Unlock()
	return c, nil
}

func queryCatalog(path, key string, environ []string, env host.LookupEnv, now func() time.Time, o CatalogOptions, cached *LiveCatalog) LiveCatalog {
	run := o.RunOcx
	if run == nil {
		run = RunOcxModels
	}
	stdout, err := run(environ)
	source := ModelOcx
	var entries []CatalogEntry
	if err == nil {
		entries, err = ParseOcxModels(stdout)
	}
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, exec.ErrNotFound) {
		source = ModelNative
		read := o.ReadNative
		if read == nil {
			read = ReadNativeCatalog
		}
		entries = read(env)
		err = nil
		if entries == nil {
			err = sentinel("native catalog unavailable")
		}
	}
	if err == nil {
		state := CatalogOcx
		if source == ModelNative {
			state = CatalogNative
		}
		at := now().UTC().Format("2006-01-02T15:04:05.000Z")
		c := LiveCatalog{Catalog: Catalog{State: state, Entries: entries}, Status: "fresh", Source: source, FetchedAt: &at}
		if !persistCatalog(path, key, c, crwdir.Rename) {
			c.Message = "Model list loaded; its cache could not be saved."
		}
		return c
	}
	message := "OCX model discovery failed. Check OCX and refresh."
	if source == ModelNative {
		message = "Codex model catalog is unavailable. Check its configured path and refresh."
	}
	if cached != nil {
		c := *cached
		c.Status, c.Message = "stale", message+" Showing the last successful list."
		return c
	}
	return LiveCatalog{Catalog: Catalog{State: CatalogUnavailable, Entries: []CatalogEntry{}}, Status: "unavailable", Source: source, Message: message}
}

// ParseOcxModels is ts:37-53: disabled/pending filtering precedes ID validation;
// IDs are tested with trim but stored unchanged, and duplicate IDs keep the first row.
func ParseOcxModels(stdout string) ([]CatalogEntry, error) {
	var parsed json.RawMessage
	if err := json.Unmarshal([]byte(stdout), &parsed); err != nil {
		return nil, err
	}
	if len(parsed) == 0 || parsed[0] != '[' {
		return nil, sentinel("invalid OCX catalog")
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(parsed, &rows); err != nil {
		return nil, err
	}
	entries := make([]CatalogEntry, 0)
	seen := map[string]bool{}
	for _, row := range rows {
		m, ok := members(row)
		if !ok || m == nil {
			return nil, sentinel("invalid OCX model row")
		}
		if string(m["disabled"]) == "true" || string(m["initialSelectionPending"]) == "true" {
			continue
		}
		id, valid := stringOf(m["namespaced"])
		native := string(m["native"]) == "true"
		if !valid && native {
			id, valid = stringOf(m["id"])
		}
		if !valid || text.Trim(id) == "" {
			return nil, sentinel("invalid OCX model id")
		}
		if seen[id] {
			continue
		}
		seen[id] = true
		source, label := ModelOcx, id
		if native {
			source = ModelNative
		}
		if display, ok := stringOf(m["displayName"]); ok && text.Trim(display) != "" {
			label = display + " (" + id + ")"
		}
		entries = append(entries, CatalogEntry{id, source, label, reasoningEfforts(m["reasoningEfforts"])})
	}
	return entries, nil
}

func cachedCatalog(path, key string, now int64) *LiveCatalog {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	doc, err := parseObject(body)
	if err != nil {
		return nil
	}
	k, _ := doc.raw("key")
	if s, ok := stringOf(k); !ok || s != key {
		return nil
	}
	raw, _ := doc.raw("catalog")
	o, err := parseObject(raw)
	if err != nil {
		return nil
	}
	m, _ := members(raw)
	status, _ := stringOf(m["status"])
	source, _ := stringOf(m["source"])
	if status != "fresh" || (source != "ocx" && source != "native") {
		return nil
	}
	var rows []json.RawMessage
	if len(m["entries"]) == 0 || m["entries"][0] != '[' || json.Unmarshal(m["entries"], &rows) != nil {
		return nil
	}
	at, err := jsString(m["fetchedAt"])
	ms, valid := catalogDate(at)
	var numeric float64
	zero := len(m["fetchedAt"]) > 0 && m["fetchedAt"][0] != '"' && json.Unmarshal(m["fetchedAt"], &numeric) == nil && numeric == 0
	if err != nil || zero || !valid || ms > now+1000 {
		return nil
	}
	entries := make([]CatalogEntry, 0, len(rows))
	for _, row := range rows {
		m, ok := members(row)
		if !ok || m == nil {
			return nil
		}
		id, idOK := stringOf(m["id"])
		label, labelOK := stringOf(m["label"])
		src, _ := stringOf(m["source"])
		if !idOK || id == "" || !labelOK || (src != "ocx" && src != "native") {
			return nil
		}
		efforts := m["reasoningEfforts"]
		if string(efforts) != "null" {
			var ladder []json.RawMessage
			if len(efforts) == 0 || efforts[0] != '[' || json.Unmarshal(efforts, &ladder) != nil {
				return nil
			}
			for _, e := range ladder {
				if _, ok := stringOf(e); !ok {
					return nil
				}
			}
		}
		entries = append(entries, CatalogEntry{id, ModelSource(src), label, reasoningEfforts(efforts)})
	}
	state, _ := stringOf(m["state"])
	return &LiveCatalog{Catalog: Catalog{State: CatalogState(state), Entries: entries}, Status: status, Source: ModelSource(source), FetchedAt: &at, raw: o, fetchedMS: ms}
}

// catalogDate covers ISO timestamps/date forms, RFC dates and the legacy numeric
// forms accepted by Date.parse in the oracle's cache. Cache reads keep the original spelling.
func catalogDate(s string) (int64, bool) {
	s = text.Trim(s)
	if i := strings.Index(s, " ("); i >= 0 && strings.HasSuffix(s, ")") {
		s = text.Trim(s[:i])
	}
	// V8 normalizes days 29..31 and midnight written as 24:00. It still
	// rejects month 0/13 and day 0/32; retain those validation boundaries.
	extraDay := int64(0)
	if len(s) >= 10 && s[4] == '-' && s[7] == '-' {
		year, e1 := strconv.Atoi(s[:4])
		month, e2 := strconv.Atoi(s[5:7])
		day, e3 := strconv.Atoi(s[8:10])
		if e1 == nil && e2 == nil && e3 == nil {
			if month < 1 || month > 12 || day < 1 || day > 31 {
				return 0, false
			}
			date := time.Date(year, time.Month(month), day, 0, 0, 0, 0, time.UTC)
			s = date.Format("2006-01-02") + s[10:]
			if len(s) >= 16 && s[10] == 'T' && s[11:16] == "24:00" {
				tail := s[16:]
				tail = strings.TrimPrefix(tail, ":00")
				if strings.HasPrefix(tail, ".") {
					tail = tail[1:]
					for len(tail) > 0 && tail[0] == '0' {
						tail = tail[1:]
					}
				}
				if tail != "" && tail[0] != 'Z' && tail[0] != '+' && tail[0] != '-' {
					return 0, false
				}
				s = s[:11] + "00:00" + s[16:]
				extraDay = 24 * 60 * 60 * 1000
			}
		}
	}
	for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04Z07:00", "2006-01-02T15:04:05Z0700", "2006-01-02", "2006-01", "2006", time.RFC1123, time.RFC1123Z, time.RFC822, time.RFC822Z, time.ANSIC, time.UnixDate, time.RFC850, "Mon Jan 02 2006 15:04:05 GMT-0700", "Jan 2 2006", "January 2, 2006", "2006/1/2", "2006,1,2", "1/2/2006", "1-2-2006", "1.2.2006"} {
		if at, err := time.Parse(layout, s); err == nil {
			return at.UnixMilli() + extraDay, true
		}
	}
	for _, layout := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04:05"} {
		if at, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return at.UnixMilli() + extraDay, true
		}
	}
	if len(s) <= 2 {
		if n, err := strconv.Atoi(s); err == nil && n >= 0 {
			year, month := n, time.January
			switch {
			case n == 0:
				year = 2000
			case n <= 12:
				year, month = 2001, time.Month(n)
			case n < 32:
				return 0, false
			case n < 50:
				year += 2000
			default:
				year += 1900
			}
			return time.Date(year, month, 1, 0, 0, 0, 0, time.Local).UnixMilli(), true
		}
	}
	if parts := strings.Split(s, "."); len(parts) == 2 {
		month, e1 := strconv.Atoi(parts[0])
		day, e2 := strconv.Atoi(parts[1])
		if e1 == nil && e2 == nil && month >= 1 && month <= 12 && day >= 1 && day <= 31 {
			return time.Date(2001, time.Month(month), day, 0, 0, 0, 0, time.Local).UnixMilli(), true
		}
	}
	return 0, false
}

// persistCatalog retains ts:71-80's exclusive 0600 temp and rename. This oracle
// already avoids in-place truncation; failed publication preserves the prior file.
func persistCatalog(path, key string, c LiveCatalog, rename func(string, string) error) bool {
	body, err := Stringify(struct {
		Key     string      `json:"key"`
		Catalog LiveCatalog `json:"catalog"`
	}{key, c}, "")
	if err != nil || os.MkdirAll(filepath.Dir(path), 0700) != nil {
		return false
	}
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return false
	}
	defer os.Remove(f.Name()) // Only this invocation's exclusive temporary file.
	_, writeErr := f.Write(append(body, '\n'))
	closeErr := f.Close()
	return writeErr == nil && closeErr == nil && rename(f.Name(), path) == nil
}

type catalogOutput struct {
	buffer   bytes.Buffer
	cancel   context.CancelFunc
	overflow bool
}

func (b *catalogOutput) Write(p []byte) (int, error) {
	if len(p) > ocxBufferLimit-b.buffer.Len() {
		b.overflow = true
		b.cancel()
		return 0, sentinel("OCX output exceeded 4 MiB")
	}
	return b.buffer.Write(p)
}

// ocxExecutable searches the supplied POSIX PATH (including relative/empty
// elements); denied candidates are skipped, with EACCES retained at exhaustion.
func ocxExecutable(env host.LookupEnv) (string, error) {
	path, set := env("PATH")
	if !set {
		path = "/bin:/usr/bin"
	}
	var denied error
	for _, dir := range strings.Split(path, string(os.PathListSeparator)) {
		candidate := filepath.Join(dir, "ocx")
		info, err := os.Stat(candidate)
		if errors.Is(err, os.ErrPermission) {
			denied = err
			continue
		}
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return "", err
		}
		if info.IsDir() || info.Mode().Perm()&0111 == 0 {
			denied = &os.PathError{Op: "exec", Path: candidate, Err: os.ErrPermission}
			continue
		}
		return filepath.Abs(candidate)
	}
	if denied != nil {
		return "", denied
	}
	return "", &os.PathError{Op: "exec", Path: "ocx", Err: os.ErrNotExist}
}

// RunOcxModels ports ts:27-35 without a runtime Node dependency. It kills and
// reaps only its direct child, as execFile does; each stream has its own 4 MiB bound.
func RunOcxModels(environ []string) (string, error) {
	if environ == nil {
		environ = os.Environ()
	}
	file, err := ocxExecutable(catalogEnv(environ))
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	out, stderr := &catalogOutput{cancel: cancel}, &catalogOutput{cancel: cancel}
	cmd := exec.CommandContext(ctx, file, "models", "live", "--json")
	cmd.Env, cmd.Stdout, cmd.Stderr = environ, out, stderr
	cmd.WaitDelay = 2 * time.Second
	err = cmd.Run()
	if out.overflow || stderr.overflow {
		return "", sentinel("OCX output exceeded 4 MiB")
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		return "", err
	}
	return out.buffer.String(), nil
}
