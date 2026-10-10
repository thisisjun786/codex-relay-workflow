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
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/crwdir"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/host"
	"github.com/thisisjun786/codex-relay-workflow/internal/pabcd/text"
)

const CatalogTTLMS = 30_000
const ocxBufferLimit = 4 * 1024 * 1024

// ErrCatalogUnsupported is the OCX refusing the live-catalog command line itself: this host's OCX
// does not read the running proxy's model catalog at all. That is a different state from a catalog
// that could not be read, and RunOcxModels returns it so a caller can tell the two apart without
// re-deciding the judgement.
var ErrCatalogUnsupported = sentinel("this OCX does not support reading the live model catalog")

// unsupportedMessage is what a caller shows for that state.
const unsupportedMessage = "This OCX does not support reading the live model catalog (ocx models live --json)."

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

// catalogSource is the source one request discovers from, resolved once when the request starts: the
// process directory relative paths are read from, the native home and the native catalog file chosen
// from it, the OCX executable PATH finds (or why none is found) and the OPENCODEX_HOME it runs with, or,
// while that is blank, the HOME it falls back to. The key names this snapshot, and the discovery reads
// that native file and runs that executable, so a configuration or PATH that changes while the request
// discovers cannot put another source's list under this key (CRW-1132).
type catalogSource struct {
	dir        string
	nativeHome string
	nativePath string
	ocxExe     string
	ocxErr     error
	ocxHome    string
}

func resolveCatalogSource(env host.LookupEnv) catalogSource {
	dir, err := os.Getwd()
	if err != nil {
		dir = ""
	}
	h, _ := env("CODEX_HOME")
	ocx, _ := env("OPENCODEX_HOME")
	userHome, _ := host.Home(env)
	nativeHome := ""
	if text.Trim(h) != "" {
		nativeHome = text.Trim(h)
	} else if userHome != "" {
		nativeHome = filepath.Join(userHome, ".codex")
	}
	ocxHome := ocx
	if text.Trim(ocxHome) == "" {
		ocxHome = userHome
	}
	s := catalogSource{dir: dir, nativeHome: inDir(dir, nativeHome), nativePath: inDir(dir, NativeCatalogPath(env)), ocxHome: inDir(dir, ocxHome)}
	s.ocxExe, s.ocxErr = ocxExecutable(env, dir)
	return s
}

// sourceKey names the source a cached or pending catalog belongs to: what the reader resolves from the
// environment, not the raw variables alone (CRW-1132; the oracle hashed CODEX_HOME, the catalog path,
// PATH and OPENCODEX_HOME as written, so a shared CRW_HOME with another HOME merged two native homes).
// The raw variables stay in the key, so anything that changed before still changes it. The project
// directory is not part of the key. A path is keyed as the file the reader opens: a relative one is read
// from the process directory, so the same relative text in another directory is another source.
func sourceKey(env host.LookupEnv) string {
	return resolveCatalogSource(env).key(env)
}

