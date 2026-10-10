//go:build linux

package crwconfig

import "golang.org/x/sys/unix"

// fsNames are the file systems a Linux statfs magic number names here.
var fsNames = map[int64]string{
	0xEF53:     "ext2/3/4",
	0x58465342: "xfs",
	0x9123683E: "btrfs",
	0x01021994: "tmpfs",
	0x2FC12FC1: "zfs",
	0x6969:     "nfs",
	0xF2F52010: "f2fs",
	0x794C7630: "overlayfs",
	0x4D44:     "vfat",
	0x2011BAB0: "exfat",
	0x65735546: "fuse",
	0xFF534D42: "cifs",
	0xFE534D42: "smb2",
	0x01021997: "9p",
	0x858458F6: "ramfs",
}

// statfsHost reads the file system under path: its name (empty when the type is not one this
// package names) and the space a process that is not privileged can still use.
func statfsHost(path string) (FS, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return FS{}, err
	}
	return FS{Type: fsNames[int64(st.Type)&0xFFFFFFFF], Free: uint64(st.Bavail) * uint64(st.Bsize)}, nil
}
