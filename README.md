# grout-cni

A [CNI](https://www.cni.dev/) plugin for [grout](https://github.com/DPDK/grout), a DPDK-based graph router. It brings DPDK-accelerated networking to Kubernetes pods using TAP devices for standard workloads and virtio interfaces for pods running DPDK applications.

## How it works

The CNI binary (`grout-cni`) is invoked by kubelet (or [Multus](https://github.com/k8snetworkplumbingwg/multus-cni)) and talks directly to grout's Unix socket to create interfaces, assign addresses, and manage bridges. There is no separate daemon — grout holds all interface/address/route state, and IPAM is delegated to an external plugin (host-local, whereabouts) via CNI chaining.

Supported CNI operations: **ADD**, **DEL**, **CHECK**, **GC**, **STATUS** (CNI spec 1.1.0).

### Interface types

- **TAP** (default): creates a kernel TAP device, moves it into the pod network namespace. Standard pod networking.
- **virtio**: creates a vhost-user socket for DPDK applications. The pod connects with the `net_virtio_user` PMD — full userspace dataplane, no kernel networking stack.

## Build

```sh
make build    # produces bin/grout-cni (static binary, CGO_ENABLED=0)
```

## Test

```sh
make test     # unit tests
make e2e      # integration tests against a real grout container (requires Docker)
make lint     # golangci-lint
```

## Configuration

The plugin is configured via the CNI network config (conflist or NetworkAttachmentDefinition):

```json
{
  "cniVersion": "1.1.0",
  "name": "grout-net",
  "plugins": [
    {
      "type": "grout-cni",
      "groutSocketPath": "/run/grout/grout.sock",
      "bridge": "br-pods",
      "interfaceType": "tap",
      "mtu": 1500,
      "logLevel": "warn",
      "ipam": {
        "type": "host-local",
        "ranges": [[{"subnet": "10.244.0.0/24"}]],
        "routes": [{"dst": "0.0.0.0/0"}]
      }
    }
  ]
}
```

| Field | Default | Description |
|-------|---------|-------------|
| `groutSocketPath` | `/run/grout/grout.sock` | Path to grout's Unix control socket |
| `bridge` | derived from network name | Name of the grout bridge to attach pods to |
| `interfaceType` | `tap` | `tap` or `virtio` |
| `mtu` | kernel default (1500) | MTU for the pod interface |
| `logLevel` | `warn` | `debug`, `info`, `warn`, or `error` |

## Container image

```sh
make image    # builds grout-cni container image via podman
```

## Related projects

- [grout](https://github.com/DPDK/grout) — the DPDK-based graph router

## License

Apache License 2.0 — see [LICENSE](LICENSE).
