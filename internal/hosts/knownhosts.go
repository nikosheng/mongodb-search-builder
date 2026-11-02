package hosts

import (
	"fmt"
	"os"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// knownHostsCallbackFromFile builds a strict host-key callback from a
// known_hosts file. It errors clearly if the file is missing, since this
// tool should never silently trust an unknown host.
func knownHostsCallbackFromFile(path string) (ssh.HostKeyCallback, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("known_hosts file %s not found; add the target hosts with 'ssh-keyscan' before running mongodb-tls: %w", path, err)
	}
	cb, err := knownhosts.New(path)
	if err != nil {
		return nil, fmt.Errorf("parse known_hosts file %s: %w", path, err)
	}
	return cb, nil
}
