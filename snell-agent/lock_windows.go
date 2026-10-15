package agent

import (
	"errors"
	"golang.org/x/sys/windows"
	"os"
)

func lockState(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, errors.New("open state lock failed")
	}
	var o windows.Overlapped
	if windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &o) != nil {
		f.Close()
		return nil, errors.New("state directory is in use")
	}
	return f, nil
}
func syncStateDir(string) error { return nil }
