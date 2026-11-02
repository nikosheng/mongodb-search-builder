package pki

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net"
	"strings"
	"time"
)

// HandshakeResult reports the outcome of a TLS connectivity check against
// one MongoDB (or mongos) endpoint.
type HandshakeResult struct {
	Address    string
	Verified   bool
	PeerCommon string
	SANs       []string
	NotAfter   time.Time
	Err        error
}

// VerifyHandshake dials addr ("host:port") with TLS, trusting only caPEM,
// and reports whether the handshake and certificate validation succeeded.
// It does not present a client certificate, matching how mongot connects to
// MongoDB when allowConnectionsWithoutCertificates is enabled.
func VerifyHandshake(ctx context.Context, addr string, caPEM []byte, timeout time.Duration) HandshakeResult {
	res := HandshakeResult{Address: addr}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		res.Err = fmt.Errorf("invalid CA certificate PEM")
		return res
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		res.Err = fmt.Errorf("address must be host:port: %w", err)
		return res
	}
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	dialer := tls.Dialer{NetDialer: &net.Dialer{Timeout: timeout}, Config: &tls.Config{RootCAs: roots, ServerName: host}}
	rawConn, err := dialer.DialContext(dctx, "tcp", addr)
	if err != nil {
		res.Err = fmt.Errorf("TLS handshake failed: %w", err)
		return res
	}
	conn := rawConn.(*tls.Conn)
	defer conn.Close()
	state := conn.ConnectionState()
	if len(state.PeerCertificates) == 0 {
		res.Err = fmt.Errorf("server presented no certificate")
		return res
	}
	leaf := state.PeerCertificates[0]
	res.Verified = true
	res.PeerCommon = leaf.Subject.CommonName
	res.SANs = append(append([]string{}, leaf.DNSNames...), ipStrings(leaf.IPAddresses)...)
	res.NotAfter = leaf.NotAfter
	return res
}

// Summary renders a one-line human-readable result for CLI output.
func (r HandshakeResult) Summary() string {
	if r.Err != nil {
		return fmt.Sprintf("%s: FAILED (%v)", r.Address, r.Err)
	}
	daysLeft := int(time.Until(r.NotAfter).Hours() / 24)
	return fmt.Sprintf("%s: ok, CN=%s SAN=%s expires in %d days", r.Address, r.PeerCommon, strings.Join(r.SANs, ","), daysLeft)
}
