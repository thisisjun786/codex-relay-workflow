package script

import (
	"fmt"
	"strings"
)

// importKind is the shape of one static import clause.
type bindKind int

const (
	bindNamed bindKind = iota
	bindDefault
	bindNamespace
)

// binding is one local name an import introduces.
type binding struct {
	kind   bindKind
	export string // the exported name (named imports only)
	local  string
}

// importDecl is one static import of the header.
type importDecl struct {
	spec     string
	bindings []binding
	off      int
}

// header is the parsed head of a script: the static imports, the meta object literal and where the body starts.
type header struct {
	imports []importDecl
	// metaSrc is the object literal's source text.
	metaSrc string
	// bodyStart is the byte offset of the first byte after the meta statement.
	bodyStart int
}

// parseHeader reads the header of src from its token stream: zero or more static imports and then exactly one
// `export const meta = <object literal>`. Anything else before the body (another statement, another export, a
// dynamic import) is an error; the body is checked by checkBody.
func parseHeader(name, src string, toks []token) (*header, error) {
	h := &header{}
	i := 0
	fail := func(off int, format string, args ...any) (*header, error) {
		return nil, errAt(name, src, off, format, args...)
	}
	for i < len(toks) && toks[i].kind == tokIdent && toks[i].text == "import" {
		decl := importDecl{off: toks[i].start}
		i++
		if i >= len(toks) {
			return fail(decl.off, "incomplete import")
		}
		t := toks[i]
		switch {
		case t.kind == tokPunct && t.text == "(":
			return fail(decl.off, "dynamic import() is not allowed")
		case t.kind == tokPunct && t.text == ".":
			return fail(decl.off, "import.meta is not available")
		case t.kind == tokString:
			decl.spec = t.value()
			i++
		default:
			// [default] [, {named} | * as ns] from 'spec'
			expectComma := false
			if t.kind == tokIdent {
				decl.bindings = append(decl.bindings, binding{kind: bindDefault, local: t.text})
				i++
				expectComma = true
			}
			if expectComma {
				if i < len(toks) && toks[i].kind == tokPunct && toks[i].text == "," {
					i++
				} else {
					expectComma = false
				}
			}
			if i < len(toks) && toks[i].kind == tokPunct && toks[i].text == "{" {
				i++
				for {
					if i >= len(toks) {
						return fail(decl.off, "unterminated import list")
					}
					if toks[i].kind == tokPunct && toks[i].text == "}" {
						i++
						break
					}
					if toks[i].kind != tokIdent {
						return fail(toks[i].start, "unexpected %q in import list", toks[i].text)
					}
					b := binding{kind: bindNamed, export: toks[i].text, local: toks[i].text}
					i++
					if i+1 < len(toks) && toks[i].kind == tokIdent && toks[i].text == "as" && toks[i+1].kind == tokIdent {
						b.local = toks[i+1].text
						i += 2
					}
					if b.export == "default" && b.local == "default" {
						return fail(toks[i-1].start, "import {default} needs a local name")
					}
					decl.bindings = append(decl.bindings, b)
					if i < len(toks) && toks[i].kind == tokPunct && toks[i].text == "," {
						i++
					}
				}
			} else if i+2 < len(toks) && toks[i].kind == tokPunct && toks[i].text == "*" && toks[i+1].kind == tokIdent && toks[i+1].text == "as" && toks[i+2].kind == tokIdent {
				decl.bindings = append(decl.bindings, binding{kind: bindNamespace, local: toks[i+2].text})
				i += 3
			} else if !expectComma && len(decl.bindings) == 0 {
				return fail(decl.off, "malformed import")
			}
			if i+1 >= len(toks) || toks[i].kind != tokIdent || toks[i].text != "from" || toks[i+1].kind != tokString {
				return fail(decl.off, "malformed import: expected from '<module>'")
			}
			decl.spec = toks[i+1].value()
			i += 2
		}
		if i < len(toks) && toks[i].kind == tokIdent && toks[i].text == "with" {
			return fail(toks[i].start, "import attributes are not supported")
		}
		if i < len(toks) && toks[i].kind == tokPunct && toks[i].text == ";" {
			i++
		}
		h.imports = append(h.imports, decl)
	}
	if i >= len(toks) {
		return fail(len(src), "the script has no `export const meta = {...}`")
	}
	t := toks[i]
	if t.kind == tokIdent && t.text == "export" {
		if i+3 < len(toks) && toks[i+1].text == "const" && toks[i+2].text == "meta" && toks[i+3].kind == tokPunct && toks[i+3].text == "=" {
			i += 4
			if i >= len(toks) || toks[i].kind != tokPunct || toks[i].text != "{" {
				return fail(t.start, "meta must be an object literal")
			}
			open := toks[i].start
			depth := 0
			closeEnd := -1
			for ; i < len(toks); i++ {
				if toks[i].kind != tokPunct {
					continue
				}
				switch toks[i].text {
				case "{":
					depth++
				case "}":
					depth--
				}
				if depth == 0 {
					closeEnd = toks[i].end
					i++
					break
				}
			}
			if closeEnd < 0 {
				return fail(open, "meta object literal is not closed")
			}
			h.metaSrc = src[open:closeEnd]
			h.bodyStart = closeEnd
			if i < len(toks) && toks[i].kind == tokPunct && toks[i].text == ";" && onlyBlank(src[closeEnd:toks[i].start]) {
				h.bodyStart = toks[i].end
			}
			return h, nil
		}
		return fail(t.start, "the only export a script may have is `export const meta = {...}`")
	}
	if t.kind == tokIdent && t.text == "import" {
		return fail(t.start, "import after the header")
	}
	return fail(t.start, "the header holds imports and then `export const meta = {...}`; found %s before it", describe(t))
}

