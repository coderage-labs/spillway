package admin

// UnixSupported reports whether a unix-socket admin listener is available.
//
// False on Windows, deliberately, even though Windows 10+ does implement
// AF_UNIX and Go can listen on it. The reason for offering a socket at all is
// that it can be mode 0600, which puts the admin API — and therefore the
// account list and the settings endpoint — out of reach of every other user
// on the machine. Chmod does not do that on Windows; it toggles a read-only
// bit. The socket would work and the protection would not, which is worse
// than not offering it, because the user would believe they had hardened
// something.
//
// TCP loopback plus an admin token is the Windows path, and it defends
// against the same thing: another user can reach 127.0.0.1 but cannot read a
// token file their ACL denies them.
const UnixSupported = false

// unixUnsupportedReason explains the refusal at the point of failure.
const unixUnsupportedReason = "a unix-socket admin listener is not supported on Windows: " +
	"its protection comes from mode 0600, and Chmod there only toggles a read-only bit. " +
	"Use a loopback address such as 127.0.0.1:7657 with admin.token set"
