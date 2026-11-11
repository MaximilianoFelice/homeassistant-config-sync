//go:build !linux

package scan

import "io/fs"

// sysStat no tiene ctime ni inode portables fuera de Linux. El motor corre
// en Linux (la app y local); esto solo existe para que compile en otros lados.
func sysStat(fs.FileInfo) (ctimeNs int64, ino uint64) { return 0, 0 }
