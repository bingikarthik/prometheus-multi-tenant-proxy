package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"github.com/prometheus-multi-tenant-proxy/internal/config"
	"github.com/prometheus-multi-tenant-proxy/internal/discovery"
	"github.com/prometheus-multi-tenant-proxy/internal/tenant"
	"github.com/prometheus-multi-tenant-proxy/internal/remote_write"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sirupsen/logrus"
)

// Handler represents the main proxy handler
type Handler struct {
	config           *config.Config
	serviceDiscovery discovery.Discovery
	tenantManager    tenant.Manager
	
	// Inject remote write controller for collected metrics
	remoteWriteController interface{ GetAllCollectedMetrics() map[string][]remote_write.Metric }
	
	// Load balancer for backend targets
	loadBalancer *LoadBalancer
	
	// Metrics
	requestsTotal    *prometheus.CounterVec
	requestDuration  *prometheus.HistogramVec
	backendRequests  *prometheus.CounterVec
}

// NewHandler creates a new proxy handler
func NewHandler(cfg *config.Config, serviceDiscovery discovery.Discovery, tenantManager tenant.Manager, remoteWriteController interface{ GetAllCollectedMetrics() map[string][]remote_write.Metric }) (*Handler, error) {
	h := &Handler{
		config:           cfg,
		serviceDiscovery: serviceDiscovery,
		tenantManager:    tenantManager,
		remoteWriteController: remoteWriteController,
	}
	
	// Initialize load balancer
	h.loadBalancer = NewLoadBalancer(serviceDiscovery)
	
	// Initialize metrics if enabled
	if cfg.Proxy.EnableMetrics {
		h.initMetrics()
	}
	
	return h, nil
}

// ServeHTTP implements the http.Handler interface
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	
	// Create router
	router := mux.NewRouter()
	
	// Health check endpoint
	router.HandleFunc("/health", h.handleHealth).Methods("GET")
	
	// Metrics endpoint (if enabled)
	if h.config.Proxy.EnableMetrics {
		router.Handle("/metrics", promhttp.Handler()).Methods("GET")
	}
	
	// Debug endpoints
	router.HandleFunc("/debug/targets", h.handleDebugTargets).Methods("GET")
	router.HandleFunc("/debug/tenants", h.handleDebugTenants).Methods("GET")
	
	// Collected metrics endpoint
	router.HandleFunc("/collected-metrics", h.handleCollectedMetrics).Methods("GET")
	
	// Prometheus API endpoints
	router.PathPrefix("/api/v1/").HandlerFunc(h.handlePrometheusAPI)
	router.PathPrefix("/").HandlerFunc(h.handlePrometheusAPI)
	
	// Serve the request
	router.ServeHTTP(w, r)
	
	// Record metrics
	if h.config.Proxy.EnableMetrics {
		duration := time.Since(start).Seconds()
		h.requestDuration.WithLabelValues(r.Method, r.URL.Path).Observe(duration)
		h.requestsTotal.WithLabelValues(r.Method, r.URL.Path, fmt.Sprintf("%d", 200)).Inc()
	}
}

// handleHealth handles health check requests
func (h *Handler) handleHealth(w http.ResponseWriter, r *http.Request) {
	targets := h.serviceDiscovery.GetTargets()
	healthyTargets := 0
	
	for _, target := range targets {
		if target.Healthy {
			healthyTargets++
		}
	}
	
	status := map[string]interface{}{
		"status":          "healthy",
		"total_targets":   len(targets),
		"healthy_targets": healthyTargets,
		"tenants":         len(h.tenantManager.GetAllTenants()),
	}
	
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	
	// Simple JSON response
	fmt.Fprintf(w, `{"status":"%s","total_targets":%d,"healthy_targets":%d,"tenants":%d}`,
		status["status"], status["total_targets"], status["healthy_targets"], status["tenants"])
}

