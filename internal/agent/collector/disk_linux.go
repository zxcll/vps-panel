//go:build linux

package collector

import "golang.org/x/sys/unix"

// readDisk 返回挂载点的总容量和已用容量（字节）。
// 已用按"非 root 用户视角"算：总量减去可用量，包含预留给 root 的那部分，
// 和 df 的 Use% 口径一致。
func readDisk(path string) (total, used int64, err error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, 0, err
	}
	bs := int64(st.Bsize)
	total = int64(st.Blocks) * bs
	avail := int64(st.Bavail) * bs
	used = total - avail
	if used < 0 {
		used = 0
	}
	return total, used, nil
}
