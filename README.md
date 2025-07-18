<p align="center">
  <img src="docs/kubelb-logo.png#gh-light-mode-only" width="700px" />
  <img src="docs/kubelb-logo-dark.png#gh-dark-mode-only" width="700px" />
</p>

# KubeLB CLI

The KubeLB CLI provides tools to manage KubeLB load balancers and create secure tunnels to expose local services through the KubeLB infrastructure.

## Installation

### Prerequisites

- Go 1.24.5+

### Build from source

```bash
make build
```

This will create the binary in `./bin/kubelb`.

### Install to system

```bash
make install
```

This will install the binary to your Go bin directory.

## Configuration

The KubeLB CLI supports multiple ways to configure kubeconfig and tenant settings, with the following precedence (highest to lowest):

### Kubeconfig Configuration

1. **`--kubeconfig` flag**: Explicitly specify kubeconfig path
2. **`KUBECONFIG` environment variable**: Standard Kubernetes environment variable
3. **In-cluster configuration**: Automatically used when running inside a Kubernetes cluster
4. **Default kubeconfig discovery**: Uses `~/.kube/config` and other standard locations

### Tenant Configuration

1. **`--tenant, -t` flag**: Explicitly specify tenant name
2. **`TENANT_NAME` environment variable**: Set tenant via environment variable
3. **Error if neither provided**: Tenant is required for most commands (error message includes both flag and environment variable options)

### Examples

```bash
# Using flags
kubelb loadbalancer list --tenant mycompany --kubeconfig ./kubeconfig

# Using environment variables
export TENANT_NAME=mycompany
export KUBECONFIG=./kubeconfig
kubelb loadbalancer list

# Mixed (flags override environment variables)
export TENANT_NAME=from-env
kubelb loadbalancer list --tenant from-flag  # Uses "from-flag"

# In-cluster usage (when running inside Kubernetes)
kubectl run kubelb-cli --image=kubelb-cli --rm -it -- loadbalancer list --tenant mycompany

# Tenant namespace examples
kubelb loadbalancer list --tenant mycompany          # Uses namespace: tenant-mycompany
kubelb loadbalancer list --tenant tenant-mycompany  # Uses namespace: tenant-mycompany (no double prefix)
```

## Global Flags

- `--kubeconfig`: Path to kubeconfig file
- `--tenant, -t`: Tenant name (creates namespace `tenant-{name}`, or uses as-is if already prefixed)
- `--timeout`: Timeout for operations (e.g., 30s, 5m)

## Commands

### `kubelb version`

Display version information about the KubeLB CLI.

```bash
kubelb version
```

**Output:**

- `GitVersion`: Version from git tags
- `GitCommit`: Git commit hash
- `BuildDate`: Build timestamp
- `Platform`: Operating system and architecture
- `Compiler`: Go compiler used
- `GoVersion`: Go version used for building

**Example:**

```
GitVersion: v1.1.1-25-gf214f27-dirty
GitCommit:  f214f278fe7ce491ccf7ee8ec9c56d767f60378c
BuildDate:  2025-07-16T09:38:06Z
Platform:   darwin/arm64
Compiler:   gc
GoVersion:  go1.24.5
```

### `kubelb loadbalancer` (alias: `lb`)

Manage KubeLB load balancers in your tenant namespace.

#### `kubelb loadbalancer list`

List all load balancers in the specified tenant namespace.

```bash
# Using flags
kubelb loadbalancer list --tenant mycompany --kubeconfig ./kubeconfig

# Using environment variables
export TENANT_NAME=mycompany
kubelb loadbalancer list

# Using alias and short flag
kubelb lb list -t mycompany
```

**Output:**
Displays a table with:

- `NAME`: LoadBalancer resource name
- `ENDPOINTS`: IP addresses (shows first IP + count if multiple)
- `PORTS`: Exposed ports (shows first port + count if multiple)
- `TYPE`: Service type (ClusterIP, NodePort, LoadBalancer)
- `CLI GENERATED`: Whether the resource was created by the CLI (true/false)
- `AGE`: Time since creation

**Example:**

```bash
kubelb lb list -t mycompany
```

```
NAME            ENDPOINTS       PORTS       TYPE         CLI GENERATED AGE
web-service     10.0.1.100      80/TCP      LoadBalancer true          2h
api-service     10.0.1.101      8080/TCP    ClusterIP    false         1h
```

#### `kubelb loadbalancer get`

Get a specific load balancer and output its complete YAML representation.

```bash
# Using flags
kubelb loadbalancer get my-lb --tenant mycompany --kubeconfig ./kubeconfig

# Using environment variables
export TENANT_NAME=mycompany
kubelb loadbalancer get my-lb

# Using alias and short flag
kubelb lb get my-lb -t mycompany
```

**Output:**
Displays the complete YAML representation of the LoadBalancer resource, including:

- Complete resource metadata (name, namespace, labels, annotations)
- Full specification (endpoints, ports, type, etc.)
- Current status information
- All fields as stored in the Kubernetes API

**Example:**

```bash
kubelb lb get web-service -t mycompany
```

```yaml
apiVersion: kubelb.k8c.io/v1alpha1
kind: LoadBalancer
metadata:
  annotations:
    kubelb.k8c.io/cli-resource: "true"
  creationTimestamp: "2025-07-16T07:38:06Z"
  name: web-service
  namespace: tenant-mycompany
  resourceVersion: "12345"
  uid: 550e8400-e29b-41d4-a716-446655440000
spec:
  type: LoadBalancer
  ports:
  - port: 80
    protocol: TCP
    targetPort: 8080
  endpoints:
  - addresses:
    - ip: 10.0.1.100
status:
  conditions:
  - type: Ready
    status: "True"
```
