package main

import (
	"os"
	"syscall"
	"unsafe"
)

var exportKernel = syscall.NewLazyDLL("kernel32.dll")
var exportLockFile = exportKernel.NewProc("LockFileEx")
var exportMoveFile = exportKernel.NewProc("MoveFileExW")

type exportLock struct {
	file       *os.File
	overlapped syscall.Overlapped
}

func lockExport(path string) (*exportLock, error) {
	f, err := openExportLock(path)
	if err != nil {
		return nil, err
	}
	l := &exportLock{file: f}
	ok, _, callErr := exportLockFile.Call(f.Fd(), 3, 0, 1, 0, uintptr(unsafe.Pointer(&l.overlapped)))
	if ok == 0 {
		f.Close()
		return nil, callErr
	}
	return l, nil
}
func (l *exportLock) Close() error { return l.file.Close() }
func replaceExportState(source, dest string) error {
	from, err := syscall.UTF16PtrFromString(source)
	if err != nil {
		return err
	}
	to, err := syscall.UTF16PtrFromString(dest)
	if err != nil {
		return err
	}
	ok, _, callErr := exportMoveFile.Call(uintptr(unsafe.Pointer(from)), uintptr(unsafe.Pointer(to)), 1|8)
	if ok == 0 {
		return callErr
	}
	return nil
}

// MoveFileExW uses WRITE_THROUGH; Windows does not provide POSIX directory fsync.
func syncExportDir(string) error { return nil }
