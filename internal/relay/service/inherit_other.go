//go:build !linux

package service

import "errors"

func closeRangeCloExec() error { return errors.ErrUnsupported }

// kernelMaxDescriptors is unknown away from Linux; the sweep lists /dev/fd there instead.
func kernelMaxDescriptors() (uint64, error) { return 0, errors.ErrUnsupported }
