// Package mitm implements the CONNECT-terminating side of spillway's proxy:
// a per-install CA plus a precomputed leaf certificate for every host
// spillway will ever need one for, all minted once at startup and written
// to disk — no CA private key is kept anywhere past that. All crypto is
// crypto/x509 — no hand-rolled ASN.1.
//
// This is issue #69's second design, and it deletes the bug class instead
// of relocating it. Both the original design (§5, §6.7: key only in the OS
// keychain) and #69's first draft (key moved to disk) kept the CA private
// key around somewhere, so that leaves could be minted on demand as new
// hosts showed up. But a keychain read that failed ambiguously during a
// routine upgrade (#65) was indistinguishable from "no CA yet", and even
// once #65 stopped that from silently regenerating, the same hiccup left a
// daemon that simply would not start — because the key still had to be
// read from *somewhere* before anything could be signed.
//
// The fix is to notice that on-demand signing was never actually needed.
// The full set of hosts spillway will ever terminate CONNECT for —
// internal/proxy.Handler's allowedHosts — is known before the first
// request reaches it: the global upstream plus every configured account's
// upstream, fixed for the life of the process, because nothing adds an
// account to a running pool (a config change needs a restart to take
// effect — the same reasoning issue #46 relies on for its restart notice).
// So EnsureCA generates the CA, mints a leaf for every one of those hosts
// up front, writes the CA cert and the leaf certs+keys to disk, and lets
// the CA private key fall out of scope when it returns. There is no key at
// rest anywhere — not in a keychain, not on disk — so there is nothing to
// fail to read on the next start and no ambiguous error to mishandle: the
// entire class of bug #65 and #69's first draft fought over cannot recur,
// because its precondition no longer exists. Leaf() below is a lookup into
// that precomputed set, never a signing operation.
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
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/coderage-labs/spillway/internal/secrets"
)

// leavesBlobName is the FileStore blob (see chainPath) holding the
// precomputed leaf chain for the current CA generation: one cert+key per
// host, plus — as its map keys — the set of hosts it covers.
const leavesBlobName = "mitm-ca-leaves"

// leafRecord is one host's precomputed leaf, as stored on disk.
type leafRecord struct {
	CertPEM []byte `json:"cert"`
	KeyPEM  []byte `json:"key"`
}

// chainManifest is the full stored leaf set for one CA cert generation.
type chainManifest struct {
	Leaves map[string]leafRecord `json:"leaves"`
}

// CA holds a self-signed install CA and a fixed, precomputed set of leaf
// certificates. There is deliberately no private-key field: generateChain
// discards the CA key the moment it is done signing, so nothing here can
// mint a certificate that was not requested at EnsureCA time (see the
// package doc).
type CA struct {
	cert    *x509.Certificate
	certPEM []byte
	leaves  map[string]*tls.Certificate // fixed after construction; read-only
	// Regenerated is true only when this call to EnsureCA actually minted a
	// new CA in place of one that already existed — never on the
	// stored-chain-reused happy path (an ordinary restart, #70's fix) and
	// never on a true first-ever install (nothing existed to strand). It is
	// the single fact issue #66's stale-CA warning is built on: only a
	// caller told Regenerated=true may treat a later recurring MITM
	// handshake failure as evidence of a stranded client (see
	// internal/proxy's mitmFailLogger).
	Regenerated bool
}

// CertPEM returns the CA certificate in PEM form (what clients trust via
// NODE_EXTRA_CA_CERTS).
func (c *CA) CertPEM() []byte { return c.certPEM }

// Leaf returns the precomputed certificate for host. host must be one of
// the hosts EnsureCA was called with — there is no on-demand minting (see
// the package doc); the CA private key that would require was discarded
// when EnsureCA returned.
func (c *CA) Leaf(host string) (*tls.Certificate, error) {
	leaf, ok := c.leaves[host]
	if !ok {
		return nil, fmt.Errorf("mitm: no precomputed leaf for host %q (not in the configured upstream set)", host)
	}
	return leaf, nil
}

// chainPath returns where the precomputed leaf chain is stored on disk,
// beside pemPath: "spillway-ca.pem" -> "spillway-ca-leaves.json" in the
// same directory. A FileStore blob (internal/secrets/file.go) rather than
// a bare file, so persisting it reuses that file's existing
// chmod-before-content, atomic-rename, per-path-locked write path instead
// of a second hand-rolled writer. Only the per-host private keys inside it
// are secret — the leaf certs and the CA cert are public — but one file,
// one lock, one atomic write is simpler than splitting them, and 0600 on a
// mixed file costs nothing extra.
func chainPath(pemPath string) string {
	dir := filepath.Dir(pemPath)
	base := strings.TrimSuffix(filepath.Base(pemPath), filepath.Ext(pemPath))
	return filepath.Join(dir, base+"-leaves.json")
}

