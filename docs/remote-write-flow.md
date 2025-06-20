# Remote Write Flow

## Overview

The Prometheus Multi-Tenant Proxy handles remote write by:
1. Collecting metrics from Infrastructure Prometheus
2. Sending them to tenant-specific Prometheus instances using the standard `/api/v1/write` endpoint

## Flow Diagram

```
┌─────────────────────┐
│  Infrastructure     │
│    Prometheus       │
│                     │
│  Metrics:           │
│  - node_cpu_usage   │
│  - node_memory      │
│  - container_*      │
└──────────┬──────────┘
           │
           │ Query /api/v1/query
           │ (Pull metrics)
           ▼
┌─────────────────────┐
│  Multi-Tenant       │
│     Proxy           │
│                     │
│  RemoteWrite        │
│  Controller         │
└──────────┬──────────┘
           │
           │ POST /api/v1/write
           │ (Push metrics)
           ▼
┌─────────────────────┐
│  Tenant Prometheus  │
│  (Core Prometheus)  │
│                     │
│  namespace: webapp  │
│  service: prometheus│
│  port: 9090        │
└─────────────────────┘
```

## Configuration Example

When a tenant configures remote write:

```yaml
apiVersion: observability.ethos.io/v1alpha1
kind: MetricAccess
metadata:
  name: webapp-metrics
  namespace: webapp-team
spec:
  metrics:
    - "http_requests_total"
    - "http_request_duration_seconds"
  source: webapp-team
  
  remoteWrite:
    enabled: true
    interval: "30s"
    target:
      type: "prometheus"
      prometheus:
        serviceName: "prometheus"  # Tenant's Prometheus service
        servicePort: 9090         # Tenant's Prometheus port
```

## What Happens Internally

1. **Metric Collection**: The proxy queries the Infrastructure Prometheus to collect the specified metrics

2. **URL Construction**: The proxy automatically constructs the remote write URL:
   ```
   http://prometheus.webapp-team.svc.cluster.local:9090/api/v1/write
   ```
   
   Note: `/api/v1/write` is ALWAYS used - it's the standard Prometheus remote write endpoint

3. **Remote Write**: The proxy sends the metrics to the tenant's Prometheus using the Prometheus remote write protocol

## Why We Removed the Path Configuration

- The path is ALWAYS `/api/v1/write` for Prometheus remote write
- This is a Prometheus standard that cannot be changed
- Allowing configuration would only introduce potential for errors
- The proxy handles this automatically, ensuring correct behavior

## Benefits

1. **Simplicity**: Tenants only need to specify their Prometheus service name and port
2. **Reliability**: No chance of misconfiguration with wrong paths
3. **Standards Compliance**: Always uses the correct Prometheus remote write endpoint 