func onlyBlank(s string) bool {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t':
		default:
			return false
		}
	}
	return true
}

func describe(t token) string {
	switch t.kind {
	case tokString, tokTemplate, tokRegex:
		return "a literal"
	}
	return fmt.Sprintf("%q", t.text)
}

// checkBody rejects what the body may not contain: a dynamic import(), import.meta, and an import or export
// declaration. A property named import or export and an object key of that name are left alone.
func checkBody(name, src string, toks []token, from int) error {
	for i, t := range toks {
		if t.start < from || t.kind != tokIdent || (t.text != "import" && t.text != "export") {
			continue
		}
		if isMemberName(toks, i) {
			continue
		}
		var next token
		if i+1 < len(toks) {
			next = toks[i+1]
		}
		if t.text == "import" {
			switch {
			case next.kind == tokPunct && next.text == "(":
				if isMethodDeclaration(src, toks, i+1) {
					continue
				}
				return errAt(name, src, t.start, "dynamic import() is not allowed")
			case next.kind == tokPunct && next.text == ".":
				return errAt(name, src, t.start, "import.meta is not available")
			case next.kind == tokString || next.kind == tokIdent || next.kind == tokPunct && (next.text == "{" || next.text == "*"):
				return errAt(name, src, t.start, "import after the header")
			}
			continue
		}
		switch {
		case next.kind == tokIdent || next.kind == tokPunct && (next.text == "{" || next.text == "*"):
			return errAt(name, src, t.start, "export after the header: the only export a script may have is `export const meta`")
		}
	}
	return nil
}

// isMemberName reports whether the identifier at toks[i] is the name of a property access (`o.import`, `o?.import`,
// with whatever whitespace or comments lie between the dot and the name). A dot that is the last of three adjacent
// dots is a spread (`...import(x)`), which applies to the expression that follows, so it does not count.
func isMemberName(toks []token, i int) bool {
	if i == 0 || toks[i-1].kind != tokPunct || toks[i-1].text != "." {
		return false
	}
	if i >= 2 && toks[i-2].kind == tokPunct && toks[i-2].text == "." && toks[i-2].end == toks[i-1].start {
		return false
	}
	return true
}

// isMethodDeclaration reports whether the parenthesis at toks[open] starts the parameter list of a method named
// import (`{ import(x) {...} }`, `class { static async import() {...} }`): its closing parenthesis is followed by an
// opening brace on the same line. A call cannot be followed by a brace on its own line of the source (that would be a
// syntax error), and the one that starts a block on the next line is still read as a call, so the check fails closed.
func isMethodDeclaration(src string, toks []token, open int) bool {
	depth := 0
	for k := open; k < len(toks); k++ {
		if toks[k].kind != tokPunct {
			continue
		}
		switch toks[k].text {
		case "(":
			depth++
		case ")":
			depth--
			if depth == 0 {
				if k+1 >= len(toks) || toks[k+1].kind != tokPunct || toks[k+1].text != "{" {
					return false
				}
				return !strings.ContainsAny(src[toks[k].end:toks[k+1].start], "\n\r")
			}
		}
	}
	return false
}
