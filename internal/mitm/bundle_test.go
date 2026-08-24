package mitm

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// wellKnownRootPEM is ISRG Root X1 (Let's Encrypt's root), pulled straight
// off this machine's real system keychain — a genuine, widely-trusted public
// root, not a fixture invented for the test. Used as a stand-in "system
// roots" bundle so the combine/validate logic can be tested deterministically,
// without depending on which roots happen to be installed on whatever OS runs
// this test.
const wellKnownRootPEM = `-----BEGIN CERTIFICATE-----
MIIFazCCA1OgAwIBAgIRAIIQz7DSQONZRGPgu2OCiwAwDQYJKoZIhvcNAQELBQAw
TzELMAkGA1UEBhMCVVMxKTAnBgNVBAoTIEludGVybmV0IFNlY3VyaXR5IFJlc2Vh
cmNoIEdyb3VwMRUwEwYDVQQDEwxJU1JHIFJvb3QgWDEwHhcNMTUwNjA0MTEwNDM4
WhcNMzUwNjA0MTEwNDM4WjBPMQswCQYDVQQGEwJVUzEpMCcGA1UEChMgSW50ZXJu
ZXQgU2VjdXJpdHkgUmVzZWFyY2ggR3JvdXAxFTATBgNVBAMTDElTUkcgUm9vdCBY
MTCCAiIwDQYJKoZIhvcNAQEBBQADggIPADCCAgoCggIBAK3oJHP0FDfzm54rVygc
h77ct984kIxuPOZXoHj3dcKi/vVqbvYATyjb3miGbESTtrFj/RQSa78f0uoxmyF+
0TM8ukj13Xnfs7j/EvEhmkvBioZxaUpmZmyPfjxwv60pIgbz5MDmgK7iS4+3mX6U
A5/TR5d8mUgjU+g4rk8Kb4Mu0UlXjIB0ttov0DiNewNwIRt18jA8+o+u3dpjq+sW
T8KOEUt+zwvo/7V3LvSye0rgTBIlDHCNAymg4VMk7BPZ7hm/ELNKjD+Jo2FR3qyH
B5T0Y3HsLuJvW5iB4YlcNHlsdu87kGJ55tukmi8mxdAQ4Q7e2RCOFvu396j3x+UC
B5iPNgiV5+I3lg02dZ77DnKxHZu8A/lJBdiB3QW0KtZB6awBdpUKD9jf1b0SHzUv
KBds0pjBqAlkd25HN7rOrFleaJ1/ctaJxQZBKT5ZPt0m9STJEadao0xAH0ahmbWn
OlFuhjuefXKnEgV4We0+UXgVCwOPjdAvBbI+e0ocS3MFEvzG6uBQE3xDk3SzynTn
jh8BCNAw1FtxNrQHusEwMFxIt4I7mKZ9YIqioymCzLq9gwQbooMDQaHWBfEbwrbw
qHyGO0aoSCqI3Haadr8faqU9GY/rOPNk3sgrDQoo//fb4hVC1CLQJ13hef4Y53CI
rU7m2Ys6xt0nUW7/vGT1M0NPAgMBAAGjQjBAMA4GA1UdDwEB/wQEAwIBBjAPBgNV
HRMBAf8EBTADAQH/MB0GA1UdDgQWBBR5tFnme7bl5AFzgAiIyBpY9umbbjANBgkq
hkiG9w0BAQsFAAOCAgEAVR9YqbyyqFDQDLHYGmkgJykIrGF1XIpu+ILlaS/V9lZL
ubhzEFnTIZd+50xx+7LSYK05qAvqFyFWhfFQDlnrzuBZ6brJFe+GnY+EgPbk6ZGQ
3BebYhtF8GaV0nxvwuo77x/Py9auJ/GpsMiu/X1+mvoiBOv/2X/qkSsisRcOj/KK
NFtY2PwByVS5uCbMiogziUwthDyC3+6WVwW6LLv3xLfHTjuCvjHIInNzktHCgKQ5
ORAzI4JMPJ+GslWYHb4phowim57iaztXOoJwTdwJx4nLCgdNbOhdjsnvzqvHu7Ur
TkXWStAmzOVyyghqpZXjFaH3pO3JLF+l+/+sKAIuvtd7u+Nxe5AW0wdeRlN8NwdC
jNPElpzVmbUq4JUagEiuTDkHzsxHpFKVK7q4+63SM1N95R1NbdWhscdCb+ZAJzVc
oyi3B43njTOQ5yOf+1CceWxG1bQVs5ZufpsMljq4Ui0/1lvh+wjChP4kqKOJ2qxq
4RgqsahDYVvTH9w7jXbyLeiNdd8XM2w9U/t7y0Ff/9yi0GE44Za4rF2LN9d11TPA
mRGunUHBcnWEvgJBQl9nJEiU0Zsnvgc/ubhPgXRR4Xq37Z0j4r7g1SgEEzwxA57d
emyPxgcYxn/eR44/KJ4EBs+lVDR3veyJm+kXQ99b21/+jh5Xos1AnX5iItreGCc=
-----END CERTIFICATE-----
`

