package script

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"sort"
	"strings"
	"sync"

	"github.com/evanw/esbuild/pkg/api"
)

// LibPrefix is the specifier prefix of the helper library embedded in the binary.
const LibPrefix = "crw:lib/"

const (
	nsLib = "crwlib"
	nsDir = "crwwf"
)

// FileDigest is one source file a helper bundle was built from.
type FileDigest struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
}

// Helper is one bundled helper module of a script: the import specifier, the global its bundle assigns, and the
// digest of the sources it was built from (the run record keeps it beside the script's own digest).
type Helper struct {
	Spec       string       `json:"spec"`
	GlobalName string       `json:"global"`
	Digest     string       `json:"digest"`
	Files      []FileDigest `json:"files"`
	code       string
}

// BundledSource is the executable text of a helper, for static source consumers.
func (h Helper) BundledSource() string { return h.code }

func sha(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// bundler resolves and loads helper sources. Its resolver admits only the embedded library (crw:lib/<name>) and
// relative paths that stay inside the script's workflows directory; nothing else is reachable from a helper.
type bundler struct {
	lib, dir fs.FS

	mu          sync.Mutex
	files       map[string][]byte // namespace:path -> contents, of the current bundle
	usesSchemas bool
}

// errDynamicImport is the message of a dynamic import() in a helper: a helper may not reach another module at run time.
const errDynamicImport = "dynamic import() is not allowed in a helper: import its modules statically in the helper's header"

func (b *bundler) resolveErr(msg string) (api.OnResolveResult, error) {
	return api.OnResolveResult{}, errors.New(msg)
}

// resolve maps an import path seen from importer (namespace ns, path imp) to a file of one of the two roots.
func (b *bundler) resolve(p, importer, ns string) (api.OnResolveResult, error) {
	switch {
	case strings.HasPrefix(p, LibPrefix):
		name := strings.TrimPrefix(p, LibPrefix)
		clean := path.Clean(name)
		if name == "" || clean != name || !fs.ValidPath(clean) {
			return b.resolveErr(fmt.Sprintf("%q is not a valid library module name", p))
		}
		return b.find(nsLib, b.lib, clean, p)
	case strings.HasPrefix(p, "./") || strings.HasPrefix(p, "../") || p == "." || p == "..":
		root, rns := b.dir, nsDir
		base := "."
		if ns == nsLib {
			root, rns = b.lib, nsLib
		}
		if importer != "" && (ns == nsLib || ns == nsDir) {
			base = path.Dir(importer)
		}
		joined := path.Join(base, p)
		if joined == ".." || strings.HasPrefix(joined, "../") || !fs.ValidPath(joined) {
			return b.resolveErr(fmt.Sprintf("helper %q leaves the workflows directory; only crw:lib/<name> and relative paths inside the workflows directory may be imported", p))
		}
		if root == nil {
			return b.resolveErr(fmt.Sprintf("helper %q: this script has no workflows directory to import from", p))
		}
		return b.find(rns, root, joined, p)
	}
	return b.resolveErr(fmt.Sprintf("import %q is not allowed: only crw:lib/<name> and relative paths inside the workflows directory may be imported", p))
}

// find looks the module up as written and with ".js" appended.
func (b *bundler) find(ns string, root fs.FS, name, spec string) (api.OnResolveResult, error) {
	if root == nil {
		return b.resolveErr(fmt.Sprintf("helper %q: no library is available", spec))
	}
	for _, cand := range []string{name, name + ".js"} {
		if !strings.HasSuffix(cand, ".js") {
			continue
		}
		data, err := fs.ReadFile(root, cand)
		if err != nil {
			continue
		}
		b.mu.Lock()
		b.files[ns+":"+cand] = data
		if ns == nsLib && cand == "schemas.js" {
			b.usesSchemas = true
		}
		b.mu.Unlock()
		return api.OnResolveResult{Path: cand, Namespace: ns}, nil
	}
	return b.resolveErr(fmt.Sprintf("helper %q was not found (looked for %s and %s.js)", spec, name, name))
}

func (b *bundler) plugin() api.Plugin {
	return api.Plugin{
		Name: "crw-resolver",
		Setup: func(build api.PluginBuild) {
			build.OnResolve(api.OnResolveOptions{Filter: ".*"}, func(args api.OnResolveArgs) (api.OnResolveResult, error) {
				if args.Kind == api.ResolveJSDynamicImport {
					return b.resolveErr(errDynamicImport)
				}
				return b.resolve(args.Path, args.Importer, args.Namespace)
			})
			build.OnLoad(api.OnLoadOptions{Filter: ".*", Namespace: nsLib}, b.load)
			build.OnLoad(api.OnLoadOptions{Filter: ".*", Namespace: nsDir}, b.load)
		},
	}
}

func (b *bundler) load(args api.OnLoadArgs) (api.OnLoadResult, error) {
	b.mu.Lock()
	data, ok := b.files[args.Namespace+":"+args.Path]
	b.mu.Unlock()
	if !ok {
		return api.OnLoadResult{}, fmt.Errorf("helper %s was not resolved", args.Path)
	}
	src := string(data)
	// esbuild resolves an import("literal") through the resolver above; a computed import(expr) is never resolved (and
	// one in code the bundle drops is never seen at all), so the source is read for it as well. A source the lexer cannot
	// read is refused: the check fails closed.
	toks, err := tokenize(src)
	if err != nil {
		off := 0
		var le *lexError
		if errors.As(err, &le) {
			off = le.off
		}
		return api.OnLoadResult{}, fmt.Errorf("%s:%d: %v (the helper is read for dynamic import() before it is bundled)", args.Path, lineOf(src, off), err)
	}
	for i, t := range toks {
		if t.kind != tokIdent || t.text != "import" || i+1 >= len(toks) || toks[i+1].kind != tokPunct || toks[i+1].text != "(" {
			continue
		}
		if isMemberName(toks, i) || isMethodDeclaration(src, toks, i+1) {
			continue
		}
		return api.OnLoadResult{}, fmt.Errorf("%s:%d: %s", args.Path, lineOf(src, t.start), errDynamicImport)
	}
	loader := api.LoaderJS
	return api.OnLoadResult{Contents: &src, Loader: loader}, nil
}

// bundle builds one helper entry point into an IIFE assigned to global.
func (b *bundler) bundle(spec, global string) (*Helper, error) {
	b.files = map[string][]byte{}
	res := api.Build(api.BuildOptions{
		EntryPoints: []string{spec},
		Bundle:      true,
		Write:       false,
		Format:      api.FormatIIFE,
		GlobalName:  global,
		Platform:    api.PlatformNeutral,
		Target:      api.ES2017,
		LogLevel:    api.LogLevelSilent,
		Plugins:     []api.Plugin{b.plugin()},
		Outfile:     global + ".js",
	})
	if len(res.Errors) > 0 {
		return nil, fmt.Errorf("helper %q: %s", spec, messages(res.Errors))
	}
	if len(res.OutputFiles) != 1 {
		return nil, fmt.Errorf("helper %q: the bundle produced %d files", spec, len(res.OutputFiles))
	}
	h := &Helper{Spec: spec, GlobalName: global, code: string(res.OutputFiles[0].Contents)}
	keys := make([]string, 0, len(b.files))
	for k := range b.files {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var all strings.Builder
	for _, k := range keys {
		d := sha(b.files[k])
		h.Files = append(h.Files, FileDigest{Path: k, Digest: d})
		all.WriteString(k + "\x00" + d + "\n")
	}
	h.Digest = sha([]byte(all.String()))
	return h, nil
}

func messages(msgs []api.Message) string {
	var parts []string
	for _, m := range msgs {
		s := m.Text
		if m.Location != nil {
			s = fmt.Sprintf("%s:%d: %s", m.Location.File, m.Location.Line, m.Text)
		}
		parts = append(parts, s)
	}
	return strings.Join(parts, "; ")
}
