package role

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// dispatchProcessStart is the start time of process pid as the owner record keeps it: the starttime of
// /proc/<pid>/stat (field 22, clock ticks since boot), as text. It only distinguishes two processes that
// had the same pid, so it is compared for equality and never read as a time.
func dispatchProcessStart(pid int) (string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return "", err
	}
	// The command name (field 2) sits in parentheses and may hold spaces or a ')', so the fields count from the last ')'.
	rest := string(data)
	i := strings.LastIndexByte(rest, ')')
	if i < 0 {
		return "", errors.New("malformed /proc stat")
	}
	fields := strings.Fields(rest[i+1:]) // fields[0] is field 3, the state
	if len(fields) < 20 {
		return "", errors.New("malformed /proc stat")
	}
	return fields[19], nil
}
