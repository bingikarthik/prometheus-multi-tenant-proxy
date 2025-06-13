# Team BKarthik Prometheus Setup Example

This directory contains example YAML files for setting up Prometheus as a StatefulSet for team `bkarthik` with the corresponding MetricAccess custom resource.

## Files

1. **team-bkarthik-prometheus-deployment.yaml** - Complete setup with all components
2. **team-bkarthik-minimal.yaml** - Minimal setup for quick testing

## Architecture

```
┌─────────────────────────┐
│  Infrastructure         │
│  Prometheus             │
│                         │
│  Metrics with label:    │
│  source="team-bkarthik" │
└───────────┬─────────────┘
            │
            │ Multi-Tenant Proxy
            │ reads MetricAccess CR
            │
            ▼
┌─────────────────────────┐
│  MetricAccess CR        │
│  namespace:             │
│  ns-team-bkarthik       │
│                         │
│  source: team-bkarthik  │
│  remoteWrite:           │
│    serviceName:         │
│    prometheus-test      │
└───────────┬─────────────┘
            │
            │ Remote Write to
            │ /api/v1/write
            ▼
┌─────────────────────────┐
│  Team Prometheus        │
│  (StatefulSet)          │
│                         │
│  Service:               │
│  prometheus-test:9090   │
│                         │
│  namespace:             │
│  ns-team-bkarthik       │
└─────────────────────────┘
```

## Deployment Steps

### 1. Create the namespace (if it doesn't exist)

```bash
kubectl create namespace ns-team-bkarthik
```

### 2. Deploy the complete setup

```bash
kubectl apply -f team-bkarthik-prometheus-deployment.yaml
```

Or for minimal setup:

```bash
kubectl apply -f team-bkarthik-minimal.yaml
```

### 3. Verify the deployment

```bash
# Check if StatefulSet is running
kubectl get statefulset -n ns-team-bkarthik

# Check if pods are running
kubectl get pods -n ns-team-bkarthik

# Check if service is created
kubectl get service prometheus-test -n ns-team-bkarthik

# Check MetricAccess resource
kubectl get metricaccess -n ns-team-bkarthik
```

### 4. Access Prometheus UI

```bash
# Port forward to access Prometheus UI
kubectl port-forward -n ns-team-bkarthik svc/prometheus-test 9090:9090

# Open browser at http://localhost:9090
```

## Key Components Explained

### StatefulSet Configuration

- **Service Name**: `prometheus-test` - This is referenced in the MetricAccess CR
- **Remote Write Receiver**: Enabled with `--web.enable-remote-write-receiver` flag
- **Storage**: Uses PVC in production setup, emptyDir in minimal setup

### Service Configuration

```yaml
metadata:
  name: prometheus-test  # This name is used in MetricAccess
spec:
  ports:
  - port: 9090  # This port is used in MetricAccess
```

### MetricAccess Configuration

```yaml
spec:
  source: team-bkarthik  # Identifies metrics in infra Prometheus
  
  metrics:  # Patterns of metrics this team can access
    - "container_cpu_usage_seconds_total"
    - '{namespace="ns-team-bkarthik"}'
  
  remoteWrite:
    target:
      prometheus:
        serviceName: "prometheus-test"  # Must match Service name
        servicePort: 9090              # Must match Service port
```

## How It Works

1. **Infrastructure Prometheus** collects metrics from the entire cluster
2. **Multi-Tenant Proxy** reads the MetricAccess CR to understand:
   - Which metrics team bkarthik can access (`metrics` field)
   - Which source to filter by (`source` field)
   - Where to send the metrics (`remoteWrite` configuration)
3. **Proxy queries** infrastructure Prometheus for allowed metrics
4. **Proxy writes** the metrics to team's Prometheus at `http://prometheus-test.ns-team-bkarthik.svc.cluster.local:9090/api/v1/write`
5. **Team's Prometheus** stores the metrics locally for the team to query

## Testing the Setup

### 1. Check if remote write is working

```bash
# Check Prometheus logs
kubectl logs -n ns-team-bkarthik prometheus-0

# Look for remote write receiver logs
```

### 2. Query metrics in team's Prometheus

```bash
# Port forward
kubectl port-forward -n ns-team-bkarthik svc/prometheus-test 9090:9090

# Query via API
curl http://localhost:9090/api/v1/query?query=up
```

### 3. Verify MetricAccess is processed

```bash
# Check MetricAccess status
kubectl describe metricaccess -n ns-team-bkarthik bkarthik-metrics
```

## Troubleshooting

1. **Prometheus not starting**: Check logs with `kubectl logs -n ns-team-bkarthik prometheus-0`
2. **Service not accessible**: Verify service endpoints with `kubectl get endpoints -n ns-team-bkarthik`
3. **Remote write not working**: Ensure `--web.enable-remote-write-receiver` flag is set
4. **No metrics appearing**: Check if multi-tenant proxy is running and has access to both namespaces

## Customization

- **Metrics Access**: Modify the `metrics` array in MetricAccess to control which metrics the team can access
- **Storage**: Replace `emptyDir` with a PersistentVolumeClaim for production use
- **Resources**: Adjust CPU/memory requests and limits based on workload
- **Retention**: Add `--storage.tsdb.retention.time=15d` to control data retention 