//go:build windows

package udpio

import (
	"errors"

	"golang.org/x/sys/windows"
)

// tooLong reports whether a read failed because the datagram was longer than the
// buffer. Windows discards the excess, as Linux and macOS do, but reports WSAEMSGSIZE
// and no source address instead of returning the truncated datagram.
func tooLong(err error) bool { return errors.Is(err, windows.WSAEMSGSIZE) }
