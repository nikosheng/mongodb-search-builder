# MongoDB Search Kubernetes CLI

This CLI prepares MongoDB Enterprise replica sets for MongoDB Search and Vector Search on a single Kubernetes cluster. It can create an operator-managed replica set or connect Search to an existing external replica set. Search can run as one `mongot` instance or as multiple instances behind an operator-managed Envoy load balancer.

| Use case | Database | Search | Guide |
| --- | --- | --- | --- |
| 1 | New operator-managed replica set | One `mongot` | [Managed, single Search replica](#use-case-1-managed-replica-set-with-one-search-replica) |
| 2 | New operator-managed replica set | Multiple `mongot` replicas and Envoy | [Managed, multiple Search replicas](#use-case-2-managed-replica-set-with-multiple-search-replicas) |
| 3 | Existing external replica set | One `mongot` | [External, single Search replica](#use-case-3-external-replica-set-with-one-search-replica) |
| 4 | Existing external replica set | Multiple `mongot` replicas and Envoy | [External, multiple Search replicas](#use-case-4-external-replica-set-with-multiple-search-replicas) |

The examples use placeholder values. Replace them before running a deployment. The CLI has unit tests, but these deployment paths have **not been verified end-to-end on a live Kubernetes cluster**. Check the [MongoDB installation guide](https://www.mongodb.com/docs/kubernetes/current/fts-vs/deployment/single-cluster-installation/) and operator compatibility for your environment.

## Prerequisites

For all use cases:

1. Install Go 1.24 or newer to build the CLI.
2. Have a running Kubernetes cluster, a locally configured kubeconfig context, sufficient cluster privileges, and persistent storage. The cluster needs enough capacity for the database, operator, cert-manager, Search, and, where applicable, Envoy. The CLI defaults to requests of **2 CPU and 4 GiB memory per Search replica**; see [Resource sizing](#resource-sizing-and-scaling) to change them.
3. Ensure the machine running the CLI can access the Kubernetes API and the OCI registries containing the Helm charts, and that cluster nodes can pull the required images.
4. Choose a unique resource `name` and namespace. Names must use lowercase DNS-label characters; `name` is limited to 32 characters. For managed deployments, choose a name unique across the cluster because it is also used for cluster-scoped certificate issuers.

The CLI uses Kubernetes client-go and the Helm Go SDK: `kubectl` and `helm` executables are not required to deploy. If available, `kubectl` is useful for inspecting contexts, resources, events, and logs.

Additional requirements:

- **Managed:** MongoDB Ops Manager or Cloud Manager, an organization ID, project name, and API credentials with the permissions required to manage the project and deployment. The cluster must reach Ops Manager or Cloud Manager. Use a supported MongoDB Enterprise version in `X.Y.Z-ent` format, 8.3 or later.
- **External:** An existing TLS-enabled MongoDB Enterprise replica set, version 8.3 or later. Kubernetes must reach its MongoDB members; those members must be able to reach a Kubernetes `LoadBalancer` endpoint on port `27028`. You need control of DNS, the external MongoDB settings, and the required TLS certificates. See [external preparation](#prepare-an-external-replica-set).

The CLI does not configure sharded clusters, Community Edition, automatic embeddings, application users, or Search indexes.

## Build and configuration basics

From the project directory:

```sh
go build -o mongodb-search .
```

Each command reads a YAML or JSON file. The default is `config.yaml`; all examples explicitly pass `-config config.yaml`. Start with `config.yaml.sample` for managed mode or `examples/external.yaml` for external mode. For example:

```sh
cp config.yaml.sample config.yaml
```

Edit `config.yaml` in your preferred editor. The root `config.yaml` is ignored by Git and can contain credentials; other filenames are not automatically ignored. Protect files containing plaintext passwords and keys. Quote YAML values such as passwords, particularly when they contain special characters. Paths to external certificate files are read by the CLI on the machine where it runs, relative to its working directory unless absolute paths are used.

The `context` must be the exact name of a context in your kubeconfig, not the cluster name or namespace. If `kubectl` is installed, list contexts with:

```sh
kubectl config get-contexts -o name
```

The kubeconfig follows the normal client-go loading rules, including `KUBECONFIG` when set. All commands validate the complete configuration, including `status` and `uninstall`, so retain the configuration for later operations.

### Credentials

Enter your credentials directly in `config.yaml`. For a managed deployment, fill in all four values:

```yaml
opsManager:
  url: https://cloud.mongodb.com
  orgId: your-organization-id
  project: search-demo
  publicKey: 'your-public-key'
  privateKey: 'your-private-key'
adminPassword: 'your-admin-password'
syncPassword: 'your-search-sync-password'
```

For external mode, set `syncPassword` to the password of the existing Search synchronization user. Replace the placeholders in `examples/managed.yaml` or `examples/external.yaml` with your own values. Credentials are also stored in Kubernetes Secrets during deployment.

## Use case 1: Managed replica set with one Search replica

Use this when you want the operator to create a TLS-enabled Enterprise replica set and connect it to one `mongot` instance. The CLI creates an `admin.mdb-admin` user with the `root` role and an `admin.search-sync-source` user with the `searchCoordinator` role. The operator manages the MongoDB Search connection settings.

### 1. Create and fill in `config.yaml`

```sh
cp config.yaml.sample config.yaml
```

Edit it to match this example, replacing every placeholder:

```yaml
context: your-kubernetes-context
namespace: mongodb
name: mdb-rs
mode: managed
version: 8.3.4-ent
members: 3
searchReplicas: 1
timeout: 20m
opsManager:
  url: https://cloud.mongodb.com
  orgId: your-organization-id
  project: search-demo
  publicKey: 'your-ops-manager-public-key'
  privateKey: 'your-ops-manager-private-key'
adminPassword: 'your-admin-password'
syncPassword: 'your-search-sync-password'
```

- Set `context` to your kubeconfig context, `namespace` to the target namespace, and `name` to a new deployment name.
- Set `opsManager.url` to your Cloud Manager or Ops Manager base URL, `orgId` to the actual organization ID, and `project` to the project name the operator should use. Provide the associated API credentials.
- Choose strong, separate passwords. The CLI uses `adminPassword` for `mdb-admin` and `syncPassword` for `search-sync-source`.
- `members: 3` requests three database members; `searchReplicas: 1` requests one Search instance. See the [configuration reference](#configuration-reference) for defaults and optional chart versions.

### 2. Run the deployment

```sh
./mongodb-search install -config config.yaml
```

`install` runs `init`, `certs`, `deploy-db`, and `deploy-search` in order. It installs or reuses the operator and cert-manager, creates a self-signed CA and certificates, requests the database and users, then requests Search. It waits for readiness at each stage. The generated self-signed CA is intended for a controlled deployment; plan an appropriate PKI for production.

### 3. Check and use the deployment

```sh
./mongodb-search status -config config.yaml
```

Look for `Running` phases for `MongoDB` and `MongoDBSearch` and inspect the pod phases. Connect an authorized MongoDB client to the replica set using network-reachable MongoDB service addresses, TLS, the CA, and the `mdb-admin` credentials or a separately created application user. Then create Search or Vector Search indexes and run queries as described in [Verify Search](#verify-search-and-vector-search).

## Use case 2: Managed replica set with multiple Search replicas

Use this when you want a new managed replica set with multiple `mongot` instances and an operator-managed Envoy load balancer. `mongod` connects to Envoy, which distributes Search traffic to the `mongot` instances.

### 1. Create and fill in `config.yaml`

```sh
cp config.yaml.sample config.yaml
```

Fill in the managed settings and credentials as in use case 1, but request at least two Search replicas:

```yaml
context: your-kubernetes-context
namespace: mongodb
name: mdb-rs
mode: managed
version: 8.3.4-ent
members: 3
searchReplicas: 2
timeout: 20m
opsManager:
  url: https://cloud.mongodb.com
  orgId: your-organization-id
  project: search-demo
  publicKey: 'your-ops-manager-public-key'
  privateKey: 'your-ops-manager-private-key'
adminPassword: 'your-admin-password'
syncPassword: 'your-search-sync-password'
```

Replace the placeholders and make sure the cluster has capacity for every requested Search replica and the proxy. The CLI requests the managed Envoy configuration when `searchReplicas` is greater than one; you do not have to supply Envoy configuration or certificates in managed mode.

### 2. Deploy and check

```sh
./mongodb-search install -config config.yaml
./mongodb-search status -config config.yaml
```

The operator manages Search endpoint settings on the managed MongoDB deployment. After both resources reach `Running`, follow [Verify Search](#verify-search-and-vector-search). A `Running` phase alone does not establish that queries and indexing work end-to-end.

## Prepare an external replica set

Complete these steps before either external use case:

1. Verify the existing replica set runs MongoDB Enterprise 8.3 or later, uses TLS, and has network-reachable member addresses. Obtain the public CA certificate that signed the MongoDB server certificates.
2. In Cloud Manager or Ops Manager, open **Security > Users**, add the user `search-sync-source` in authentication database `admin`, and grant it the `searchCoordinator` role on `admin`. Set a strong password. If your deployment is managed by another system, create the equivalent user through that system. Use this same password for `syncPassword`. The CLI does **not** create the user in external mode.
3. Choose a DNS name such as `search.example.com` for the external Search endpoint. Arrange for it to resolve to the Kubernetes LoadBalancer ingress, and ensure the MongoDB members can reach port `27028`. Kubernetes must also be able to reach each member on the configured MongoDB port. Open the necessary firewall rules in both directions.
4. Obtain the Search CA and certificates listed in the relevant use case below. Use PEM files and unencrypted private keys; include intermediate certificates in the certificate file if needed. Configure the external MongoDB deployment to trust the Search CA and provide the TLS client identity required by your deployment. The CLI verifies that the provided Search certificates and keys match, chain to `searchCAFile`, have the appropriate server/client purpose, and, where required, match `external.hostname`.
5. Prepare to configure the external `mongod` startup parameters in [Connect external MongoDB to Search](#connect-external-mongodb-to-search). The CLI cannot change those settings for you.

For an OpenSSL test certificate walkthrough, see the [external certificate guide](docs/external-certificate-guide.md).

## Use case 3: External replica set with one Search replica

Use this for an existing external replica set and a single Kubernetes-hosted `mongot`. The external LoadBalancer exposes the Search endpoint to MongoDB.

### 1. Prepare certificates

In addition to the database CA, obtain a Search CA and a `mongot` server certificate and private key signed by that CA. The server certificate must be valid for `external.hostname`, for example `search.example.com`. Ensure the MongoDB members trust the Search CA.

### 2. Create and fill in `config.yaml`

```sh
cp examples/external.yaml config.yaml
```

Replace its contents with the following and edit the placeholders:

```yaml
context: your-kubernetes-context
namespace: mongodb
name: external-search
mode: external
searchReplicas: 1
timeout: 20m
syncPassword: 'your-existing-search-sync-user-password'
external:
  hosts:
    - db1.example.com:27017
    - db2.example.com:27017
    - db3.example.com:27017
  hostname: search.example.com
  databaseCAFile: /absolute/path/to/database-ca.pem
  searchCAFile: /absolute/path/to/search-ca.pem
  searchCertFile: /absolute/path/to/mongot.crt
  searchKeyFile: /absolute/path/to/mongot.key
```

Replace `hosts` with the real replica set member addresses, each in `host:port` form, and `hostname` with the DNS name chosen above, without a port. Set the certificate paths to readable local files. The sync password must match the existing `admin.search-sync-source` user. Envoy certificate fields are unnecessary for one replica.

### 3. Deploy Search

```sh
./mongodb-search install -config config.yaml
```

In external mode, `deploy-db` is a no-op. The CLI imports the provided certificates, creates the Search resource, and creates a LoadBalancer Service named `<name>-external`. During `deploy-search`, it waits for the Search resource to report `Running`.

### 4. Connect and verify

Follow [Connect external MongoDB to Search](#connect-external-mongodb-to-search) to configure DNS and MongoDB. If `install` is still waiting for Search readiness, use a second terminal to run `status` and continue those steps. After the external connection is configured, check status and perform an end-to-end query.

## Use case 4: External replica set with multiple Search replicas

Use this for an existing external replica set with multiple `mongot` instances behind an operator-managed Envoy proxy. The external LoadBalancer exposes the proxy, not individual Search instances.

### 1. Prepare certificates

Obtain the database CA, Search CA, and a `mongot` server certificate and key. Also obtain:

- An Envoy **server** certificate and key signed by the Search CA, with a DNS SAN matching `external.hostname`.
- An Envoy **client** certificate and key signed by the Search CA, valid for client authentication.

Ensure external MongoDB trusts the Search CA and can authenticate to Search as required. The CLI verifies the `mongot` certificate for server authentication but does not check it against `external.hostname` in multi-replica mode; the external hostname is checked against the Envoy server certificate instead.

### 2. Create and fill in `config.yaml`

```sh
cp examples/external.yaml config.yaml
```

Replace its contents with this example, then replace every placeholder and path:

```yaml
context: your-kubernetes-context
namespace: mongodb
name: external-search
mode: external
searchReplicas: 2
timeout: 20m
syncPassword: 'your-existing-search-sync-user-password'
external:
  hosts:
    - db1.example.com:27017
    - db2.example.com:27017
    - db3.example.com:27017
  hostname: search.example.com
  databaseCAFile: /absolute/path/to/database-ca.pem
  searchCAFile: /absolute/path/to/search-ca.pem
  searchCertFile: /absolute/path/to/mongot.crt
  searchKeyFile: /absolute/path/to/mongot.key
  envoyCertFile: /absolute/path/to/envoy.crt
  envoyKeyFile: /absolute/path/to/envoy.key
  envoyClientCertFile: /absolute/path/to/envoy-client.crt
  envoyClientKeyFile: /absolute/path/to/envoy-client.key
```

`searchReplicas: 2` enables the operator-managed proxy. The four Envoy certificate and key paths are required whenever `searchReplicas` is greater than one. The sync password and MongoDB hosts must refer to the existing external deployment.

### 3. Deploy, connect, and verify

```sh
./mongodb-search install -config config.yaml
```

The CLI creates Search, waits for the operator-created proxy Service, and creates `<name>-external` as a LoadBalancer Service using the proxy selector. In a second terminal if the install is still waiting, follow [Connect external MongoDB to Search](#connect-external-mongodb-to-search). Check status and perform an end-to-end query once DNS and MongoDB configuration are in place.

### Connect external MongoDB to Search

1. After the CLI creates the external Service, inspect the ingress:

   ```sh
   ./mongodb-search status -config config.yaml
   ```

   The command prints `External service ingress` when the Service exists. If ingress is empty, wait for the cluster's LoadBalancer provider to provision it. Point the `external.hostname` DNS record to the ingress and verify it resolves and is reachable from each MongoDB member. DNS and LoadBalancer provisioning are outside the CLI.

2. In the Cloud Manager or Ops Manager deployment editor, under **Process Configuration > Advanced Configuration Options**, add these `setParameter` startup options to each external `mongod`. Substitute your configured hostname:

   | Parameter | Value |
   | --- | --- |
   | `mongotHost` | `search.example.com:27028` |
   | `searchIndexManagementHostAndPort` | `search.example.com:27028` |
   | `skipAuthenticationToSearchIndexManagementServer` | `false` |
   | `searchTLSMode` | `requireTLS` |
   | `useGrpcForSearch` | `true` |

   Review and deploy the changes using your deployment manager; it may perform a rolling restart. If you manage `mongod` another way, configure the equivalent startup parameters there. Confirm TLS trust and client authentication as required by your deployment.

3. Run `./mongodb-search status -config config.yaml` again. Once Search is running, verify indexing and queries. If an earlier `install` timed out while DNS or MongoDB settings were being prepared, resolve the issue and rerun `./mongodb-search deploy-search -config config.yaml`.

## Verify Search and Vector Search

`status` reports Kubernetes resource phases and namespace pod phases; it does not check connectivity, index creation, replication, or query results. To validate the use case:

1. Connect to the target replica set with `mongosh` using reachable MongoDB member addresses, the appropriate CA and TLS credentials, and an authorized user. The CLI does not expose a managed MongoDB connection outside Kubernetes or create application users. The user must have privileges to write documents, create and list Search indexes, and query the collection. Run the following in a disposable test database:

   ```javascript
   use search_cli_demo
   db.items.insertMany([
     { title: "MongoDB search guide", embedding: [1.0, 0.0, 0.0] },
     { title: "Kubernetes operations", embedding: [0.0, 1.0, 0.0] }
   ])
   ```

2. Create a text Search index and a Vector Search index:

   ```javascript
   db.items.createSearchIndex("text_demo", { mappings: { dynamic: true } })
   db.items.createSearchIndex("vector_demo", "vectorSearch", {
     fields: [{ type: "vector", path: "embedding", numDimensions: 3, similarity: "cosine" }]
   })
   ```

3. Index creation is asynchronous. Repeat the following until both indexes report `queryable: true`; inspect their status for errors:

   ```javascript
   db.items.getSearchIndexes()
   ```

4. Run both queries. The text query should match the MongoDB document; the vector query should return the document closest to `[1.0, 0.0, 0.0]`:

   ```javascript
   db.items.aggregate([
     { $search: { index: "text_demo", text: { query: "MongoDB", path: "title" } } },
     { $project: { title: 1 } }
   ]).toArray()

   db.items.aggregate([
     { $vectorSearch: { index: "vector_demo", path: "embedding", queryVector: [1.0, 0.0, 0.0], numCandidates: 40, limit: 2 } },
     { $project: { title: 1, score: { $meta: "vectorSearchScore" } } }
   ]).toArray()
   ```

   These small vectors are only a connectivity test. For a real application, generate document and query embeddings with the same model and set `numDimensions` to its output size.

5. If a query fails, inspect the MongoDB and Search resources, pod logs, events, network routes, TLS trust, and index status. See the [index creation reference](https://www.mongodb.com/docs/manual/reference/method/db.collection.createSearchIndex/), [Vector Search query reference](https://www.mongodb.com/docs/vector-search/query/aggregation-stages/vector-search-stage/), and [MongoDB installation guide](https://www.mongodb.com/docs/kubernetes/current/fts-vs/deployment/single-cluster-installation/).

## Configuration reference

Unknown fields are rejected. YAML and JSON use the same field names. Defaults are applied when a field is omitted or, for numeric fields, set to zero.

| Field | Applies to | Meaning |
| --- | --- | --- |
| `context` | All | Required kubeconfig context name. |
| `namespace` | All | Kubernetes namespace; default `mongodb`. Lowercase DNS label, at most 63 characters. |
| `name` | All | Resource name; default `mdb-rs`. Lowercase DNS label, at most 32 characters. |
| `mode` | All | Required: `managed` or `external`. |
| `version` | Managed | Enterprise server version, e.g. `8.3.4-ent`; default `8.3.4-ent`. Must be 8.3 or later in `X.Y.Z-ent` format. |
| `members` | Managed | Database member count, 1–50; default `3`. Not used to create external members. |
| `searchReplicas` | All | Number of `mongot` replicas; default `1`. Values greater than 1 enable managed Envoy. |
| `mongodbResources` | Managed | Optional `requests` and `limits` for CPU and memory on each MongoDB database container. |
| `searchResources` | All | Optional `requests` and `limits` for CPU and memory on each Search container. Unspecified requests default to 2 CPU and 4Gi memory. |
| `timeout` | All | Positive Go duration, e.g. `20m`; default `15m`. Used for individual waits and Helm operations, not a single overall installation deadline. |
| `operatorChartVersion` | All | Optional MongoDB operator Helm chart version. Omit to use Helm's chart selection. |
| `certManagerChartVersion` | All | Optional cert-manager Helm chart version. Omit to use Helm's chart selection. |
| `opsManager.url` | Managed | Required Ops Manager or Cloud Manager base URL. |
| `opsManager.orgId` | Managed | Required organization ID. |
| `opsManager.project` | Managed | Required project name. |
| `opsManager.publicKey` | Managed | Cloud Manager or Ops Manager public API key. |
| `opsManager.privateKey` | Managed | Cloud Manager or Ops Manager private API key. |
| `adminPassword` | Managed | Password for the `mdb-admin` user. |
| `syncPassword` | All | Password for the `search-sync-source` user. |
| `external.hosts` | External | Required nonempty list of MongoDB replica set members in `host:port` format. |
| `external.hostname` | External | Required DNS name for the Search endpoint, without a port. Must match the public-facing Search or Envoy server certificate. |
| `external.databaseCAFile` | External | Required local PEM file containing the CA trusted for the MongoDB server certificates. |
| `external.searchCAFile` | External | Required local PEM file containing the CA for the Search-side certificates. |
| `external.searchCertFile`, `external.searchKeyFile` | External | Required local `mongot` server certificate and unencrypted key. |
| `external.envoyCertFile`, `external.envoyKeyFile` | External, multiple replicas | Required Envoy server certificate and unencrypted key. |
| `external.envoyClientCertFile`, `external.envoyClientKeyFile` | External, multiple replicas | Required Envoy client certificate and unencrypted key. |

## Commands and staged deployment

All commands accept `-config FILE`. `install` runs the stages below and calls `status` after they succeed. You can instead run them individually, in order:

```sh
./mongodb-search init -config config.yaml
./mongodb-search certs -config config.yaml
./mongodb-search deploy-db -config config.yaml
./mongodb-search deploy-search -config config.yaml
./mongodb-search status -config config.yaml
```

| Command | Action |
| --- | --- |
| `init` | Checks the configured credentials, ensures namespaces exist, and installs or reuses the operator and cert-manager. |
| `certs` | Generates a self-signed CA and certificates in managed mode, or imports and validates supplied files in external mode. |
| `deploy-db` | Creates the managed MongoDB resource and users; skips database creation in external mode. |
| `deploy-search` | Creates the Search resource and, in external mode, a LoadBalancer Service. |
| `status` | Prints resource phases, pod phases, and external ingress if available. |
| `uninstall` | Removes deployment resources; requires `--yes`. See [Teardown](#teardown). |

Stages can be rerun after resolving errors. Resources already present are not adopted or overwritten unless marked as owned by this deployment. Keep the same configuration, context, namespace, and name for reruns and teardown.

## Resource sizing and scaling

Add optional resource settings to `config.yaml` before installation or to update an existing deployment:

```yaml
mongodbResources:
  requests:
    cpu: "1"
    memory: 2Gi
  limits:
    cpu: "2"
    memory: 4Gi
searchResources:
  requests:
    cpu: "2"
    memory: 4Gi
  limits:
    cpu: "3"
    memory: 6Gi
```

These settings are **per container, per replica**, not totals for the deployment. `mongodbResources` applies to the managed `mongodb-enterprise-database` container; the CLI does not manage resources on an external database, the MongoDB agent, or Envoy. `searchResources` applies to each Search container in either mode. Do not include `mongodbResources` in external mode.

Use positive Kubernetes resource quantities: CPU `"1"` is one core and `"500m"` is half a core; memory examples include `512Mi` and `4Gi`. Quote numeric CPU values. Each supplied request must be no greater than its corresponding limit. You can omit individual fields or limits. For Search, omitted request fields default individually to CPU `"2"` and memory `4Gi`; any limits must be at least those effective requests. If `mongodbResources` is omitted, the CLI does not set database container resources. If limits are omitted, the CLI does not set them; Kubernetes admission policies may add defaults.

### Change CPU or memory after deployment

1. Edit `mongodbResources` and/or `searchResources` in the same configuration file used to deploy. Ensure the cluster has sufficient capacity for the new requests.
2. Apply the relevant stages. For an external replica set, run only `deploy-search`:

   ```sh
   ./mongodb-search deploy-db -config config.yaml
   ./mongodb-search deploy-search -config config.yaml
   ./mongodb-search status -config config.yaml
   ```

   Alternatively, rerun `./mongodb-search install -config config.yaml` to run all stages in order. These commands apply updates to CLI-owned resources; they do not directly resize the external database.

3. Check the operator's reconciliation, pod rollout, and actual container requests and limits using your Kubernetes tooling. `status` and the stage waits check resource phases, which can still reflect an earlier reconciliation; they do **not** prove that the updated pods are ready. Updating pod templates can restart pods and affect availability; a single Search replica can experience an interruption. If requests exceed available capacity, pods may remain pending. The exact rollout behavior depends on your operator and Kubernetes versions.

### Change replica counts

Change `members` for a managed replica set or `searchReplicas` for Search, then rerun the relevant stage. For a transition from one Search replica to multiple replicas, run `certs` before `deploy-search` to prepare the Envoy certificates. External mode requires the four Envoy certificate and key paths in the configuration, and its Search endpoint and MongoDB connectivity must be checked after the transition. Verify that the operator has completed the rollout and the replica set and Search are healthy before continuing. These scaling paths have not been verified on a live cluster.

## Troubleshooting

| Symptom | What to check |
| --- | --- |
| Config parse or validation error | Check spelling and indentation, required fields, DNS-label names, version format, and positive timeout. Unknown fields are rejected. |
| Credential is required | Fill in the required inline credential in `config.yaml` and check that it is not empty. |
| Cannot connect to Kubernetes | Verify the configured context exists, kubeconfig credentials are valid, the API is reachable, and the identity has sufficient permissions. |
| Helm install or image pull fails | Check access to chart registries and container registries, chart compatibility, cluster capacity, and events. |
| Resource wait times out | Inspect resource status, pod events and logs, storage, and network access. Increase `timeout` if provisioning needs more time; rerun the failed stage after fixing the cause. |
| External certificate validation fails | Check paths, PEM encoding, matching unencrypted key, CA chain, certificate validity and usage, and the hostname SAN. |
| External ingress remains empty | Verify the cluster supports `LoadBalancer` Services and the provider has provisioned an address. |
| Search runs but queries fail | Verify DNS, both network directions, MongoDB startup parameters, TLS trust and client identity, sync-user credentials, Search index readiness, and MongoDB/Search logs. |
| Refusing to overwrite an unowned resource | Choose a unique deployment name or inspect the existing resource; the CLI does not adopt it. |

## Teardown

To remove a deployment, use the same context, name, namespace, and mode as the deployment:

```sh
./mongodb-search uninstall -config config.yaml --yes
```

This is destructive: the CLI removes its owned Search and managed MongoDB resources, detected StatefulSet PVCs, certificates, credentials, and eligible CLI-installed Helm releases. Storage class reclaim policies determine whether underlying volumes are deleted. In external mode, the external MongoDB replica set, its user, its configuration, and DNS are not removed by the CLI; restore those settings separately if appropriate.

The CLI retains namespaces for manual removal because they can contain unrelated resources. It refuses to delete resources it does not own and retains shared Helm releases it does not own. Uninstall can stop when other resources still depend on an owned operator or cert-manager; resolve those dependencies before retrying. Review the target context and storage before invoking teardown.
