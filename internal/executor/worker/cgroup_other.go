//go:build !linux

package worker

import (
	"context"
	"errors"
)

type CgroupRunner struct {
	Root                   string
	Directory              string
	Env                    []string
	CommandUID, CommandGID uint32
}

func (CgroupRunner) Available() error {
	return errors.New("bounded worker commands require Linux cgroup v2")
}

func (CgroupRunner) Run(context.Context, Group) (CommandResult, error) {
	return CommandResult{}, errors.New("bounded worker commands require Linux cgroup v2")
}
func (CgroupRunner) InspectGroup(context.Context, Group, bool) (CommandResult, error) {
	return CommandResult{}, errors.New("bounded worker commands require Linux cgroup v2")
}
