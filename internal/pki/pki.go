// Package pki creates and validates the certificate authority and leaf
// certificates used to enable TLS on a MongoDB replica set or sharded
// cluster. It supports two workflows: generating a private CA and member
// certificates for test or self-managed PKI environments, and validating
// certificates issued by an external CA before they are distributed to
// MongoDB hosts.
package pki

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"strings"
	"time"
)

// KeyAlgorithm selects the private key algorithm used when generating
// certificates.
type KeyAlgorithm string

const (
	RSA   KeyAlgorithm = "rsa"
	ECDSA KeyAlgorithm = "ecdsa"
)

// CAOptions configures a generated certificate authority.
type CAOptions struct {
	Subject      string
	KeyAlgorithm KeyAlgorithm
	KeySize      int // RSA only; ignored for ECDSA (uses P-256)
	ValidityDays int
}

// CA holds a generated or loaded certificate authority.
type CA struct {
	Cert    *x509.Certificate
	CertPEM []byte
	Key     crypto.Signer
	KeyPEM  []byte
}

// LeafOptions configures a single server (and optionally client) certificate
// issued by a CA for one MongoDB process.
type LeafOptions struct {
	CommonName         string
	DNSNames           []string
	IPAddresses        []net.IP
	Organization       string
	OrganizationalUnit string
	KeyAlgorithm       KeyAlgorithm
	KeySize            int
	ValidityDays       int
	ClientAuth         bool // also sets ExtKeyUsageClientAuth, needed for intra-cluster mTLS
}

func generateKey(alg KeyAlgorithm, size int) (crypto.Signer, error) {
	switch alg {
	case "", RSA:
		if size == 0 {
			size = 3072
		}
		return rsa.GenerateKey(rand.Reader, size)
	case ECDSA:
		return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	default:
		return nil, fmt.Errorf("unsupported key algorithm %q", alg)
	}
}

func marshalKey(key crypto.Signer) ([]byte, error) {
	switch k := key.(type) {
	case *rsa.PrivateKey:
		return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(k)}), nil
	case *ecdsa.PrivateKey:
		b, err := x509.MarshalECPrivateKey(k)
		if err != nil {
			return nil, err
		}
		return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: b}), nil
	default:
		return nil, fmt.Errorf("unsupported key type %T", key)
	}
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	return rand.Int(rand.Reader, limit)
}

