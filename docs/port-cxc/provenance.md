# CXC v0.2.40: provenance

The Go runtime ports the behavior of CodexClaw ("CXC") v0.2.40. This file records which upstream revision that is and how the copies on the development host were compared with it. The notices the port owes are in [NOTICE](../../NOTICE); the oracle behavior the port is measured against is described in [the corpus README](../../contract/schema/cxc/README.md).

## Upstream

| | |
| --- | --- |
| Repository | <https://github.com/lidge-jun/codexclaw> |
| Tag | `v0.2.40`, a lightweight tag: `git cat-file -t v0.2.40` prints `commit` |
| Commit | `3c1459acadeb1906d97c00a598e1457327ae372d` (`git ls-remote` and a local clone agree) |
| Plugin version | `0.2.40+codex.20260929183231`, the version of the plugin manifest at that commit and of the installed plugin cache compared below |
| License | MIT, "Copyright (c) 2026 lidge-jun" ([NOTICE](../../NOTICE)) |

## What was compared

Measured on 2026-10-03. Each row is one identity, with the number of regular files compared and the digest of the file listing defined under "Method".

| Compared | Result | Files | Listing digest |
| --- | --- | --- | --- |
| `git archive v0.2.40` extracted, against the extracted source the corpus was recorded from | identical, file by file | 2593 | `b477cbe83dd8b95504a9c96dbab99b4da4cc106c095550b65ee44d72eeb20bb6` |
| `plugins/codexclaw/` of that extracted source, against the installed plugin cache of version `0.2.40+codex.20260929183231` | identical, file by file; the permission bits match too (1105 files 664, 15 files 775) | 1120 | `3b2aa30cccf3874a82e9b5f32ba168d0c6a9cfc6fbed6c492876f849c32534bd` |

None of the compared trees holds a symbolic link. The file-mode listing of the second row has the digest `0172d277aca871be9bb582006336d7cef427758ea164e412d0aacf21fa99cd9a`.

## Method

`<tree>` is a directory of extracted files. Every command reads; none writes outside `<scratch>`.

```sh
# The tag's source, extracted from a clone of the upstream repository
git -C <codexclaw clone> rev-parse 'v0.2.40^{commit}'
git -C <codexclaw clone> archive v0.2.40 | tar -x -C <scratch>/tag-tree

# One "sha256  ./relative/path" line per regular file, paths in byte order
listing() { (cd "$1" && find . -type f -print0 | LC_ALL=C sort -z | xargs -0 sha256sum); }

# The whole tree: the extracted source against the tag's source
listing <scratch>/tag-tree | sha256sum
listing <extracted source> | sha256sum

# The plugin payload: the extracted source against the installed plugin cache
listing <extracted source>/plugins/codexclaw | sha256sum
listing <installed plugin cache of version 0.2.40+codex.20260929183231> | sha256sum

# Permission bits of the payload, same order
modes() { (cd "$1" && find . -type f -printf '%m %p\n' | LC_ALL=C sort); }
modes <extracted source>/plugins/codexclaw | sha256sum
modes <installed plugin cache of version 0.2.40+codex.20260929183231> | sha256sum
```

Two trees are identical when their digests are equal; `diff` of the two listings names any file that differs. A digest depends on the listing format, so another format gives another digest for the same trees.

## Limits

- The comparison is of file contents, and of permission bits for the payload only. It does not compare Git objects (an archive drops history and tree hashes), timestamps, ownership or symbolic-link targets.
- The tag-to-commit link was read from the remote and a clone at the time of measurement; a tag can be moved upstream.
- That the committed compiled `dist/` matches `src/` is the corpus README's own measurement, not repeated here.
- It is a snapshot. It does not show what Codex loads from the cache at run time, nor that a later cache update stays identical.
