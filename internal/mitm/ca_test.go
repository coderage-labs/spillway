package mitm

import (
	"crypto/tls"
	"crypto/x509"
	"os"
	"path/filepath"
	"testing"

	"github.com/coderage-labs/spillway/internal/secrets"

	"github.com/coderage-labs/spillway/internal/testmode"
)

func TestEnsureCAGenerateAndReload(t *testing.T) {
	store := secrets.NewFake()
	pemPath := filepath.Join(t.TempDir(), "spillway-ca.pem")

	ca1, err := EnsureCA(store, pemPath)
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
	ca2, err := EnsureCA(store, pemPath)
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
	ca1, err := EnsureCA(store, pemPath)
	if err != nil {
		t.Fatal(err)
	}

	// Corrupt the pem half: keychain/pem mismatch → regenerate both.
	if err := os.WriteFile(pemPath, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	ca2, err := EnsureCA(store, pemPath)
	if err != nil {
		t.Fatalf("EnsureCA after pem corruption: %v", err)
	}
	if string(ca2.CertPEM()) == string(ca1.CertPEM()) {
		t.Error("expected regeneration after mismatch")
	}

	// And the reverse: drop the key half.
	store2 := secrets.NewFake()
	ca3, err := EnsureCA(store2, pemPath)
	if err != nil {
		t.Fatalf("EnsureCA after key loss: %v", err)
	}
	if string(ca3.CertPEM()) == string(ca2.CertPEM()) {
		t.Error("expected regeneration after key loss")
	}
}

func TestLeafValidatesAgainstCA(t *testing.T) {
	store := secrets.NewFake()
	ca, err := EnsureCA(store, filepath.Join(t.TempDir(), "ca.pem"))
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
