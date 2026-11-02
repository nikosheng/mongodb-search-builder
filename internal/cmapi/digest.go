package cmapi

import (
	"crypto/md5"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"strings"
)

// parseDigestChallenge extracts key="value" pairs from a WWW-Authenticate
// Digest header.
func parseDigestChallenge(header string) (map[string]string, error) {
	header = strings.TrimSpace(header)
	const prefix = "Digest "
	if !strings.HasPrefix(header, prefix) {
		return nil, fmt.Errorf("expected a Digest challenge, got %q", header)
	}
	fields := map[string]string{}
	rest := strings.TrimPrefix(header, prefix)
	for _, part := range splitDigestFields(rest) {
		kv := strings.SplitN(part, "=", 2)
		if len(kv) != 2 {
			continue
		}
		k := strings.TrimSpace(kv[0])
		v := strings.Trim(strings.TrimSpace(kv[1]), `"`)
		fields[k] = v
	}
	if fields["realm"] == "" || fields["nonce"] == "" {
		return nil, fmt.Errorf("digest challenge missing realm or nonce: %q", header)
	}
	return fields, nil
}

// splitDigestFields splits a comma-separated list of key="value" pairs while
// respecting quoted commas.
func splitDigestFields(s string) []string {
	var out []string
	var buf strings.Builder
	inQuotes := false
	for _, r := range s {
		switch r {
		case '"':
			inQuotes = !inQuotes
			buf.WriteRune(r)
		case ',':
			if inQuotes {
				buf.WriteRune(r)
			} else {
				out = append(out, buf.String())
				buf.Reset()
			}
		default:
			buf.WriteRune(r)
		}
	}
	if buf.Len() > 0 {
		out = append(out, buf.String())
	}
	return out
}

func md5hex(s string) string {
	h := md5.Sum([]byte(s))
	return hex.EncodeToString(h[:])
}

func randomCNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func mustPath(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	if u.RawQuery != "" {
		return u.Path + "?" + u.RawQuery
	}
	return u.Path
}

// buildDigestHeader implements RFC 7616 Digest authentication (qop=auth) as
// used by the Cloud Manager / Ops Manager Public API.
func buildDigestHeader(challenge, username, password, method, uri string, nc int) (string, error) {
	fields, err := parseDigestChallenge(challenge)
	if err != nil {
		return "", err
	}
	realm := fields["realm"]
	nonce := fields["nonce"]
	qop := fields["qop"]
	opaque := fields["opaque"]
	algorithm := fields["algorithm"]
	if algorithm == "" {
		algorithm = "MD5"
	}
	if algorithm != "MD5" {
		return "", fmt.Errorf("unsupported digest algorithm %q", algorithm)
	}
	cnonce, err := randomCNonce()
	if err != nil {
		return "", err
	}
	ha1 := md5hex(username + ":" + realm + ":" + password)
	ha2 := md5hex(method + ":" + uri)
	ncStr := fmt.Sprintf("%08x", nc)
	var response string
	if strings.Contains(qop, "auth") {
		response = md5hex(strings.Join([]string{ha1, nonce, ncStr, cnonce, "auth", ha2}, ":"))
	} else {
		response = md5hex(strings.Join([]string{ha1, nonce, ha2}, ":"))
	}
	var b strings.Builder
	fmt.Fprintf(&b, `Digest username="%s", realm="%s", nonce="%s", uri="%s", response="%s"`, username, realm, nonce, uri, response)
	if strings.Contains(qop, "auth") {
		fmt.Fprintf(&b, `, qop=auth, nc=%s, cnonce="%s"`, ncStr, cnonce)
	}
	if opaque != "" {
		fmt.Fprintf(&b, `, opaque="%s"`, opaque)
	}
	fmt.Fprintf(&b, `, algorithm=%s`, algorithm)
	return b.String(), nil
}
