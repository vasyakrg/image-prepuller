//go:build unix

package agent

import "syscall"

// osStatFS reports capacity and available bytes of the filesystem holding
// path. The CRI ImageFsInfo call only exposes usage, so capacity comes from
// statfs; an error is reported as zeroes and disables the disk guard.
func osStatFS(path string) (capacity, available int64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0
	}
	blockSize := uint64(st.Bsize)
	return int64(st.Blocks * blockSize), int64(st.Bavail * blockSize)
}
