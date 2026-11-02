# mongodb-tls: Enable TLS on an Existing Replica Set

`mongodb-tls` is a companion CLI to `mongodb-search`. It automates enabling
TLS on a MongoDB replica set that is already managed by Cloud Manager or Ops
Manager: generating or importing a certificate authority and per-host
certificates, distributing them to each MongoDB host over SSH, and driving a
staged `allowTLS` → `preferTLS` → `requireTLS` rollout through the Automation
API. It finishes by printing the CA file you use as `external.databaseCAFile`
in `mongodb-search`.

**Phase 1 scope:** a single replica set. Sharded-cluster support (config
servers, shards, and `mongos` routers) is planned for a later release and is
currently rejected with a clear error.

This tool does not install the MongoDB Agent, does not take backups on your
behalf, and does not change MongoDB user authentication; it only changes
`net.tls` transport settings. Take a current backup and confirm your Cloud
Manager or Ops Manager project has **Project Automation Admin** access
before you run it against production.

## Prerequisites

- An existing MongoDB Enterprise replica set, version 8.3 or later,
  registered in a Cloud Manager or Ops Manager project with Automation.
- A Cloud Manager or Ops Manager [programmatic API key](https://www.mongodb.com/docs/cloud-manager/reference/api/apiKeys/) (public/private key
  pair) with Project Automation Admin on that project.
- SSH access to every MongoDB host, as a user that can write to the target
  TLS directory (directly or via `sudo`), with that user's public key already
  authorized and the host already present in your `~/.ssh/known_hosts` file
  (`ssh-keyscan -H <host> >> ~/.ssh/known_hosts`).
- Go 1.24+ to build the CLI, from the repository root:

  ```sh
  go build -o mongodb-tls ./cmd/mongodb-tls
  ```

## 1. Create the configuration

```sh
cp examples/mongodb-tls.yaml mongodb-tls.yaml
```

Edit `mongodb-tls.yaml`: set `cloudManager.url`/`orgId`/`projectId`/`publicKey`,
export the private API key (`export MDB_CM_API_PRIVATE_KEY=...`) rather than
inlining it, set `deployment.name` to the replica set `_id` exactly as shown
in Cloud/Ops Manager, and list every member under `hosts` with its
`processName` matching the automation config's process name (visible via
`mongodb-tls plan`, see below), its SSH access, and its target TLS directory.

`tls.caFileOnHosts` and `tls.certFileOnHostsTemplate` must resolve to the
same directory as each host's `targetDir`; the CLI validates this and
rejects a mismatched configuration before it does anything.

## 2. Plan (read-only)

```sh
./mongodb-tls plan -config mongodb-tls.yaml
```

This fetches the automation config, matches each `hosts[]` entry to a known
process, and prints the certificate and CA paths it will use. It makes no
changes. Fix any `NOT FOUND in automation config` lines before continuing;
they usually mean `processName` doesn't match Cloud/Ops Manager's naming.

## 3. Create or import certificates

For a test deployment or when you manage your own PKI, generate a private CA
and one certificate per host:

```sh
./mongodb-tls pki generate -config mongodb-tls.yaml
```

Certificates are written to `pki/` (configurable via `pki.dir`): `db-ca.pem`
(public CA), `db-ca.key` (keep this off every MongoDB host), and one
`<processName>.pem` per host (combined certificate and key for
`net.tls.certificateKeyFile`). Re-running `pki generate` reuses the existing
CA and reissues leaf certificates, so you can add hosts or rotate leaf
certificates without re-trusting a new CA everywhere.