// handleDebugTargets handles debug requests for discovered targets
func (h *Handler) handleDebugTargets(w http.ResponseWriter, r *http.Request) {
	targets := h.serviceDiscovery.GetTargets()
	
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	
	fmt.Fprintf(w, `{"targets":[`)
	for i, target := range targets {
		if i > 0 {
			fmt.Fprintf(w, ",")
		}
		fmt.Fprintf(w, `{"url":"%s","healthy":%t,"last_seen":"%s"}`,
			target.URL, target.Healthy, target.LastSeen.Format(time.RFC3339))
	}
	fmt.Fprintf(w, `]}`)
}

// handleDebugTenants handles debug requests for tenant information
func (h *Handler) handleDebugTenants(w http.ResponseWriter, r *http.Request) {
	tenants := h.tenantManager.GetAllTenants()
	
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	
	fmt.Fprintf(w, `{"tenants":[`)
	i := 0
	for _, tenant := range tenants {
		if i > 0 {
			fmt.Fprintf(w, ",")
		}
		fmt.Fprintf(w, `{"id":"%s","name":"%s","namespace":"%s","patterns":%d}`,
			tenant.ID, tenant.Name, tenant.Namespace, len(tenant.MetricPatterns))
		i++
	}
	fmt.Fprintf(w, `]}`)
}

