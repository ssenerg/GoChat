package drpc

import (
	"testing"

	ierrors "github.com/ssenerg/gochat/internal/errors"
)

func TestByteRange(t *testing.T) {
	cases := []struct {
		offset, length, size int64
		wantOff, wantLen     int64
		some                 bool
	}{
		{0, 0, 100, 0, 0, true},     // the whole file
		{0, 100, 100, 0, 0, true},   // the whole file, asked for by length
		{0, 2, 100, 0, 2, true},     // Safari's first probe
		{10, 0, 100, 10, 90, true},  // to the end
		{90, 50, 100, 90, 10, true}, // a length past the end is cut
		{100, 0, 100, 0, 0, false},  // past the end
		{0, 0, 0, 0, 0, false},      // an empty file
	}
	for _, c := range cases {
		off, n, some, err := byteRange(c.offset, c.length, c.size)
		if err != nil || off != c.wantOff || n != c.wantLen || some != c.some {
			t.Errorf("byteRange(%d, %d, %d) = %d, %d, %v, %v", c.offset, c.length, c.size, off, n, some, err)
		}
	}
	if _, _, _, err := byteRange(-1, 0, 10); !ierrors.Is(err, ierrors.CodeInvalidArgument) {
		t.Errorf("a negative offset: %v", err)
	}
}
