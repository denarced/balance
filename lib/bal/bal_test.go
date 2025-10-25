package bal

import (
	"fmt"
	"testing"

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
