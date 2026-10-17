
## Helpers

A helper is a module the header imports. Two kinds are admitted, and nothing else:

- `crw:lib/<name>`: the helper library embedded in the binary (`internal/flow/workflows/lib/<name>.js`).
- A relative path (`./x.js`, `../lib/y.js`, with or without the `.js`) that stays inside the script's workflows
  directory, the directory the script file is in. A path that leaves it, an absolute path, a bare package name, a URL and
  a symbolic link that points out of the directory are load errors.

Helpers are ES modules with static `import` and `export`. A helper with top-level `await` is a load error (the bundle is an
IIFE), and so is a dynamic `import()` in a helper, literal or computed, direct or in a helper it imports: a helper reaches
other modules only through static imports the resolver checks. Every helper source is read by the same lexer as the body
for this check, and a helper the lexer cannot read is a load error, so the check never passes a source it did not see. There is no `require`, `fs`, `net` or `exec` in a script or a helper.

The run record holds the SHA-256 digest of the script's bytes and, for every bundled helper, the digest of the sources it
was built from.
