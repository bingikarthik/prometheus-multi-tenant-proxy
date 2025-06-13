# Prometheus Multi-Tenant Proxy

A sophisticated, open-source Go application that provides secure multi-tenant access to Prometheus metrics in Kubernetes environments. This proxy implements dynamic service discovery, tenant-based access control, and intelligent request routing.

## Features

### 🔍 Dynamic Service Discovery
- **Kubernetes API Integration**: Automatically discovers Prometheus instances using Kubernetes service discovery
- **Label-based Filtering**: Filter services based on labels like `app=prometheus`, `role=infra`, or `tenant=admin`
- **Annotation Support**: Additional filtering using Kubernetes annotations
- **Multi-Resource Support**: Discover from Services, Pods, or Endpoints
- **Continuous Refresh**: Automatically updates the list of available backends

### 🏢 Multi-Tenant Architecture
- **Custom Resource Definitions**: Uses `MetricAccess` CRDs to define tenant access rules
- **Flexible Metric Patterns**: Support for exact matches, regex patterns, and PromQL-style selectors
- **Namespace Isolation**: Tenant isolation based on Kubernetes namespaces
- **Dynamic Configuration**: Real-time updates when tenant configurations change

### 🚦 Advanced Proxy Features
- **Load Balancing**: Round-robin and random load balancing across healthy backends
- **Health Checking**: Automatic health monitoring of Prometheus backends
- **Request Filtering**: Filter metrics based on tenant access rules
- **Caching**: Optional query result caching for improved performance

### 📊 Observability
- **Prometheus Metrics**: Built-in metrics for monitoring proxy performance
- **Structured Logging**: JSON-formatted logs with configurable levels
- **Health Endpoints**: Health check and debug endpoints
- **Request Tracing**: Detailed request logging for troubleshooting

## Architecture

```
┌─────────────┐    ┌──────────────────┐    ┌─────────────┐
│   Client    │───▶│  Multi-Tenant    │───▶│ Prometheus  │
│             │    │     Proxy        │    │  Instance 1 │
└─────────────┘    │                  │    └─────────────┘
                   │  ┌─────────────┐ │    ┌─────────────┐
                   │  │ Service     │ │───▶│ Prometheus  │
                   │  │ Discovery   │ │    │  Instance 2 │
                   │  └─────────────┘ │    └─────────────┘
                   │                  │
                   │  ┌─────────────┐ │    ┌─────────────┐
                   │  │ Tenant      │ │    │ MetricAccess│
                   │  │ Manager     │ │◀───│   CRDs      │
                   │  └─────────────┘ │    └─────────────┘
                   └──────────────────┘
```

## Quick Start

### Prerequisites

- Kubernetes cluster (1.19+)
- Go 1.21+ (for building from source)
- kubectl configured to access your cluster

### 1. Deploy the CRD

```bash
kubectl apply -f deploy/kubernetes/crd.yaml
```

### 2. Create the namespace and deploy the proxy

```bash
make k8s-deploy
```

### 3. Create a tenant configuration

```yaml
apiVersion: observability.ethos.io/v1alpha1
kind: MetricAccess
metadata:
  name: my-app-metrics
  namespace: my-namespace
spec:
  metrics:
    - "http_requests_total"
    - "http_request_duration_seconds"
    - '{job="my-app"}'
  source: my-namespace
```

```bash
kubectl apply -f my-tenant-config.yaml
```

### 4. Query metrics through the proxy

```bash
curl -H "X-Tenant-Namespace: my-namespace" \
  http://prometheus-multi-tenant-proxy:8080/api/v1/query?query=up
```

## Configuration

### Proxy Configuration

The proxy is configured via a YAML file. Here's a complete example:

```yaml
# Service Discovery Configuration
discovery:
  kubernetes:
    namespaces:
      - monitoring
      - kube-system
    label_selectors:
      app: prometheus
    annotation_selectors:
      prometheus.io/scrape: "true"
    port: "9090"
    resource_types:
      - Service
      - Pod
  refresh_interval: 30s

# Tenant Management
tenants:
  watch_all_namespaces: true

# Proxy Behavior
proxy:
  enable_caching: true
  cache_ttl: 5m
  enable_metrics: true
  enable_request_logging: true
  max_concurrent_requests: 100
  backend_timeout: 30s

# Optional Authentication
auth:
  type: "jwt"
  jwt:
    secret_key: "your-secret-key"
    issuer: "prometheus-proxy"
    audience: "prometheus-users"
```

### MetricAccess Custom Resource

Define tenant access rules using the `MetricAccess` CRD:

```yaml
apiVersion: observability.ethos.io/v1alpha1
kind: MetricAccess
metadata:
  name: tenant-metrics-access
  namespace: tenant-namespace
spec:
  # Metric patterns (supports exact, regex, and PromQL selectors)
  metrics:
    - "foo"                           # Exact match
    - "bar{label=\"x\"}"             # Metric with specific label
    - '{__name__="foo",label="x"}'   # PromQL-style selector
    - "http_.*"                      # Regex pattern
  
  # Source namespace
  source: tenant-namespace
  
  # Additional label selectors
  labelSelectors:
    environment: "production"
```

