package mitm

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
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

func TestEnsureCAGenerateAndReload(t *testing.T) {
	store := secrets.NewFake()
	pemPath := filepath.Join(t.TempDir(), "spillway-ca.pem")

	ca1, err := EnsureCA(store, pemPath, nil)
	if err != nil {
		t.Fatalf("EnsureCA: %v", err)
	}
	if len(ca1.CertPEM()) == 0 {
		t.Fatal("empty CA pem")
	}
	testmode.AssertPrivateFile(t, pemPath)
	// Key is in the store, never alongside the pem.
	if _, err := store.GetRaw(CAKeyName); err != nil {
		t.Errorf("CA key not in store: %v", err)
	}

	// Second call loads the same CA.
	ca2, err := EnsureCA(store, pemPath, nil)
	if err != nil {
		t.Fatalf("EnsureCA reload: %v", err)
	}
	if string(ca2.CertPEM()) != string(ca1.CertPEM()) {
		t.Error("reload produced a different CA")
	}
}

func TestEnsureCARegeneratesOnMismatch(t *testing.T) {
	store := secrets.NewFake()
	pemPath := filepath.Join(t.TempDir(), "spillway-ca.pem")
	ca1, err := EnsureCA(store, pemPath, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Corrupt the pem half: key is fine, pem is garbage -> cert-only rewrite.
	if err := os.WriteFile(pemPath, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	ca2, err := EnsureCA(store, pemPath, nil)
	if err != nil {
		t.Fatalf("EnsureCA after pem corruption: %v", err)
	}
	if string(ca2.CertPEM()) == string(ca1.CertPEM()) {
		t.Error("expected regeneration after mismatch")
	}

	// And the reverse: drop the key half (genuinely absent -> ErrNotFound).
	store2 := secrets.NewFake()
	ca3, err := EnsureCA(store2, pemPath, nil)
	if err != nil {
		t.Fatalf("EnsureCA after key loss: %v", err)
	}
	if string(ca3.CertPEM()) == string(ca2.CertPEM()) {
		t.Error("expected regeneration after key loss")
	}
}

func TestLeafValidatesAgainstCA(t *testing.T) {
	store := secrets.NewFake()
	ca, err := EnsureCA(store, filepath.Join(t.TempDir(), "ca.pem"), nil)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := ca.Leaf("api.anthropic.com")
	if err != nil {
		t.Fatal(err)
	}
	// Chain served includes the CA; leaf[0] must verify against the CA pool.
	x509Leaf, err := x509.ParseCertificate(leaf.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca.CertPEM())
	if _, err := x509Leaf.Verify(x509.VerifyOptions{DNSName: "api.anthropic.com", Roots: pool}); err != nil {
		t.Errorf("leaf does not validate against CA: %v", err)
	}
	// Cached on second call.
	leaf2, _ := ca.Leaf("api.anthropic.com")
	if leaf2 != leaf {
		t.Error("leaf not cached")
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

// --- issue #65: keychain-error guard -------------------------------------

// TestEnsureCAPreservesOnKeychainError is the core regression test for #65:
// a keychain read failure that is NOT secrets.ErrNotFound (locked, denied,
// transient) must not be treated as "no CA yet". The existing CA (key in
// the store, cert on disk) must survive untouched and the error must be
// surfaced to the caller.
func TestEnsureCAPreservesOnKeychainError(t *testing.T) {
	store := secrets.NewFake()
	pemPath := filepath.Join(t.TempDir(), "spillway-ca.pem")

	ca1, err := EnsureCA(store, pemPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	keyBefore, err := store.GetRaw(CAKeyName)
	if err != nil {
		t.Fatalf("key not stored: %v", err)
	}
	pemBefore, err := os.ReadFile(pemPath)
	if err != nil {
		t.Fatalf("pem not written: %v", err)
	}

	// Simulate a locked/denied keychain: an error that is NOT ErrNotFound.
	simulated := errors.New("keychain locked")
	store.SetGetRawErr(simulated)

	var logbuf bytes.Buffer
	ca2, err := EnsureCA(store, pemPath, testLogger(&logbuf))
	if err == nil {
		t.Fatal("expected EnsureCA to fail loudly on a non-ErrNotFound keychain error")
	}
	if !errors.Is(err, simulated) {
		t.Errorf("returned error does not wrap the underlying keychain error: %v", err)
	}
	if ca2 != nil {
		t.Error("expected nil CA on keychain error")
	}

	// The existing CA must be untouched: same key, same pem, on disk.
	store.SetGetRawErr(nil) // restore normal lookups to read it back
	keyAfter, err := store.GetRaw(CAKeyName)
	if err != nil {
		t.Fatalf("key vanished from store: %v", err)
	}
	if !bytes.Equal(keyBefore, keyAfter) {
		t.Error("existing CA key was modified despite the keychain error")
	}
	pemAfter, err := os.ReadFile(pemPath)
	if err != nil {
		t.Fatalf("pem vanished from disk: %v", err)
	}
	if !bytes.Equal(pemBefore, pemAfter) {
		t.Error("existing CA pem was rewritten despite the keychain error")
	}
	if string(pemBefore) != string(ca1.CertPEM()) {
		t.Error("sanity: pem on disk should match the original CA")
	}
}

// TestEnsureCARegeneratesOnGenuineAbsence proves the first-run path (and
// "key deliberately deleted" path) still works: secrets.ErrNotFound from
// the store — not just "any error" — is what triggers regeneration.
func TestEnsureCARegeneratesOnGenuineAbsence(t *testing.T) {
	store := secrets.NewFake() // GetRaw returns wrapped ErrNotFound: nothing stored yet.
	pemPath := filepath.Join(t.TempDir(), "spillway-ca.pem")

	if _, err := os.Stat(pemPath); !os.IsNotExist(err) {
		t.Fatalf("sanity: pem should not exist yet")
	}

	var logbuf bytes.Buffer
	ca, err := EnsureCA(store, pemPath, testLogger(&logbuf))
	if err != nil {
		t.Fatalf("EnsureCA on genuine absence: %v", err)
	}
	if len(ca.CertPEM()) == 0 {
		t.Fatal("empty CA pem")
	}
	if _, err := store.GetRaw(CAKeyName); err != nil {
		t.Errorf("CA key not stored after generation: %v", err)
	}
	if _, err := os.Stat(pemPath); err != nil {
		t.Errorf("CA pem not written after generation: %v", err)
	}
}

// TestEnsureCAKeyPresentPemMissingRewritesFromKey: the key is present and
// good, the pem file does not exist. The deliberate choice (see EnsureCA's
// doc comment) is to rewrite ONLY the cert, from the existing key, rather
// than a full regenerate — a full regenerate would rotate the key and cause
// exactly the #65 outage. Assert that choice explicitly: the key in the
// store must be byte-identical before and after.
func TestEnsureCAKeyPresentPemMissingRewritesFromKey(t *testing.T) {
	store := secrets.NewFake()
	pemPath := filepath.Join(t.TempDir(), "spillway-ca.pem")

	ca1, err := EnsureCA(store, pemPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	keyBefore, err := store.GetRaw(CAKeyName)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(pemPath); err != nil {
		t.Fatal(err)
	}

	var logbuf bytes.Buffer
	ca2, err := EnsureCA(store, pemPath, testLogger(&logbuf))
	if err != nil {
		t.Fatalf("EnsureCA after pem removal: %v", err)
	}

	keyAfter, err := store.GetRaw(CAKeyName)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(keyBefore, keyAfter) {
		t.Error("key was rotated when only the pem was missing — this is the outage the fix prevents")
	}
	if string(ca2.CertPEM()) == string(ca1.CertPEM()) {
		t.Error("expected a freshly written cert (new serial), even though the key is unchanged")
	}
	// New leaves must validate against the rewritten cert.
	leaf, err := ca2.Leaf("example.com")
	if err != nil {
		t.Fatal(err)
	}
	x509Leaf, err := x509.ParseCertificate(leaf.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(ca2.CertPEM())
	if _, err := x509Leaf.Verify(x509.VerifyOptions{DNSName: "example.com", Roots: pool}); err != nil {
		t.Errorf("leaf from rewritten cert does not validate: %v", err)
	}
}

// TestEnsureCAKeyPresentPemCorruptRewritesFromKey: same as above but the
// pem file exists and is readable, just not valid PEM/cert content.
func TestEnsureCAKeyPresentPemCorruptRewritesFromKey(t *testing.T) {
	store := secrets.NewFake()
	pemPath := filepath.Join(t.TempDir(), "spillway-ca.pem")

	ca1, err := EnsureCA(store, pemPath, nil)
	if err != nil {
		t.Fatal(err)
	}
	keyBefore, err := store.GetRaw(CAKeyName)
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(pemPath, []byte("not a pem"), 0o600); err != nil {
		t.Fatal(err)
	}

	ca2, err := EnsureCA(store, pemPath, nil)
	if err != nil {
		t.Fatalf("EnsureCA after pem corruption: %v", err)
	}

	keyAfter, err := store.GetRaw(CAKeyName)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(keyBefore, keyAfter) {
		t.Error("key was rotated when only the pem was corrupt")
	}
	if string(ca2.CertPEM()) == string(ca1.CertPEM()) {
		t.Error("expected a freshly written cert")
	}
}

// --- logging: replacing an existing CA warns about restart; first run doesn't ---

func TestEnsureCALogsRestartWarningOnlyWhenReplacing(t *testing.T) {
	// First run: no key, no pem. Must NOT tell anyone to restart anything.
	store := secrets.NewFake()
	pemPath := filepath.Join(t.TempDir(), "spillway-ca.pem")

	var firstRunLog bytes.Buffer
	if _, err := EnsureCA(store, pemPath, testLogger(&firstRunLog)); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(firstRunLog.String()), "restart") {
		t.Errorf("first-run generation must not mention restarting anything; got log: %s", firstRunLog.String())
	}

	// Now simulate the #65 scenario: key genuinely gone (fresh store) but a
	// pem from a previous install is still on disk -> full regenerate,
	// replacing an existing CA -> must warn to restart.
	store2 := secrets.NewFake()
	var replaceLog bytes.Buffer
	if _, err := EnsureCA(store2, pemPath, testLogger(&replaceLog)); err != nil {
		t.Fatal(err)
	}
	got := replaceLog.String()
	if !strings.Contains(strings.ToLower(got), "restart") {
		t.Errorf("regeneration that replaces an existing CA must warn about restarting proxied CLIs; got log: %s", got)
	}
	if !strings.Contains(got, "level=WARN") {
		t.Errorf("expected the restart warning at WARN level; got log: %s", got)
	}
}
