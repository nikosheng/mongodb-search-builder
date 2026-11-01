# External MongoDB Search certificate guide

This guide prepares the PEM files used by [`examples/external.yaml`](../examples/external.yaml). For production, obtain certificates from your organization's PKI and follow its certificate issuance and rotation policies. The OpenSSL example below creates a private CA for a test deployment.

## Required files

| Configuration field | File contents |
| --- | --- |
| `external.databaseCAFile` | Public CA certificate or CA bundle that signs the existing MongoDB server certificates. Obtain it from your MongoDB administrator; a newly generated CA will not validate existing servers. |
| `external.searchCAFile` | Public CA certificate or bundle that signs the Search-side certificates. External MongoDB must trust this CA. |
| `external.searchCertFile`, `external.searchKeyFile` | Search (`mongot`) server certificate and matching unencrypted private key. |
| `external.envoyCertFile`, `external.envoyKeyFile` | Envoy server certificate and matching unencrypted private key. Its DNS SAN must match `external.hostname`. Required for multiple Search replicas. |
| `external.envoyClientCertFile`, `external.envoyClientKeyFile` | Envoy client certificate and matching unencrypted private key, valid for client authentication. Required for multiple Search replicas. |

Use PEM encoding. Include intermediate certificates after the leaf certificate when necessary. The CLI reads these files on the machine where it runs and imports the certificates and keys into Kubernetes. Keep private keys restricted and out of Git. Do not copy the Search CA private key to the cluster or provide it to the CLI.

The Search CA and MongoDB CA can be different. The Search CA signs the Search, Envoy server, and Envoy client certificates. The MongoDB CA validates the existing MongoDB server certificates.

## Generate test certificates with OpenSSL

These commands generate a Search CA and three leaf certificate/key pairs for the multi-replica example. They require OpenSSL and a shell. Replace the three values below with the `name`, `namespace`, and `external.hostname` in your configuration **before** running the commands. `SEARCH_HOST` must be a DNS name, without a port.

```sh
NAME=external-search
NAMESPACE=mongodb
SEARCH_HOST=search.example.com
```

Create a protected working directory outside the repository and move into it:

```sh
mkdir -p "$HOME/mongodb-search-pki"
chmod 700 "$HOME/mongodb-search-pki"
cd "$HOME/mongodb-search-pki"
umask 077
```

### 1. Obtain the database CA

Ask the administrator of the existing replica set for its MongoDB server CA certificate or bundle in PEM format. Place a copy in this directory as `database-ca.pem`. Each MongoDB server certificate must chain to a CA in this bundle, and the member addresses in `external.hosts` must match the server certificates.

Do not substitute the new Search CA for this file unless it actually signs the existing MongoDB server certificates.

### 2. Create the Search CA

```sh
openssl genrsa -out search-ca.key 3072
openssl req -new -x509 -sha256 -days 3650 \
  -key search-ca.key -out search-ca.pem \
  -subj "/CN=MongoDB Search Test CA" \
  -extensions v3_ca -config /dev/stdin <<'EOF'
[req]
distinguished_name = dn
[dn]
[v3_ca]
basicConstraints = critical,CA:TRUE,pathlen:0
keyUsage = critical,keyCertSign,cRLSign
subjectKeyIdentifier = hash
EOF
```

Configure the external MongoDB deployment to trust `search-ca.pem` and provide any client TLS identity required by your deployment. Keep `search-ca.key` secure: anyone holding it can issue certificates trusted by this CA.

### 3. Create the mongot certificate

The example includes the Search service and wildcard pod DNS names used by this CLI's managed certificate configuration, as well as the external hostname for single-replica use. Confirm these names match the actual Search service and pod names in your operator version, particularly if your cluster domain is not `cluster.local`.

