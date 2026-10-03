//go:build dev

package skillport

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// Stage copies each folder of the source skills tree into the staging root with the name table
// applied and writes its record. It never overwrites: a folder whose staged directory or record
// exists is refused. Each folder is a unit; the names done before a failure stay complete.
func Stage(root string, src Source, folders []string) ([]string, error) {
	return stage(root, src, folders, os.Rename)
}

func stage(root string, src Source, folders []string, rename func(string, string) error) ([]string, error) {
	sub, err := newSubstituter(root)
	if err != nil {
		return nil, err
	}
	if got, err := Listing(src.skills()); err != nil || got != src.Origin.SkillsListing {
		return nil, fmt.Errorf("the source skills tree does not match the pinned origin %s (listing %s, %v)", src.Origin.Tag, got, err)
	}
	var done []string
	for _, folder := range folders {
		name, err := stageOne(root, src, sub, folder, rename)
		if err != nil {
			return done, fmt.Errorf("%s: %w", folder, err)
		}
		done = append(done, name)
	}
	return done, nil
}

// stageOne writes the skill into a temp directory and its record into a temp file, then renames the
// directory and the record into place; a failure removes what this call made and nothing else.
func stageOne(root string, src Source, sub *substituter, folder string, rename func(string, string) error) (name string, err error) {
	if !validFolder(folder) {
		return "", errors.New("not a skill folder name")
	}
	if sub.isStub(folder) {
		return "", errors.New("the name table does not port this skill (redirect stub or out of scope)")
	}
	name = prefix + folder
	target, record := filepath.Join(root, StagingRoot, name), recordPath(root, name)
	for _, p := range []string{target, record} {
		if _, err := os.Lstat(p); err == nil {
			return "", fmt.Errorf("%s exists: stage never overwrites", p)
		}
	}
	if info, err := os.Stat(filepath.Join(src.skills(), folder)); err != nil || !info.IsDir() {
		return "", errors.New("no such skill in the source tree")
	}
	files, err := sub.render(filepath.Join(src.skills(), folder))
	if err != nil {
		return "", err
	}
	if err = os.MkdirAll(filepath.Join(root, StagingRoot), 0o755); err != nil {
		return "", err
	}
	tmp, err := os.MkdirTemp(filepath.Join(root, StagingRoot), ".stage-")
	if err != nil {
		return "", err
	}
	tmpRecord := ""
	defer func() {
		if err != nil {
			os.RemoveAll(tmp)
			os.Remove(tmpRecord)
		}
	}()
	skill := &Skill{Origin: src.Origin, Table: sub.digest, From: folder, Files: map[string]FileEntry{}}
	for p, f := range files {
		skill.Files[p] = FileEntry{sum(f.data), f.exec}
		dest := filepath.Join(tmp, filepath.FromSlash(p))
		mode := os.FileMode(0o644)
		if f.exec {
			mode = 0o755
		}
		if err = os.MkdirAll(filepath.Dir(dest), 0o755); err == nil {
			if err = os.WriteFile(dest, f.data, mode); err == nil {
				err = os.Chmod(dest, mode)
			}
		}
		if err != nil {
			return "", err
		}
	}
	data, err := encode(skill)
	if err != nil {
		return "", err
	}
	if tmpRecord, err = writeTemp(record, data); err != nil {
		return "", err
	}
	if err = rename(tmp, target); err != nil {
		return "", err
	}
	if err = rename(tmpRecord, record); err != nil {
		os.RemoveAll(target)
		return "", err
	}
	return name, nil
}
