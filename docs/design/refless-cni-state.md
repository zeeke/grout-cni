# Refless CNI state: using grout as the source of truth

## Problem

CNI plugins typically maintain a node-local reference store (a directory of
JSON files under `/var/lib/cni/`) to map each attachment — identified by
(containerID, ifName) — to the interface the plugin created. DEL, CHECK, and
GC look up the attachment in the store rather than querying the network
backend.

This works but creates a secondary source of truth that can drift: the store
says an interface exists while grout has lost it (restart, crash), or vice
versa. The two must be reconciled, and the reconciliation logic is another
failure surface.

## Design

grout-cni avoids a ref store entirely. grout's interface list is the sole
source of truth for what exists on the node.

The key mechanism is a **deterministic port name** derived from the
attachment identity:

```
portName = "grk-" + base32(sha256(containerID + "\x00" + ifName))[:11]
```

The 4-byte `grk-` prefix marks the port as owned by grout-cni (so GC never
touches grout interfaces owned by other applications), and the 11-character
hash fits grout's IFNAMSIZ (15-byte) limit. Including `ifName` in the hash
ensures multi-attach pods (multiple NADs on one container) get distinct ports.

### How each CNI verb recovers the attachment

| Verb  | Recovery |
|-------|----------|
| ADD   | Computes the port name, creates the port in grout. |
| DEL   | Recomputes the port name, finds it in `InterfaceList`, deletes it. |
| CHECK | Recomputes the port name, confirms it exists in grout. For TAP, also enters the pod netns and verifies the interface. For virtio, checks the deterministic vhost-user socket. |
| GC    | Lists all `grk-*` ports on this network's bridge, compares against the runtime's valid-attachment list, deletes the difference. |

### Lock path co-location

The CNI serialization lock is co-located with grout's control socket
(`<socket>.lock`) so concurrent CNI calls contending over the same grout
instance serialize correctly, while calls to different grout instances (e.g.
in a multi-instance test setup) do not block each other.

### Vhost-user socket path

For virtio interfaces, the vhost-user socket path is also deterministic:
`<grout-socket-dir>/vhost/<portName>.sock`. DEL removes it; CHECK confirms
it exists.

## Trade-offs

- **Pro**: No secondary state to drift, corrupt, or lose on node reboot.
- **Pro**: No cleanup of stale ref files.
- **Con**: DEL/CHECK/GC must list all grout interfaces (one API call) rather
  than a single file read. Acceptable at CNI call frequency.
- **Con**: The 55-bit hash has a theoretical collision probability (~1 in
  36 trillion for 2000 concurrent attachments). The portname unit tests
  validate no collisions across a representative range.
