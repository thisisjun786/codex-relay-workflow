package service

import (
	"errors"
	"io"
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

// Private self-spawn handshake: EOF releases the first worker's policy receipt
// after the launcher has observed supervisor readiness. Later workers see EOF
// immediately. A launcher crash also closes the gate; no timer grants authority.
const publicationFDEnv = "CODEX_SESSION_RELAY_PUBLICATION_FD"

func duplicateFile(fd int) (*os.File, error) {
	copy, err := unix.Dup(fd)
	if err != nil {
		return nil, err
	}
	unix.CloseOnExec(copy)
	return os.NewFile(uintptr(copy), "startup-publication"), nil
}

func awaitPublication() error {
	value := os.Getenv(publicationFDEnv)
	if value == "" {
		return nil
	}
	fd, err := strconv.Atoi(value)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), "startup-publication")
	_, err = io.Copy(io.Discard, file)
	return errors.Join(err, file.Close())
}
