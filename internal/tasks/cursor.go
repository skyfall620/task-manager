package tasks

import (
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"time"
)

type listCursor struct {
	CreatedAt time.Time
	ID        uint64
}

func encodeListCursor(task Task) string {
	var data [16]byte
	binary.BigEndian.PutUint64(data[:8], uint64(task.CreatedAt.UTC().UnixMicro()))
	binary.BigEndian.PutUint64(data[8:], task.ID)
	return base64.RawURLEncoding.EncodeToString(data[:])
}

func decodeListCursor(value string) (*listCursor, error) {
	if len(value) != 22 {
		return nil, fmt.Errorf("%w: invalid cursor", ErrInvalidInput)
	}

	data, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil || len(data) != 16 {
		return nil, fmt.Errorf("%w: invalid cursor", ErrInvalidInput)
	}

	cursor := &listCursor{
		CreatedAt: time.UnixMicro(int64(binary.BigEndian.Uint64(data[:8]))).UTC(),
		ID:        binary.BigEndian.Uint64(data[8:]),
	}
	if !validListCursor(cursor) {
		return nil, fmt.Errorf("%w: invalid cursor", ErrInvalidInput)
	}

	return cursor, nil
}

func validListCursor(cursor *listCursor) bool {
	return cursor.ID > 0 && cursor.CreatedAt.Year() >= 1000 && cursor.CreatedAt.Year() <= 9999 &&
		cursor.CreatedAt.Equal(cursor.CreatedAt.Truncate(time.Microsecond))
}

func taskBeforeCursor(task Task, cursor *listCursor) bool {
	return task.CreatedAt.Before(cursor.CreatedAt) || (task.CreatedAt.Equal(cursor.CreatedAt) && task.ID < cursor.ID)
}
