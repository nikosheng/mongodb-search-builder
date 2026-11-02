package hosts

import (
	"crypto/rand"
	"crypto/rsa"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// startTestSSHServer runs a minimal SSH server that executes every "exec"
// request with the local shell. This is only safe for tests: production
// Dialer (DialSSH) always verifies the host key against known_hosts and
// never shells out locally.
func startTestSSHServer(t *testing.T) (addr string, cleanup func()) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{NoClientAuth: true}
	cfg.AddHostKey(signer)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go handleConn(t, conn, cfg)
		}
	}()
	return ln.Addr().String(), func() {
		ln.Close()
		close(done)
	}
}

func handleConn(t *testing.T, conn net.Conn, cfg *ssh.ServerConfig) {
	sconn, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	defer sconn.Close()
	go ssh.DiscardRequests(reqs)
	for newChannel := range chans {
		if newChannel.ChannelType() != "session" {
			newChannel.Reject(ssh.UnknownChannelType, "unsupported channel type")
			continue
		}
		channel, requests, err := newChannel.Accept()
		if err != nil {
			continue
		}
		go handleSession(channel, requests)
	}
}

func handleSession(channel ssh.Channel, requests <-chan *ssh.Request) {
	defer channel.Close()
	for req := range requests {
		if req.Type != "exec" {
			req.Reply(false, nil)
			continue
		}
		cmd := parseExecPayload(req.Payload)
		req.Reply(true, nil)
		c := exec.Command("sh", "-c", cmd)
		c.Stdin = channel
		c.Stdout = channel
		c.Stderr = channel.Stderr()
		err := c.Run()
		code := 0
		if exitErr, ok := err.(*exec.ExitError); ok {
			code = exitErr.ExitCode()
		} else if err != nil {
			code = 1
		}
		channel.SendRequest("exit-status", false, exitStatusPayload(code))
		channel.Close()
		return
	}
}

func parseExecPayload(payload []byte) string {
	if len(payload) < 4 {
		return ""
	}
	n := int(payload[0])<<24 | int(payload[1])<<16 | int(payload[2])<<8 | int(payload[3])
	if 4+n > len(payload) {
		return string(payload[4:])
	}
	return string(payload[4 : 4+n])
}

func exitStatusPayload(code int) []byte {
	b := make([]byte, 4)
	b[0] = byte(code >> 24)
	b[1] = byte(code >> 16)
	b[2] = byte(code >> 8)
	b[3] = byte(code)
	return b
}

func testDialer(addr string) Dialer {
	return func(entry Entry, timeout time.Duration) (*ssh.Client, error) {
		conn, err := net.DialTimeout("tcp", addr, timeout)
		if err != nil {
			return nil, err
		}
		cfg := &ssh.ClientConfig{User: entry.SSHUser, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: timeout}
		c, chans, reqs, err := ssh.NewClientConn(conn, addr, cfg)
		if err != nil {
			return nil, err
		}
		return ssh.NewClient(c, chans, reqs), nil
	}
}

func writeTempFile(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "src-*.pem")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err = io.WriteString(f, content); err != nil {
		t.Fatal(err)
	}
	return f.Name()
}

func TestDistributeUploadsAndIsIdempotent(t *testing.T) {
	addr, cleanup := startTestSSHServer(t)
	defer cleanup()
	remoteDir := t.TempDir()
	src := writeTempFile(t, "cert-and-key-contents\n")

	d := &Distributor{Dial: testDialer(addr), Timeout: 5 * time.Second}
	entry := Entry{ProcessName: "rs0_1", Hostname: "mongo1.example.com", SSHUser: "mongod", TargetDir: remoteDir}
	files := []File{{LocalPath: src, RemoteName: "rs0_1.pem", Mode: 0o400}}

	res := d.Distribute(entry, files)
	if res.Err != nil {
		t.Fatalf("first distribute failed: %v", res.Err)
	}
	if len(res.Uploaded) != 1 || len(res.Skipped) != 0 {
		t.Fatalf("expected one upload, got uploaded=%v skipped=%v", res.Uploaded, res.Skipped)
	}
	remotePath := filepath.Join(remoteDir, "rs0_1.pem")
	got, err := os.ReadFile(remotePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "cert-and-key-contents\n" {
		t.Fatalf("unexpected remote content: %q", got)
	}
	info, err := os.Stat(remotePath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o400 {
		t.Fatalf("expected mode 0400, got %v", info.Mode().Perm())
	}

	// Second distribution of identical content should be skipped, not re-uploaded.
	res2 := d.Distribute(entry, files)
	if res2.Err != nil {
		t.Fatalf("second distribute failed: %v", res2.Err)
	}
	if len(res2.Uploaded) != 0 || len(res2.Skipped) != 1 {
		t.Fatalf("expected the second distribute to skip the unchanged file, got uploaded=%v skipped=%v", res2.Uploaded, res2.Skipped)
	}
}

func TestDistributeDetectsChangedContent(t *testing.T) {
	addr, cleanup := startTestSSHServer(t)
	defer cleanup()
	remoteDir := t.TempDir()
	src := writeTempFile(t, "version-1\n")
	d := &Distributor{Dial: testDialer(addr), Timeout: 5 * time.Second}
	entry := Entry{ProcessName: "rs0_1", Hostname: "mongo1.example.com", SSHUser: "mongod", TargetDir: remoteDir}
	files := []File{{LocalPath: src, RemoteName: "rs0_1.pem"}}
	if res := d.Distribute(entry, files); res.Err != nil {
		t.Fatal(res.Err)
	}
	if err := os.WriteFile(src, []byte("version-2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := d.Distribute(entry, files)
	if res.Err != nil {
		t.Fatal(res.Err)
	}
	if len(res.Uploaded) != 1 {
		t.Fatalf("expected changed content to be re-uploaded, got %+v", res)
	}
	got, err := os.ReadFile(filepath.Join(remoteDir, "rs0_1.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "version-2\n" {
		t.Fatalf("remote file was not updated: %q", got)
	}
}

func TestDistributeMissingLocalFile(t *testing.T) {
	addr, cleanup := startTestSSHServer(t)
	defer cleanup()
	d := &Distributor{Dial: testDialer(addr), Timeout: 5 * time.Second}
	entry := Entry{ProcessName: "rs0_1", Hostname: "h", SSHUser: "mongod", TargetDir: t.TempDir()}
	res := d.Distribute(entry, []File{{LocalPath: "/nonexistent/path.pem", RemoteName: "x.pem"}})
	if res.Err == nil {
		t.Fatal("expected an error for a missing local file")
	}
}

func TestDistributeDialFailure(t *testing.T) {
	d := &Distributor{Dial: func(entry Entry, timeout time.Duration) (*ssh.Client, error) {
		return nil, net.ErrClosed
	}, Timeout: time.Second}
	res := d.Distribute(Entry{ProcessName: "p", Hostname: "h"}, nil)
	if res.Err == nil {
		t.Fatal("expected dial failure to be reported")
	}
}
