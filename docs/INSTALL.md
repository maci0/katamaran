# Installation Guide

This guide covers three install paths:

1. Local binary build (single host or manual node copy)
2. Container image build
3. Kubernetes node install via DaemonSet

## Prerequisites

- Linux host
- Go 1.24+
- Root privileges on nodes where migration runs (`sudo`)
- `iproute2` tools (`ip`, `tc`)
- Kernel modules available on migration nodes:
  - `sch_plug`
  - `ipip`
  - `ip6_tunnel`
  - `ip_gre`
  - `ip6_gre`

**AMD Zen 4+ hosts:** Disable AVIC before running Kata VMs. A known AMD errata (#1235) causes KVM crashes with nested virtualization when AVIC is enabled (default since Linux 6.18). See the [Testing Guide](TESTING.md#disable-avic-on-amd-zen-4-hosts) for details.

For Kubernetes install paths:

- Kubernetes cluster
- Kata Containers runtime installed on target nodes
- `kubectl`
- `podman` (or Docker-compatible workflow)

## Option 1: Build Local Binary

From repository root:

```bash
make
```

Or manually:

```bash
go build -o bin/katamaran ./cmd/katamaran/
```

Install globally on a host:

```bash
sudo install -m 0755 ./bin/katamaran /usr/local/bin/katamaran
```

Quick sanity check:

```bash
katamaran --help
```

## Option 2: Build Container Image

Build image:

```bash
make image
```

Or manually using podman/docker directly:

```bash
podman build -t localhost/katamaran:dev .
```

Sanity check:

```bash
podman run --rm localhost/katamaran:dev --help
```

## Option 3: Install on Kubernetes Nodes (DaemonSet)

This installs `katamaran` onto `/usr/local/bin/katamaran` on nodes labeled for Kata runtime. The DaemonSet also loads the kernel modules needed by katamaran (`ipip`, `ip6_tunnel`, `ip_gre`, `ip6_gre`, `sch_plug`) and enables the Kata QMP extra-monitor socket when the default Kata 3.25+ QEMU config path is present.

### Step 1: Build image

```bash
make image
```

### Step 2: Load image into cluster

(The `make image` command automatically exports `katamaran.tar`)

Minikube example:

```bash
minikube -p <profile> image load katamaran.tar
```

Kind example:

```bash
kind load image-archive katamaran.tar --name <cluster-name>
```

### Step 3: Apply DaemonSet

```bash
kubectl apply -f deploy/daemonset.yaml
kubectl -n kube-system rollout status daemonset/katamaran-deploy --timeout=120s
```

### Step 4: Verify install

```bash
kubectl -n kube-system get pods -l app=katamaran
```

Then on a target node:

```bash
ls -l /usr/local/bin/katamaran
/usr/local/bin/katamaran --help
```

## Job-Based Migration Install (Optional)

If you plan to run migrations through Kubernetes Jobs, these assets are included:

- `deploy/job-dest.yaml`
- `deploy/job-source.yaml`
- `deploy/migrate.sh` *(legacy shell harness — kept for ad-hoc CLI runs and CI smoke; production paths use the in-cluster Native orchestrator)*

The dashboard (`deploy/dashboard.yaml`) and the standalone `katamaran-orchestrator` CLI both run migrations through the Native orchestrator (client-go), which embeds the same Job templates and submits them directly via the apiserver — no `envsubst`, no kubectl, no `migrate.sh` invocation. `deploy/migrate.sh` remains useful when you want to drive a one-off migration from a developer shell without standing up the dashboard.

Show required flags for the legacy shell path:

```bash
deploy/migrate.sh --help
```

## Uninstall

### Remove DaemonSet install

```bash
kubectl -n kube-system delete daemonset katamaran-deploy
```

The DaemonSet preStop hook removes `/usr/local/bin/katamaran` from host nodes.

### Remove local binary

```bash
sudo rm -f /usr/local/bin/katamaran
rm -rf ./bin
```

### Remove local image

```bash
podman rmi localhost/katamaran:dev
```

## Troubleshooting

- `modprobe: not found` in job init container
  - Ensure runtime image includes `kmod` and node has required modules.
- DaemonSet not scheduling
  - Check node label `katacontainers.io/kata-runtime=true`.

For runtime errors (`dialing QMP socket`, `failed to add plug qdisc`, etc.), see the full [Troubleshooting](TESTING.md#troubleshooting) table in the Testing Guide.
