//go:build !linux

package service

import "errors"

func closeRangeCloExec() error { return errors.ErrUnsupported }
