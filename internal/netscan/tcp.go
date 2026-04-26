package netscan

import (
	"errors"
	"syscall"
)

// isConnRefused reports whether err is a TCP "connection refused" — meaning
// the destination host is up and actively rejecting the port. Treated as
// proof of presence by the scanner, on equal footing with a successful
// connect.
//
// Other dial errors (timeout, no route, host unreachable, network
// unreachable) intentionally don't count: they could mean the IP is
// genuinely unused or just that something is filtering us. The ARP cache
// read step (Linux-only) catches the silent-but-present case.
func isConnRefused(err error) bool {
	return errors.Is(err, syscall.ECONNREFUSED)
}
