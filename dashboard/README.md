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
| `/` | GET | Dashboard frontend |
| `/api/migrate` | POST | Start migration (form fields: `source_node`, `dest_node`, `qmp_source`, `qmp_dest`, `tap`, `dest_ip`, `vm_ip`, `image`, `shared_storage`) |
| `/api/status` | GET | JSON status: `{migrating, logs, pings}` |
| `/api/ping?target=<ip>` | GET | Start continuous ping (5/sec) to target |
| `/api/ping/stop` | GET | Stop active ping/loadgen |
| `/api/httpgen?target=<host:port>` | GET | Start HTTP load generator (5 req/sec) to target |

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
kubectl apply -f dashboard/dashboard.yaml
```

This creates:
- A `ServiceAccount` with RBAC permissions to manage Jobs and read pod logs
- A `Deployment` running the dashboard container
- A `NodePort` Service exposing port **30080**

Access the dashboard at `http://<any-node-ip>:30080`.

## Architecture

```text
┌─────────────────────────────────────────────┐
│  Browser (index.html + Chart.js)            │
│  Polls /api/status every 2s                 │
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

The server is stdlib-only Go (zero external dependencies). It uses graceful shutdown via `signal.NotifyContext`, HTTP server timeouts, and context-based cancellation for child processes.