// EnsureCA loads or creates the install CA and a precomputed leaf for every
// host in hosts (internal/proxy.Handler's allowedHosts — see the package
// doc for why the full set is known up front). logger may be nil, in which
// case slog.Default() is used.
//
// The stored chain is reused, byte-for-byte, whenever it already covers
// every host in hosts: a restart with an unchanged host set — an ordinary
// upgrade, the #65 incident's scenario — never touches anything a client
// already trusts. Any host in hosts that has no stored leaf (a config
// change added an account or upstream) forces a full regeneration: a new
// CA, a new leaf for every host, all strand-risking exactly like any CA
// replacement. That is accepted, per the issue: it only happens on a
// deliberate config change that already needs a restart to take effect
// (see #46), never on an upgrade with an unchanged host set — the failure
// this issue actually exists to fix.
//
// Migration from a pre-#69 install (a pem on disk, a key in the keychain,
// no leaf manifest) is NOT a special case here — on purpose. It is handled
// as ordinary regeneration: no leaf manifest for the current host set is
// found, so one is minted, with pemExisted (an old pem present) upgrading
// the log line to a loud restart warning. #69's first draft considered
// reading the old keychain key forward instead, to reuse the exact CA cert
// and avoid a restart. That was rejected: the read would have to succeed
// at the moment of the very upgrade that installs this fix — precisely the
// situation #65's incident happened in — so depending on it here would
// keep the exact dependency this issue exists to remove, just delayed one
// release. A one-time forced regeneration, loudly, is a bounded and
// disclosed cost; a migration path that can still be silently starved by
// the keychain is not. The old keychain entry is simply never read again —
// orphaned, not deleted (deleting it on top of everything else is one more
// way for this to go wrong for no benefit).
//
// #65's rule still holds everywhere a stored-chain read can fail
// ambiguously (see the default case below): that is never treated as
// "absent", and never triggers a rotation of whatever is already on disk.
func EnsureCA(pemPath string, hosts []string, logger *slog.Logger) (*CA, error) {
	if logger == nil {
		logger = slog.Default()
	}
	wanted := normalizeHosts(hosts)

	store := secrets.NewFileStore(chainPath(pemPath))
	manifestBytes, manErr := store.GetRaw(leavesBlobName)
	certPEM, certErr := os.ReadFile(pemPath)
	_, statErr := os.Stat(pemPath)
	pemExisted := statErr == nil

	switch {
	case manErr == nil:
		manifest, perr := parseManifest(manifestBytes)
		if perr != nil {
			// Present but unparseable. Not "absent" — only spillway ever
			// writes this value; fail loudly rather than guessing.
			return nil, fmt.Errorf("mitm: stored MITM leaf chain is corrupt: %w", perr)
		}
		if certErr == nil {
			if ca, ok := chainFromManifest(certPEM, manifest, wanted); ok {
				return ca, nil // happy path: stored chain covers every wanted host
			}
		}
		// The pem is missing/corrupt, or the stored chain doesn't cover
		// every wanted host (a config change added one). Regenerate.
		return generateChain(store, pemPath, wanted, pemExisted, logger)

	case errors.Is(manErr, secrets.ErrNotFound):
		// No stored chain: first run, or a pre-#69 install (see the doc
		// comment above) — pemExisted controls which log line fires.
		return generateChain(store, pemPath, wanted, pemExisted, logger)

	default:
		// The manifest file exists but couldn't be read/parsed as a store
		// entry (disk error, corrupt JSON below the JSON layer). Not
		// "absent" — leave whatever is on disk untouched and fail loudly
		// instead of guessing (#65's rule, applied here).
		logger.Warn("mitm: stored MITM leaf chain unavailable, existing CA left untouched", "err", manErr)
		return nil, fmt.Errorf("mitm: stored MITM leaf chain unavailable, existing CA left untouched: %w", manErr)
	}
}

// normalizeHosts dedupes and sorts, so "does the stored chain cover hosts"
// and the log lines are both deterministic.
func normalizeHosts(hosts []string) []string {
	seen := make(map[string]bool, len(hosts))
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

func parseManifest(b []byte) (chainManifest, error) {
	var m chainManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return chainManifest{}, err
	}
	return m, nil
}

// chainFromManifest builds a CA from a stored manifest IF it covers every
// host in wanted and the CA cert half is present and parses. Anything
// short of that reports ok=false so the caller regenerates rather than
// serving a partial or mismatched chain.
func chainFromManifest(certPEM []byte, manifest chainManifest, wanted []string) (ca *CA, ok bool) {
	cert, err := parseCertPEM(certPEM)
	if err != nil {
		return nil, false
	}
	leaves := make(map[string]*tls.Certificate, len(wanted))
	for _, host := range wanted {
		rec, present := manifest.Leaves[host]
		if !present {
			return nil, false
		}
		leaf, err := leafFromRecord(rec, cert)
		if err != nil {
			return nil, false
		}
		leaves[host] = leaf
	}
	return &CA{cert: cert, certPEM: certPEM, leaves: leaves}, true
}

