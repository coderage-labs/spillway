//go:build linux

package mitm

import "os"

// linuxCABundlePaths are the well-known system CA bundle file locations
// across the distros likely to run spillway, checked in order. This is the
// same list (nearly) every language runtime's own "find the system CA
// bundle" fallback carries, because there is no single standard location —
// Go's own crypto/x509/root_linux.go uses an equivalent set.
var linuxCABundlePaths = []string{
	"/etc/ssl/certs/ca-certificates.crt",     // Debian/Ubuntu/Gentoo
	"/etc/pki/tls/certs/ca-bundle.crt",       // Fedora/RHEL/CentOS
	"/etc/ssl/ca-bundle.pem",                 // OpenSUSE
	"/etc/pki/tls/cacert.pem",                // OpenELEC
	"/etc/ssl/cert.pem",                      // Alpine, FreeBSD-style
	"/usr/local/share/certs/ca-root-nss.crt", // FreeBSD base (in case of a linux-compat build)
}

// systemRootsPEMLinux reads the first well-known bundle file that exists and
// is non-empty. Unlike darwin there is no single privileged API to shell
// out to — every distro ships its roots as a plain file, just at a
// distro-specific path — so this is a search, not a command.
func systemRootsPEMLinux() ([]byte, string, bool) {
	for _, p := range linuxCABundlePaths {
		b, err := os.ReadFile(p)
		if err == nil && len(b) > 0 {
			return b, "", true
		}
	}
	return nil, "no known system CA bundle file found (checked " +
		joinPaths(linuxCABundlePaths) + ")", false
}

func joinPaths(paths []string) string {
	out := ""
	for i, p := range paths {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}

func init() { systemRootsPEM = systemRootsPEMLinux }