// handleCollectedMetrics returns all collected metrics in Prometheus text format
func (h *Handler) handleCollectedMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain")
	
	// Check if remoteWriteController is available
	if h.remoteWriteController == nil {
		fmt.Fprintf(w, "# No remote write controller available. RemoteWrite feature may be disabled.\n")
		return
	}
	
	// Get metrics from remote write controller
	metrics := h.remoteWriteController.GetAllCollectedMetrics()
	
	// If no metrics are available, try to fetch directly from target
	if len(metrics) == 0 {
		fmt.Fprintf(w, "# No collected metrics found in remote write controller.\n")
		fmt.Fprintf(w, "# Attempting to fetch metrics directly from Prometheus backend.\n\n")
		
		// Get a backend target
		targets := h.serviceDiscovery.GetTargets()
		if len(targets) == 0 {
			fmt.Fprintf(w, "# No backend targets available.\n")
			return
		}
		
		// Pick the first healthy target
		var target discovery.Target
		for _, t := range targets {
			if t.Healthy {
				target = t
				break
			}
		}
		
		if target.URL == "" {
			fmt.Fprintf(w, "# No healthy backend targets available.\n")
			return
		}
		
		// Get metric name from query parameter if provided
		queryMetric := r.URL.Query().Get("metric")
		
		// List of common metric types to fetch if no specific metric is requested
		metricTypes := []string{
			// System-level metrics
			"node_memory_MemAvailable_bytes",
			"node_cpu_seconds_total{mode=\"user\"}",
			"node_filesystem_avail_bytes",
			
			// Kubernetes API server metrics
			"apiserver_request_total",
			"apiserver_request_duration_seconds_count",
			
			// Pod/container metrics
			"container_cpu_usage_seconds_total",
			"container_memory_usage_bytes",
			"kube_pod_status_phase",
			
			// Service/endpoint metrics
			"kube_service_info",
			"kube_endpoint_info",
		}
		
		client := &http.Client{Timeout: 10 * time.Second}
		allResults := make(map[string][]byte)
		
		// If specific metric requested, only query that one
		if queryMetric != "" {
			metricTypes = []string{queryMetric}
		}
		
		// Fetch each metric type
		for _, metricType := range metricTypes {
			queryURL := fmt.Sprintf("%s/api/v1/query?query=%s", target.URL, url.QueryEscape(metricType))
			resp, err := client.Get(queryURL)
			if err != nil {
				fmt.Fprintf(w, "# Error querying backend for %s: %v\n", metricType, err)
				continue
			}
			
			body, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			
			if err != nil {
				fmt.Fprintf(w, "# Error reading response for %s: %v\n", metricType, err)
				continue
			}
			
			// Store the result
			allResults[metricType] = body
		}
		
		// If no results were fetched, return error
		if len(allResults) == 0 {
			fmt.Fprintf(w, "# No metrics could be fetched from backend\n")
			return
		}
		
		// Process and output all results
		for metricType, body := range allResults {
			fmt.Fprintf(w, "# Metric: %s\n", metricType)
			
			// Parse the Prometheus response
			var promResp struct {
				Status string `json:"status"`
				Data   struct {
					ResultType string `json:"resultType"`
					Result     []struct {
						Metric map[string]string `json:"metric"`
						Value  []interface{}     `json:"value"`
					} `json:"result"`
				} `json:"data"`
			}
			
			if err := json.Unmarshal(body, &promResp); err != nil {
				fmt.Fprintf(w, "# Error parsing response: %v\n", err)
				continue
			}
			
			// Format as Prometheus text format
			if promResp.Status == "success" {
				resultCount := 0
				for _, result := range promResp.Data.Result {
					// Build label string
					var labels []string
					for name, value := range result.Metric {
						labels = append(labels, fmt.Sprintf("%s=%q", name, value))
					}
					labelStr := ""
					if len(labels) > 0 {
						labelStr = fmt.Sprintf("{%s}", strings.Join(labels, ","))
					}
					
					// Get value and timestamp
					var value float64
					var timestamp int64
					
					if len(result.Value) >= 2 {
						// Convert timestamp (first element)
						if ts, ok := result.Value[0].(float64); ok {
							timestamp = int64(ts * 1000) // Convert to milliseconds
						}
						
						// Convert value (second element)
						if val, ok := result.Value[1].(string); ok {
							parsedVal, err := strconv.ParseFloat(val, 64)
							if err == nil {
								value = parsedVal
							}
						}
					}
					
					// Output in Prometheus text format
					metricName := result.Metric["__name__"]
					if metricName == "" {
						// Use the query if no name is found
						metricName = strings.Split(metricType, "{")[0]
					}
					fmt.Fprintf(w, "%s%s %g %d\n", metricName, labelStr, value, timestamp)
					resultCount++
					
					// Limit to 5 results per metric type to avoid overwhelming output
					if resultCount >= 5 && !strings.HasPrefix(metricType, "apiserver_") {
						fmt.Fprintf(w, "# ... more results omitted (limited to 5 samples per metric type)\n")
						break
					}
				}
				
				if len(promResp.Data.Result) == 0 {
					fmt.Fprintf(w, "# No data found for this metric\n")
				}
				
				fmt.Fprintf(w, "\n")
			} else {
				fmt.Fprintf(w, "# Error from Prometheus: %s\n\n", promResp.Status)
			}
		}
		return
	}
	
	// If we have metrics from the remote write controller, display them
	for _, jobMetrics := range metrics {
		for _, metric := range jobMetrics {
			// Build label string
			var labels []string
			for name, value := range metric.Labels {
				labels = append(labels, fmt.Sprintf("%s=%q", name, value))
			}
			labelStr := ""
			if len(labels) > 0 {
				labelStr = fmt.Sprintf("{%s}", strings.Join(labels, ","))
			}
			
			// Output in Prometheus text format
			fmt.Fprintf(w, "%s%s %g %d\n",
				metric.Name,
				labelStr,
				metric.Value,
				metric.Timestamp.UnixNano()/int64(time.Millisecond))
		}
	}
}

