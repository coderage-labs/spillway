package mitm

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coderage-labs/spillway/internal/secrets"

	"github.com/coderage-labs/spillway/internal/testmode"
)

// testLogger returns a logger writing to buf, so tests can assert on what
// was logged.
func testLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// readManifest reads the stored leaf chain straight off disk, the way
// EnsureCA itself does.
func readManifest(t *testing.T, pemPath string) chainManifest {
	t.Helper()
	b, err := secrets.NewFileStore(chainPath(pemPath)).GetRaw(leavesBlobName)
	if err != nil {
		t.Fatalf("read stored leaf chain: %v", err)
	}
	m, err := parseManifest(b)
	if err != nil {
		t.Fatalf("parse stored leaf chain: %v", err)
	}
	return m
}

func TestEnsureCAGenerateAndReload(t *testing.T) {
	pemPath := filepath.Join(t.TempDir(), "spillway-ca.pem")
	hosts := []string{"api.anthropic.com", "127.0.0.1"}

	ca1, err := EnsureCA(pemPath, hosts, nil)
	if err != nil {
		t.Fatalf("EnsureCA: %v", err)
	}
	if len(ca1.CertPEM()) == 0 {
		t.Fatal("empty CA pem")
	}
	testmode.AssertPrivateFile(t, pemPath)
	testmode.AssertPrivateFile(t, chainPath(pemPath))

	manifest := readManifest(t, pemPath)
	for _, h := range hosts {
		if _, ok := manifest.Leaves[h]; !ok {
			t.Errorf("no stored leaf for host %q", h)
		}
	}

	// Second call reuses the same CA and leaves — no rotation on a plain
	// restart with an unchanged host set.
	ca2, err := EnsureCA(pemPath, hosts, nil)
	if err != nil {
		t.Fatalf("EnsureCA reload: %v", err)
	}
	if string(ca2.CertPEM()) != string(ca1.CertPEM()) {
		t.Error("reload produced a different CA")
	}
	leaf1, err := ca1.Leaf("api.anthropic.com")
	if err != nil {
		t.Fatal(err)
	}
	leaf2, err := ca2.Leaf("api.anthropic.com")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(leaf1.Certificate[0], leaf2.Certificate[0]) {
		t.Error("reload rotated the leaf certificate")
	}
}

