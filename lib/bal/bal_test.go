package bal

import (
	"fmt"
	"testing"
	"time"

	"github.com/denarced/balance/lib/shared"
	"github.com/stretchr/testify/require"
)

func TestDropEvents(t *testing.T) {
	for i, tt := range []struct {
		name     string
		events   []EventWrapper
		expected []int
	}{
		{
			name:     "empty",
			events:   nil,
			expected: nil,
		},
		{
			name: "drop middle",
			events: []EventWrapper{
				{Name: "./todo.txt", Kind: EventTypeChmod},
				{Name: "./12", Kind: EventTypeCreate},
				{Name: "./12", Kind: EventTypeWrite},
				{Name: "./12", Kind: EventTypeDelete},
				{Name: "./main.go", Kind: EventTypeDelete},
			},
			expected: []int{0, 4},
		},
		{
			name: "multiple creates",
			events: []EventWrapper{
				{Name: "./1", Kind: EventTypeWrite},
				{Name: "./2", Kind: EventTypeCreate},
				{Name: "./2", Kind: EventTypeCreate},
				{Name: "./2", Kind: EventTypeDelete},
			},
			expected: []int{0},
		},
	} {
		t.Run(fmt.Sprintf("%d %s", i, tt.name), func(t *testing.T) {
			req := require.New(t)
			shared.InitTestLogger(t)

			expected := pick(tt.events, tt.expected...)
			filtered := dropEvents(tt.events)
			req.Equal(expected, filtered)
		})
	}
}

func pick[T any](s []T, indexes ...int) []T {
	if len(indexes) == 0 {
		return nil
	}
	result := make([]T, 0, len(indexes))
	for _, idx := range indexes {
		result = append(result, s[idx])
	}
	return result
}

func TestEventTypeToString(t *testing.T) {
	for i, tt := range []struct {
		event    EventType
		expected string
	}{
		{event: EventTypeFirst - 1, expected: "UNKNOWN"},
		{event: EventTypeFirst, expected: "CREATE"},
		{event: EventTypeCreate, expected: "CREATE"},
		{event: EventTypeWrite, expected: "WRITE"},
		{event: EventTypeDelete, expected: "DELETE"},
		{event: EventTypeRename, expected: "RENAME"},
		{event: EventTypeChmod, expected: "CHMOD"},
		{event: EventTypeLast, expected: "UNKNOWN"},
		{event: EventTypeLast + 1, expected: "UNKNOWN"},
	} {
		t.Run(fmt.Sprintf("%d %s", i, tt.expected), func(t *testing.T) {
			require.Equal(t, tt.expected, tt.event.String())
		})
	}
}

func TestDeriveOptimalDelay(t *testing.T) {
	var tests = []struct {
		name     string
		expected int64
		deltas   []time.Duration
	}{
		{"nil", 0, nil},
		{"empty", 0, []time.Duration{}},
		{"not enough", 0, make([]time.Duration, 49)},
		{"1 to 100", 95, shift(generateRange(1, 101))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			shared.InitTestLogger(t)
			require.Equal(
				t,
				time.Duration(tt.expected)*time.Millisecond,
				deriveOptimalDelay(tt.deltas),
			)
		})
	}
}

func generateRange(startInclusive, endExclusive int64) (durations []time.Duration) {
	for i := startInclusive; i < endExclusive; i++ {
		durations = append(durations, time.Duration(i)*time.Millisecond)
	}
	return
}

func shift[T any](s []T) []T {
	if len(s) == 0 {
		return s
	}
	return append([]T{s[len(s)-1]}, s[0:len(s)-1]...)
}

func TestDeriveStd(t *testing.T) {
	shared.InitTestLogger(t)
	require.InDelta(
		t,
		111.803399,
		deriveStd([]int64{100, 200, 300, 400}),
		0.000001)
}
