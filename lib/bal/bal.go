package bal

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"time"

	"github.com/denarced/gent"
	"github.com/fsnotify/fsnotify"
)

const (
	errCodeUnknown = iota + 10
	errCodeInitialDirRead
	errCodeNewWatcher
	errCodeAddWatcher
	errCodeStat
	errCodeWatcherErr
)

type EventType int

const (
	EventTypeCreate = iota
	EventTypeWrite
	EventTypeDelete
	EventTypeRename
	EventTypeChmod
	EventTypeLast
	EventTypeFirst = EventTypeCreate
)

func (v EventType) String() string {
	switch v {
	case EventTypeCreate:
		return "CREATE"
	case EventTypeWrite:
		return "WRITE"
	case EventTypeDelete:
		return "DELETE"
	case EventTypeRename:
		return "RENAME"
	case EventTypeChmod:
		return "CHMOD"
	default:
		return "UNKNOWN"
	}
}

// EventWrapper is a wrapper for fsnotify.Event.
type EventWrapper struct {
	Name   string
	Kind   EventType
	Exists bool
	Dir    bool
}

func toEventWrapper(event fsnotify.Event) (wrapper EventWrapper, err error) {
	stat, exists, err := runStat(event.Name)
	if err != nil {
		return
	}
	dir := false
	if stat != nil {
		dir = stat.IsDir()
	}
	return EventWrapper{Name: event.Name, Kind: toEventType(event), Exists: exists, Dir: dir}, nil
}

func toEventType(event fsnotify.Event) EventType {
	if event.Has(fsnotify.Create) {
		return EventTypeCreate
	}
	if event.Has(fsnotify.Remove) {
		return EventTypeDelete
	}
	if event.Has(fsnotify.Chmod) {
		return EventTypeChmod
	}
	if event.Has(fsnotify.Write) {
		return EventTypeWrite
	}
	if event.Has(fsnotify.Rename) {
		return EventTypeRename
	}
	panic(fmt.Sprintf("unknown event type: %s", event))
}

type loop struct {
	prev            time.Time
	sig             <-chan os.Signal
	events          []EventWrapper
	watcher         *fsnotify.Watcher
	shouldIgnore    func(event EventWrapper) bool
	syncCh          chan<- SyncEvent
	debounceDelayMs int64
}

// Balance starts to watch filesystem events.
// Quit watching events when a message is received from sig channel.
// Prepare watcher before calling this. Watched directories must have been already added.
// ShouldIgnore is a callback to decide whether any given file in filesystem event should be
// ignored. That is, whether change events to the file should be completely ignored, as if they
// never happened. If it's nil, nothing is ignored.
// Message is sent to syncCh channel when it seems that filesystem events have ended for now. Then
// caller can perform whatever they wish.
func Balance(
	sig <-chan os.Signal,
	watcher *fsnotify.Watcher,
	shouldIgnore func(event EventWrapper) bool,
	syncCh chan<- SyncEvent,
	debounceDelayMs int64,
) error {
	return (&loop{
		prev:            time.Now(),
		sig:             sig,
		watcher:         watcher,
		shouldIgnore:    shouldIgnore,
		syncCh:          syncCh,
		debounceDelayMs: debounceDelayMs,
	}).run()
}

func (v *loop) run() error {
	for {
		select {
		case event, ok := <-v.watcher.Events:
			if !ok {
				return nil
			}
			wrapper, err := toEventWrapper(event)
			if err != nil {
				return err
			}
			v.handleEvent(wrapper)
		case err := <-v.watcher.Errors:
			return NewErrorCode(fmt.Errorf("fsnotify error - %w", err), errCodeWatcherErr)
		case <-v.sig:
			return nil
		case <-time.After(time.Millisecond * time.Duration(v.debounceDelayMs)):
			if err := v.sync(); err != nil {
				return err
			}
		}
	}
}