// handlePrometheusAPI handles Prometheus API requests
func (h *Handler) handlePrometheusAPI(w http.ResponseWriter, r *http.Request) {
	// Extract tenant information
	tenantInfo, err := h.extractTenantInfo(r)
	if err != nil {
		h.writeError(w, http.StatusUnauthorized, fmt.Sprintf("Authentication failed: %v", err))
		return
	}

	// Get all targets
	targets := h.loadBalancer.GetAllTargets()
	if len(targets) == 0 {
		h.writeError(w, http.StatusServiceUnavailable, "No backends available")
		return
	}

	// Log request details
	logrus.WithFields(logrus.Fields{
		"tenant_id": tenantInfo.ID,
		"path":      r.URL.Path,
		"query":     r.URL.RawQuery,
		"method":    r.Method,
		"targets":   len(targets),
	}).Debug("Processing Prometheus API request")

	// Create HTTP client for querying targets
	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	// Only handle query and query_range endpoints
	if strings.Contains(r.URL.Path, "/api/v1/query") || strings.Contains(r.URL.Path, "/api/v1/query_range") {
		// Process each target
		for _, target := range targets {
			targetURL := fmt.Sprintf("http://%s", target)
			
			// Create new request for the target
			targetReq, err := http.NewRequest(r.Method, fmt.Sprintf("%s%s", targetURL, r.URL.RequestURI()), nil)
			if err != nil {
				logrus.WithFields(logrus.Fields{
					"target": targetURL,
					"error":  err,
				}).Error("Failed to create target request")
				continue
			}

			// Copy headers
			for key, values := range r.Header {
				for _, value := range values {
					targetReq.Header.Add(key, value)
				}
			}

			// Execute request
			logrus.WithFields(logrus.Fields{
				"target": targetURL,
				"path":   targetReq.URL.Path,
				"query":  targetReq.URL.RawQuery,
			}).Debug("Sending request to target")

			resp, err := client.Do(targetReq)
			if err != nil {
				logrus.WithFields(logrus.Fields{
					"target": targetURL,
					"error":  err,
				}).Error("Failed to execute request")
				continue
			}
			defer resp.Body.Close()

			// Read response body
			body, err := io.ReadAll(resp.Body)
			if err != nil {
				logrus.WithFields(logrus.Fields{
					"target": targetURL,
					"error":  err,
				}).Error("Failed to read response body")
				continue
			}

			// Parse and log response
			var result map[string]interface{}
			if err := json.Unmarshal(body, &result); err != nil {
				logrus.WithFields(logrus.Fields{
					"target": targetURL,
					"error":  err,
				}).Error("Failed to parse response JSON")
				continue
			}

			// Log response details
			logrus.WithFields(logrus.Fields{
				"target":        targetURL,
				"status":        resp.StatusCode,
				"response_size": len(body),
				"result_type":   result["resultType"],
				"status_text":   result["status"],
			}).Debug("Received response from target")

			// Write response to client
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(resp.StatusCode)
			w.Write(body)
			return
		}
		
		// If we get here, no targets responded successfully
		h.writeError(w, http.StatusServiceUnavailable, "No successful responses from any target")
		return
	} else {
		// For non-query endpoints, use standard load balancing
		target, err := h.loadBalancer.GetTarget()
		if err != nil {
			h.writeError(w, http.StatusServiceUnavailable, fmt.Sprintf("Failed to get target: %v", err))
			return
		}

		// Create reverse proxy
		targetURL := fmt.Sprintf("http://%s", target)
		proxyURL, err := url.Parse(targetURL)
		if err != nil {
			h.writeError(w, http.StatusInternalServerError, fmt.Sprintf("Failed to parse target URL: %v", err))
			return
		}

		// Create and use the reverse proxy
		proxy := httputil.NewSingleHostReverseProxy(proxyURL)
		proxy.ServeHTTP(w, r)
	}
}

// proxyToTarget proxies a request to a single target
func (h *Handler) proxyToTarget(w http.ResponseWriter, r *http.Request, target *discovery.Target) {
	targetURL, err := url.Parse(target.URL)
	if err != nil {
		h.writeError(w, http.StatusInternalServerError, fmt.Sprintf("Invalid target URL: %v", err))
		return
	}

	// Create reverse proxy
	proxy := httputil.NewSingleHostReverseProxy(targetURL)

	// Log request if enabled
	if h.config.Proxy.EnableRequestLogging {
		logrus.WithFields(logrus.Fields{
			"method":    r.Method,
			"path":      r.URL.Path,
			"target":    target.URL,
			"remote_ip": r.RemoteAddr,
		}).Error("Proxying request to single target")
	}

	// Record backend metrics
	if h.config.Proxy.EnableMetrics {
		h.backendRequests.WithLabelValues(target.URL, "success").Inc()
	}

	// Proxy the request
	proxy.ServeHTTP(w, r)
}

