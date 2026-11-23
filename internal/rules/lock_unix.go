//go:build unix

package rules

import (
	"os"
	"syscall"
)

// lockFile 对 path 加独占 flock，返回解锁函数。
// CLI 与 daemon 是两个进程，读-改-写必须串行，否则会互相覆盖（lost update）。
func lockFile(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		_ = f.Close()
		return nil, err
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
