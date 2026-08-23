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
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
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
// secret store; the cert PEM from pemPath (0600). logger may be nil, in
// which case slog.Default() is used.
//
// The store's error for the key matters (issue #65): secrets.ErrNotFound
// means the key genuinely does not exist — a fresh install, or someone
// deliberately deleted it — and it is safe to mint a new one. Any other
// error (a locked or denied keychain, a transient read failure) means we
// simply don't know whether a key exists, and treating that the same as
// "absent" is what caused the outage this guards against: minting a new key
// silently strands every already-running proxied CLI, because
// NODE_EXTRA_CA_CERTS is read once at process start and a client that
// trusted the old CA can never be made to trust the new one without a
// restart. internal/secrets/open.go draws exactly this line for the
// file-store fallback — a keychain that answers "locked" or "denied" is the
// user (or the OS) declining, not permission to downgrade — same reasoning,
// this call site.
//
// The cert half is handled separately from the key half on purpose. If the
// key is present and good but the pem is missing, unreadable, or does not
// match that key, only the certificate is recreated — from the SAME key
// pair (rewriteCert). A client that already trusts the old CA validates a
// leaf by verifying its signature against the CA's public key, not against
// the exact bytes of the old CA certificate, so reusing the key here means
// nothing already running breaks. Only a genuinely absent key (the
// ErrNotFound case) forces a full regenerate — a new key AND a new cert —
// which is the one case that unavoidably strands running clients, and is
// logged loudly enough to say so.
func EnsureCA(store secrets.Store, pemPath string, logger *slog.Logger) (*CA, error) {
	if logger == nil {
		logger = slog.Default()
	}

	keyPEM, keyErr := store.GetRaw(CAKeyName)
	certPEM, certErr := os.ReadFile(pemPath)
	// Independent of certErr: os.ReadFile can fail for a file that exists
	// (e.g. a permissions problem) and we still want to know "was there
	// already a CA someone might be trusting" for the log line below.
	_, statErr := os.Stat(pemPath)
	pemExisted := statErr == nil

	switch {
	case keyErr == nil:
		signer, perr := parseKeyPEM(keyPEM)
		if perr != nil {
			// The stored key blob itself is unparseable. This is not
			// "absent" — GetRaw succeeded — so it does not get the
			// ErrNotFound treatment below. Only spillway ever writes
			// this value; fail loudly rather than silently minting a
			// replacement over whatever this is.
			return nil, fmt.Errorf("mitm: stored CA key is corrupt: %w", perr)
		}
		if certErr == nil {
			if ca, err := parseCertAgainstKey(certPEM, signer); err == nil {
				return ca, nil // happy path: both halves present and matching
			}
		}
		reason := "CA cert file missing or unreadable"
		if certErr == nil {
			reason = "CA cert file present but did not parse or match the stored key"
		}
		return rewriteCert(signer, pemPath, reason, logger)

	case errors.Is(keyErr, secrets.ErrNotFound):
		// Genuinely no key: first run, or the key was deliberately
		// removed. Nothing to reuse — mint both halves.
		return generateCA(store, pemPath, pemExisted, logger)

	default:
		// Locked, denied, or a transient keychain failure. Leave the
		// existing CA — key in the store, cert on disk — exactly as it
		// is and fail loudly instead of guessing.
		logger.Warn("mitm: keychain unavailable, existing MITM CA left untouched",
			"err", keyErr)
		return nil, fmt.Errorf("mitm: keychain unavailable, existing CA left untouched: %w", keyErr)
	}
}

func parseKeyPEM(keyPEM []byte) (crypto.Signer, error) {
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
	return signer, nil
}

func parseCertAgainstKey(certPEM []byte, signer crypto.Signer) (*CA, error) {
	cb, _ := pem.Decode(certPEM)
	if cb == nil {
		return nil, errors.New("CA cert PEM undecodable")
	}
	cert, err := x509.ParseCertificate(cb.Bytes)
	if err != nil {
		return nil, err
	}
	pub, ok := cert.PublicKey.(interface{ Equal(crypto.PublicKey) bool })
	if !ok || !pub.Equal(signer.Public()) {
		return nil, errors.New("CA cert/key mismatch")
	}
	return &CA{cert: cert, key: signer, certPEM: certPEM, leaves: map[string]*tls.Certificate{}}, nil
}

// buildSelfSignedCert mints a fresh self-signed CA certificate for pub,
// signed by signer (which must correspond to pub). Shared by generateCA
// (new key) and rewriteCert (existing key) so both halves stay in sync.
func buildSelfSignedCert(pub crypto.PublicKey, signer crypto.Signer) (*x509.Certificate, []byte, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
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
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, pub, signer)
	if err != nil {
		return nil, nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, err
	}
	return cert, der, nil
}

// writeCertPEM writes the CA cert pem atomically, 0600.
func writeCertPEM(pemPath string, certPEM []byte) error {
	if err := os.MkdirAll(filepath.Dir(pemPath), 0o700); err != nil {
		return err
	}
	tmp := pemPath + ".tmp"
	if err := os.WriteFile(tmp, certPEM, 0o600); err != nil {
		return fmt.Errorf("write CA pem: %w", err)
	}
	if err := os.Rename(tmp, pemPath); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("rename CA pem: %w", err)
	}
	return os.Chmod(pemPath, 0o600)
}

// fingerprint is a log-friendly identity for a CA cert.
func fingerprint(cert *x509.Certificate) string {
	sum := sha256.Sum256(cert.Raw)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// generateCA mints a brand new key AND cert — the only path that can strand
// an already-running proxied CLI, since the key (and therefore what leaves
// verify against) changes. Called only when the store told us the old key
// is genuinely gone (secrets.ErrNotFound), never on an ambiguous read error.
func generateCA(store secrets.Store, pemPath string, pemExisted bool, logger *slog.Logger) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	cert, der, err := buildSelfSignedCert(key.Public(), key)
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
	if err := writeCertPEM(pemPath, certPEM); err != nil {
		return nil, err
	}

	if pemExisted {
		logger.Warn("mitm: regenerated the MITM CA — key was not found in the keychain; "+
			"already-running proxied CLIs trust the OLD CA and will fail every TLS handshake until restarted",
			"fingerprint", fingerprint(cert))
	} else {
		logger.Info("mitm: generated install CA", "fingerprint", fingerprint(cert))
	}
	return &CA{cert: cert, key: key, certPEM: certPEM, leaves: map[string]*tls.Certificate{}}, nil
}

// rewriteCert recreates only the certificate half, from an existing,
// already-verified key. The key is untouched, so this never strands an
// already-running client (see EnsureCA's doc comment).
func rewriteCert(signer crypto.Signer, pemPath string, reason string, logger *slog.Logger) (*CA, error) {
	cert, der, err := buildSelfSignedCert(signer.Public(), signer)
	if err != nil {
		return nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	if err := writeCertPEM(pemPath, certPEM); err != nil {
		return nil, err
	}
	logger.Warn("mitm: recreated the MITM CA certificate from the existing key — key unchanged, no restart needed",
		"reason", reason, "fingerprint", fingerprint(cert))
	return &CA{cert: cert, key: signer, certPEM: certPEM, leaves: map[string]*tls.Certificate{}}, nil
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
