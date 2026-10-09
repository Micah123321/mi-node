//go:build linux

package updateagent

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func syncDir(path string) error {
	f, e := os.Open(path)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}
func secureFile(path string) error {
	s, e := os.Lstat(path)
	if e != nil {
		return e
	}
	st, ok := s.Sys().(*syscall.Stat_t)
	if !ok || st.Uid != 0 || !s.Mode().IsRegular() || s.Mode().Perm() != 0600 {
		return fmt.Errorf("root-owned regular 0600 file required")
	}
	return nil
}
func Lock(binary string) (func(), error) {
	canonical, e := filepath.EvalSymlinks(binary)
	if e != nil {
		return nil, e
	}
	key := fmt.Sprintf("%x", sha256.Sum256([]byte(canonical)))
	f, e := os.OpenFile(filepath.Join(filepath.Dir(canonical), ".mi-node-update-"+key+".lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if e != nil {
		return nil, e
	}
	if e = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); e != nil {
		f.Close()
		return nil, fmt.Errorf("update already running: %w", e)
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); _ = f.Close() }, nil
}
