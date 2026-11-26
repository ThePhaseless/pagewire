package watch

import "golang.org/x/sys/unix"

// remoteFS reports whether dir sits on a filesystem where inotify misses
// changes made by other hosts or by a FUSE daemon.
// shortcut: only NFS, SMB2, CIFS, FUSE, 9P and Ceph are listed; another
// remote filesystem gets no resync, so add its magic here when one shows up.
func remoteFS(dir string) bool {
	var st unix.Statfs_t
	if unix.Statfs(dir, &st) != nil {
		return false
	}
	switch uint32(st.Type) { //nolint:gosec // f_type is a 32-bit magic, signed on some arches
	case unix.NFS_SUPER_MAGIC, unix.SMB2_SUPER_MAGIC, unix.CIFS_SUPER_MAGIC,
		unix.FUSE_SUPER_MAGIC, unix.V9FS_MAGIC, unix.CEPH_SUPER_MAGIC:
		return true
	}
	return false
}
