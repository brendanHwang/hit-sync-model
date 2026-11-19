package main

import (
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

var lockFileEx = syscall.NewLazyDLL("kernel32.dll").NewProc("LockFileEx")

func lockFile(file *os.File) error {
	const failImmediately = 0x1
	const exclusiveLock = 0x2
	var offset syscall.Overlapped
	result, _, err := lockFileEx.Call(file.Fd(), failImmediately|exclusiveLock, 0, 1, 0, uintptr(unsafe.Pointer(&offset)))
	runtime.KeepAlive(file)
	if result == 0 {
		return err
	}
	return nil
}
