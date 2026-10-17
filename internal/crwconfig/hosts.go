package crwconfig

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/executor"
)

// ExecutionHost is an operator-approved identity, never a provider credential.
// Repository keys are canonical forge names; checkout roots are transport-local.
type ExecutionHost struct {
	ID                 string            `json:"id"`
	Transport          string            `json:"transport"`
	Endpoint           string            `json:"endpoint,omitempty"`
	KnownHosts         string            `json:"known_hosts,omitempty"`
	Root               string            `json:"root"`
	Repositories       map[string]string `json:"repositories"`
	Kind               executor.Kind     `json:"kind"`
	CapabilityRevision string            `json:"capability_revision"`
	LaunchProfile      string            `json:"launch_profile"`
	ToolchainProfile   string            `json:"toolchain_profile"`
	AuthProfile        string            `json:"auth_profile"`
	TrustProfile       string            `json:"trust_profile"`
	Supervision        string            `json:"supervision"`
}

type HostCapacity struct {
	Sessions  int `json:"session_limit"`
	TestSlots int `json:"test_slots"`
	TestCPU   int `json:"test_cpu_budget"`
}

var hostName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,127}$`)
var hostDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)
var repositoryName = regexp.MustCompile(`^[a-z0-9_.-]+/[a-z0-9_.-]+$`)

func (h ExecutionHost) Validate() error {
	if !hostName.MatchString(h.ID) || h.CapabilityRevision == "" {
		return fmt.Errorf("host needs an opaque id and capability revision")
	}
	if h.Transport != "local" && h.Transport != "ssh" {
		return fmt.Errorf("host transport is local or ssh")
	}
	if h.Transport == "ssh" && (!hostName.MatchString(h.Endpoint) || !filepath.IsAbs(h.KnownHosts)) {
		return fmt.Errorf("SSH requires an approved alias and absolute pinned known_hosts file")
	}
	if !filepath.IsAbs(h.Root) || filepath.Clean(h.Root) != h.Root || h.Root == "/" {
		return fmt.Errorf("host root must be an absolute owned directory")
	}
	if !hostDigest.MatchString(h.LaunchProfile) || !hostDigest.MatchString(h.ToolchainProfile) {
		return fmt.Errorf("host profiles need SHA256 digests")
	}
	if h.Kind != executor.Codex && h.Kind != executor.Devin && h.Kind != executor.ClaudeCode {
		return fmt.Errorf("unknown executor kind")
	}
	if h.AuthProfile == "" || h.TrustProfile == "" || h.Supervision != "worker-rpc" {
		return fmt.Errorf("approved authentication, workspace trust and worker-rpc supervision profiles are required")
	}
	if len(h.Repositories) == 0 {
		return fmt.Errorf("host repository allowlist is empty")
	}
	seenRoots := map[string]bool{}
	for repo, root := range h.Repositories {
		owner, name, _ := strings.Cut(repo, "/")
		if !repositoryName.MatchString(repo) || owner == "." || owner == ".." || name == "." || name == ".." || strings.HasSuffix(repo, ".git") || !filepath.IsAbs(root) || filepath.Clean(root) != root || seenRoots[root] {
			return fmt.Errorf("repository %q must be a canonical owner/repo with an absolute checkout root", repo)
		}
		seenRoots[root] = true
	}
	return nil
}
func (p HostCapacity) Validate(effectiveCPU int) error {
	if p.Sessions < 1 || p.Sessions > 1024 || p.TestSlots < 1 || p.TestSlots > 1024 || p.TestCPU < 1 || p.TestCPU >= effectiveCPU {
		return fmt.Errorf("capacity needs positive session/test limits and explicit CPU budget below observed effective CPUs")
	}
	return nil
}
func (f *File) ExecutionHosts() ([]ExecutionHost, error) {
	var section struct {
		Hosts []ExecutionHost `json:"hosts"`
	}
	if err := f.Section("execution", &section); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for _, h := range section.Hosts {
		if err := h.Validate(); err != nil {
			return nil, err
		}
		if seen[h.ID] {
			return nil, fmt.Errorf("duplicate execution host %s", h.ID)
		}
		seen[h.ID] = true
	}
	return section.Hosts, nil
}