// fakeSpillwayCAPEM returns a freshly self-signed cert PEM standing in for
// spillway's own CA cert — a distinct cert from wellKnownRootPEM so a test
// can tell "did the bundle actually include the system roots" apart from
// "does the bundle merely contain spillway's own cert".
func fakeSpillwayCAPEM(t *testing.T) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "spillway local CA (test)"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// bundleHasCommonName parses every certificate in pemBytes and reports
// whether any has the given Subject Common Name. The PEM bundle is
// base64-encoded DER, so a plain substring search for a human-readable name
// (e.g. "ISRG Root X1") never matches the encoded bytes — this has to
// actually parse the certs to check identity.
func bundleHasCommonName(t *testing.T, pemBytes []byte, cn string) bool {
	t.Helper()
	rest := pemBytes
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			return false
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err == nil && cert.Subject.CommonName == cn {
			return true
		}
	}
}

// TestCombineBundleContainsWellKnownRootAndSpillwayCA is the test the issue
// asks for directly: the combined bundle must contain BOTH a well-known
// public root AND spillway's own CA — a bundle with only spillway's CA (the
// bug this guards against: silently dropping the system roots) must fail
// this loudly.
func TestCombineBundleContainsWellKnownRootAndSpillwayCA(t *testing.T) {
	caPEM := fakeSpillwayCAPEM(t)
	got := combineBundle([]byte(wellKnownRootPEM), caPEM)

	if !bundleHasCommonName(t, got, "ISRG Root X1") {
		t.Error("combined bundle does not contain the well-known public root (ISRG Root X1)")
	}
	if !bytes.Contains(got, caPEM) {
		t.Error("combined bundle does not contain spillway's own CA cert")
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(got) {
		t.Fatal("combined bundle does not parse as valid PEM certificates at all")
	}
}

// TestCombineBundleMissingRootsFailsTheCheckLoudly is the mirror of the test
// above, proving it actually bites: a "bundle" holding only spillway's CA —
// exactly what you'd get if the system-roots step silently produced nothing —
// must NOT contain the well-known root.
func TestCombineBundleMissingRootsFailsTheCheckLoudly(t *testing.T) {
	caPEM := fakeSpillwayCAPEM(t)
	got := combineBundle(nil, caPEM) // no system roots at all

	if bundleHasCommonName(t, got, "ISRG Root X1") {
		t.Fatal("a bundle built with no system roots must not contain the well-known root")
	}
	if !bytes.Contains(got, caPEM) {
		t.Error("spillway's own CA cert should still be present")
	}
}

func TestValidRootBundle(t *testing.T) {
	if validRootBundle(nil) {
		t.Error("empty input must not validate")
	}
	if validRootBundle([]byte("not a certificate")) {
		t.Error("garbage input must not validate")
	}
	if !validRootBundle([]byte(wellKnownRootPEM)) {
		t.Error("a real root cert PEM should validate")
	}
}

func TestBundlePathFor(t *testing.T) {
	got := bundlePathFor("/home/u/.config/spillway/spillway-ca.pem")
	want := "/home/u/.config/spillway/spillway-ca-bundle.pem"
	if got != want {
		t.Errorf("bundlePathFor = %q, want %q", got, want)
	}
}

// TestEnsureCABundleSkipsWhenRootsUnavailable is THE critical-trap test: when
// the platform's system roots cannot be confidently obtained, EnsureCABundle
// must write nothing at all and report ok=false — never a partial bundle
// that would replace (not extend) a subprocess's trust store.
func TestEnsureCABundleSkipsWhenRootsUnavailable(t *testing.T) {
	orig := systemRootsPEM
	t.Cleanup(func() { systemRootsPEM = orig })
	systemRootsPEM = func() ([]byte, string, bool) {
		return nil, "stub: unavailable on this platform", false
	}

	dir := t.TempDir()
	pemPath := filepath.Join(dir, "spillway-ca.pem")
	caPEM := fakeSpillwayCAPEM(t)

	var logBuf bytes.Buffer
	path, ok := EnsureCABundle(pemPath, caPEM, testLogger(&logBuf))
	if ok {
		t.Error("ok should be false when system roots are unavailable")
	}
	if path != "" {
		t.Errorf("bundlePath should be empty, got %q", path)
	}
	if _, err := os.Stat(bundlePathFor(pemPath)); !os.IsNotExist(err) {
		t.Fatalf("bundle file must never be written when roots are unavailable; stat err = %v", err)
	}
	if !strings.Contains(logBuf.String(), "skipping combined CA bundle") {
		t.Errorf("expected a log line explaining the skip, got: %s", logBuf.String())
	}
}

// TestEnsureCABundleSkipsWhenRootsFailValidation covers the "helper succeeded
// but returned garbage" half of the trap — e.g. a shell command that exits 0
// with empty or malformed stdout must not be treated as success.
func TestEnsureCABundleSkipsWhenRootsFailValidation(t *testing.T) {
	orig := systemRootsPEM
	t.Cleanup(func() { systemRootsPEM = orig })
	systemRootsPEM = func() ([]byte, string, bool) {
		return []byte("not actually a certificate"), "", true
	}

	dir := t.TempDir()
	pemPath := filepath.Join(dir, "spillway-ca.pem")
	path, ok := EnsureCABundle(pemPath, fakeSpillwayCAPEM(t), nil)
	if ok || path != "" {
		t.Fatalf("expected skip on unparseable roots, got path=%q ok=%v", path, ok)
	}
	if _, err := os.Stat(bundlePathFor(pemPath)); !os.IsNotExist(err) {
		t.Fatalf("bundle file must not be written on validation failure; stat err = %v", err)
	}
}

// TestEnsureCABundleWritesPublicFile covers the happy path: a real bundle
// gets written, contains both certs, and — unlike the leaf manifest and CA
// pem beside it — is NOT locked down to 0600, because it holds only public
// certificates.
func TestEnsureCABundleWritesPublicFile(t *testing.T) {
	orig := systemRootsPEM
	t.Cleanup(func() { systemRootsPEM = orig })
	systemRootsPEM = func() ([]byte, string, bool) {
		return []byte(wellKnownRootPEM), "", true
	}

	dir := t.TempDir()
	pemPath := filepath.Join(dir, "spillway-ca.pem")
	caPEM := fakeSpillwayCAPEM(t)

	path, ok := EnsureCABundle(pemPath, caPEM, nil)
	if !ok {
		t.Fatal("expected success with valid stubbed roots")
	}
	if path != bundlePathFor(pemPath) {
		t.Errorf("path = %q, want %q", path, bundlePathFor(pemPath))
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bundleHasCommonName(t, got, "ISRG Root X1") || !bytes.Contains(got, caPEM) {
		t.Error("written bundle missing the well-known root or spillway's CA")
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if perm := fi.Mode().Perm(); perm != 0o644 {
			t.Errorf("bundle file mode = %o, want 644 (public certs only)", perm)
		}
	}
}

// TestSystemRootsPEMOnThisPlatform is a light real-world sanity check on
// whatever platform actually runs this test (CI covers darwin, linux and
// windows — see .github/workflows/ci.yml): darwin and linux have an
// implementation and must find something real; anything else (windows) is
// allowed to report unavailable, since that path is deliberately
// unimplemented.
func TestSystemRootsPEMOnThisPlatform(t *testing.T) {
	pemBytes, reason, ok := systemRootsPEM()
	switch runtime.GOOS {
	case "darwin", "linux":
		if !ok {
			t.Fatalf("expected to find real system roots on %s, got reason: %s", runtime.GOOS, reason)
		}
		if !validRootBundle(pemBytes) {
			t.Fatalf("system roots on %s did not parse as valid certificates", runtime.GOOS)
		}
	default:
		t.Logf("GOOS=%s has no systemRootsPEM implementation; ok=%v reason=%s", runtime.GOOS, ok, reason)
	}
}
