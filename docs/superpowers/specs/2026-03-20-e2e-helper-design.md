# katamaran-e2e-helper Design Spec

## Problem

The E2E test harness (`scripts/e2e.sh`) has outgrown bash for several operations:

- **QMP hotplug:** `printf | nc -U` with sleep delays, no error checking, no response parsing. The project already has a proper QMP client in Go.
- **QEMU topology extraction:** Parsing `/proc/PID/cmdline` with `grep -oP` regex and sed substitutions to reconstruct the destination QEMU command line.
- **tc mirred removal:** nsenter + tc filter del, simple but benefits from proper error reporting.

These operations are fragile, untestable as bash, and increasingly complex as new features (storage modes, NFS) are added.

## Solution

A Go binary (`katamaran-e2e-helper`) providing four subcommands that replace the fragile bash operations. Reuses `internal/qmp` for QMP interactions. Deployed to nodes via the E2E DaemonSet (not the production DaemonSet — this is a test-only binary).

## Subcommands

### `qmp-hotplug-disk`

Hotplug a virtio-blk device into a running QEMU via QMP.

```
katamaran-e2e-helper qmp-hotplug-disk \
  --socket /run/vc/vm/.../extra-monitor.sock \
  --image /tmp/disk.img \
  --node-name drive-virtio-disk0 \
  --pci-bus pci-bridge-0 \
  --pci-addr 0x8
```

Uses `internal/qmp.Client` for proper handshake, sequential `blockdev-add` + `device_add` execution, and error checking. Replaces the `qmp_hotplug_disk()` bash function.

Flags:
- `--socket` (required) — QMP Unix socket path
- `--image` (required) — path to raw disk image
- `--node-name` (default: `drive-virtio-disk0`) — QEMU block node name
- `--pci-bus` (default: `pci-bridge-0`) — PCI bus for device placement
- `--pci-addr` (default: `0x8`) — PCI address slot on the bus

Output: human-readable status to stderr. Exit 0 on success, 1 with error message on failure. QEMU QMP error messages (e.g., "node-name already in use") are surfaced directly.

**Prerequisite:** Requires new `BlockdevAddArgs` and `DeviceAddArgs` types in `internal/qmp/types.go` (sealed `Args` interface).

### `qmp-extract-topology`

Extract QEMU device topology from `/proc/PID/cmdline`.

```
katamaran-e2e-helper qmp-extract-topology --pid 12345
katamaran-e2e-helper qmp-extract-topology --pid 12345 --json
```

Reads `/proc/PID/cmdline` directly (null-delimited), iterates arguments pairwise. For flags like `-name`, `-m`, `-smp`, the value is the next argument. For embedded values like `mem-path=` within `-object` arguments, the value is extracted by string splitting within the argument.

Extracts:
- UUID, MAC address, machine type, SMP config, memory size
- nvdimm image path (from `-object memory-backend-file` with `mem-path=`, excluding `/dev/shm`)
- vsock device ID, chardev-fs ID
- Kernel append line (treated as raw string, not split)
- Sandbox ID (from `-name sandbox-XXXX`)
- Sandbox dir (from QMP socket paths in args)
- Full argument list (for replay)

Flags:
- `--pid` (required) — QEMU process PID
- `--json` — output machine-readable JSON instead of human-readable table

Errors: returns exit 1 with clear message if `/proc/PID/cmdline` is empty or unreadable (process exited, zombie, invalid PID).

### `qmp-build-dest-cmdline`

Generate the destination QEMU command line from source topology.

```
katamaran-e2e-helper qmp-build-dest-cmdline \
  --topology-file /tmp/topo.json \
  --sandbox manual-dst-123 \
  --vm-dir /run/vc/vm/manual-dst-123 \
  --nvdimm-copy /tmp/dst-nvdimm.img \
  --netns /proc/456/ns/net
```

Reads topology JSON from a file. Performs:
1. Path substitutions (source sandbox dir to dest, nvdimm path to writable copy)
2. Strips `-incoming` and `-daemonize` flags (and their arguments) from source args
3. Strips `readonly=on`/`readonly=true` only from the nvdimm `-object` argument (not globally)
4. Prepends `nsenter --net=<netns>` and the QEMU binary path
5. Appends `-incoming defer -daemonize`

Flags:
- `--topology-file` (required) — path to topology JSON file
- `--sandbox` (required) — destination sandbox ID
- `--vm-dir` (required) — destination VM directory path
- `--nvdimm-copy` (required) — path to writable nvdimm image copy
- `--netns` (required) — network namespace path for nsenter

Output: a single-line shell-ready command string (space-separated, properly quoted). This is directly consumable by `node_exec`.

**Note:** No `--topology-file -` (stdin) support. The bash integration writes topology JSON to a tempfile on the node, then passes the path. This avoids `node_exec` stdin piping issues (docker exec needs `-i`, minikube ssh stdin forwarding is unreliable).

### `tc-remove-redirect`

Remove Kata's tc mirred ingress redirect filter.

```
katamaran-e2e-helper tc-remove-redirect --pid 12345 --iface eth0
```

Enters the QEMU process's network namespace via Go's `syscall.Setns()` (with `runtime.LockOSThread`) and executes `tc filter del dev <iface> ingress`. No `nsenter` binary dependency.

