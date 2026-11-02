package main

import (
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"github.com/nikofeng/mongodb-ea-search/internal/pki"
)

const (
	caCertFile = "db-ca.pem"
	caKeyFile  = "db-ca.key"
)

// pkiGenerate creates (or reuses) a private CA in cfg.PKI.Dir and issues one
// leaf certificate per configured host. Reusing an existing CA lets the
// operator add hosts or rotate leaf certificates without changing the trust
// root that is already distributed to clients.
func pkiGenerate(c *Config) error {
	if err := os.MkdirAll(c.PKI.Dir, 0o700); err != nil {
		return fmt.Errorf("create pki directory: %w", err)
	}
	ca, err := loadOrCreateCA(c)
	if err != nil {
		return err
	}
	org := c.PKI.MemberAttrs["O"]
	ou := c.PKI.MemberAttrs["OU"]
	for _, h := range c.Hosts {
		dnsNames := []string{h.Hostname}
		if c.PKI.SAN.IncludeLocalhost {
			dnsNames = append(dnsNames, "localhost")
		}
		dnsNames = append(dnsNames, c.PKI.SAN.ExtraDNS...)
		var ips []net.IP
		for _, s := range c.PKI.SAN.ExtraIPs {
			if ip := net.ParseIP(s); ip != nil {
				ips = append(ips, ip)
			}
		}
		certPEM, keyPEM, err := ca.IssueLeaf(pki.LeafOptions{
			CommonName:         h.Hostname,
			DNSNames:           dnsNames,
			IPAddresses:        ips,
			Organization:       org,
			OrganizationalUnit: ou,
			KeyAlgorithm:       pki.KeyAlgorithm(c.PKI.KeyAlgorithm),
			KeySize:            c.PKI.KeySize,
			ValidityDays:       c.PKI.ValidityDays,
			ClientAuth:         true,
		})
		if err != nil {
			return fmt.Errorf("issue certificate for %s: %w", h.ProcessName, err)
		}
		if err = writeLeafFiles(c.PKI.Dir, h.ProcessName, certPEM, keyPEM); err != nil {
			return err
		}
		if cert, err := parseCertPEM(certPEM); err == nil {
			fmt.Printf("issued %s: %s\n", h.ProcessName, pki.Fingerprint(cert))
		}
	}
	fmt.Printf("CA certificate: %s\n", filepath.Join(c.PKI.Dir, caCertFile))
	fmt.Println("Keep", filepath.Join(c.PKI.Dir, caKeyFile), "private; it is never uploaded to a MongoDB host.")
	return nil
}

func loadOrCreateCA(c *Config) (*pki.CA, error) {
	certPath := filepath.Join(c.PKI.Dir, caCertFile)
	keyPath := filepath.Join(c.PKI.Dir, caKeyFile)
	if fileExists(certPath) && fileExists(keyPath) {
		certPEM, err := os.ReadFile(certPath)
		if err != nil {
			return nil, err
		}
		keyPEM, err := os.ReadFile(keyPath)
		if err != nil {
			return nil, err
		}
		fmt.Println("reusing existing CA at", certPath)
		return pki.LoadCA(certPEM, keyPEM)
	}
	ca, err := pki.GenerateCA(pki.CAOptions{
		Subject:      c.PKI.CASubject,
		KeyAlgorithm: pki.KeyAlgorithm(c.PKI.KeyAlgorithm),
		KeySize:      c.PKI.KeySize,
	})
	if err != nil {
		return nil, fmt.Errorf("generate CA: %w", err)
	}
	if err = writeFileOverwrite(certPath, ca.CertPEM, 0o444); err != nil {
		return nil, err
	}
	if err = writeFileOverwrite(keyPath, ca.KeyPEM, 0o600); err != nil {
		return nil, err
	}
	fmt.Println("generated new CA at", certPath)
	return ca, nil
}

func writeLeafFiles(dir, processName string, certPEM, keyPEM []byte) error {
	if err := writeFileOverwrite(filepath.Join(dir, processName+".crt"), certPEM, 0o444); err != nil {
		return err
	}
	if err := writeFileOverwrite(filepath.Join(dir, processName+".key"), keyPEM, 0o600); err != nil {
		return err
	}
	combined := pki.CombinePEM(certPEM, keyPEM)
	if err := writeFileOverwrite(filepath.Join(dir, processName+".pem"), combined, 0o400); err != nil {
		return err
	}
	return nil
}

// writeFileOverwrite writes path with the given mode, removing any
// pre-existing read-only file first so regenerating certificates does not
// fail with a permission error.
func writeFileOverwrite(path string, data []byte, mode os.FileMode) error {
	if fileExists(path) {
		if err := os.Remove(path); err != nil {
			return fmt.Errorf("remove existing %s: %w", path, err)
		}
	}
	return os.WriteFile(path, data, mode)
}

// pkiImport validates externally issued certificates against the configured
// CA and per-host hostnames, then copies normalized combined PEM files into
// cfg.PKI.Dir for distribution.
func pkiImport(c *Config) error {
	if err := os.MkdirAll(c.PKI.Dir, 0o700); err != nil {
		return fmt.Errorf("create pki directory: %w", err)
	}
	caPEM, err := os.ReadFile(c.PKI.CAFile)
	if err != nil {
		return fmt.Errorf("read pki.caFile: %w", err)
	}
	if err = writeFileOverwrite(filepath.Join(c.PKI.Dir, caCertFile), caPEM, 0o444); err != nil {
		return err
	}
	leaves := map[string]*pki.ImportedLeaf{}
	for _, h := range c.Hosts {
		entry := c.PKI.Certs[h.ProcessName]
		certPEM, err := os.ReadFile(entry.CertFile)
		if err != nil {
			return fmt.Errorf("read certFile for %s: %w", h.ProcessName, err)
		}
		keyPEM, err := os.ReadFile(entry.KeyFile)
		if err != nil {
			return fmt.Errorf("read keyFile for %s: %w", h.ProcessName, err)
		}
		leaf, err := pki.ValidateLeaf(certPEM, keyPEM, caPEM, h.Hostname, true)
		if err != nil {
			return fmt.Errorf("certificate for %s (%s): %w", h.ProcessName, h.Hostname, err)
		}
		if days := pki.DaysUntilExpiry(leaf.Cert); days < 30 {
			fmt.Printf("warning: certificate for %s expires in %d days\n", h.ProcessName, days)
		}
		leaves[h.ProcessName] = leaf
		if err = writeLeafFiles(c.PKI.Dir, h.ProcessName, certPEM, keyPEM); err != nil {
			return err
		}
		fmt.Printf("validated %s: %s\n", h.ProcessName, pki.Fingerprint(leaf.Cert))
	}
	if err = pki.ValidateMemberAttributes(leaves); err != nil {
		return err
	}
	fmt.Println("CA certificate:", filepath.Join(c.PKI.Dir, caCertFile))
	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func parseCertPEM(certPEM []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, fmt.Errorf("invalid certificate PEM")
	}
	return x509.ParseCertificate(block.Bytes)
}