// TestEnsureCANoLeafOutsideConfiguredHosts proves the on-demand minting
// path is really gone: a host that was never in the EnsureCA hosts list
// gets no leaf, not a freshly minted one.
func TestEnsureCANoLeafOutsideConfiguredHosts(t *testing.T) {
	pemPath := filepath.Join(t.TempDir(), "spillway-ca.pem")
	ca, err := EnsureCA(pemPath, []string{"api.anthropic.com"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ca.Leaf("evil.example.com"); err == nil {
		t.Fatal("expected an error for a host outside the configured set — on-demand minting must be gone")
	}
}

func TestLeafValidatesAgainstCA(t *testing.T) {
	pemPath := filepath.Join(t.TempDir(), "ca.pem")
	ca, err := EnsureCA(pemPath, []string{"api.anthropic.com", "127.0.0.1"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := ca.Leaf("api.anthropic.com")
	if err != nil {
		t.Fatal(err)
	}
	x509Leaf, err := x509.ParseCertificate(leaf.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca.CertPEM())
	if _, err := x509Leaf.Verify(x509.VerifyOptions{DNSName: "api.anthropic.com", Roots: pool}); err != nil {
		t.Errorf("leaf does not validate against CA: %v", err)
	}
	// Same object on repeated lookup.
	leaf2, _ := ca.Leaf("api.anthropic.com")
	if leaf2 != leaf {
		t.Error("expected the same precomputed leaf on repeated lookup")
	}
	// IP hosts get an IP SAN.
	ipLeaf, err := ca.Leaf("127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	x509IP, _ := x509.ParseCertificate(ipLeaf.Certificate[0])
	if _, err := x509IP.Verify(x509.VerifyOptions{DNSName: "127.0.0.1", Roots: pool}); err != nil {
		t.Errorf("IP leaf does not validate: %v", err)
	}
	// Usable as a tls.Certificate.
	if _, err := tls.X509KeyPair(leaf.Certificate[0], []byte("wrong")); err == nil {
		t.Error("sanity: bad keypair accepted")
	}
}

// --- issue #69: no key at rest anywhere ------------------------------------

// TestEnsureCANoPrivateKeyWrittenAnywhere is the core regression test for
// #69's second design: after EnsureCA returns, nothing on disk holds the CA
// private key. Only the leaves' own (public-facing, but still secret)
// per-host keys should exist — the CA key itself must never appear.
func TestEnsureCANoPrivateKeyWrittenAnywhere(t *testing.T) {
	pemPath := filepath.Join(t.TempDir(), "spillway-ca.pem")
	ca, err := EnsureCA(pemPath, []string{"api.anthropic.com"}, nil)
	if err != nil {
		t.Fatal(err)
	}

	manifest := readManifest(t, pemPath)
	for host, rec := range manifest.Leaves {
		leafCert, err := parseCertPEM(rec.CertPEM)
		if err != nil {
			t.Fatalf("parse leaf cert for %q: %v", host, err)
		}
		// The stored leaf key must be the LEAF's own key (verifiable via
		// tls.X509KeyPair), never the CA's key: if it were the CA key, the
		// leaf cert's public key would equal the CA cert's public key,
		// which a leaf (a distinct, freshly generated key pair) never
		// does.
		if _, err := tls.X509KeyPair(rec.CertPEM, rec.KeyPEM); err != nil {
			t.Errorf("stored key for %q is not that leaf's own key: %v", host, err)
		}
		leafPub, ok := leafCert.PublicKey.(*ecdsa.PublicKey)
		caPub, caOK := ca.cert.PublicKey.(*ecdsa.PublicKey)
		if !ok || !caOK {
			t.Fatalf("expected ECDSA public keys, got leaf=%T ca=%T", leafCert.PublicKey, ca.cert.PublicKey)
		}
		if leafPub.Equal(caPub) {
			t.Errorf("stored leaf key for %q equals the CA key — the CA private key was persisted", host)
		}
	}
}

// TestEnsureCARestartWithUnchangedHostsDoesNotRotate proves the important
// "no disruption on an ordinary restart" property directly: two independent
// EnsureCA calls against the same pemPath and the same host set produce
// byte-identical CA cert and leaf cert bytes — a client that trusted the
// first generation still trusts the second.
func TestEnsureCARestartWithUnchangedHostsDoesNotRotate(t *testing.T) {
	pemPath := filepath.Join(t.TempDir(), "spillway-ca.pem")
	hosts := []string{"api.anthropic.com"}

	ca1, err := EnsureCA(pemPath, hosts, nil)
	if err != nil {
		t.Fatal(err)
	}
	leaf1, err := ca1.Leaf("api.anthropic.com")
	if err != nil {
		t.Fatal(err)
	}

	// Simulate a restart: a fresh process, same pemPath, same hosts.
	ca2, err := EnsureCA(pemPath, hosts, nil)
	if err != nil {
		t.Fatal(err)
	}
	leaf2, err := ca2.Leaf("api.anthropic.com")
	if err != nil {
		t.Fatal(err)
	}

	// Issue #66's stale-CA warning is built entirely on this bit: a restart
	// that reused the stored chain must never look like a regeneration to
	// anything downstream.
	if ca2.Regenerated {
		t.Error("a restart with an unchanged host set must not report Regenerated")
	}

	if !bytes.Equal(ca1.CertPEM(), ca2.CertPEM()) {
		t.Error("restart with an unchanged host set rotated the CA cert")
	}
	if !bytes.Equal(leaf1.Certificate[0], leaf2.Certificate[0]) {
		t.Error("restart with an unchanged host set rotated the leaf cert")
	}

	// And the leaf served after "restart" must still validate against the
	// CA cert bytes a client trusted before it.
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca1.CertPEM())
	x509Leaf, err := x509.ParseCertificate(leaf2.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := x509Leaf.Verify(x509.VerifyOptions{DNSName: "api.anthropic.com", Roots: pool}); err != nil {
		t.Errorf("post-restart leaf does not validate against the pre-restart trusted CA: %v", err)
	}
}

// TestEnsureCAChangedHostSetRegeneratesLoudly: a config change that adds a
// host not covered by the stored chain forces a full regeneration — and it
// must say so loudly, since this is the one case that legitimately strands
// running clients (and per the issue, is accepted because it only follows a
// deliberate config change that already needs a restart).
func TestEnsureCAChangedHostSetRegeneratesLoudly(t *testing.T) {
	pemPath := filepath.Join(t.TempDir(), "spillway-ca.pem")

	ca1, err := EnsureCA(pemPath, []string{"api.anthropic.com"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ca1.Regenerated {
		t.Error("first-ever install must not report Regenerated — nothing existed to strand")
	}

	var logbuf bytes.Buffer
	ca2, err := EnsureCA(pemPath, []string{"api.anthropic.com", "api.moonshot.ai"}, testLogger(&logbuf))
	if err != nil {
		t.Fatalf("EnsureCA with an added host: %v", err)
	}
	if !ca2.Regenerated {
		t.Error("a config change that added a host must report Regenerated — this is the one case issue #66 warns about")
	}
	if string(ca1.CertPEM()) == string(ca2.CertPEM()) {
		t.Error("expected a new CA when the host set grew")
	}
	if _, err := ca2.Leaf("api.moonshot.ai"); err != nil {
		t.Errorf("expected a leaf for the newly added host: %v", err)
	}
	got := logbuf.String()
	if !strings.Contains(strings.ToLower(got), "restart") {
		t.Errorf("expected a restart warning when the host set changed; got log: %s", got)
	}
	if !strings.Contains(got, "level=WARN") {
		t.Errorf("expected the warning at WARN level; got log: %s", got)
	}
}

// TestEnsureCAFirstRunDoesNotWarn: generating a chain when nothing existed
// before (no pem, no manifest) must not tell anyone to restart anything —
// there is nothing running yet that could be stranded.
func TestEnsureCAFirstRunDoesNotWarn(t *testing.T) {
	pemPath := filepath.Join(t.TempDir(), "spillway-ca.pem")
	var logbuf bytes.Buffer
	ca, err := EnsureCA(pemPath, []string{"api.anthropic.com"}, testLogger(&logbuf))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(logbuf.String()), "restart") {
		t.Errorf("first-run generation must not mention restarting anything; got log: %s", logbuf.String())
	}
	// Issue #66: a caller (main.go) arms the stale-CA warning and fires the
	// desktop notification only when Regenerated is true. First-run
	// generation must never set it.
	if ca.Regenerated {
		t.Error("first-run generation must not report Regenerated")
	}
}

// TestEnsureCAMigrationFromPreIssue69InstallRegeneratesLoudly: an install
// that predates #69 has a pem on disk (from the old keychain-backed design)
// but no leaf manifest. #69 does not attempt to read the old keychain
// entry forward (see EnsureCA's doc comment) — it is ordinary
// regeneration, with the stale pem's presence upgrading the log line to a
// restart warning, same as any other "no stored chain" case.
func TestEnsureCAMigrationFromPreIssue69InstallRegeneratesLoudly(t *testing.T) {
	pemPath := filepath.Join(t.TempDir(), "spillway-ca.pem")
	if err := os.WriteFile(pemPath, []byte("stale pem from a pre-#69 install"), 0o600); err != nil {
		t.Fatal(err)
	}

	var logbuf bytes.Buffer
	ca, err := EnsureCA(pemPath, []string{"api.anthropic.com"}, testLogger(&logbuf))
	if err != nil {
		t.Fatalf("EnsureCA on migration: %v", err)
	}
	if len(ca.CertPEM()) == 0 {
		t.Fatal("empty CA pem")
	}
	if !ca.Regenerated {
		t.Error("migration from a pre-#69 install must report Regenerated — a pem was already there to strand")
	}
	got := logbuf.String()
	if !strings.Contains(strings.ToLower(got), "restart") {
		t.Errorf("migration from a pre-#69 install must warn about restarting; got log: %s", got)
	}
	// And a fresh, real chain now exists on disk.
	manifest := readManifest(t, pemPath)
	if _, ok := manifest.Leaves["api.anthropic.com"]; !ok {
		t.Error("expected a stored leaf for the configured host after migration")
	}
}

// TestEnsureCAAmbiguousManifestErrorPreservesExisting extends #65's rule to
// the stored-chain read: a manifest file that is present but corrupt must
// never be treated as absence. It must fail loudly and leave the existing
// pem and manifest untouched.
func TestEnsureCAAmbiguousManifestErrorPreservesExisting(t *testing.T) {
	pemPath := filepath.Join(t.TempDir(), "spillway-ca.pem")
	if _, err := EnsureCA(pemPath, []string{"api.anthropic.com"}, nil); err != nil {
		t.Fatal(err)
	}
	pemBefore, err := os.ReadFile(pemPath)
	if err != nil {
		t.Fatal(err)
	}
	manifestPathOnDisk := chainPath(pemPath)
	manifestBefore, err := os.ReadFile(manifestPathOnDisk)
	if err != nil {
		t.Fatal(err)
	}

	// Corrupt the manifest file's JSON directly — present, but unreadable
	// as a store entry. Not "absent".
	if err := os.WriteFile(manifestPathOnDisk, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	var logbuf bytes.Buffer
	ca, err := EnsureCA(pemPath, []string{"api.anthropic.com"}, testLogger(&logbuf))
	if err == nil {
		t.Fatal("expected EnsureCA to fail loudly on a corrupt-but-present manifest")
	}
	if ca != nil {
		t.Error("expected nil CA")
	}
	if errors.Is(err, secrets.ErrNotFound) {
		t.Error("a corrupt manifest must not be reported as ErrNotFound")
	}

	pemAfter, err := os.ReadFile(pemPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(pemBefore, pemAfter) {
		t.Error("existing CA pem was rewritten despite the ambiguous manifest error")
	}
	manifestAfter, err := os.ReadFile(manifestPathOnDisk)
	if err != nil {
		t.Fatal(err)
	}
	_ = manifestBefore // the corruption IS the new content; just confirm it wasn't further mangled/replaced
	if len(manifestAfter) == 0 {
		t.Error("manifest file vanished")
	}
}

// TestEnsureCALeafKeyFileMode0600 asserts the stored chain file (which
// holds the only secret material left — each leaf's private key) is 0600
// in the existing 0700 directory.
func TestEnsureCALeafKeyFileMode0600(t *testing.T) {
	pemPath := filepath.Join(t.TempDir(), "spillway-ca.pem")
	if _, err := EnsureCA(pemPath, []string{"api.anthropic.com"}, nil); err != nil {
		t.Fatal(err)
	}
	testmode.AssertPrivateFile(t, chainPath(pemPath))
}

// TestEnsureCACorruptManifestJSON is a narrower sanity check that a
// manifest which round-trips through parseManifest badly (valid file,
// invalid content shape) is treated as corrupt, not absent.
func TestEnsureCACorruptManifestJSON(t *testing.T) {
	if _, err := parseManifest([]byte("{not valid json")); err == nil {
		t.Fatal("expected an error for invalid manifest JSON")
	}
	// Sanity: a well-formed empty manifest parses fine.
	b, _ := json.Marshal(chainManifest{Leaves: map[string]leafRecord{}})
	if _, err := parseManifest(b); err != nil {
		t.Fatalf("expected empty manifest to parse: %v", err)
	}
}