Flags:
- `--pid` (required) — QEMU process PID (for network namespace)
- `--iface` (default: `eth0`) — interface to clear redirect from

Exit codes:
- 0 — filter removed, or no filter existed (expected case)
- 1 — unexpected error (invalid PID, permission denied, interface not found)

## Package Structure

```
cmd/katamaran-e2e-helper/
  main.go                  — flag parsing, subcommand dispatch

internal/
  qmp/
    types.go               — add BlockdevAddArgs, DeviceAddArgs (sealed)

  e2ehelper/
    hotplug.go             — qmp-hotplug-disk implementation
    hotplug_test.go        — test with fake QMP server (reuse startFakeQMP pattern)
    topology.go            — extract-topology + build-dest-cmdline
    topology_test.go       — test with fixture cmdline data
    tc.go                  — tc-remove-redirect (command construction)
    tc_test.go             — test command construction (execution requires root)
```

All code in `internal/e2ehelper` — not a public API. Each file is focused and independently testable.

Test fixtures: `internal/e2ehelper/testdata/cmdline.fixture` — a real `/proc/PID/cmdline` captured from a Kata QEMU process for topology parsing tests.

## Integration With e2e.sh

The bash script calls the helper via `node_exec`:

```bash
# Extract topology (replaces grep -oP / sed parsing)
TOPO_FILE="/tmp/katamaran-topo-$$.json"
node_exec "${NODE1}" "katamaran-e2e-helper qmp-extract-topology --pid ${PID} --json > ${TOPO_FILE}"

# Hotplug disk (replaces printf | nc -U)
node_exec "${NODE1}" "katamaran-e2e-helper qmp-hotplug-disk --socket ${SOCK} --image ${IMG}"

# Copy topology to dest node
node_exec "${NODE1}" "cat ${TOPO_FILE}" | node_exec "${NODE2}" "cat > ${TOPO_FILE}"

# Build dest command (replaces sed/while-loop reconstruction)
DST_CMD=$(node_exec "${NODE2}" "katamaran-e2e-helper qmp-build-dest-cmdline \
    --topology-file ${TOPO_FILE} --sandbox ${ID} --vm-dir ${DIR} \
    --nvdimm-copy ${DST_IMG} --netns /proc/${PID}/ns/net")
node_exec "${NODE2}" "${DST_CMD}"

# Remove tc redirect (replaces nsenter --net=... tc filter del)
node_exec "${NODE1}" "katamaran-e2e-helper tc-remove-redirect --pid ${PID}"
```

## Build and Deploy

- `make build` produces both `bin/katamaran` and `bin/katamaran-e2e-helper`
- `Dockerfile` copies both binaries into the image
- DaemonSet init container installs both to `/usr/local/bin/` on nodes
- `make test` runs helper unit tests alongside existing tests

**E2E-only deployment:** The helper binary is a test tool. Production deployments should use the main `katamaran` binary only. The DaemonSet in `deploy/daemonset.yaml` installs both because E2E tests reuse it, but production users deploying via other means should only deploy `katamaran`.

## What Stays in Bash

- Cluster provisioning (minikube start, kind create)
- kubectl operations (pod creation, waiting, log collection)
- NFS server deployment and mount
- virtiofsd startup (the `--migration-on-error=guest-error` flag is critical but the operation is a simple `nohup` one-liner that doesn't benefit from Go extraction)
- Calling `deploy/migrate.sh` for the actual migration
- Ping proof verification (grep through logs)
- Module building (`build-minikube-modules.sh`)
- Sandbox directory creation, nvdimm copy, tap interface creation (simple shell commands)

## Output Convention

All subcommands follow the same pattern:
- **Human-readable by default** — operators running manual steps see formatted output
- **`--json` flag** — machine-readable JSON for scripting (e2e.sh always passes this)
- **Errors to stderr** — never mixed with data output
- **Exit codes** — 0 success, 1 failure with descriptive message

## Topology JSON Schema

```json
{
  "pid": 12345,
  "binary": "/opt/kata/bin/qemu-system-x86_64",
  "uuid": "abc-def-123",
  "mac": "52:54:00:ab:cd:ef",
  "machine": "q35,accel=kvm,nvdimm=on",
  "smp": "2,cores=2,threads=1,sockets=1",
  "mem": "2048M",
  "nvdimm_path": "/opt/kata/share/kata-containers/kata-ubuntu-noble.image",
  "vsock_id": "vsock-12345",
  "chardev_fs_id": "char-virtiofs",
  "kernel_append": "tsc=reliable no_timer_check ...",
  "sandbox_id": "abc123",
  "sandbox_dir": "/run/vc/vm/abc123",
  "args": ["/opt/kata/bin/qemu-system-x86_64", "-name", "sandbox-abc123", "..."]
}
```

The `args` field contains the full argument list for replay by `qmp-build-dest-cmdline`.

Fields are extracted by iterating the argument list pairwise. `sandbox_id` is extracted from the `-name sandbox-XXXX` argument. `sandbox_dir` is inferred from QMP socket paths in the argument list. `nvdimm_path` is extracted from the `-object memory-backend-file` argument containing `mem-path=`, excluding entries pointing to `/dev/shm` (which is the RAM backend, not the rootfs image).
