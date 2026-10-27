//go:build !windows

package main

import (
	"os"
	"syscall"
)

type exportLock struct{ file *os.File }

func lockExport(path string) (*exportLock, error) {
	f, err := openExportLock(path)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, err
	}
	return &exportLock{f}, nil
}
func (l *exportLock) Close() error                 { return l.file.Close() }
func replaceExportState(source, dest string) error { return os.Rename(source, dest) }
func syncExportDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}
