//go:build !linux

package collector

// 探针的目标平台是 Linux VPS。非 Linux 上留一个空实现，
// 只是为了让 go vet / go test ./... 在开发机上也能跑通。
func readDisk(path string) (total, used int64, err error) {
	return 0, 0, nil
}
