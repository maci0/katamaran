# Katamaran Dashboard

A web UI for orchestrating katamaran live migrations, visualizing ping latency (zero-drop proof), and running load generators during cutover.

## Features

- **Migration orchestration** — fill in source/destination node details and trigger `deploy/migrate.sh` with one click
- **Ping latency chart** — real-time Chart.js graph showing per-packet latency; buffered packets during cutover appear as RTT spikes
- **HTTP load generator** — continuous HTTP GET requests to a target, graphed alongside ping data
- **Live stats** — packets transmitted, dropped, average latency, max latency (computed from ping data)
- **Color-coded log viewer** — red for errors, amber for warnings, green for success, blue for `>>>` markers; auto-scrolls with new entries
- **Status badges** — idle (gray), migration running (blue pulse), loadgen active (green pulse)
- **Dark theme** — navy/slate backgrounds with SVG katamaran boat + animated wave header

## API Endpoints

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/healthz` | GET | Kubernetes liveness probe (lightweight, always returns 200 OK) |
| `/readyz` | GET | Kubernetes readiness probe (returns 200 if `migrate.sh` is found, 503 otherwise) |
| `/` | GET | Dashboard frontend |
| `/api/migrate` | POST | Start migration (form fields: `source_node`, `dest_node`, `qmp_source`, `qmp_dest`, `tap`, `tap_netns`, `dest_ip`, `vm_ip`, `image`, `shared_storage`, `downtime`) |
| `/api/migrate/stop` | POST | Cancel running migration |
| `/api/status` | GET | JSON status: `{version, uptime_seconds, migrating, migration_id, migration_elapsed_seconds, last_migration_result, last_migration_error, migrations_started, migrations_succeeded, migrations_failed, loadgen_running, loadgen_type, logs, pings}` |
| `/api/ping?target=<ip>` | POST | Start continuous ping (5/sec) to target |
| `/api/ping/stop` | POST | Stop active ping/loadgen |
| `/api/httpgen?target=<host:port>` | POST | Start HTTP load generator (5 req/sec) to target |
| `/api/httpgen/stop` | POST | Stop active ping/loadgen |
| `/debug/pprof/` | GET | Runtime profiling (requires `--enable-debug`) |
| `/debug/vars` | GET | Runtime metrics via expvar (requires `--enable-debug`) |

## Building the Container

From the repository root (the build context needs `deploy/` for `migrate.sh`):

```bash
make dashboard
```

The Dockerfile supports multi-arch builds via `TARGETARCH` (defaults to `amd64`). To build for `arm64`:

```bash
podman build --platform linux/arm64 -t localhost/katamaran-dashboard:dev -f Dockerfile.dashboard .
```

## Running Locally (Docker/Podman)

```bash
podman run -d --rm -p 8080:8080 \
  -v $HOME/.kube/config:/root/.kube/config:ro \
  --network host \
  localhost/katamaran-dashboard:dev
```

Then visit http://localhost:8080

> **Note:** `--network host` is required so the dashboard can reach the Kubernetes API and migration targets. The kubeconfig mount is needed for `kubectl` commands used by `deploy/migrate.sh`.

## Running In-Cluster

```bash
kubectl apply -f deploy/dashboard.yaml
```

This creates:
- A `ServiceAccount` with RBAC permissions to manage Jobs and read pod logs
- A `Deployment` running the dashboard container
- A `ClusterIP` Service on port **8080**

Access the dashboard via `kubectl port-forward -n kube-system svc/katamaran-dashboard 8080:8080`, then open `http://localhost:8080`.

## Architecture

```text
┌─────────────────────────────────────────────┐
│  Browser (index.html + Chart.js)            │
│  Polls /api/status every 1s                 │
└──────────────┬──────────────────────────────┘
               │ HTTP
┌──────────────▼──────────────────────────────┐
│  Go HTTP server (main.go, port 8080)        │
│  - /api/migrate → exec deploy/migrate.sh    │
│  - /api/ping    → exec ping subprocess      │
│  - /api/httpgen → HTTP GET loop             │
│  - /api/status  → JSON {logs, pings, state} │
└─────────────────────────────────────────────┘
```

The server has zero third-party dependencies. It uses graceful shutdown via `signal.NotifyContext`, HTTP server timeouts, and context-based cancellation for child processes.