func (v *loop) handleEvent(event EventWrapper) {
	if v.shouldIgnore != nil && !event.Dir && v.shouldIgnore(event) {
		return
	}
	curr := time.Now()
	// delta := curr.Sub(v.prev)
	// fmt.Printf("%12s % 5d %+v\n", curr.Format("04:05.000000"), delta.Milliseconds(), event)
	v.events = append(v.events, event)
	v.prev = curr
}

func (v *loop) sync() error {
	for _, each := range v.events {
		if each.Kind != EventTypeCreate {
			continue
		}
		if !each.Exists || !each.Dir {
			continue
		}

		if err := v.watcher.Add(each.Name); err != nil {
			return NewErrorCode(
				fmt.Errorf("failed to add fsnotify watcher - %w", err),
				errCodeAddWatcher,
			)
		}
	}
	// Dirs were only needed for adding new watches so drop them now.
	v.events = gent.Filter(v.events, func(event EventWrapper) bool {
		return !event.Dir
	})
	filtered := dropEvents(v.events)
	if len(filtered) == 0 {
		return nil
	}
	// curr := time.Now()
	// fmt.Printf("%12s %s %d\n", curr.Format("04:05.000000"), "sync", len(filtered))
	v.syncCh <- createSyncEvent(filtered)
	v.events = nil
	return nil
}

func createSyncEvent(filtered []EventWrapper) SyncEvent {
	filepathSet := gent.NewSet[string]()
	for _, each := range filtered {
		filepathSet.Add(each.Name)
	}
	filepaths := filepathSet.ToSlice()
	sort.Strings(filepaths)
	return SyncEvent{
		Files:         filepaths,
		EventWrappers: filtered,
	}
}

func dropEvents(events []EventWrapper) []EventWrapper {
	type indexed struct {
		index int
		event EventWrapper
	}
	nameToEvent := map[string][]indexed{}
	for i, each := range events {
		named := nameToEvent[each.Name]
		if named == nil {
			named = []indexed{{i, each}}
		} else {
			named = append(named, indexed{i, each})
		}
		nameToEvent[each.Name] = named
	}

	var dropped []int
	for _, ind := range nameToEvent {
		createIndex := -1
		for _, each := range ind {
			if createIndex < 0 && each.event.Kind == EventTypeCreate {
				createIndex = each.index
			} else if each.event.Kind == EventTypeDelete && createIndex >= 0 {
				// Momentarily existing file found, can be ignored, came and went.
				for i := createIndex; i <= each.index; i++ {
					dropped = append(dropped, i)
				}
				createIndex = -1
			}
		}
	}

	if len(dropped) == 0 {
		return events
	}
	for i := len(dropped) - 1; i >= 0; i-- {
		index := dropped[i]
		events = slices.Delete(events, index, index+1)
	}
	if len(events) == 0 {
		return nil
	}
	return events
}

func runStat(filep string) (stat os.FileInfo, exists bool, err error) {
	stat, err = os.Stat(filep)
	if err == nil {
		exists = true
		return
	}
	if os.IsNotExist(err) {
		err = nil
		return
	}
	err = NewErrorCode(fmt.Errorf("stat failed - %w", err), errCodeStat)
	return
}

type ErrorCode struct {
	Err  error
	Code int
}

func (v *ErrorCode) Error() string {
	return v.Err.Error()
}

func NewErrorCode(err error, code int) *ErrorCode {
	return &ErrorCode{Err: err, Code: code}
}

// FindFileAbove finds a file in current working directory or in parent up until home directory.
func FindFileAbove(filen string) (string, error) {
	home, homeErr := os.UserHomeDir()
	wd, wdErr := os.Getwd()
	if wdErr != nil {
		return "", wdErr
	}

	// There needs to be some kind of a limit to avoid an eternal loop.
	for range 100 {
		filep := filepath.Join(wd, filen)
		_, err := os.Stat(filep)
		if err == nil {
			return filep, nil
		}
		if homeErr == nil && wd == home {
			break
		}
		parent := filepath.Dir(wd)
		if wd == parent {
			break
		}
		wd = parent
	}
	return "", nil
}

type SyncEvent struct {
	// Files that changed in some way.
	Files         []string
	EventWrappers []EventWrapper
}
