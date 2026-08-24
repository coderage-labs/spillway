//go:build darwin

package mitm

import "os/exec"

// darwinRootKeychains are dumped in order and concatenated: the read-only
// system root store first, then the (usually empty, but user- or
// MDM-managed) admin-added roots in the System keychain. Missing or
// unreadable keychains are skipped rather than failing the whole call — an
// empty result from ALL of them is what actually means "unavailable".
var darwinRootKeychains = []string{
	"/System/Library/Keychains/SystemRootCertificates.keychain",
	"/Library/Keychains/System.keychain",
}

// systemRootsPEMDarwin shells out to the `security` CLI, the only way to
// reach the Security framework's root store as PEM bytes: it is not a file
// on disk, and Go's crypto/x509 on darwin talks to the framework internally
// without exposing enumeration (see the package doc). `security
// find-certificate -a -p <keychain>` is the standard, documented way other
// tools (Homebrew's ca-certificates formula among them) get at this.
func systemRootsPEMDarwin() ([]byte, string, bool) {
	var out []byte
	for _, keychain := range darwinRootKeychains {
		b, err := exec.Command("security", "find-certificate", "-a", "-p", keychain).Output()
		if err != nil {
			continue // this keychain may not exist or be readable; try the rest
		}
		out = append(out, b...)
	}
	if len(out) == 0 {
		return nil, "`security find-certificate` returned no certificates from any known keychain", false
	}
	return out, "", true
}

func init() { systemRootsPEM = systemRootsPEMDarwin }
