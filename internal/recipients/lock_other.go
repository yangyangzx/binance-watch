//go:build !unix

package recipients

// lockFile 非 unix 平台退化为无操作（收件人主要由本机 Web 面板单进程写入）。
func lockFile(string) (func(), error) {
	return func() {}, nil
}
