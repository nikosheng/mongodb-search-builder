package pki

import (
	"context"
	"crypto/tls"
	"net"
	"testing"
	"time"
)

func TestGenerateCAAndLeaf(t *testing.T) {
	ca, err := GenerateCA(CAOptions{Subject: "Test CA", ValidityDays: 365})
	if err != nil {
		t.Fatal(err)
	}
	if !ca.Cert.IsCA {
		t.Fatal("generated certificate is not a CA")
	}
	certPEM, keyPEM, err := ca.IssueLeaf(LeafOptions{
		CommonName:         "mongo1.example.com",
		DNSNames:           []string{"mongo1.example.com"},
		Organization:       "ExampleCorp",
		OrganizationalUnit: "MongoDB Server",
		ValidityDays:       397,
	})
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := ValidateLeaf(certPEM, keyPEM, ca.CertPEM, "mongo1.example.com", false)
	if err != nil {
		t.Fatalf("validate leaf: %v", err)
	}
	if leaf.Organization != "ExampleCorp" || leaf.OrganizationalUnit != "MongoDB Server" {
		t.Fatalf("unexpected leaf attributes: %+v", leaf)
	}
	if _, err = ValidateLeaf(certPEM, keyPEM, ca.CertPEM, "other.example.com", false); err == nil {
		t.Fatal("expected hostname mismatch error")
	}
}

func TestIssueLeafRequiresSAN(t *testing.T) {
	ca, err := GenerateCA(CAOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = ca.IssueLeaf(LeafOptions{CommonName: "no-san"}); err == nil {
		t.Fatal("expected error for leaf without SAN")
	}
}

func TestValidateLeafWrongCA(t *testing.T) {
	ca1, _ := GenerateCA(CAOptions{Subject: "CA1"})
	ca2, _ := GenerateCA(CAOptions{Subject: "CA2"})
	certPEM, keyPEM, err := ca1.IssueLeaf(LeafOptions{CommonName: "host", DNSNames: []string{"host"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ValidateLeaf(certPEM, keyPEM, ca2.CertPEM, "host", false); err == nil {
		t.Fatal("expected chain validation failure against the wrong CA")
	}
}

func TestValidateLeafRequiresClientAuth(t *testing.T) {
	ca, _ := GenerateCA(CAOptions{})
	certPEM, keyPEM, err := ca.IssueLeaf(LeafOptions{CommonName: "host", DNSNames: []string{"host"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ValidateLeaf(certPEM, keyPEM, ca.CertPEM, "host", true); err == nil {
		t.Fatal("expected failure: certificate lacks clientAuth EKU")
	}
	certPEM, keyPEM, err = ca.IssueLeaf(LeafOptions{CommonName: "host", DNSNames: []string{"host"}, ClientAuth: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = ValidateLeaf(certPEM, keyPEM, ca.CertPEM, "host", true); err != nil {
		t.Fatalf("expected clientAuth certificate to validate: %v", err)
	}
}

func TestValidateMemberAttributes(t *testing.T) {
	ca, _ := GenerateCA(CAOptions{})
	mk := func(cn, org, ou string) *ImportedLeaf {
		certPEM, keyPEM, err := ca.IssueLeaf(LeafOptions{CommonName: cn, DNSNames: []string{cn}, Organization: org, OrganizationalUnit: ou})
		if err != nil {
			t.Fatal(err)
		}
		leaf, err := ValidateLeaf(certPEM, keyPEM, ca.CertPEM, cn, false)
		if err != nil {
			t.Fatal(err)
		}
		return leaf
	}
	ok := map[string]*ImportedLeaf{"a": mk("a", "Org", "OU"), "b": mk("b", "Org", "OU")}
	if err := ValidateMemberAttributes(ok); err != nil {
		t.Fatalf("expected matching attributes to pass: %v", err)
	}
	mismatched := map[string]*ImportedLeaf{"a": mk("a", "Org", "OU"), "b": mk("b", "Other", "OU")}
	if err := ValidateMemberAttributes(mismatched); err == nil {
		t.Fatal("expected mismatched attributes to fail")
	}
}

func TestCombinePEM(t *testing.T) {
	combined := CombinePEM([]byte("CERT"), []byte("KEY"))
	if string(combined) != "CERT\nKEY" {
		t.Fatalf("unexpected combined PEM: %q", combined)
	}
}

func TestVerifyHandshake(t *testing.T) {
	ca, err := GenerateCA(CAOptions{})
	if err != nil {
		t.Fatal(err)
	}
	certPEM, keyPEM, err := ca.IssueLeaf(LeafOptions{CommonName: "127.0.0.1", DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}})
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{pair}})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 1)
		conn.Read(buf)
	}()
	res := VerifyHandshake(context.Background(), ln.Addr().String(), ca.CertPEM, 2*time.Second)
	if res.Err != nil || !res.Verified {
		t.Fatalf("expected successful handshake, got %+v", res)
	}
}

func TestLoadCARejectsNonCACert(t *testing.T) {
	ca, _ := GenerateCA(CAOptions{})
	certPEM, keyPEM, err := ca.IssueLeaf(LeafOptions{CommonName: "host", DNSNames: []string{"host"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = LoadCA(certPEM, keyPEM); err == nil {
		t.Fatal("expected error loading a non-CA certificate as a CA")
	}
}