If your organization already has a CA, issue one certificate per MongoDB
host from it instead (same CA, same `O`/`OU`, each with a `subjectAltName`
matching that host's `hostname`, `extendedKeyUsage = serverAuth, clientAuth`).
Then set `pki.mode: import`, point `pki.caFile` at the CA certificate, and
list each host's certificate and key under `pki.certs`:

```sh
./mongodb-tls pki import -config mongodb-tls.yaml
```

`pki import` validates that every certificate matches its key, chains to the
CA, is valid for its host's `hostname`, carries `serverAuth`/`clientAuth`,
and that all hosts share the same `O`/`OU`, then copies normalized files into
`pki.dir` the same way `pki generate` does.

## 4. Distribute files to the hosts

```sh
./mongodb-tls distribute -config mongodb-tls.yaml
```

Copies `db-ca.pem` and each host's `<processName>.pem` to `targetDir` over
SSH, creating the directory and setting ownership and `0400`/`0444`
permissions as configured. Distribution is idempotent: files with identical
content are skipped on a re-run.

## 5. Roll out TLS

Each stage is a separate, explicit command, since moving to `requireTLS`
before every client supports TLS will break connections.

```sh
./mongodb-tls rollout allow   -config mongodb-tls.yaml --yes
# confirm every client (including mongot, application servers, backup agents,
# and your own mongosh sessions) connects over TLS and trusts db-ca.pem
./mongodb-tls rollout prefer  -config mongodb-tls.yaml --yes
# confirm again
./mongodb-tls rollout require -config mongodb-tls.yaml --yes
```

Each command snapshots the current automation config to `rollback.snapshotDir`
before changing it, submits the new `net.tls` settings for every host in
`hosts[]`, and waits for Automation to report every targeted process at the
new goal version. If convergence fails, check `mongodb-tls status` and the
MongoDB Agent logs on the affected host, then either fix the issue and
re-run the same stage or run `mongodb-tls rollback --yes` to restore the
prior configuration.

`mongodb-search`'s `mongot` connects without a client certificate, so keep
`tls.allowConnectionsWithoutCertificates: true` (the default) through every
stage.

To run `plan`, `pki`, `distribute`, and `rollout allow` in one step, stopping
there for manual confirmation before `prefer`/`require`:

```sh
./mongodb-tls install -config mongodb-tls.yaml --yes
```

Add `--auto` to additionally advance through `preferTLS` and `requireTLS`
automatically, pausing `tls.rollout.pauseBetweenStages` (default `10m`)
between each stage. Only use `--auto` once you are confident every client
will pick up TLS within that pause window.

## 6. Verify and check status

```sh
./mongodb-tls status -config mongodb-tls.yaml
./mongodb-tls verify -config mongodb-tls.yaml
```

`status` prints each process's current `net.tls.mode` and whether Automation
has converged. `verify` opens a direct TLS connection to each host's MongoDB
port using the generated/imported CA and reports the handshake result,
certificate SANs, and expiry.

## 7. Use the result with mongodb-search

After `requireTLS` converges, set the public CA file as
`external.databaseCAFile`:

```yaml
external:
  databaseCAFile: /absolute/path/to/pki/db-ca.pem
```

This is the MongoDB CA produced by `mongodb-tls`; it is distinct from
`searchCAFile`, which signs the Search and Envoy certificates described in
[external-certificate-guide.md](external-certificate-guide.md).

## Rolling back

```sh
./mongodb-tls rollback -config mongodb-tls.yaml --yes
```

Restores the most recently saved automation config snapshot from
`rollback.snapshotDir` and waits for convergence. This reverts Automation
settings only; it does not remove certificate files already copied to the
hosts.

## Manual alternative (no SSH / no Automation API access)

If you cannot grant this tool SSH or Automation API access, you can still
produce and apply the certificates by hand. Generate a CA and per-host
certificates with OpenSSL, copy the files to each host yourself, then
configure TLS directly in Cloud Manager or Ops Manager.

### Create a private CA

```sh
mkdir -p "$HOME/mongodb-tls" && cd "$HOME/mongodb-tls" && umask 077
openssl req -x509 -newkey rsa:3072 -sha256 -days 3650 \
  -keyout db-ca.key -out db-ca.pem \
  -subj "/CN=MongoDB Replica Set CA" \
  -addext "basicConstraints=critical,CA:TRUE,pathlen:0" \
  -addext "keyUsage=critical,keyCertSign,cRLSign"
```

Keep `db-ca.key` private; never place it on a MongoDB host.

### Create one certificate per node

```sh
for host in mongo1.example.com mongo2.example.com mongo3.example.com; do
  cat > "${host}.ext" <<EOF
basicConstraints = critical,CA:FALSE
keyUsage = critical,digitalSignature,keyEncipherment
extendedKeyUsage = serverAuth,clientAuth
subjectAltName = DNS:${host}
EOF
  openssl req -new -newkey rsa:3072 -nodes \
    -keyout "${host}.key" -out "${host}.csr" \
    -subj "/O=ExampleOrg/OU=MongoDB Server/CN=${host}"
  openssl x509 -req -in "${host}.csr" \
    -CA db-ca.pem -CAkey db-ca.key -CAcreateserial \
    -out "${host}.crt" -days 397 -sha256 -extfile "${host}.ext"
  cat "${host}.crt" "${host}.key" > "${host}.pem"
  chmod 600 "${host}.key" "${host}.pem"
done
```

Verify each certificate, then copy its `.pem` file to that host and
`db-ca.pem` to all hosts and clients:

```sh
for host in mongo1.example.com mongo2.example.com mongo3.example.com; do
  openssl verify -CAfile db-ca.pem "${host}.crt"
done
```

### Configure TLS in Cloud Manager or Ops Manager

Edit the deployment's process configuration for each member: set
`net.tls.mode` to `allowTLS`, `net.tls.certificateKeyFile` to that member's
`.pem` path, `net.tls.CAFile` to `db-ca.pem`'s path, and
`net.tls.allowConnectionsWithoutCertificates` to `true`. Apply as a rolling
update, confirm each member rejoins and the replica set stays healthy, then
repeat for `preferTLS` and finally `requireTLS` once every client uses TLS.

## References

- [MongoDB TLS certificate guide](https://www.mongodb.com/docs/manual/core/tls/certificate-tutorial/)
- [Cluster TLS upgrade procedure](https://www.mongodb.com/docs/manual/tutorial/upgrade-cluster-to-ssl/)
- [Cloud Manager / Ops Manager Public API: Automation Config](https://www.mongodb.com/docs/ops-manager/current/reference/api/automation-config/)
