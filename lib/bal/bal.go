package bal

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/denarced/balance/lib/shared"
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
	// Adding watches for dirs "." and "src" results in events for files "./main.c" and
	// "src/main.c". It makes it really confusing to create include and exclude rules so the name is
	// canonicalized here. I'm guessing most people don't want to keep adding the obvious "./" at
	// beginning of every glob. It would be different if glob "main.c" would match to both
	// "./main.c" and "src/main.c" and "./main.c" only to "./main.c" but that's not the case.
	// Without this fix, "main.c" wouldn't match to anything while '**/main.c" would. The outcome
	// would be that every glob would be either "./some" or "**/some". I.e. the maximum possible
	// hassle.
	return EventWrapper{
		Name:   strings.TrimPrefix(event.Name, "./"),
		Kind:   toEventType(event),
		Exists: exists,
		Dir:    dir,
	}, nil
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
	deltas          []time.Duration
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
	delta := v.recordDelta(curr, fmt.Sprintf("%+v", event))
	if len(v.events) > 0 && delta.Milliseconds() < v.debounceDelayMs {
		v.deltas = append(v.deltas, delta)
	}
	v.events = append(v.events, event)
	v.prev = curr
}

func (v *loop) recordDelta(now time.Time, message string) time.Duration {
	delta := now.Sub(v.prev)
	if shared.IsDebugEnabled() {
		message := fmt.Sprintf(
			"%12s % 5d %s",
			now.Format("04:05.000000"),
			delta.Milliseconds(),
			message,
		)
		shared.Logger.Debug(message)
	}
	return delta
}

func (v *loop) sync() error {
	if len(v.events) == 0 {
		return nil
	}
	for _, each := range v.events {
		if each.Kind != EventTypeCreate {
			continue
		}
		if !each.Exists || !each.Dir {
			continue
		}

		w := each
		w.Name += "/"
		if v.shouldIgnore(w) {
			shared.Logger.Debug("Skip dir.", "dir", each.Name)
			continue
		}
		shared.Logger.Debug("Add dir.", "dir", each.Name)

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
	curr := time.Now()
	if shared.IsDebugEnabled() {
		v.recordDelta(curr, fmt.Sprintf("sync %d", len(filtered)))
	}
	v.syncCh <- createSyncEvent(filtered)
	v.events = nil
	v.prev = curr
	if shared.IsDebugEnabled() {
		optimal := deriveOptimalDelay(v.deltas)
		millis := gent.Map(v.deltas, func(d time.Duration) int64 {
			return d.Milliseconds()
		})
		shared.Logger.Debug("Optimal delay calculated.",
			"optimal", optimal.Milliseconds(),
			"min", deriveMin(millis),
			"max", deriveMax(millis),
			"avg", deriveAverage(millis),
			"std", deriveStd(millis),
			"count", len(millis))
	}
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

func deriveOptimalDelay(deltas []time.Duration) time.Duration {
	if len(deltas) < 50 {
		return 0
	}
	slices.Sort(deltas)
	count := len(deltas)
	index := count*1000/100*95/1000 - 1
	return deltas[index]
}

func deriveMin(s []int64) int64 {
	var minimum int64
	for _, each := range s {
		minimum = min(minimum, each)
	}
	return minimum
}

func deriveMax(s []int64) int64 {
	var maximus int64
	for _, each := range s {
		maximus = max(maximus, each)
	}
	return maximus
}

func deriveAverage(s []int64) float64 {
	if len(s) == 0 {
		return 0
	}
	var count int
	var total float64
	for _, each := range s {
		count++
		total += float64(each)
	}
	return total / float64(count)
}

func deriveStd(s []int64) float64 {
	n := len(s)
	if n == 0 || n == 1 {
		return 0.0
	}

	sumSqDiff := deriveSquareDifferenceSum(s)
	variance := sumSqDiff / float64(n)
	return math.Sqrt(variance)
}

func deriveSum(s []int64) int64 {
	var sum int64
	for _, val := range s {
		sum += val
		if sum < 0 {
			panic("int64 overflow: impressive!")
		}
	}
	return sum
}

func deriveSquareDifferenceSum(s []int64) float64 {
	mean := float64(deriveSum(s)) / float64(len(s))
	var sum float64
	for _, val := range s {
		diff := float64(val) - mean
		sum += diff * diff
	}
	return sum
}
