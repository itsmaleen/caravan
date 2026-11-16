//go:build linux

package watch

import (
	"io/fs"
	"os"
	"path/filepath"
	"sync"
	"unsafe"

	"golang.org/x/sys/unix"
)

const mask = unix.IN_CREATE | unix.IN_DELETE | unix.IN_MODIFY | unix.IN_CLOSE_WRITE |
	unix.IN_MOVED_FROM | unix.IN_MOVED_TO | unix.IN_ATTRIB | unix.IN_DELETE_SELF | unix.IN_MOVE_SELF

// New watches root recursively with inotify, one watch per directory. skip
// (relative path, isDir) keeps excluded directories such as node_modules from
// using up the per-user watch limit.
func New(root string, skip func(rel string, isDir bool) bool) (*Watcher, error) {
	fd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), "inotify") // non-blocking: Close unblocks Read
	w := newWatcher()
	var mu sync.Mutex
	paths := map[int32]string{}

	addTree := func(dir string) {
		_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil || !d.IsDir() {
				return nil
			}
			if rel, _ := filepath.Rel(root, p); rel != "." && skip != nil && skip(rel, true) {
				return filepath.SkipDir
			}
			wd, err := unix.InotifyAddWatch(fd, p, mask)
			if err != nil {
				return nil // unreadable dir or watch limit: the safety rescan still covers it
			}
			mu.Lock()
			paths[int32(wd)] = p
			mu.Unlock()
			return nil
		})
	}
	addTree(root)

	go func() {
		buf := make([]byte, 64*1024)
		for {
			n, err := f.Read(buf)
			if err != nil {
				return
			}
			for off := 0; off+unix.SizeofInotifyEvent <= n; {
				ev := (*unix.InotifyEvent)(unsafe.Pointer(&buf[off]))
				nameLen := int(ev.Len)
				if ev.Mask&unix.IN_ISDIR != 0 && ev.Mask&(unix.IN_CREATE|unix.IN_MOVED_TO) != 0 && nameLen > 0 {
					raw := buf[off+unix.SizeofInotifyEvent : off+unix.SizeofInotifyEvent+nameLen]
					name := string(raw[:clen(raw)])
					mu.Lock()
					parent := paths[ev.Wd]
					mu.Unlock()
					if parent != "" {
						addTree(filepath.Join(parent, name))
					}
				}
				off += unix.SizeofInotifyEvent + nameLen
			}
			w.notify()
		}
	}()
	var once sync.Once
	w.close = func() error {
		var err error
		once.Do(func() { err = f.Close() })
		return err
	}
	return w, nil
}

func clen(b []byte) int {
	for i, c := range b {
		if c == 0 {
			return i
		}
	}
	return len(b)
}