func (s catalogSource) key(env host.LookupEnv) string {
	p, ok := env("PATH")
	if !ok {
		p, _ = env("Path")
	}
	h, _ := env("CODEX_HOME")
	cache, _ := env("CODEX_MODELS_CACHE_PATH")
	ocx, _ := env("OPENCODEX_HOME")
	b, _ := Stringify([]string{h, cache, p, ocx, s.nativeHome, s.nativePath, s.ocxExe, s.ocxHome}, "")
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// inDir is a relative path prefixed with the process directory as written, without lexical cleaning, so
// it names the file the kernel opens for the relative path: a symbolic link followed by ".." resolves
// to the link target's parent, which a lexical cleaning would replace with the link's own directory. An
// absolute or empty path, or one with no known directory, stays as written.
func inDir(dir, p string) string {
	if p == "" || dir == "" || filepath.IsAbs(p) {
		return p
	}
	return strings.TrimSuffix(dir, string(os.PathSeparator)) + string(os.PathSeparator) + p
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
	src := resolveCatalogSource(env)
	path, key := filepath.Join(filepath.Dir(store), "model-catalog.json"), src.key(env)
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
	c := queryCatalog(path, key, src, environ, env, now, o, cached)
	r.mu.Lock()
	q.catalog = c
	delete(r.pending, pendingKey)
	close(q.done)
	r.mu.Unlock()
	return c, nil
}

func queryCatalog(path, key string, src catalogSource, environ []string, env host.LookupEnv, now func() time.Time, o CatalogOptions, cached *LiveCatalog) LiveCatalog {
	run := o.RunOcx
	if run == nil {
		run = func(environ []string) (string, error) { return runOcx(src, environ) }
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
			read = func(host.LookupEnv) []CatalogEntry { return readNativeCatalogAt(src.nativePath) }
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
	// An OCX that refuses the command line is not a catalog that could not be read: it is a host
	// whose OCX does not answer the live-catalog command at all, and the answer must say so.
	unsupported := errors.Is(err, ErrCatalogUnsupported)
	if unsupported {
		message = unsupportedMessage
	}
	if cached != nil {
		c := *cached
		c.Status, c.Message = "stale", message+" Showing the last successful list."
		if unsupported {
			// The cached object is merged into the marshalled answer, so the state has to be
			// replaced there as well: setting the struct field alone would be overwritten.
			c.Catalog.State = CatalogUnsupported
			c.raw = withState(c.raw, CatalogUnsupported)
		}
		return c
	}
	if unsupported {
		return LiveCatalog{Catalog: Catalog{State: CatalogUnsupported, Entries: []CatalogEntry{}}, Status: "unavailable", Source: source, Message: message}
	}
	return LiveCatalog{Catalog: Catalog{State: CatalogUnavailable, Entries: []CatalogEntry{}}, Status: "unavailable", Source: source, Message: message}
}

// withState is the raw object with its state member replaced, for a cached catalog whose state the
// caller is answering differently. A nil raw object is returned unchanged: nothing is merged into
// the answer then, so the struct's own state is what is marshalled.
func withState(raw object, state CatalogState) object {
	if raw == nil {
		return nil
	}
	out := append(object{}, raw...)
	out.set("state", string(state))
	return out
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
		entries = append(entries, newCatalogEntry(id, source, label, m["reasoningEfforts"]))
	}
	return entries, nil
}

// catalogTimeLayout is how the writer spells fetchedAt (queryCatalog).
const catalogTimeLayout = "2006-01-02T15:04:05.000Z"

// cachedCatalog is the cache file's catalog when every member that claims freshness is one this writer
// produces for the key's source: the status "fresh", a source of ocx or native, the state that source
// writes, a fetchedAt in the writer's spelling that is not in the future, and entries with a non-blank
// ID and label, a known source and an effort ladder that is null or a list of strings (CRW-1132; the
// oracle coerced a fetchedAt of any type and accepted any state and blank IDs and labels). Anything
// else is no cache: the caller discovers again, and a successful read replaces the file. Members this
// validation does not name stay in the raw object and are returned as they were read.
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
	state, stateOK := stringOf(m["state"])
	if status != "fresh" || (source != "ocx" && source != "native") {
		return nil
	}
	wantState := CatalogOcx
	if source == "native" {
		wantState = CatalogNative
	}
	if !stateOK || CatalogState(state) != wantState {
		return nil
	}
	var rows []json.RawMessage
	if len(m["entries"]) == 0 || m["entries"][0] != '[' || json.Unmarshal(m["entries"], &rows) != nil {
		return nil
	}
	at, atOK := stringOf(m["fetchedAt"])
	written, err := time.Parse(catalogTimeLayout, at)
	if !atOK || err != nil || written.Format(catalogTimeLayout) != at || written.UnixMilli() > now+1000 {
		return nil
	}
	entries := make([]CatalogEntry, 0, len(rows))
	seen := map[string]bool{}
	for _, row := range rows {
		m, ok := members(row)
		if !ok || m == nil {
			return nil
		}
		id, idOK := stringOf(m["id"])
		label, labelOK := stringOf(m["label"])
		src, _ := stringOf(m["source"])
		if !idOK || text.Trim(id) == "" || !labelOK || text.Trim(label) == "" || (src != "ocx" && src != "native") || seen[id] {
			return nil
		}
		seen[id] = true
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
		var ladder []string
		_ = json.Unmarshal(efforts, &ladder) // Already validated: retain blanks and duplicates in cached rows.
		entries = append(entries, CatalogEntry{id, ModelSource(src), label, &ladder})
	}
	message, _ := stringOf(m["message"])
	return &LiveCatalog{Catalog: Catalog{State: CatalogState(state), Entries: entries}, Status: status, Source: ModelSource(source), FetchedAt: &at, Message: message, raw: o, fetchedMS: written.UnixMilli()}
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

// ocxExecutable searches the supplied POSIX PATH (including relative/empty elements) from the process
// directory dir; denied candidates are skipped, with EACCES retained at exhaustion. Each candidate is the PATH element and
// "ocx" joined as execvp joins them, without lexical cleaning, and a relative one is prefixed with dir
// as written (inDir), so the file tested is the file the kernel executes and the one returned.
func ocxExecutable(env host.LookupEnv, dir string) (string, error) {
	path, set := env("PATH")
	if !set {
		path = "/bin:/usr/bin"
	}
	var denied, missing error
	for _, element := range strings.Split(path, string(os.PathListSeparator)) {
		candidate := "ocx"
		if element != "" {
			candidate = strings.TrimSuffix(element, "/") + "/ocx"
		}
		candidate = inDir(dir, candidate)
		if !filepath.IsAbs(candidate) {
			// No process directory is known to name a relative candidate by.
			missing = &os.PathError{Op: "exec", Path: candidate, Err: os.ErrNotExist}
			continue
		}
		info, err := os.Stat(candidate)
		if errors.Is(err, os.ErrPermission) {
			denied = err
			continue
		}
		if err != nil {
			if errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ENOTDIR) {
				missing = err
				continue
			}
			return "", err
		}
		if info.IsDir() || info.Mode().Perm()&0111 == 0 {
			denied = &os.PathError{Op: "exec", Path: candidate, Err: os.ErrPermission}
			continue
		}
		return candidate, nil
	}
	if denied != nil {
		return "", denied
	}
	if missing != nil {
		return "", missing
	}
	return "", &os.PathError{Op: "exec", Path: "ocx", Err: os.ErrNotExist}
}

