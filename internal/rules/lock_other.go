//go:build !unix

package rules

// 非 unix 平台：不做跨进程锁（进程内仍由 Store.mu 保护）。
func lockFile(string) (func(), error) { return func() {}, nil }
