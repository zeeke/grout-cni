# grout-cni

[![CI](https://github.com/zeeke/grout-cni/actions/workflows/ci.yml/badge.svg)](https://github.com/zeeke/grout-cni/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/zeeke/grout-cni.svg)](https://pkg.go.dev/github.com/zeeke/grout-cni)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

A [CNI](https://www.cni.dev/) plugin for [grout](https://github.com/DPDK/grout), a DPDK-based graph router. It brings DPDK-accelerated networking to Kubernetes pods using TAP devices for standard workloads and **virtio interfaces for pods running DPDK applications** — no SR-IOV hardware required.

## Architecture

```
┌──────────────────────────────────────────────────────────┐
│ Node                                                     │
│                                                          │
│  ┌─────────┐    ┌─────────────┐                          │
│  │ kubelet │───>│  grout-cni  │                          │
│  │ /Multus │    │ (CNI binary)│                          │
│  └─────────┘    └──────┬──────┘                          │
│                        │ Unix socket                     │
│                 ┌──────▼──────┐                           │
│                 │    grout    │                           │
│                 │(DPDK router)│                           │
│                 └──────┬──────┘                           │
│                        │                                 │
│       TAP devices      │      vhost-user sockets         │
│     ┌──────────┐       │       ┌──────────┐              │
│     │ Pod      │◄──────┘──────>│ Pod      │              │
│     │ (kernel) │               │ (DPDK)   │              │
│     └──────────┘               └──────────┘              │
└──────────────────────────────────────────────────────────┘
```

The CNI binary talks directly to grout's Unix control socket — there is no separate daemon. grout holds all interface, address, and route state; IPAM is delegated to an external plugin (host-local, whereabouts) via CNI chaining. A file lock serializes concurrent CNI calls on each node.

Supported CNI operations: **ADD**, **DEL**, **CHECK**, **GC**, **STATUS** (CNI spec 1.1.0).

### Interface types

| Type | How it works | Use case |
|------|-------------|----------|
| **TAP** (default) | Creates a kernel TAP device, moves it into the pod netns | Standard pod networking |
| **virtio** | Creates a vhost-user socket; pod connects with `net_virtio_user` PMD | DPDK applications — full userspace dataplane |

### Deployment scenarios

grout-cni works as:

1. **Multus secondary CNI** — attach high-performance or DPDK-capable secondary interfaces to pods alongside an existing primary CNI (Calico, Cilium, OVN-K, etc.)
2. **Primary CNI** — grout-cni is the cluster's only CNI plugin; every pod gets its interface through grout

## Quick start

### Prerequisites

- A running Kubernetes cluster
- [grout](https://github.com/DPDK/grout) deployed on the node (DaemonSet or systemd)
- [Multus](https://github.com/k8snetworkplumbingwg/multus-cni) (for secondary CNI mode)

### Install

```sh
# Build the CNI binary
make build

# Copy to the CNI bin directory on each node
cp bin/grout-cni /opt/cni/bin/
```

Or use the container image:

```sh
make image    # builds via podman
```

### Configure

Create a NetworkAttachmentDefinition for Multus:

```yaml
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: grout-tap
spec:
  config: |
    {
      "cniVersion": "1.1.0",
      "name": "grout-tap",
      "type": "grout-cni",
      "bridge": "br-tap",
      "ipam": {
        "type": "host-local",
        "ranges": [[{"subnet": "10.244.0.0/24"}]],
        "routes": [{"dst": "0.0.0.0/0"}]
      }
    }
```

For virtio/DPDK pods:

```yaml
apiVersion: k8s.cni.cncf.io/v1
kind: NetworkAttachmentDefinition
metadata:
  name: grout-virtio
spec:
  config: |
    {
      "cniVersion": "1.1.0",
      "name": "grout-virtio",
      "type": "grout-cni",
      "bridge": "br-virtio",
      "interfaceType": "virtio",
      "ipam": {
        "type": "host-local",
        "ranges": [[{"subnet": "10.245.0.0/24"}]],
        "routes": [{"dst": "0.0.0.0/0"}]
      }
    }
```

### Use

Annotate pods to attach a grout-cni interface:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: my-pod
  annotations:
    k8s.v1.cni.cncf.io/networks: grout-tap
spec:
  containers:
  - name: app
    image: busybox
```

## Configuration reference

| Field | Default | Description |
|-------|---------|-------------|
| `groutSocketPath` | `/run/grout/grout.sock` | Path to grout's Unix control socket |
| `bridge` | derived from network name | Name of the grout bridge to attach pods to |
| `interfaceType` | `tap` | `tap` or `virtio` |
| `mtu` | kernel default (1500) | MTU for the pod interface |
| `logLevel` | `warn` | `debug`, `info`, `warn`, or `error` |

## Development

```sh
make build    # build bin/grout-cni (static binary, CGO_ENABLED=0)
make test     # unit tests
make lint     # golangci-lint
make e2e      # integration tests against a real grout container (requires Docker)
make kind-e2e # full Kubernetes e2e with kind, Multus, and grout
make image    # container image via podman
```

See [CONTRIBUTING.md](CONTRIBUTING.md) for development workflow details.

## Related projects

- [grout](https://github.com/DPDK/grout) — the DPDK-based graph router
- [Multus CNI](https://github.com/k8snetworkplumbingwg/multus-cni) — multi-network support for Kubernetes

## License

Apache License 2.0 — see [LICENSE](LICENSE).
