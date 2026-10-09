package hook

import "strings"

// The worktree deletion guard reads an inline program of an interpreter by its tokens, not by a list of removal spellings.
// A program that can remove a file, a program that starts another program, and a program that reaches a name only at run
// time are refused in a managed worktree: their effect is code the text does not show. Ordinary programs are allowed.
// The rule is lexical over the whole text, strings and comments included, so a removal word inside a string is refused too;
// that is an accepted price (CRW-1028, known-defects).

// worktreeDelProgramRefusal returns the reason an inline program of the language lang is refused, or "" when the guard
// reads it and it neither removes a file nor starts another program.
func worktreeDelProgramRefusal(lang, src string) string {
	if (lang == "perl" || lang == "ruby") && strings.ContainsRune(src, '`') {
		return "a program that starts another program"
	}
	if lang == "ruby" && strings.Contains(src, "%x") {
		return "a program that starts another program"
	}
	if worktreeDelComputedSubscript(src) {
		return "a program with a computed attribute name"
	}
	if lang == "perl" && worktreeDelPerlVariableLoad(src) {
		return "a program that loads code from a variable"
	}
	for _, tok := range worktreeDelProgramTokens(src) {
		switch {
		case worktreeDelRemovesToken(tok):
			return "a program that removes files"
		case worktreeDelStartsToken(tok):
			return "a program that starts another program"
		case worktreeDelRunTimeToken(lang, tok):
			return "a program with a name the guard cannot read"
		}
	}
	return ""
}

// worktreeDelProgramTokens splits the text into identifier-like tokens (letters, digits and underscores).
func worktreeDelProgramTokens(src string) []string {
	var out []string
	start := -1
	for i, r := range src {
		if worktreeDelIdentRune(r) {
			if start < 0 {
				start = i
			}
			continue
		}
		if start >= 0 {
			out = append(out, src[start:i])
			start = -1
		}
	}
	if start >= 0 {
		out = append(out, src[start:])
	}
	return out
}

func worktreeDelIdentRune(r rune) bool {
	return r == '_' || (r >= '0' && r <= '9') || (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

// worktreeDelRemovesToken: a name that is a removal in any language. A token that starts with rm (rm, rmdir, rmtree, rm_r,
// rm_rf, rmsync, rmrf) or that says it removes, erases, destroys or overwrites a file (remove, unlink, delete, trash,
// truncate, erase, wipe, shred, purge, destroy) is a removal: os.remove, Path.unlink, FileUtils.rm_r, Dir.rmdir,
// remove_entry, Deno.remove, fs.promises.rm and the like.
func worktreeDelRemovesToken(tok string) bool {
	t := strings.ToLower(tok)
	if strings.HasPrefix(t, "rm") {
		return true
	}
	for _, f := range []string{"remove", "unlink", "delete", "trash", "truncate", "erase", "wipe", "shred", "purge", "destroy"} {
		if strings.Contains(t, f) {
			return true
		}
	}
	return false
}

// worktreeDelStartsToken: a name that starts another program (subprocess, os.system, os.popen, os.exec*, os.spawn*,
// child_process, Kernel#system, Perl system and exec, Node process.binding, qx).
func worktreeDelStartsToken(tok string) bool {
	t := strings.ToLower(tok)
	if t == "qx" {
		return true
	}
	for _, f := range []string{"exec", "spawn", "system", "popen", "subprocess", "child_process", "fork", "binding"} {
		if strings.Contains(t, f) {
			return true
		}
	}
	return false
}

// worktreeDelRunTimeCommon names, in every language, a name that reaches a call only at run time or runs code the text does
// not show: the attribute and import machinery, and the modules that load, unpickle or compile code or call into C.
var worktreeDelRunTimeCommon = map[string]bool{
	"getattr": true, "setattr": true, "delattr": true, "__import__": true, "importlib": true, "globals": true,
	"locals": true, "vars": true, "builtins": true, "__builtins__": true, "__getattribute__": true, "__class__": true,
	"__subclasses__": true, "__globals__": true, "constructor": true, "eval": true, "exec": true, "execfile": true,
	"compile": true, "runpy": true, "pickle": true, "marshal": true, "shelve": true, "ctypes": true, "cffi": true,
	"code": true, "codeop": true, "zipimport": true, "pkgutil": true, "pty": true, "imp": true,
}

// worktreeDelRunTimeLang names the run-time names of one language beyond the common set.
var worktreeDelRunTimeLang = map[string]map[string]bool{
	"node": {"vm": true, "module": true, "worker_threads": true, "WebAssembly": true, "Function": true},
	"ruby": {"instance_eval": true, "class_eval": true, "module_eval": true, "send": true, "public_send": true,
		"load": true, "binding": true},
}

// worktreeDelRunTimeToken: a name that reaches a call only at run time, or runs code the text does not show, in the
// language of the program.
func worktreeDelRunTimeToken(lang, tok string) bool {
	return worktreeDelRunTimeCommon[tok] || worktreeDelRunTimeLang[lang][tok]
}

// worktreeDelPerlVariableLoad: require or do followed by a variable loads code the text does not show (require $file,
// do $file, require($x)).
func worktreeDelPerlVariableLoad(src string) bool {
	for _, sp := range shellIRTokenSpans(src) {
		switch strings.ToLower(src[sp[0]:sp[1]]) {
		case "require", "do":
			if c := shellIRNextNonSpace(src, sp[1]); c == '$' || c == '(' && strings.HasPrefix(strings.TrimLeft(src[sp[1]:], " \t\r\n("), "$") {
				return true
			}
		}
	}
	return false
}

// worktreeDelComputedSubscript reports a subscript whose key is neither a number or slice nor a plain string literal:
// obj[expr] or a concatenated key can name a call the text does not spell. A plain literal key is a name the token scan
// already reads.
func worktreeDelComputedSubscript(src string) bool {
	for i := 1; i < len(src); i++ {
		if src[i] != '[' {
			continue
		}
		prev := src[i-1]
		if !(prev == ')' || prev == ']' || (prev < 128 && worktreeDelIdentRune(rune(prev)))) {
			continue
		}
		end := strings.IndexByte(src[i:], ']')
		if end < 0 {
			return true
		}
		if !worktreeDelPlainKey(src[i+1 : i+end]) {
			return true
		}
	}
	return false
}

// worktreeDelPlainKey: a number, a slice of numbers, or one string literal with no quote, escape or operator in it.
func worktreeDelPlainKey(k string) bool {
	k = strings.TrimSpace(k)
	if k == "" {
		return false
	}
	numeric := true
	for _, r := range k {
		if !(r >= '0' && r <= '9') && r != ':' && r != '-' && r != ' ' {
			numeric = false
			break
		}
	}
	if numeric {
		return true
	}
	if len(k) < 2 || (k[0] != '\'' && k[0] != '"') || k[len(k)-1] != k[0] {
		return false
	}
	return !strings.ContainsAny(k[1:len(k)-1], "'\"\\+")
}
