//go:build !linux

package scan

import "io/fs"

// sysStat has no portable ctime or inode outside Linux. The engine runs on
// Linux (the app and locally); this only exists so it compiles elsewhere.
func sysStat(fs.FileInfo) (ctimeNs int64, ino uint64) { return 0, 0 }