// GenerateCA creates a new self-signed certificate authority suitable for
// signing MongoDB server certificates. The returned CA.KeyPEM must be kept
// private and must never be placed on a MongoDB host.
func GenerateCA(opts CAOptions) (*CA, error) {
	if opts.Subject == "" {
		opts.Subject = "MongoDB TLS CA"
	}
	if opts.ValidityDays == 0 {
		opts.ValidityDays = 3650
	}
	key, err := generateKey(opts.KeyAlgorithm, opts.KeySize)
	if err != nil {
		return nil, fmt.Errorf("generate CA key: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: opts.Subject},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.AddDate(0, 0, opts.ValidityDays),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, key.Public(), key)
	if err != nil {
		return nil, fmt.Errorf("create CA certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	keyPEM, err := marshalKey(key)
	if err != nil {
		return nil, err
	}
	return &CA{
		Cert:    cert,
		CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		Key:     key,
		KeyPEM:  keyPEM,
	}, nil
}

// LoadCA parses a previously generated or externally issued CA certificate
// and private key, for example when resuming a rollout or rotating leaf
// certificates against an existing CA.
func LoadCA(certPEM, keyPEM []byte) (*CA, error) {
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		return nil, fmt.Errorf("invalid CA certificate PEM")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse CA certificate: %w", err)
	}
	if !cert.IsCA {
		return nil, fmt.Errorf("certificate is not a CA (basicConstraints CA:FALSE)")
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, fmt.Errorf("invalid CA key PEM")
	}
	key, err := parsePrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse CA key: %w", err)
	}
	return &CA{Cert: cert, CertPEM: certPEM, Key: key, KeyPEM: keyPEM}, nil
}

func parsePrivateKey(der []byte) (crypto.Signer, error) {
	if k, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return k, nil
	}
	if k, err := x509.ParseECPrivateKey(der); err == nil {
		return k, nil
	}
	if k, err := x509.ParsePKCS8PrivateKey(der); err == nil {
		if signer, ok := k.(crypto.Signer); ok {
			return signer, nil
		}
	}
	return nil, fmt.Errorf("unrecognized private key format")
}

// IssueLeaf signs a new server (and optionally client) certificate for one
// MongoDB process. The returned PEM files satisfy MongoDB's member
// certificate requirements: a single issuing CA, a Subject Alternative Name
// matching the process hostname, and an Extended Key Usage of serverAuth
// (and clientAuth when ClientAuth is set).
func (ca *CA) IssueLeaf(opts LeafOptions) (certPEM, keyPEM []byte, err error) {
	if opts.CommonName == "" {
		return nil, nil, fmt.Errorf("leaf certificate requires a common name")
	}
	if len(opts.DNSNames) == 0 && len(opts.IPAddresses) == 0 {
		return nil, nil, fmt.Errorf("leaf certificate for %q requires at least one DNS name or IP address", opts.CommonName)
	}
	if opts.ValidityDays == 0 {
		opts.ValidityDays = 397
	}
	key, err := generateKey(opts.KeyAlgorithm, opts.KeySize)
	if err != nil {
		return nil, nil, fmt.Errorf("generate key for %s: %w", opts.CommonName, err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}
	eku := []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	if opts.ClientAuth {
		eku = append(eku, x509.ExtKeyUsageClientAuth)
	}
	subject := pkix.Name{CommonName: opts.CommonName}
	if opts.Organization != "" {
		subject.Organization = []string{opts.Organization}
	}
	if opts.OrganizationalUnit != "" {
		subject.OrganizationalUnit = []string{opts.OrganizationalUnit}
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               subject,
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.AddDate(0, 0, opts.ValidityDays),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           eku,
		BasicConstraintsValid: true,
		DNSNames:              opts.DNSNames,
		IPAddresses:           opts.IPAddresses,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, key.Public(), ca.Key)
	if err != nil {
		return nil, nil, fmt.Errorf("sign certificate for %s: %w", opts.CommonName, err)
	}
	keyPEM, err = marshalKey(key)
	if err != nil {
		return nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	return certPEM, keyPEM, nil
}

// CombinePEM concatenates a certificate and key into the single PEM file
// MongoDB's certificateKeyFile setting expects.
func CombinePEM(certPEM, keyPEM []byte) []byte {
	out := make([]byte, 0, len(certPEM)+len(keyPEM)+1)
	out = append(out, certPEM...)
	if len(certPEM) > 0 && certPEM[len(certPEM)-1] != '\n' {
		out = append(out, '\n')
	}
	out = append(out, keyPEM...)
	return out
}

// ImportedLeaf describes a validated, externally issued member certificate.
type ImportedLeaf struct {
	Cert               *x509.Certificate
	CertPEM            []byte
	KeyPEM             []byte
	Organization       string
	OrganizationalUnit string
}

// ValidateLeaf checks that a certificate and key belong together, chain to
// the given CA, carry the required Extended Key Usage, and (when hostname is
// non-empty) are valid for that hostname. It mirrors the checks MongoDB
// itself performs when `net.tls.certificateKeyFile` is loaded.
func ValidateLeaf(certPEM, keyPEM, caPEM []byte, hostname string, requireClientAuth bool) (*ImportedLeaf, error) {
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("certificate and key do not match: %w", err)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, fmt.Errorf("parse certificate: %w", err)
	}
	if hostname != "" {
		if err = leaf.VerifyHostname(hostname); err != nil {
			return nil, fmt.Errorf("certificate is not valid for hostname %q: %w", hostname, err)
		}
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("invalid CA certificate PEM")
	}
	intermediates := x509.NewCertPool()
	for _, der := range pair.Certificate[1:] {
		ic, e := x509.ParseCertificate(der)
		if e != nil {
			return nil, fmt.Errorf("parse intermediate certificate: %w", e)
		}
		intermediates.AddCert(ic)
	}
	if _, err = leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		return nil, fmt.Errorf("certificate does not chain to the provided CA for server authentication: %w", err)
	}
	if requireClientAuth && len(leaf.ExtKeyUsage) > 0 && !hasExtKeyUsage(leaf.ExtKeyUsage, x509.ExtKeyUsageClientAuth) {
		return nil, fmt.Errorf("certificate is missing the clientAuth extended key usage required for this deployment")
	}
	var org, ou string
	if len(leaf.Subject.Organization) > 0 {
		org = leaf.Subject.Organization[0]
	}
	if len(leaf.Subject.OrganizationalUnit) > 0 {
		ou = leaf.Subject.OrganizationalUnit[0]
	}
	return &ImportedLeaf{Cert: leaf, CertPEM: certPEM, KeyPEM: keyPEM, Organization: org, OrganizationalUnit: ou}, nil
}

func hasExtKeyUsage(usages []x509.ExtKeyUsage, want x509.ExtKeyUsage) bool {
	for _, u := range usages {
		if u == want || u == x509.ExtKeyUsageAny {
			return true
		}
	}
	return false
}

// ValidateMemberAttributes checks that every member certificate shares the
// same Organization and Organizational Unit, as MongoDB requires for
// replica-set and sharded-cluster member certificates signed by one CA.
func ValidateMemberAttributes(leaves map[string]*ImportedLeaf) error {
	var wantOrg, wantOU string
	var first string
	for name, leaf := range leaves {
		if first == "" {
			first, wantOrg, wantOU = name, leaf.Organization, leaf.OrganizationalUnit
			continue
		}
		if leaf.Organization != wantOrg || leaf.OrganizationalUnit != wantOU {
			return fmt.Errorf("certificate for %q has O=%q OU=%q, which does not match %q (O=%q OU=%q); all member certificates must share identical attributes",
				name, leaf.Organization, leaf.OrganizationalUnit, first, wantOrg, wantOU)
		}
	}
	return nil
}

// DaysUntilExpiry returns the number of whole days remaining before cert
// expires, which can be negative for already-expired certificates.
func DaysUntilExpiry(cert *x509.Certificate) int {
	return int(time.Until(cert.NotAfter).Hours() / 24)
}

// Fingerprint returns a short, human-readable identity summary for logging.
func Fingerprint(cert *x509.Certificate) string {
	return fmt.Sprintf("CN=%s SAN=%s expires=%s", cert.Subject.CommonName, strings.Join(append(cert.DNSNames, ipStrings(cert.IPAddresses)...), ","), cert.NotAfter.Format(time.RFC3339))
}

func ipStrings(ips []net.IP) []string {
	out := make([]string, len(ips))
	for i, ip := range ips {
		out[i] = ip.String()
	}
	return out
}