## Metric Pattern Types

### 1. Exact Match
```yaml
metrics:
  - "http_requests_total"
```

### 2. Regex Patterns
```yaml
metrics:
  - "http_.*"           # All metrics starting with "http_"
  - ".*_duration_.*"    # All metrics containing "_duration_"
```

### 3. PromQL-style Selectors
```yaml
metrics:
  - '{job="my-app"}'                    # All metrics from specific job
  - '{__name__="up",instance=".*"}'     # Up metric from any instance
  - "http_requests_total{method="GET"}" # Specific metric with label
```

## Development

### Building from Source

```bash
# Clone the repository
git clone https://github.com/your-org/prometheus-multi-tenant-proxy
cd prometheus-multi-tenant-proxy

# Install dependencies
make deps

# Build the binary
make build

# Run tests
make test

# Build Docker image
make docker-build
```

### Local Development

```bash
# Setup development environment
make dev-setup

# Run locally with debug logging
make dev-run
your-registry.com
# Test a request
make dev-test-request
```

### Project Structure

```
├── api/v1alpha1/           # Custom Resource Definitions
├── cmd/proxy/              # Main application entry point
├── internal/
│   ├── config/            # Configuration management
│   ├── discovery/         # Kubernetes service discovery
│   ├── proxy/             # HTTP proxy and load balancing
│   └── tenant/            # Tenant management and access control
├── deploy/kubernetes/      # Kubernetes manifests
├── examples/              # Example configurations
├── Dockerfile             # Container image definition
├── Makefile              # Build and deployment automation
└── README.md             # This file
```

## Deployment Options

### Kubernetes (Recommended)

Deploy using the provided Kubernetes manifests:

```bash
# Deploy everything
make k8s-deploy

# Or step by step
kubectl apply -f deploy/kubernetes/crd.yaml
kubectl apply -f deploy/kubernetes/configmap.yaml
kubectl apply -f deploy/kubernetes/deployment.yaml
```

### Docker

Run as a Docker container:

```bash
docker run -p 8080:8080 \
  -v /path/to/config.yaml:/etc/config/config.yaml \
  prometheus-multi-tenant-proxy:latest \
  --config=/etc/config/config.yaml
```

### Binary

Run the compiled binary directly:

```bash
./prometheus-multi-tenant-proxy \
  --config=config.yaml \
  --port=8080 \
  --log-level=info
```

## API Endpoints

### Prometheus API (Proxied)
- `GET /api/v1/query` - Query endpoint
- `GET /api/v1/query_range` - Range query endpoint
- `GET /api/v1/series` - Series endpoint
- `GET /api/v1/labels` - Labels endpoint

### Management Endpoints
- `GET /health` - Health check
- `GET /metrics` - Prometheus metrics (if enabled)
- `GET /debug/targets` - Show discovered targets
- `GET /debug/tenants` - Show tenant information

## Security Considerations

### Authentication
- Support for JWT tokens
- API key authentication
- Custom authentication plugins

### Authorization
- Namespace-based tenant isolation
- Metric-level access control
- Label-based filtering

### Network Security
- TLS support for client connections
- mTLS for backend communication
- Network policies for pod-to-pod communication

## Monitoring and Troubleshooting

### Metrics

The proxy exposes the following metrics:

- `prometheus_proxy_requests_total` - Total requests processed
- `prometheus_proxy_request_duration_seconds` - Request duration
- `prometheus_proxy_backend_requests_total` - Backend requests
- `prometheus_proxy_targets_discovered` - Number of discovered targets
- `prometheus_proxy_tenants_active` - Number of active tenants

### Logging

Structured JSON logs include:

```json
{
  "level": "info",
  "time": "2023-12-07T10:30:00Z",
  "msg": "Proxying request",
  "tenant": "my-namespace/my-app",
  "method": "GET",
  "path": "/api/v1/query",
  "target": "http://prometheus-1:9090",
  "remote_ip": "10.244.1.5"
}
```

### Debug Endpoints

- `/debug/targets` - View discovered Prometheus targets
- `/debug/tenants` - View active tenant configurations
- `/health` - Health status and basic statistics

## Contributing

We welcome contributions! Please see our [Contributing Guide](CONTRIBUTING.md) for details.

### Development Workflow

1. Fork the repository
2. Create a feature branch
3. Make your changes
4. Add tests
5. Run the test suite: `make ci`
6. Submit a pull request

## License

This project is licensed under the Apache License 2.0 - see the [LICENSE](LICENSE) file for details.

## Support

- 📖 [Documentation](docs/)
- 🐛 [Issue Tracker](https://github.com/your-org/prometheus-multi-tenant-proxy/issues)
- 💬 [Discussions](https://github.com/your-org/prometheus-multi-tenant-proxy/discussions)
- 📧 [Mailing List](mailto:prometheus-proxy@your-org.com)

## Roadmap

- [ ] Advanced query filtering and rewriting
- [ ] Multi-cluster support
- [ ] Grafana integration
- [ ] Advanced authentication providers (OIDC, LDAP)
- [ ] Query result caching with Redis
- [ ] Horizontal pod autoscaling support
- [ ] Custom metrics and alerting rules