// RunOcxModels ports ts:27-35 without a runtime Node dependency. It kills and
// reaps only its direct child, as execFile does; each stream has its own 4 MiB bound.
func RunOcxModels(environ []string) (string, error) {
	if environ == nil {
		environ = os.Environ()
	}
	return runOcx(resolveCatalogSource(catalogEnv(environ)), environ)
}

// runOcx runs the executable the request's source resolved, from the process directory that source was
// resolved in, so relative OPENCODEX_HOME and HOME values name the homes its key names.
func runOcx(src catalogSource, environ []string) (string, error) {
	if src.ocxErr != nil {
		return "", src.ocxErr
	}
	file := src.ocxExe
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	out, stderr := &catalogOutput{cancel: cancel}, &catalogOutput{cancel: cancel}
	cmd := exec.CommandContext(ctx, file, "models", "live", "--json")
	cmd.Env, cmd.Stdout, cmd.Stderr, cmd.Dir = environ, out, stderr, src.dir
	cmd.WaitDelay = 2 * time.Second
	err := cmd.Run()
	if out.overflow || stderr.overflow {
		return "", sentinel("OCX output exceeded 4 MiB")
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		if rejectedCommandLine(err, out.buffer.String(), stderr.buffer.String()) {
			return "", ErrCatalogUnsupported
		}
		return "", err
	}
	return out.buffer.String(), nil
}

// rejectedCommandLine reports whether the OCX refused the command line itself rather than failing
// while running it. Both cases exit non-zero with nothing on stdout, so the exit status alone cannot
// tell them apart; the argument rejection is the one that prints the command's usage block. The
// signal is deliberately narrow, because a failure it does not recognize stays an ordinary read
// failure rather than being reported as an OCX that cannot answer at all.
func rejectedCommandLine(err error, stdout, stderr string) bool {
	var exit *exec.ExitError
	if !errors.As(err, &exit) {
		return false
	}
	if stdout != "" {
		return false
	}
	for _, line := range strings.Split(stderr, "\n") {
		if strings.HasPrefix(text.Trim(line), "Usage: ocx models") {
			return true
		}
	}
	return false
}
