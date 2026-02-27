# Katamaran Dashboard

A simple web UI to orchestrate Katamaran live migrations, visualize ping latency (zero-drop proof), and generate HTTP load during cutover.

## Building the Container

From the repository root (to include the `deploy/` directory in the build context):

```bash
podman build -t localhost/katamaran-dashboard:dev -f Dockerfile.dashboard .
```

## Running Locally (Docker/Podman)

If you are running the Katamaran E2E test environments (like minikube or kind) locally on your host, you can run the dashboard and expose its port:

```bash
podman run -d --rm -p 8080:8080 -v $HOME/.kube/config:/root/.kube/config:ro --network host localhost/katamaran-dashboard:dev
```
Then visit http://localhost:8080

## Running In-Cluster

```bash
kubectl apply -f dashboard/dashboard.yaml
```
The dashboard will be exposed as a NodePort service on port `30080`.
