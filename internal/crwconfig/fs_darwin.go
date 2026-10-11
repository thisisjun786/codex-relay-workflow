//go:build darwin

package crwconfig

import "golang.org/x/sys/unix"

// statfsHost reads the file system under path: its name and the space a process that is not
// privileged can still use.
func statfsHost(path string) (FS, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return FS{}, err
	}
	name := make([]byte, 0, len(st.Fstypename))
	for _, c := range st.Fstypename {
		if c == 0 {
			break
		}
		name = append(name, byte(c))
	}
	return FS{Type: string(name), Free: uint64(st.Bavail) * uint64(st.Bsize)}, nil
}
