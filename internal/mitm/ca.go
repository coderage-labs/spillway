// Package mitm implements the CONNECT-terminating side of spillway's proxy:
// a per-install CA whose private key lives only in the OS keychain (design
// doc §5, §6.7), plus per-host leaf certificates minted lazily and cached in
// memory. All crypto is crypto/x509 — no hand-rolled ASN.1.
package mitm

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/coderage-labs/spillway/internal/secrets"
)

// CAKeyName is the secret-store key holding the CA private key (PKCS#8 PEM).
// The key never touches disk — a daemon restart reloads it from the keychain
// and existing CLI sessions keep working (§6.7).
const CAKeyName = "mitm-ca-key"

// CA signs per-host leaf certificates for terminated CONNECT tunnels.
type CA struct {
	cert    *x509.Certificate
	key     crypto.Signer
	certPEM []byte

	mu     sync.Mutex
	leaves map[string]*tls.Certificate
}

// CertPEM returns the CA certificate in PEM form (what clients trust via
// NODE_EXTRA_CA_CERTS).
func (c *CA) CertPEM() []byte { return c.certPEM }

// EnsureCA loads or creates the install CA. The private key comes from the
// secret store; the cert PEM from pemPath (0600). If either half is missing
// or they don't match (key rotated, pem deleted), both are regenerated —
// a stale half must never produce leaves clients reject.
func EnsureCA(store secrets.Store, pemPath string) (*CA, error) {
	keyPEM, keyErr := store.GetRaw(CAKeyName)
	certPEM, certErr := os.ReadFile(pemPath)
	if keyErr == nil && certErr == nil {
		if ca, err := parseCA(certPEM, keyPEM); err == nil {
			return ca, nil
		}
		// Mismatch or corrupt half: fall through and regenerate both.
	}
	return generateCA(store, pemPath)
}

func parseCA(certPEM, keyPEM []byte) (*CA, error) {
	cb, _ := pem.Decode(certPEM)
	if cb == nil {
		return nil, errors.New("CA cert PEM undecodable")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, err
	}
	kb, _ := pem.Decode(keyPEM)
	if kb == nil {
		return nil, errors.New("CA key PEM undecodable")
	}
	keyAny, err := x509.ParsePKCS8PrivateKey(kb.Bytes)
	if err != nil {
		return nil, err
	}
	signer, ok := keyAny.(crypto.Signer)
	if !ok {
		return nil, errors.New("CA key is not a signer")
	}
	pub, ok := cert.PublicKey.(interface{ Equal(crypto.PublicKey) bool })
	if !ok || !pub.Equal(signer.Public()) {
		return nil, errors.New("CA cert/key mismatch")
	}
	return &CA{cert: cert, key: signer, certPEM: certPEM, leaves: map[string]*tls.Certificate{}}, nil
}

func generateCA(store secrets.Store, pemPath string) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "spillway local CA", Organization: []string{"spillway"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})

	if err := store.SetRaw(CAKeyName, keyPEM); err != nil {
		return nil, fmt.Errorf("store CA key: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(pemPath), 0o700); err != nil {
		return nil, err
	}
	tmp := pemPath + ".tmp"
	if err := os.WriteFile(tmp, certPEM, 0o600); err != nil {
		return nil, fmt.Errorf("write CA pem: %w", err)
	}
	if err := os.Rename(tmp, pemPath); err != nil {
		os.Remove(tmp)
		return nil, fmt.Errorf("rename CA pem: %w", err)
	}
	if err := os.Chmod(pemPath, 0o600); err != nil {
		return nil, err
	}
	return &CA{cert: cert, key: key, certPEM: certPEM, leaves: map[string]*tls.Certificate{}}, nil
}

// Leaf returns a certificate for host, minting and caching it on first use.
// IP hosts get an IP SAN; names get a DNS SAN.
func (c *CA) Leaf(host string) (*tls.Certificate, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if leaf, ok := c.leaves[host]; ok {
		return leaf, nil
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(397 * 24 * time.Hour), // platform leaf limit
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, key.Public(), c.key)
	if err != nil {
		return nil, err
	}
	leaf := &tls.Certificate{
		Certificate: [][]byte{der, c.cert.Raw},
		PrivateKey:  key,
	}
	c.leaves[host] = leaf
	return leaf, nil
}
