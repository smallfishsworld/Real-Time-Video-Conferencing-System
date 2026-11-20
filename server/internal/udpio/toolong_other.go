//go:build !windows

package udpio

// tooLong is always false here: a datagram longer than the buffer is returned
// truncated to it, without an error.
func tooLong(error) bool { return false }
