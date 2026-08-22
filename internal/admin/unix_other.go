//go:build !windows

package admin

// UnixSupported reports whether a unix-socket admin listener is available.
const UnixSupported = true

const unixUnsupportedReason = "" // unreachable; see unix_windows.go
