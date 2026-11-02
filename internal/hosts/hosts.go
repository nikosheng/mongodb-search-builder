// Package hosts copies TLS certificate files to MongoDB and mongos hosts
// over SSH, using an inventory that maps each automation-config process name
// to a host, SSH credentials, and a target directory.
package hosts

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// Entry describes how to reach one MongoDB/mongos host and where its TLS
// files should be placed.
type Entry struct {
	ProcessName string
	Hostname    string
	SSHUser     string
	SSHPort     int
	SSHKeyFile  string
	TargetDir   string
	Owner       string // "user:group", applied with chown if the SSH user can
	Become      string // "" | "sudo"
}

// File is one local file to place on a remote host.
type File struct {
	LocalPath  string
	RemoteName string
	Mode       os.FileMode
}

// Result reports the outcome of distributing files to one host.
type Result struct {
	ProcessName string
	Hostname    string
	Uploaded    []string
	Skipped     []string
	Err         error
}

// Dialer creates SSH client connections; production code uses DialSSH, and
// tests can substitute an in-process server dialer.
type Dialer func(entry Entry, timeout time.Duration) (*ssh.Client, error)

// DialSSH connects to entry.Hostname:entry.SSHPort using the configured key
// file, with host key verification against the user's known_hosts file.
func DialSSH(entry Entry, timeout time.Duration) (*ssh.Client, error) {
	keyBytes, err := os.ReadFile(entry.SSHKeyFile)
	if err != nil {
		return nil, fmt.Errorf("read SSH key %s: %w", entry.SSHKeyFile, err)
	}
	signer, err := ssh.ParsePrivateKey(keyBytes)
	if err != nil {
		return nil, fmt.Errorf("parse SSH key %s: %w", entry.SSHKeyFile, err)
	}
	hostKeyCallback, err := knownHostsCallback()
	if err != nil {
		return nil, err
	}
	port := entry.SSHPort
	if port == 0 {
		port = 22
	}
	cfg := &ssh.ClientConfig{
		User:            entry.SSHUser,
		Auth:            []ssh.AuthMethod{ssh.PublicKeys(signer)},
		HostKeyCallback: hostKeyCallback,
		Timeout:         timeout,
	}
	addr := net.JoinHostPort(entry.Hostname, fmt.Sprintf("%d", port))
	client, err := ssh.Dial("tcp", addr, cfg)
	if err != nil {
		return nil, fmt.Errorf("SSH dial %s: %w", addr, err)
	}
	return client, nil
}

func knownHostsCallback() (ssh.HostKeyCallback, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("determine home directory for known_hosts: %w", err)
	}
	return knownHostsCallbackFromFile(path.Join(home, ".ssh", "known_hosts"))
}

// Distributor copies files to hosts over SSH using an exec-based transfer
// (cat > path via a remote shell command), which avoids requiring an SFTP
// subsystem on the remote host.
type Distributor struct {
	Dial    Dialer
	Timeout time.Duration
}

// NewDistributor returns a Distributor using real SSH connections.
func NewDistributor(timeout time.Duration) *Distributor {
	if timeout == 0 {
		timeout = 20 * time.Second
	}
	return &Distributor{Dial: DialSSH, Timeout: timeout}
}

// Distribute uploads files to one host and applies ownership/permissions.
// It is idempotent: if the remote file already has identical content, it is
// skipped.
func (d *Distributor) Distribute(entry Entry, files []File) Result {
	res := Result{ProcessName: entry.ProcessName, Hostname: entry.Hostname}
	client, err := d.Dial(entry, d.Timeout)
	if err != nil {
		res.Err = err
		return res
	}
	defer client.Close()

	if entry.TargetDir != "" {
		if err = runRemote(client, becomeCmd(entry, fmt.Sprintf("mkdir -p %s", shellQuote(entry.TargetDir)))); err != nil {
			res.Err = fmt.Errorf("create target directory: %w", err)
			return res
		}
	}

	for _, f := range files {
		local, err := os.ReadFile(f.LocalPath)
		if err != nil {
			res.Err = fmt.Errorf("read local file %s: %w", f.LocalPath, err)
			return res
		}
		remotePath := path.Join(entry.TargetDir, f.RemoteName)
		same, err := remoteContentMatches(client, entry, remotePath, local)
		if err != nil {
			res.Err = fmt.Errorf("check existing remote file %s: %w", remotePath, err)
			return res
		}
		if same {
			res.Skipped = append(res.Skipped, remotePath)
			continue
		}
		if err = uploadFile(client, entry, remotePath, local, f.Mode); err != nil {
			res.Err = fmt.Errorf("upload %s: %w", remotePath, err)
			return res
		}
		res.Uploaded = append(res.Uploaded, remotePath)
	}

	if entry.Owner != "" {
		for _, f := range files {
			remotePath := path.Join(entry.TargetDir, f.RemoteName)
			if err = runRemote(client, becomeCmd(entry, fmt.Sprintf("chown %s %s", shellQuote(entry.Owner), shellQuote(remotePath)))); err != nil {
				res.Err = fmt.Errorf("chown %s: %w", remotePath, err)
				return res
			}
		}
	}
	return res
}

func becomeCmd(entry Entry, cmd string) string {
	if entry.Become == "sudo" {
		return "sudo " + cmd
	}
	return cmd
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func runRemote(client *ssh.Client, cmd string) error {
	session, err := client.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()
	var stderr bytes.Buffer
	session.Stderr = &stderr
	if err = session.Run(cmd); err != nil {
		return fmt.Errorf("%s: %w: %s", cmd, err, stderr.String())
	}
	return nil
}

func remoteContentMatches(client *ssh.Client, entry Entry, remotePath string, local []byte) (bool, error) {
	session, err := client.NewSession()
	if err != nil {
		return false, err
	}
	defer session.Close()
	var out bytes.Buffer
	session.Stdout = &out
	cmd := becomeCmd(entry, fmt.Sprintf("sha256sum %s 2>/dev/null || true", shellQuote(remotePath)))
	if err = session.Run(cmd); err != nil {
		// A nonzero exit here (without "|| true" succeeding) means the file
		// is absent; treat that as "not matching" rather than an error.
		return false, nil
	}
	fields := strings.Fields(out.String())
	if len(fields) == 0 {
		return false, nil
	}
	remoteSum := fields[0]
	sum := sha256.Sum256(local)
	return remoteSum == hex.EncodeToString(sum[:]), nil
}

func uploadFile(client *ssh.Client, entry Entry, remotePath string, content []byte, mode os.FileMode) error {
	session, err := client.NewSession()
	if err != nil {
		return err
	}
	defer session.Close()
	if mode == 0 {
		mode = 0o400
	}
	stdin, err := session.StdinPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	session.Stderr = &stderr
	tmpPath := remotePath + ".tmp-mongodb-tls"
	cmd := becomeCmd(entry, fmt.Sprintf("sh -c 'cat > %s && chmod %o %s && mv -f %s %s'", shellQuote(tmpPath), mode, shellQuote(tmpPath), shellQuote(tmpPath), shellQuote(remotePath)))
	if err = session.Start(cmd); err != nil {
		return err
	}
	if _, err = io.Copy(stdin, bytes.NewReader(content)); err != nil {
		stdin.Close()
		return err
	}
	stdin.Close()
	if err = session.Wait(); err != nil {
		return fmt.Errorf("%w: %s", err, stderr.String())
	}
	return nil
}
