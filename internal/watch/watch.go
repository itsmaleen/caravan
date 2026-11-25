// Package watch tells callers that something under a directory changed.
//
// It is a hint, not a record: an event means "rescan now", and callers keep a
// slow periodic rescan as a safety net in case the OS drops events (FSEvents
// and inotify can both coalesce or overflow). Where no native watcher is
// available New returns ErrUnsupported and callers fall back to polling.
package watch

import (
	"errors"
	"time"
)

// ErrUnsupported means this build has no native watcher; poll instead.
var ErrUnsupported = errors.New("watch: native file watching not supported in this build")

// Watcher delivers coalesced change notifications on C. A burst of file
// changes produces at least one, usually a single, value.
type Watcher struct {
	C     <-chan struct{}
	c     chan struct{}
	close func() error
}

// Close stops the watcher.
func (w *Watcher) Close() error {
	if w == nil || w.close == nil {
		return nil
	}
	return w.close()
}

func newWatcher() *Watcher {
	c := make(chan struct{}, 1)
	return &Watcher{C: c, c: c}
}

// notify sends without blocking: one pending value is enough to mean "rescan".
func (w *Watcher) notify() {
	select {
	case w.c <- struct{}{}:
	default:
	}
}

// Settle waits until no event has arrived for quiet (or max has passed), so a
// burst of writes is picked up by one rescan instead of many.
func Settle(c <-chan struct{}, quiet, max time.Duration) {
	deadline := time.After(max)
	for {
		select {
		case <-c:
		case <-time.After(quiet):
			return
		case <-deadline:
			return
		}
	}
}
