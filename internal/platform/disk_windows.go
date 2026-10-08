//go:build windows

package platform

import (
	"syscall"
	"unsafe"
)

var getDiskFreeSpaceEx = syscall.NewLazyDLL("kernel32.dll").NewProc("GetDiskFreeSpaceExW")

// FreeBytes returns the free space available to this user on dir's disk.
func FreeBytes(dir string) (int64, error) {
	p, err := syscall.UTF16PtrFromString(dir)
	if err != nil {
		return 0, err
	}
	var avail, total, free uint64
	r, _, err := getDiskFreeSpaceEx.Call(uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&avail)), uintptr(unsafe.Pointer(&total)), uintptr(unsafe.Pointer(&free)))
	if r == 0 {
		return 0, err
	}
	return int64(avail), nil
}

// DiskSpace returns the size of dir's disk and the space free to this user.
func DiskSpace(dir string) (total, free int64, err error) {
	p, err := syscall.UTF16PtrFromString(dir)
	if err != nil {
		return 0, 0, err
	}
	var avail, size, all uint64
	r, _, err := getDiskFreeSpaceEx.Call(uintptr(unsafe.Pointer(p)),
		uintptr(unsafe.Pointer(&avail)), uintptr(unsafe.Pointer(&size)), uintptr(unsafe.Pointer(&all)))
	if r == 0 {
		return 0, 0, err
	}
	return int64(size), int64(avail), nil
}
