# Usage Guide

This guide shows how to run `katamaran` in both direct CLI mode and Kubernetes Job mode.

## Command Overview

`katamaran` has two modes:

- `dest` — destination-side listener and packet buffering setup
- `source` — source-side migration orchestrator

Build the tool:

```bash
make
```

General form:

```bash
katamaran -mode <source|dest> [flags]
```

## Flags

| Flag | Required | Mode | Default | Description |
|------|----------|------|---------|-------------|
| `-mode` | yes | both | `""` | Migration role: `source` or `dest` |
| `-qmp` | no | both | `/run/vc/vm/extra-monitor.sock` | QEMU QMP socket path |
| `-tap` | recommended | dest | `""` | Destination tap interface for `tc sch_plug` buffering |
| `-dest-ip` | yes | source | `""` | Destination node IP |
| `-vm-ip` | yes | source | `""` | VM pod IP used for route/tunnel cutover |
| `-drive-id` | no | both | `drive-virtio-disk0` | QEMU block device id |
| `-shared-storage` | no | both | `false` | Skip NBD storage mirroring |
| `-tunnel-mode` | no | source | `ipip` | `ipip` or `gre` |
| `-downtime` | no | source | `25` | Maximum allowed downtime during VM pause (ms) |

## Direct CLI Usage

### 1) Destination node (run first)

```bash
sudo /usr/local/bin/katamaran \
  -mode dest \
  -qmp /run/vc/vm/<sandbox-id>/extra-monitor.sock \
  -tap tap0_kata
```

### 2) Source node

```bash
sudo /usr/local/bin/katamaran \
  -mode source \
  -qmp /run/vc/vm/<sandbox-id>/extra-monitor.sock \
  -dest-ip <destination-node-ip> \
  -vm-ip <vm-pod-ip> \
  -tunnel-mode ipip
```

### Shared storage mode (Ceph/NFS)

```bash
# destination
sudo /usr/local/bin/katamaran -mode dest -qmp /run/vc/vm/<id>/extra-monitor.sock -tap tap0_kata -shared-storage

# source
sudo /usr/local/bin/katamaran -mode source -qmp /run/vc/vm/<id>/extra-monitor.sock \
  -dest-ip <destination-node-ip> -vm-ip <vm-pod-ip> -shared-storage
```

### GRE mode (cloud VPC networks)

```bash
sudo /usr/local/bin/katamaran -mode source -qmp /run/vc/vm/<id>/extra-monitor.sock \
  -dest-ip <destination-node-ip> -vm-ip <vm-pod-ip> -tunnel-mode gre
```

## Kubernetes Job-Based Usage

The repository includes:

- `deploy/job-dest.yaml`
- `deploy/job-source.yaml`
- `deploy/migrate.sh`

`deploy/migrate.sh` renders templates with `envsubst`, starts destination job first, waits for readiness, starts source job, then collects logs.

### Required inputs

- source node name
- destination node name
- destination tap interface name
- source and destination QMP socket paths
- destination node IP
- VM pod IP
- image reference

### Example

```bash
deploy/migrate.sh \
  --source-node <source-node-name> \
  --dest-node <dest-node-name> \
  --tap <dest-tap-iface> \
  --qmp-source /run/vc/vm/<src-id>/extra-monitor.sock \
  --qmp-dest /run/vc/vm/<dst-id>/extra-monitor.sock \
  --dest-ip <destination-node-ip> \
  --vm-ip <vm-pod-ip> \
  --image localhost/katamaran:dev \
  --shared-storage \
  --tunnel-mode ipip \
  --downtime 25 \
  --context <kube-context>
```

Show orchestrator help:

```bash
deploy/migrate.sh --help
```

## Validation Rules

- `-dest-ip` and `-vm-ip` are required in `source` mode
- `-dest-ip` and `-vm-ip` must be the same address family
- `-tunnel-mode` must be `ipip` or `gre`
- Job orchestration requires `--tap` for zero-drop buffering path

## Operational Notes

- NBD storage mirror port: `10809`
- RAM migration port: `4444`
- `-tap` is critical for `sch_plug` buffering during STOP→RESUME cutover
- On failure, `deploy/migrate.sh` keeps jobs for forensic debugging output

## Troubleshooting

- `invalid -tunnel-mode`
  - use `ipip` or `gre`
- `migration did not complete`
  - check logs from source and destination jobs/services

For the full error reference (`dialing QMP socket`, `failed to add plug qdisc`, storage/RAM timeouts, etc.), see the [Troubleshooting](../docs/TESTING.md#troubleshooting) table in the Testing Guide.
