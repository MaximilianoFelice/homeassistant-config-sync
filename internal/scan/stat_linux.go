//go:build linux

package scan

import (
	"io/fs"
	"syscall"
)

// sysStat devuelve ctime e inode. ctime cambia con cualquier escritura o
// chmod, aunque alguien restaure el mtime a mano: por eso entra en la clave
// de la cache.
func sysStat(fi fs.FileInfo) (ctimeNs int64, ino uint64) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0
	}
	return st.Ctim.Sec*1e9 + st.Ctim.Nsec, st.Ino
}
