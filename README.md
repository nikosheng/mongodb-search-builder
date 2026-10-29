# MongoDB Search Kubernetes CLI

Deploy MongoDB Enterprise replica sets and MongoDB Search / Vector Search to one Kubernetes cluster, or connect Search to an external Enterprise replica set. A single `mongot` connects directly; multiple replicas use the operator-managed Envoy load balancer.

## Requirements

- Go 1.24 or newer to build; a running Kubernetes cluster with a configured kubeconfig context, sufficient cluster privileges, storage, and access to OCI registries.
- A supported MongoDB Enterprise version 8.3 or later and sufficient CPU/memory. Managed MongoDB requires an Ops Manager or Cloud Manager organization and API credentials. The cluster must be able to reach it.
- External mode requires a reachable TLS-enabled Enterprise replica set with an `admin.search-sync-source` user having the `searchCoordinator` role. The Kubernetes cluster needs outbound access to MongoDB, and MongoDB must reach the Kubernetes Search endpoint.
- The tool installs the MongoDB Kubernetes Operator and cert-manager with Helm's Go SDK if their standard deployments are absent. Chart versions can be pinned with `operatorChartVersion` and `certManagerChartVersion`. It uses client-go directly; `kubectl` and `helm` binaries are not required.

## Build and install

```sh
go build -o mongodb-search .
cp examples/managed.yaml config.yaml
# Edit config.yaml, especially context and Ops Manager settings.
export OPS_MANAGER_PUBLIC_KEY='your-public-key'
export OPS_MANAGER_PRIVATE_KEY='your-private-key'
export MONGODB_ADMIN_PASSWORD='your-admin-password'
export MONGODB_SEARCH_SYNC_PASSWORD='your-search-sync-password'
./mongodb-search install -config config.yaml
./mongodb-search status -config config.yaml
```

The managed install creates a TLS-enabled Enterprise `MongoDB` replica set, an administrator and a Search synchronization user, and an associated `MongoDBSearch` resource. The operator sets the MongoDB search parameters. Credentials are read from the named environment variables and stored in Kubernetes Secrets. The generated self-signed CA is intended for a controlled deployment; provision a suitable PKI for production. The application-specific database users and Search indexes are created separately.

Individual stages can be rerun in order: `init`, `certs`, `deploy-db`, `deploy-search`. Each uses `-config config.yaml`. `install` runs all four and waits for readiness. Existing resources are not adopted or overwritten. Keep the same configuration, context, namespace, and name on reruns and teardown.

## External replica set

Start with `examples/external.yaml`. Supply the database CA, Search CA, and the `mongot` certificate and private key. Certificates must chain to the Search CA. For multiple Search replicas provide an Envoy server certificate valid for `external.hostname` and an Envoy client certificate, signed by the Search CA. For a single replica, the `mongot` server certificate must be valid for that hostname; Envoy files are unnecessary. Keys must be unencrypted. MongoDB must trust the Search CA and present an appropriate TLS client certificate as required by the Search deployment.

Set `MONGODB_SEARCH_SYNC_PASSWORD` to the password for the existing sync user. The CLI creates a `LoadBalancer` Service; point the chosen DNS hostname at its ingress, then configure each external MongoDB server (for example via Ops Manager) with `mongotHost` and `searchIndexManagementHostAndPort` set to `search.example.com:27028`, `searchTLSMode=requireTLS`, `useGrpcForSearch=true`, and `skipAuthenticationToSearchIndexManagementServer=false`. Use `status` to see the service ingress. Provisioning and DNS propagation are external to this CLI; a Search `Running` phase does not prove queries work end-to-end.

## Teardown

```sh
./mongodb-search uninstall -config config.yaml --yes
```

This is destructive: it removes the CLI-owned database and Search resources and their detected StatefulSet PVCs, certificates, and credentials, plus CLI-installed Helm releases. Namespaces are retained for manual removal because they can contain unrelated resources. Storage class reclaim policies determine whether underlying volumes are deleted. Resources already present before installation aren't adopted. Shared Helm releases are retained if not owned by this deployment; uninstall stops if other MongoDB resources still depend on the owned operator. Re-run after resolving remaining dependencies. Review volumes and the target kubeconfig context before invoking teardown.

The tool follows the [single-cluster installation guide](https://www.mongodb.com/docs/kubernetes/current/fts-vs/deployment/single-cluster-installation/) and [Search resource reference](https://www.mongodb.com/docs/kubernetes/current/reference/fts-vs-settings/). Sharded topologies and automated embedding are not configured by this CLI.
