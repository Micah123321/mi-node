//go:build !windows

package agent

import (
	"errors"
	"golang.org/x/sys/unix"
	"os"
)

func lockState(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, errors.New("open state lock failed")
	}
	if unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB) != nil {
		f.Close()
		return nil, errors.New("state directory is in use")
	}
	return f, nil
}
func syncStateDir(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return errors.New("open state directory for sync failed")
	}
	defer f.Close()
	if f.Sync() != nil {
		return errors.New("sync state directory failed")
	}
	return nil
}
