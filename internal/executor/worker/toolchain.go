package worker

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/thisisjun786/codex-relay-workflow/internal/executor"
	"github.com/thisisjun786/codex-relay-workflow/internal/tmploc"
)

type ToolFile struct {
	Path       string `json:"path"`
	Version    string `json:"version"`
	SHA256     string `json:"sha256"`
	Source     string `json:"source"`
	Executable bool   `json:"executable"`
}
type Toolchain struct {
	OS    string     `json:"os"`
	Arch  string     `json:"arch"`
	Files []ToolFile `json:"files"`
}
type ToolchainReadback struct {
	Digest string     `json:"digest"`
	Root   string     `json:"root"`
	Files  []ToolFile `json:"files"`
}

func (m Toolchain) Digest() string { b, _ := json.Marshal(m); return executor.Digest(b) }
func (m Toolchain) Validate() error {
	if m.OS != runtime.GOOS || m.Arch != runtime.GOARCH {
		return errors.New("toolchain platform differs from worker")
	}
	if len(m.Files) == 0 || len(m.Files) > 1024 {
		return errors.New("toolchain must contain 1 to 1024 pinned files")
	}
	seen := map[string]bool{}
	for _, f := range m.Files {
		if f.Path == "." || !path.IsAbs(f.Source) || !filepath.IsAbs(f.Source) || !filepath.IsLocal(f.Path) || strings.Contains(f.Path, "\\") || path.Clean(f.Path) != f.Path || f.Version == "" || len(f.SHA256) != 64 {
			return fmt.Errorf("invalid pinned tool file %q", f.Path)
		}
		if _, err := hex.DecodeString(f.SHA256); err != nil {
			return err
		}
		if seen[f.Path] {
			return errors.New("duplicate tool path")
		}
		seen[f.Path] = true
	}
	return nil
}
func toolBytes(file string) ([]byte, error) {
	info, err := os.Lstat(file)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 64<<20 {
		return nil, errors.New("tool artifact must be a regular file of at most 64 MiB")
	}
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, 64<<20+1))
}
func ReadToolchain(root string, m Toolchain) (ToolchainReadback, error) {
	out := ToolchainReadback{Digest: m.Digest(), Root: filepath.Join(root, "versions", m.Digest()), Files: m.Files}
	if err := m.Validate(); err != nil {
		return out, err
	}
	real, err := filepath.EvalSymlinks(out.Root)
	if err != nil {
		return out, err
	}
	if real != out.Root {
		return out, errors.New("toolchain version directory is a symlink")
	}
	for _, f := range m.Files {
		p := filepath.Join(out.Root, filepath.FromSlash(f.Path))
		real, err := filepath.EvalSymlinks(p)
		if err != nil {
			return out, err
		}
		if real != p {
			return out, errors.New("toolchain file contains a symlink")
		}
		b, err := toolBytes(p)
		if err != nil {
			return out, err
		}
		if executor.Digest(b) != f.SHA256 {
			return out, fmt.Errorf("toolchain drift at %s", f.Path)
		}
		info, err := os.Stat(p)
		if err != nil {
			return out, err
		}
		mode := os.FileMode(0600)
		if f.Executable {
			mode = 0700
		}
		if info.Mode().Perm() != mode {
			return out, fmt.Errorf("toolchain mode drift at %s", f.Path)
		}
	}
	return out, nil
}

// InstallToolchain writes a new owned version atomically. It executes no artifact,
// copies no login state and never overwrites an existing version. The controller
// separately requires drain and retains its applying intent across lost replies.
func InstallToolchain(root string, m Toolchain) (ToolchainReadback, error) {
	if err := m.Validate(); err != nil {
		return ToolchainReadback{}, err
	}
	if err := privateDirectory(root); err != nil {
		return ToolchainReadback{}, err
	}
	versions := filepath.Join(root, "versions")
	if err := privateDirectory(versions); err != nil {
		return ToolchainReadback{}, err
	}
	dest := filepath.Join(versions, m.Digest())
	if _, err := os.Lstat(dest); err == nil {
		return ReadToolchain(root, m)
	} else if !errors.Is(err, os.ErrNotExist) {
		return ToolchainReadback{}, err
	}
	stage, err := tmploc.MkdirTempIn(versions, ".stage-")
	if err != nil {
		return ToolchainReadback{}, err
	}
	defer os.RemoveAll(stage)
	for _, f := range m.Files {
		b, err := toolBytes(f.Source)
		if err != nil {
			return ToolchainReadback{}, err
		}
		if executor.Digest(b) != f.SHA256 {
			return ToolchainReadback{}, fmt.Errorf("artifact digest mismatch: %s", f.Path)
		}
		target := filepath.Join(stage, filepath.FromSlash(f.Path))
		if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return ToolchainReadback{}, err
		}
		mode := os.FileMode(0600)
		if f.Executable {
			mode = 0700
		}
		fd, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
		if err != nil {
			return ToolchainReadback{}, err
		}
		_, writeErr := fd.Write(b)
		err = errors.Join(writeErr, fd.Sync(), fd.Close())
		if err != nil {
			return ToolchainReadback{}, err
		}
	}
	if err = os.Rename(stage, dest); err != nil {
		if _, e := os.Lstat(dest); e == nil {
			return ReadToolchain(root, m)
		}
		return ToolchainReadback{}, err
	}
	dir, err := os.Open(versions)
	if err != nil {
		return ToolchainReadback{}, err
	}
	err = errors.Join(dir.Sync(), dir.Close())
	if err != nil {
		return ToolchainReadback{}, err
	}
	return ReadToolchain(root, m)
}

// ActivateToolchain updates only the worker's owned profile record, after the
// caller has checked both controller and worker drain. Existing versions stay.
func ActivateToolchain(root string, m Toolchain) error {
	if _, err := ReadToolchain(root, m); err != nil {
		return err
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	stage, err := tmploc.MkdirTempIn(root, ".active-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	f, err := os.OpenFile(filepath.Join(stage, "profile.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	_, writeErr := f.Write(raw)
	err = errors.Join(writeErr, f.Sync(), f.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), filepath.Join(root, "active-toolchain.json")); err != nil {
		return err
	}
	dir, err := os.Open(root)
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}
func ActiveToolchain(root string, fallback Toolchain) (Toolchain, error) {
	p := filepath.Join(root, "active-toolchain.json")
	info, err := os.Lstat(p)
	if errors.Is(err, os.ErrNotExist) {
		return fallback, nil
	}
	if err != nil {
		return Toolchain{}, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 {
		return Toolchain{}, errors.New("active toolchain record is not private and regular")
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		return Toolchain{}, err
	}
	var m Toolchain
	if err = json.Unmarshal(raw, &m); err != nil {
		return Toolchain{}, err
	}
	if _, err = ReadToolchain(root, m); err != nil {
		return Toolchain{}, err
	}
	return m, nil
}
