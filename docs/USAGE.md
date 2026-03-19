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
katamaran --mode <source|dest> [flags]
```

## Flags

### Common flags

| Flag | Required | Default | Description |
|------|----------|---------|-------------|
| `--mode` | yes | `""` | Migration role: `source` or `dest` |
| `--qmp` | no | `/run/vc/vm/extra-monitor.sock` | QEMU QMP socket path |
| `--drive-id` | no | `drive-virtio-disk0` | QEMU block device id |
| `--shared-storage` | no | `false` | Skip NBD storage mirroring |
| `--multifd-channels` | no | `4` | Parallel TCP channels for RAM migration (0 to disable) |

### Source mode flags

| Flag | Required | Default | Description |
|------|----------|---------|-------------|
| `--dest-ip` | yes | `""` | Destination node IP |
| `--vm-ip` | yes | `""` | VM pod IP used for route/tunnel cutover |
| `--tunnel-mode` | no | `ipip` | `ipip`, `gre`, or `none` |
| `--downtime` | no | `25` | Maximum allowed downtime during VM pause (ms) |
| `--auto-downtime` | no | `false` | Auto-calculate downtime based on RTT (overrides `--downtime`) |

### Destination mode flags

| Flag | Required | Default | Description |
|------|----------|---------|-------------|
| `--tap` | recommended | `""` | Destination tap interface for `tc sch_plug` buffering |
| `--tap-netns` | no | `""` | Network namespace path for tap interface (e.g. `/proc/PID/ns/net`) |

## Direct CLI Usage

### 1) Destination node (run first)

```bash
sudo /usr/local/bin/katamaran \
  --mode dest \
  --qmp /run/vc/vm/<sandbox-id>/extra-monitor.sock \
  --tap tap0_kata
```

### 2) Source node

```bash
sudo /usr/local/bin/katamaran \
  --mode source \
  --qmp /run/vc/vm/<sandbox-id>/extra-monitor.sock \
  --dest-ip <destination-node-ip> \
  --vm-ip <vm-pod-ip> \
  --tunnel-mode ipip
```

### Shared storage mode (Ceph/NFS)

```bash
# destination
sudo /usr/local/bin/katamaran --mode dest --qmp /run/vc/vm/<id>/extra-monitor.sock --tap tap0_kata --shared-storage

# source
sudo /usr/local/bin/katamaran --mode source --qmp /run/vc/vm/<id>/extra-monitor.sock \
  --dest-ip <destination-node-ip> --vm-ip <vm-pod-ip> --shared-storage
```

### GRE mode (cloud VPC networks)

```bash
sudo /usr/local/bin/katamaran --mode source --qmp /run/vc/vm/<id>/extra-monitor.sock \
  --dest-ip <destination-node-ip> --vm-ip <vm-pod-ip> --tunnel-mode gre
```

### Tap in a different network namespace

```bash
sudo /usr/local/bin/katamaran --mode dest --qmp /run/vc/vm/<id>/extra-monitor.sock \
  --tap tap0_kata --tap-netns /proc/12345/ns/net
```

### Auto-downtime calculation

```bash
sudo /usr/local/bin/katamaran --mode source --qmp /run/vc/vm/<id>/extra-monitor.sock \
  --dest-ip <destination-node-ip> --vm-ip <vm-pod-ip> --auto-downtime
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

- `--dest-ip` and `--vm-ip` are required in `source` mode
- `--dest-ip` and `--vm-ip` must be the same address family
- `--tunnel-mode` must be `ipip`, `gre`, or `none`
- `--downtime` must be a positive integer
- Source-only flags in dest mode (and vice versa) produce warnings
- Job orchestration requires `--tap` for zero-drop buffering path

## Operational Notes

- NBD storage mirror port: `10809`
- RAM migration port: `4444`
- `--tap` is critical for `sch_plug` buffering during STOP→RESUME cutover
- On failure, `deploy/migrate.sh` keeps jobs for forensic debugging output

## Troubleshooting

- `invalid --tunnel-mode`
  - use `ipip`, `gre`, or `none`
- `migration did not complete`
  - check logs from source and destination jobs/services

For the full error reference (`dialing QMP socket`, `failed to add plug qdisc`, storage/RAM timeouts, etc.), see the [Troubleshooting](../docs/TESTING.md#troubleshooting) table in the Testing Guide.
