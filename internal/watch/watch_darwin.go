//go:build darwin && cgo

package watch

/*
#cgo LDFLAGS: -framework CoreServices
#include <CoreServices/CoreServices.h>
#include <unistd.h>
#include <stdint.h>

// The callback only writes a byte to a non-blocking pipe; Go reads the other
// end. A full pipe drops the byte, which is fine: one pending byte already
// means "rescan".
static void caravan_fse_cb(ConstFSEventStreamRef s, void *info, size_t n, void *paths,
                           const FSEventStreamEventFlags flags[], const FSEventStreamEventId ids[]) {
	char b = 1;
	(void)write((int)(intptr_t)info, &b, 1);
}

typedef struct {
	FSEventStreamRef stream;
	dispatch_queue_t queue;
} caravan_fse;

// Fills *w and returns 1, or returns 0 with nothing left to release.
static int caravan_fse_start(const char *path, int fd, double latency, caravan_fse *w) {
	CFStringRef p = CFStringCreateWithCString(NULL, path, kCFStringEncodingUTF8);
	CFArrayRef paths = CFArrayCreate(NULL, (const void **)&p, 1, &kCFTypeArrayCallBacks);
	FSEventStreamContext ctx = {0, (void *)(intptr_t)fd, NULL, NULL, NULL};
	FSEventStreamRef s = FSEventStreamCreate(NULL, caravan_fse_cb, &ctx, paths,
		kFSEventStreamEventIdSinceNow, latency,
		kFSEventStreamCreateFlagFileEvents | kFSEventStreamCreateFlagNoDefer);
	CFRelease(paths);
	CFRelease(p);
	if (s == NULL) return 0;
	// dispatch_queue_create returns a reference we own; it is released once
	// the stream no longer uses the queue.
	dispatch_queue_t q = dispatch_queue_create("caravan.watch", NULL);
	FSEventStreamSetDispatchQueue(s, q);
	if (!FSEventStreamStart(s)) {
		FSEventStreamInvalidate(s);
		FSEventStreamRelease(s);
		dispatch_release(q);
		return 0;
	}
	w->stream = s;
	w->queue = q;
	return 1;
}

static void caravan_fse_stop(caravan_fse *w) {
	FSEventStreamStop(w->stream);
	FSEventStreamInvalidate(w->stream);
	FSEventStreamRelease(w->stream);
	dispatch_release(w->queue);
	w->stream = NULL;
	w->queue = NULL;
}
*/
import "C"

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"unsafe"
)

// New watches root recursively with FSEvents. skip is ignored here: FSEvents
// watches a whole tree at the kernel level, so excluded paths cost nothing
// beyond a rescan that finds no change.
func New(root string, skip func(rel string, isDir bool) bool) (*Watcher, error) {
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	r, wp, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	// Take the descriptor once: Fd() puts the file back into blocking mode, so
	// a second call after SetNonblock would undo it, and the callback would
	// block on a full pipe instead of dropping the byte.
	wfd := wp.Fd()
	if err := syscall.SetNonblock(int(wfd), true); err != nil {
		r.Close()
		wp.Close()
		return nil, err
	}
	cpath := C.CString(real)
	defer C.free(unsafe.Pointer(cpath))
	var fse C.caravan_fse
	if C.caravan_fse_start(cpath, C.int(wfd), 0.02, &fse) == 0 {
		r.Close()
		wp.Close()
		return nil, errors.New("watch: FSEventStreamStart failed")
	}
	w := newWatcher()
	go func() {
		buf := make([]byte, 256)
		for {
			if _, err := r.Read(buf); err != nil {
				return
			}
			w.notify()
		}
	}()
	var once sync.Once
	w.close = func() error {
		once.Do(func() {
			C.caravan_fse_stop(&fse)
			wp.Close()
			r.Close()
		})
		return nil
	}
	return w, nil
}
