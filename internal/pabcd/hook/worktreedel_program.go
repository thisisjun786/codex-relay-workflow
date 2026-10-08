package hook

import "strings"

// The worktree deletion guard reads an inline program of an interpreter by its tokens, not by a list of removal names.
// A program that can remove a file, a program that starts another program, and a program that reaches a name only at run
// time are refused in a managed worktree: their effect is code the text does not show. Ordinary programs are allowed.

// worktreeDelProgramRefusal returns the reason an inline program of the language lang is refused, or "" when the guard
// reads it and it neither removes a file nor starts another program. The scan works on the whole text, strings and
// comments included, so it over-refuses before it under-refuses.
func worktreeDelProgramRefusal(lang, src string) string {
	if (lang == "perl" || lang == "ruby") && (strings.ContainsRune(src, '`') || (lang == "ruby" && strings.Contains(src, "%x"))) {
		return "a program that starts another program"
	}
	if worktreeDelComputedSubscript(src) {
		return "a program with a computed attribute name"
	}
	for _, tok := range worktreeDelProgramTokens(src) {
		switch {
		case worktreeDelRemovesToken(tok):
			return "a program that removes files"
		case worktreeDelStartsToken(tok):
			return "a program that starts another program"
		case worktreeDelRunTimeToken(tok):
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

// worktreeDelRemovesToken: a call whose name says it removes, in any spelling (os.remove, Path.unlink, Dir.rmdir,
// shutil.rmtree, fs.rmSync, fs.promises.rm, FileUtils.rm_rf, Perl unlink and remove_tree).
func worktreeDelRemovesToken(tok string) bool {
	t := strings.ToLower(tok)
	if t == "rm" {
		return true
	}
	for _, f := range []string{"remove", "unlink", "rmdir", "rmtree", "delete", "rm_rf", "rmsync", "trash", "truncate"} {
		if strings.Contains(t, f) {
			return true
		}
	}
	return false
}

// worktreeDelStartsToken: a name that starts another program (subprocess, os.system, os.popen, os.exec*, os.spawn*,
// child_process, Kernel#system, Perl system and exec, Node process.binding).
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

// worktreeDelRunTimeToken: a name that reaches a call only at run time, or runs code the text does not show.
func worktreeDelRunTimeToken(tok string) bool {
	switch tok {
	case "getattr", "setattr", "delattr", "__import__", "importlib", "globals", "locals", "vars", "builtins",
		"__builtins__", "__getattribute__", "__class__", "__subclasses__", "__globals__", "constructor", "Function",
		"eval", "vm":
		return true
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