// leafFromRecord rebuilds a tls.Certificate from a stored leaf record,
// verifying it was actually signed by caCert — a leaf left over from a
// different CA cert generation (e.g. the pem rewritten out from under a
// stale manifest by some other path) must not be served as if it
// validates against the current trust anchor.
func leafFromRecord(rec leafRecord, caCert *x509.Certificate) (*tls.Certificate, error) {
	leafCert, err := parseCertPEM(rec.CertPEM)
	if err != nil {
		return nil, err
	}
	if err := leafCert.CheckSignatureFrom(caCert); err != nil {
		return nil, fmt.Errorf("leaf does not verify against the stored CA cert: %w", err)
	}
	tlsCert, err := tls.X509KeyPair(rec.CertPEM, rec.KeyPEM)
	if err != nil {
		return nil, err
	}
	// Serve the full chain: leaf then CA, matching what a client's
	// verifier expects to walk.
	tlsCert.Certificate = append(tlsCert.Certificate, caCert.Raw)
	return &tlsCert, nil
}

func parseCertPEM(certPEM []byte) (*x509.Certificate, error) {
	b, _ := pem.Decode(certPEM)
	if b == nil {
		return nil, errors.New("cert PEM undecodable")
	}
	return x509.ParseCertificate(b.Bytes)
}

// buildSelfSignedCert mints a fresh self-signed CA certificate for pub,
// signed by signer (which must correspond to pub).
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

// generateChain mints a brand-new CA key, a CA cert, and a leaf for every
// host in wanted, then lets the CA key fall out of scope — see EnsureCA's
// doc comment. It always regenerates the whole chain together: unlike the
// on-disk-key design this replaces, there is no persistent key to reuse
// across calls, so a generation is a fresh, unrelated CA every time —
// partial reuse (keeping some old leaves) would silently mix trust
// anchors, so none of it is reused.
func generateChain(store *secrets.FileStore, pemPath string, wanted []string, pemExisted bool, logger *slog.Logger) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	cert, der, err := buildSelfSignedCert(key.Public(), key)
	if err != nil {
		return nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	manifest := chainManifest{Leaves: make(map[string]leafRecord, len(wanted))}
	leaves := make(map[string]*tls.Certificate, len(wanted))
	for _, host := range wanted {
		leafCertPEM, leafKeyPEM, leafTLS, err := mintLeaf(cert, key, host)
		if err != nil {
			return nil, fmt.Errorf("mint leaf for %q: %w", host, err)
		}
		manifest.Leaves[host] = leafRecord{CertPEM: leafCertPEM, KeyPEM: leafKeyPEM}
		leaves[host] = leafTLS
	}
	// key falls out of scope here and is never referenced again. This is
	// the whole point of #69 (see the package doc): nothing left to
	// write, nothing left to keep, nothing that can fail to read on the
	// next start.

	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	if err := store.SetRaw(leavesBlobName, manifestBytes); err != nil {
		return nil, fmt.Errorf("store MITM leaf chain: %w", err)
	}
	if err := writeCertPEM(pemPath, certPEM); err != nil {
		return nil, err
	}

	if pemExisted {
		logger.Warn("mitm: regenerated the MITM CA — no stored leaf chain covered the current host set; "+
			"already-running proxied CLIs trust the OLD CA and will fail every TLS handshake until restarted",
			"fingerprint", fingerprint(cert), "hosts", wanted)
	} else {
		logger.Info("mitm: generated install CA", "fingerprint", fingerprint(cert), "hosts", wanted)
	}
	// pemExisted is exactly "something was already there to strand" — the
	// same test the log line above branches on (see the doc comment on the
	// Regenerated field).
	return &CA{cert: cert, certPEM: certPEM, leaves: leaves, Regenerated: pemExisted}, nil
}

// mintLeaf signs one host's leaf under caCert/caKey. Called only from
// generateChain, at startup, once per host in the wanted set — never on
// demand (see the package doc). IP hosts get an IP SAN; names get a DNS
// SAN.
func mintLeaf(caCert *x509.Certificate, caKey crypto.Signer, host string) (certPEM, keyPEM []byte, tlsCert *tls.Certificate, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, nil, err
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
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, key.Public(), caKey)
	if err != nil {
		return nil, nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, nil, err
	}
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	tlsCert = &tls.Certificate{Certificate: [][]byte{der, caCert.Raw}, PrivateKey: key}
	return certPEM, keyPEM, tlsCert, nil
}
