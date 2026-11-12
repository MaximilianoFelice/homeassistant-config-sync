//go:build linux

package scan

import (
	"io/fs"
	"syscall"
)

// sysStat returns ctime and inode. ctime changes on any write or chmod, even
// if someone restores the mtime by hand: that is why it is part of the cache
// key.
func sysStat(fi fs.FileInfo) (ctimeNs int64, ino uint64) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0
	}
	return st.Ctim.Sec*1e9 + st.Ctim.Nsec, st.Ino
}