// extractTenantInfo extracts tenant information from the request
func (h *Handler) extractTenantInfo(r *http.Request) (*tenant.TenantInfo, error) {
	// Try to extract tenant from different sources
	
	// 1. From X-Tenant-Namespace header
	namespace := r.Header.Get("X-Tenant-Namespace")
	if namespace != "" {
		return h.tenantManager.GetTenant(namespace)
	}
	
	// 2. From Authorization header (if auth is configured)
	if h.config.Auth != nil {
		return h.extractTenantFromAuth(r)
	}
	
	// 3. From query parameter
	namespace = r.URL.Query().Get("namespace")
	if namespace != "" {
		return h.tenantManager.GetTenant(namespace)
	}
	
	return nil, fmt.Errorf("no tenant information found in request")
}

// extractTenantFromAuth extracts tenant information from authentication
func (h *Handler) extractTenantFromAuth(r *http.Request) (*tenant.TenantInfo, error) {
	// This is a simplified implementation
	// In a real scenario, you'd validate the auth token and extract tenant info
	
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return nil, fmt.Errorf("missing authorization header")
	}
	
	// For now, just extract namespace from a custom header
	namespace := r.Header.Get("X-Tenant-Namespace")
	if namespace == "" {
		return nil, fmt.Errorf("missing tenant namespace")
	}
	
	return h.tenantManager.GetTenant(namespace)
}

// modifyResponse modifies the response based on tenant access rules
func (h *Handler) modifyResponse(resp *http.Response, tenantInfo *tenant.TenantInfo) error {
	// For query endpoints, we need to filter the response
	if strings.Contains(resp.Request.URL.Path, "/api/v1/query") {
		return h.filterQueryResponse(resp, tenantInfo)
	}
	
	// For other endpoints, pass through as-is
	return nil
}

// filterQueryResponse filters query responses based on tenant access rules
func (h *Handler) filterQueryResponse(resp *http.Response, tenantInfo *tenant.TenantInfo) error {
	// Read the response body
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response body: %w", err)
	}
	resp.Body.Close()
	
	// TODO: Parse the Prometheus response and filter metrics based on tenant access rules
	// This is a complex operation that would involve:
	// 1. Parsing the JSON response
	// 2. Extracting metric names and labels from the result
	// 3. Validating each metric against tenant access rules
	// 4. Filtering out unauthorized metrics
	// 5. Reconstructing the response
	
	// For now, we'll pass through the response as-is
	// In a production implementation, you'd want to implement proper filtering
	
	// Create a new response body
	resp.Body = io.NopCloser(strings.NewReader(string(body)))
	resp.ContentLength = int64(len(body))
	
	return nil
}

// writeError writes an error response
func (h *Handler) writeError(w http.ResponseWriter, statusCode int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	fmt.Fprintf(w, `{"error":"%s"}`, message)
}

// initMetrics initializes Prometheus metrics
func (h *Handler) initMetrics() {
	h.requestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "prometheus_proxy_requests_total",
			Help: "Total number of requests processed by the proxy",
		},
		[]string{"method", "path", "status"},
	)
	
	h.requestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name: "prometheus_proxy_request_duration_seconds",
			Help: "Duration of requests processed by the proxy",
		},
		[]string{"method", "path"},
	)
	
	h.backendRequests = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "prometheus_proxy_backend_requests_total",
			Help: "Total number of requests sent to backends",
		},
		[]string{"backend", "status"},
	)
	
	// Register metrics
	prometheus.MustRegister(h.requestsTotal)
	prometheus.MustRegister(h.requestDuration)
	prometheus.MustRegister(h.backendRequests)
} 