```sh
cat > mongot.ext <<EOF
basicConstraints = critical,CA:FALSE
keyUsage = critical,digitalSignature,keyEncipherment
extendedKeyUsage = serverAuth,clientAuth
subjectAltName = DNS:${SEARCH_HOST},DNS:${NAME}-search-0-svc.${NAMESPACE}.svc.cluster.local,DNS:*.${NAME}-search-0-svc.${NAMESPACE}.svc.cluster.local
EOF

openssl req -new -newkey rsa:2048 -nodes \
  -keyout mongot.key -out mongot.csr -subj "/CN=mongot"
openssl x509 -req -in mongot.csr -CA search-ca.pem -CAkey search-ca.key \
  -CAcreateserial -out mongot.crt -days 397 -sha256 -extfile mongot.ext
```

### 4. Create the Envoy server certificate

The external hostname must match the DNS SAN. The internal proxy service name below follows this CLI's managed certificate configuration; confirm it for your cluster.

```sh
cat > envoy.ext <<EOF
basicConstraints = critical,CA:FALSE
keyUsage = critical,digitalSignature,keyEncipherment
extendedKeyUsage = serverAuth
subjectAltName = DNS:${SEARCH_HOST},DNS:${NAME}-search-0-proxy-svc.${NAMESPACE}.svc.cluster.local
EOF

openssl req -new -newkey rsa:2048 -nodes \
  -keyout envoy.key -out envoy.csr -subj "/CN=${SEARCH_HOST}"
openssl x509 -req -in envoy.csr -CA search-ca.pem -CAkey search-ca.key \
  -CAcreateserial -out envoy.crt -days 397 -sha256 -extfile envoy.ext
```

### 5. Create the Envoy client certificate

```sh
cat > envoy-client.ext <<'EOF'
basicConstraints = critical,CA:FALSE
keyUsage = critical,digitalSignature
extendedKeyUsage = clientAuth
EOF

openssl req -new -newkey rsa:2048 -nodes \
  -keyout envoy-client.key -out envoy-client.csr -subj "/CN=search-envoy-client"
openssl x509 -req -in envoy-client.csr -CA search-ca.pem -CAkey search-ca.key \
  -CAcreateserial -out envoy-client.crt -days 397 -sha256 -extfile envoy-client.ext
```

### 6. Check and configure the files

Check the chains, purposes, and external hostname:

```sh
openssl verify -CAfile search-ca.pem -purpose sslserver mongot.crt envoy.crt
openssl verify -CAfile search-ca.pem -purpose sslclient envoy-client.crt
openssl x509 -in envoy.crt -noout -text
```

The last command prints the certificate, including its DNS SANs. For a single Search replica, check that `mongot.crt` also includes the external hostname. The CLI additionally validates each certificate against its private key and checks the public-facing hostname when it imports the files. Internal TLS connectivity still needs to be verified after deployment.

Set the paths in your configuration to the **absolute paths** of the files. For example, replace `/home/you/mongodb-search-pki` with the actual path (on macOS, this commonly starts with `/Users/`):

```yaml
external:
  # Retain your hosts and hostname here.
  databaseCAFile: /home/you/mongodb-search-pki/database-ca.pem
  searchCAFile: /home/you/mongodb-search-pki/search-ca.pem
  searchCertFile: /home/you/mongodb-search-pki/mongot.crt
  searchKeyFile: /home/you/mongodb-search-pki/mongot.key
  envoyCertFile: /home/you/mongodb-search-pki/envoy.crt
  envoyKeyFile: /home/you/mongodb-search-pki/envoy.key
  envoyClientCertFile: /home/you/mongodb-search-pki/envoy-client.crt
  envoyClientKeyFile: /home/you/mongodb-search-pki/envoy-client.key
```

For `searchReplicas: 1`, omit the four Envoy fields. The mongot server certificate must have a DNS SAN matching `external.hostname`.

From the project directory, after initializing the deployment, run the certificate stage with your actual configuration path:

```sh
./mongodb-search init -config config.yaml
./mongodb-search certs -config config.yaml
```

Alternatively, `./mongodb-search install -config config.yaml` runs these stages as part of installation. Continue with the [external deployment instructions](../README.md#prepare-an-external-replica-set), including DNS, MongoDB startup parameters, TLS trust, and end-to-end verification.
