package mitm

// Combined CA bundle for non-Node subprocesses (issue #64).
//
// runEnv teaches Node to trust spillway's MITM CA via NODE_EXTRA_CA_CERTS,
// but every other language's TLS stack ignores that variable. Python's ssl
// reads SSL_CERT_FILE/REQUESTS_CA_BUNDLE, curl reads CURL_CA_BUNDLE, and none
// of them know spillway's CA — so a non-Node client inside a proxied CLI's
// process tree (an MCP server, most concretely) that happens to call a
// MITM'd host (api.anthropic.com) gets CERTIFICATE_VERIFY_FAILED forever.
// Measured live: ~7 failed retries/minute, 11,665 log lines, from exactly
// this.
//
// The fix is a combined bundle: the platform's system root CAs concatenated
// with spillway's own CA cert. But SSL_CERT_FILE and friends REPLACE the
// trust store rather than extend it (openssl's default verify locations are
// disabled the moment SSL_CERT_FILE is set), so a bundle missing the system
// roots would convert today's partial failure — only MITM'd hosts break —
// into a total one: every ordinary site would stop verifying for anything
// that reads these variables.
//
// Go does not make obtaining the system roots easy: x509.SystemCertPool
// returns a CertPool that cannot be enumerated back into PEM bytes
// (Subjects() is deprecated and returns names, not certificates, precisely
// to stop people trying this). So each platform needs its own way to reach
// the actual bytes — see bundle_darwin.go and bundle_linux.go. Windows uses
// its own certificate store via CryptoAPI/Schannel, largely independent of
// these env vars, and has no equivalent well-known bundle file; it is left
// unimplemented deliberately, not by oversight — see systemRootsPEM's
// default below.
//
// When a platform's roots cannot be confidently obtained, EnsureCABundle
// writes nothing and returns ok=false. That is the one rule that matters
// here: leaving subprocesses exactly as they are today (some things fail
// loudly against a MITM'd host) is strictly better than writing a bundle
// that silently breaks verification for everything.
import (
	"bytes"
	"crypto/x509"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// systemRootsPEM returns the platform's system root CA certificates as PEM,
// or ok=false with a reason when they cannot be confidently obtained.
//
// The zero-value here is the fallback for any GOOS without a platform file
// below (windows, freebsd, …): "not implemented" is indistinguishable from
// "the security helper failed" from EnsureCABundle's point of view — both
// mean skip. bundle_darwin.go and bundle_linux.go override this via init().
var systemRootsPEM = func() (pemBytes []byte, reason string, ok bool) {
	return nil, "no system root CA source implemented for GOOS=" + runtime.GOOS, false
}

// validRootBundle reports whether pemBytes parses as at least one
// certificate. A helper that "succeeds" with empty or garbage output (a
// missing binary answering on a shell $PATH typo, a keychain command with no
// permission returning an empty stdout without an error) must not be
// confused with a real root store — see the package doc's central warning.
func validRootBundle(pemBytes []byte) bool {
	if len(pemBytes) == 0 {
		return false
	}
	pool := x509.NewCertPool()
	return pool.AppendCertsFromPEM(pemBytes)
}

// combineBundle concatenates the system roots with spillway's own CA cert,
// system roots first, so spillway's cert is always present regardless of
// how many (or few) system roots trailing/leading whitespace leaves behind.
func combineBundle(rootsPEM, caCertPEM []byte) []byte {
	var buf bytes.Buffer
	buf.Write(rootsPEM)
	if buf.Len() > 0 && buf.Bytes()[buf.Len()-1] != '\n' {
		buf.WriteByte('\n')
	}
	buf.Write(caCertPEM)
	return buf.Bytes()
}

// bundlePathFor derives the combined-bundle path from the CA pem path, the
// same way chainPath derives the leaf-manifest path: beside pemPath,
// "spillway-ca.pem" -> "spillway-ca-bundle.pem".
func bundlePathFor(pemPath string) string {
	dir := filepath.Dir(pemPath)
	base := strings.TrimSuffix(filepath.Base(pemPath), filepath.Ext(pemPath))
	return filepath.Join(dir, base+"-bundle.pem")
}

// writeBundlePEM writes the combined bundle atomically. 0644, not 0600: this
// file holds only public certificates (system roots plus spillway's own CA
// cert) — nothing in it is secret, unlike the leaf manifest beside it.
func writeBundlePEM(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return fmt.Errorf("write CA bundle: %w", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename CA bundle: %w", err)
	}
	return os.Chmod(path, 0o644)
}

// EnsureCABundle builds (and writes, beside pemPath) a combined CA bundle —
// this platform's system root CAs plus caCertPEM — for the non-Node
// SSL_CERT_FILE/REQUESTS_CA_BUNDLE/CURL_CA_BUNDLE variables to point at.
//
// Returns ok=false, with nothing written to disk, whenever the system roots
// cannot be confidently obtained on this platform (systemRootsPEM's ok is
// false) or the result doesn't parse as a real root store (validRootBundle):
// see the package doc for why that is the safe failure mode. logger may be
// nil, in which case slog.Default() is used.
func EnsureCABundle(pemPath string, caCertPEM []byte, logger *slog.Logger) (bundlePath string, ok bool) {
	if logger == nil {
		logger = slog.Default()
	}
	rootsPEM, reason, rok := systemRootsPEM()
	if rok && !validRootBundle(rootsPEM) {
		rok = false
		reason = "system CA source returned data that does not parse as any certificate"
	}
	if !rok {
		logger.Warn("mitm: skipping combined CA bundle — SSL_CERT_FILE/REQUESTS_CA_BUNDLE/CURL_CA_BUNDLE "+
			"will NOT be set, so a non-Node subprocess (an MCP server, most concretely) calling a MITM'd "+
			"host will keep failing TLS verification exactly as before. Writing a bundle without a "+
			"confidently-known system root set would replace, not extend, that subprocess's whole trust "+
			"store, which is worse.",
			"reason", reason, "goos", runtime.GOOS)
		return "", false
	}

	bundlePEM := combineBundle(rootsPEM, caCertPEM)
	path := bundlePathFor(pemPath)
	if err := writeBundlePEM(path, bundlePEM); err != nil {
		logger.Warn("mitm: failed writing combined CA bundle — "+
			"SSL_CERT_FILE/REQUESTS_CA_BUNDLE/CURL_CA_BUNDLE will NOT be set", "err", err)
		return "", false
	}
	logger.Info("mitm: wrote combined CA bundle for non-Node subprocesses", "path", path)
	return path, true
}
