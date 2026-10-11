# Scripts

Utility scripts for backend development workflows.

## Scaffold audit

The structure audit of a backend project is the one script the scaffolding skill owns,
[scaffold-audit.sh](../../crw-dev-scaffolding/scripts/scaffold-audit.sh):

```bash
bash <crw-dev-scaffolding skill directory>/scripts/scaffold-audit.sh [project-path]
```

It checks:
1. Feature-based structure (`src/` with domain folders, no type-based directories)
2. Colocation (logic + test together; Go files next to their `_test.go`)
3. Public boundary exports (`index.ts`, `__init__.py`, etc.)
4. Source-of-truth docs (`docs/`, `plans/`, `adr/`, `architecture/`, `README.md`, `AGENTS.md`)
5. `.env` safety (no `.env` file, `.env` in `.gitignore`, `.env.example` present)
6. File length (< 500 lines)
7. Agent/context instructions (`AGENTS.md` or `CLAUDE.md`)
