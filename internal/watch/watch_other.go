//go:build !linux && !(darwin && cgo)

package watch

// New is unavailable in this build; callers poll instead.
func New(root string, skip func(rel string, isDir bool) bool) (*Watcher, error) {
	return nil, ErrUnsupported
}
