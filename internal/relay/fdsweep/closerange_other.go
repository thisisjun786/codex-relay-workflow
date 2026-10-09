//go:build !linux

package fdsweep

import "errors"

func closeRangeCloExec() error { return errors.ErrUnsupported }
