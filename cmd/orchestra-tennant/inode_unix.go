//go:build unix

package main

import (
	"os"
	"syscall"
)

func inode(f *os.File) uint64 {
	info, err := f.Stat()
	if err != nil {
		return 0
	}
	return inodeOf(info)
}

func inodeOf(info os.FileInfo) uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino)
	}
	return 0
}
