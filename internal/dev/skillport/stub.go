//go:build dev

// Package skillport is a placeholder: the red-first commit holds the tests and these stubs.
package skillport

import (
	"errors"
	"io"
)

const (
	StagingRoot = "port/cxc/skills"
	RecordDir   = "port/cxc/records"
)

type Origin struct {
	Tag           string `json:"tag"`
	Commit        string `json:"commit"`
	SkillsListing string `json:"skills_listing_sha256"`
}

type Source struct {
	Dir    string
	Origin Origin
}

type FileEntry struct {
	Original string `json:"original"`
	Exec     bool   `json:"exec,omitempty"`
}

type Skill struct {
	Origin Origin               `json:"origin"`
	Table  string               `json:"table_sha256"`
	From   string               `json:"from"`
	Files  map[string]FileEntry `json:"files"`
}

var errStub = errors.New("not implemented")

func DefaultOrigin() Origin                                             { return Origin{} }
func Listing(dir string) (string, error)                                { return "", errStub }
func Stage(root string, src Source, folders []string) ([]string, error) { return nil, errStub }
func Check(root string, src *Source) (int, []string)                    { return 0, nil }
func Run(args []string, stdout, stderr io.Writer) int                   { return 2 }
func run(args []string, stdout, stderr io.Writer, origin Origin) int    { return 2 }
func recordPath(root, name string) string                               { return "" }
func load(root, name string) (*Skill, error)                            { return nil, errStub }
func encode(s *Skill) ([]byte, error)                                   { return nil, errStub }
func stage(root string, src Source, folders []string, rename func(string, string) error) ([]string, error) {
	return nil, errStub
}
