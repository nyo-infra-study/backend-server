# Backend Server

Simple Go HTTP server that stores `name` and `message` in a JSON file. Includes health check endpoints for Kubernetes probes.

## API Endpoints

| Method  | Path       | Request             | Response          | Description                |
| ------- | ---------- | ------------------- | ----------------- | -------------------------- |
| `GET`   | `/healthz` | —                   | `{status}`        | Liveness probe             |
| `GET`   | `/readyz`  | —                   | `{status}`        | Readiness probe            |
| `GET`   | `/data`    | —                   | `{name, message}` | Get current data           |
| `PATCH` | `/data`    | `{name?, message?}` | `{name, message}` | Update name and/or message |

## Local Development

```bash
# Run from source
go run main.go

# Or build and run
go build -o backend-server .
./backend-server
```

Server starts on port `9000` by default. Set `PORT` env var to change it.

Data is persisted to `data.json` in the working directory.

## Docker

### Build

```bash
docker build -t backend-server .
```

### Run

```bash
docker run -p 9000:9000 backend-server
```

### Push to Docker Hub

```bash
docker tag backend-server <your-dockerhub-username>/backend-server:latest
docker push <your-dockerhub-username>/backend-server:latest
```

## Kubernetes (via Helm + ArgoCD)

We deploy this using a **Helm chart** managed by **ArgoCD**. The expected rendered output from Helm looks something like this:

```yaml
# Expected output from Helm chart
apiVersion: apps/v1
kind: Deployment
metadata:
  name: backend-server
spec:
  replicas: 1
  selector:
    matchLabels:
      app: backend-server
  template:
    metadata:
      labels:
        app: backend-server
    spec:
      containers:
        - name: backend-server
          image: <your-dockerhub-username>/backend-server:latest
          ports:
            - containerPort: 9000
          livenessProbe:
            httpGet:
              path: /healthz
              port: 9000
          readinessProbe:
            httpGet:
              path: /readyz
              port: 9000
---
apiVersion: v1
kind: Service
metadata:
  name: backend-server
spec:
  selector:
    app: backend-server
  ports:
    - port: 9000
      targetPort: 9000
```
