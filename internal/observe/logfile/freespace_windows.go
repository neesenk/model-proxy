//go:build windows

package logfile

import "golang.org/x/sys/windows"

// volumeFreeSpace reports the bytes available to the calling user and the
// total capacity of the volume holding dir. GetDiskFreeSpaceExW accepts any
// path on the volume, so dir itself identifies it.
func volumeFreeSpace(dir string) (free, total int64, err error) {
	path, err := windows.UTF16PtrFromString(dir)
	if err != nil {
		return 0, 0, err
	}
	var freeU, totalU uint64
	if err := windows.GetDiskFreeSpaceEx(path, &freeU, &totalU, nil); err != nil {
		return 0, 0, err
	}
	return int64(freeU), int64(totalU), nil
}
