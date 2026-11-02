package hostmetrics

import (
	"io/fs"
	"path/filepath"
	"syscall"
)

// DirSize returns the disk space used by everything under path, like `du -s`:
// it counts allocated blocks, counts hard-linked files once, and doesn't
// follow symlinks. Unreadable entries are skipped.
func DirSize(path string) (uint64, error) {
	type inode struct{ dev, ino uint64 }
	seen := map[inode]bool{}
	var total uint64
	err := filepath.WalkDir(path, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			if d == nil {
				return err // path itself is unreadable
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok {
			total += uint64(info.Size())
			return nil
		}
		if st.Nlink > 1 && !d.IsDir() {
			key := inode{uint64(st.Dev), st.Ino}
			if seen[key] {
				return nil
			}
			seen[key] = true
		}
		total += uint64(st.Blocks) * 512
		return nil
	})
	return total, err
}
