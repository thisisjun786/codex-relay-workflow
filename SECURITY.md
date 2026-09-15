# Security

## Report a vulnerability privately

Use GitHub's [Report a vulnerability](https://github.com/thisisjun786/workflow-skills/security/advisories/new)
form. Maintainers must enable and verify this route as part of the
[publication procedure](docs/CI.md#public-repository-activation). Do not open a public issue or PR containing an unpatched vulnerability,
credential, private Linear description, or session transcript.

Include the affected source commit, relevant skill or script, host version,
expected and observed behavior, and a minimal reproduction using synthetic data.
Describe the impact without attaching live credentials or other users' data.
Korean and English reports are welcome. This personal project has no guaranteed
response time.

## Scope and support

Security fixes target the current `dev` branch. There are no separately maintained
release lines. The `main` branch is a release-promotion branch; its name alone does
not establish a supported release.

This repository distributes instructions and an installer, not a sandbox or an
authorization service. Skills can request actions from the host; the host's actual
permissions and the user's task scope remain the boundary. Review source changes
before updating a linked installation: edits become visible through symlinks.

Report defects in a separately installed bridge, relay, CXC, or Paperthin to that
component's maintainer unless the problem is in this repository's instructions
or installer. Offline fixtures do not prove that a live host integration is safe
or compatible.
