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
| `--log-format` | no | `text` | Log output format: `text` or `json` |
| `--log-level` | no | `info` | Log level: `debug`, `info`, `warn`, or `error` |
| `--version`, `-v` | no | — | Show version and exit |

### Source mode flags

| Flag | Required | Default | Description |
|------|----------|---------|-------------|
| `--dest-ip` | yes | `""` | Destination node IP |
| `--vm-ip` | when not in pod mode | `""` | VM pod IP used for route/tunnel cutover |
| `--pod-name` | alt to --vm-ip+--qmp | `""` | Source pod name; resolver finds sandbox + VM IP at runtime |
| `--pod-namespace` | with --pod-name | `""` | Source pod namespace |
| `--emit-cmdline-to` | no | `""` | Capture source QEMU `/proc/<pid>/cmdline` to this path before migration; used by replay-cmdline orchestration |
| `--tunnel-mode` | no | `ipip` | `ipip`, `gre`, or `none` |
| `--downtime` | no | `25` | Maximum allowed downtime during VM pause, 1-60000 (ms) |
| `--auto-downtime` | no | `false` | Auto-calculate downtime based on RTT (overrides `--downtime`) |

### Destination mode flags

| Flag | Required | Default | Description |
|------|----------|---------|-------------|
| `--tap` | recommended | `""` | Destination tap interface for `tc sch_plug` buffering |
| `--tap-netns` | no | `""` | Network namespace path for tap interface (e.g. `/proc/PID/ns/net`) |
| `--dest-pod-name` | alt to --qmp | `""` | Destination pod name; resolver finds sandbox QMP socket at runtime |
| `--dest-pod-namespace` | with --dest-pod-name | `""` | Destination pod namespace |
| `--replay-cmdline` | no | `""` | Path to a captured source QEMU cmdline file. When set, dest spawns its own QEMU with the replayed cmdline + `-incoming defer` (no kata sandbox needed on dest). |

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

All modes require:

- source node name
- destination node name
- destination node IP
- image reference

Legacy explicit-fields mode also requires:

- destination tap interface name
- source and destination QMP socket paths
- VM pod IP

Pod-picker mode requires source pod name + namespace instead of source QMP, VM IP, and tap values; the resolver derives those at runtime.

### Example (legacy explicit-fields mode)

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

### Pod-picker mode (recommended)

Skip the manual sandbox/UUID/PID lookup. Pass a source pod name + namespace; the source job's resolver finds the QEMU PID, sandbox UUID, pod IP, and tap netns at runtime. Add `--replay-cmdline` so the destination job spawns its own QEMU (no kata pod needed on dest).

```bash
deploy/migrate.sh \
  --source-node <source-node-name> \
  --dest-node <dest-node-name> \
  --pod-name kata-demo \
  --pod-namespace default \
  --dest-ip <destination-node-ip> \
  --image localhost/katamaran:dev \
  --shared-storage \
  --replay-cmdline
```

Same flow from the Dashboard:

```bash
curl -sS -X POST http://127.0.0.1:8080/api/migrate \
  -d source_pod_namespace=default \
  -d source_pod_name=kata-demo \
  -d dest_node=<dest-node-name> \
  -d image=localhost/katamaran:dev \
  -d downtime=25 \
  -d shared_storage=true \
  -d replay_cmdline=true
```

See [`cmd/dashboard/README.md`](../cmd/dashboard/README.md) for the full UI flow + screenshots.

Show orchestrator help:

```bash
deploy/migrate.sh --help
```

## Structured CLI: `katamaran-orchestrator`

`bin/katamaran-orchestrator` is a thin wrapper around the same Go orchestrator package the dashboard uses. It reads a single `orchestrator.Request` JSON object on stdin and emits newline-delimited JSON `StatusUpdate` events on stdout. Exit code: 0 on success, 1 on migration failure, 2 on input error.

Useful for CI pipelines and the Migration CRD controller — no shell parsing of `migrate.sh` output.

```bash
echo '{
  "SourceNode":"worker-a",
  "DestNode":"worker-b",
  "DestIP":"10.0.0.20",
  "Image":"localhost/katamaran:dev",
  "SourcePod":{"Namespace":"default","Name":"kata-demo"},
  "DestPod":{"Namespace":"default","Name":"kata-dest-shell"},
  "SharedStorage":true,
  "ReplayCmdline":true
}' | bin/katamaran-orchestrator
```

Sample stdout:

```jsonl
{"id":"a1b2c3...","phase":"submitted","time":"2026-04-27T05:25:32.243Z"}
{"id":"a1b2c3...","phase":"succeeded","time":"2026-04-27T05:25:58.314Z"}
```

The same Go package (`internal/orchestrator`) backs the dashboard's `POST /api/migrate` handler — anything callable from the dashboard is callable from the CLI and vice versa.

## Environment Variables

| Variable | Description |
|----------|-------------|
| `KATAMARAN_MIGRATION_ID` | Correlation ID added to all log entries (set by the dashboard) |

## Validation Rules

- Source mode requires `--dest-ip` plus either `--vm-ip` or `--pod-name` + `--pod-namespace`
- When `--vm-ip` is supplied explicitly, `--dest-ip` and `--vm-ip` must be the same address family
- `--tunnel-mode` must be `ipip`, `gre`, or `none`
- `--downtime` must be between 1 and 60000
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

For the full error reference (`dialing QMP socket`, `failed to add plug qdisc`, storage/RAM timeouts, etc.), see the [Troubleshooting](TESTING.md#troubleshooting) table in the Testing Guide.
