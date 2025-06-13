package remote_write

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/common/model"
	"github.com/sirupsen/logrus"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/prometheus-multi-tenant-proxy/api/v1alpha1"
	"github.com/prometheus-multi-tenant-proxy/internal/config"
	"github.com/prometheus-multi-tenant-proxy/internal/discovery"
)

// PrometheusResponse represents the response from a Prometheus query
type PrometheusResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string `json:"resultType"`
		Result     []struct {
			Metric map[string]string `json:"metric"`
			Value  []interface{}     `json:"value"`
		} `json:"result"`
	} `json:"data"`
}

// Controller manages metric remote write for tenants
type Controller struct {
	client           client.Client
	config           config.RemoteWriteConfig
	serviceDiscovery discovery.Discovery

	// Active remote write jobs
	mu   sync.RWMutex
	jobs map[string]*RemoteWriteJob // namespace/name -> job

	// In-memory store for latest collected metrics per job
	collectedMetrics map[string][]Metric // namespace/name -> []Metric

	// Control channels
	stopCh chan struct{}
	doneCh chan struct{}
}

// RemoteWriteJob represents an active remote write job for a tenant
type RemoteWriteJob struct {
	MetricAccess *v1alpha1.MetricAccess
	Targets      []discovery.Target
	StopCh       chan struct{}
	
	// Metrics for monitoring
	LastRun       time.Time
	LastError     error
	MetricsCount  int64
	SuccessCount  int64
	FailureCount  int64
}

// NewController creates a new remote write controller
func NewController(client client.Client, cfg config.RemoteWriteConfig, serviceDiscovery discovery.Discovery) *Controller {
	return &Controller{
		client:           client,
		config:           cfg,
		serviceDiscovery: serviceDiscovery,
		jobs:             make(map[string]*RemoteWriteJob),
		collectedMetrics:  make(map[string][]Metric),
		stopCh:           make(chan struct{}),
		doneCh:           make(chan struct{}),
	}
}

// Start begins the remote write controller
func (c *Controller) Start(ctx context.Context) error {
	logrus.Info("Starting remote write controller")
	
	// Initial load of existing MetricAccess resources with remote write enabled
	if err := c.loadExistingRemoteWriteJobs(ctx); err != nil {
		logrus.Errorf("Failed to load existing remote write jobs: %v", err)
	}
	
	// Start reconciliation loop
	go c.reconcileLoop(ctx)
	
	return nil
}

// Stop gracefully shuts down the controller
func (c *Controller) Stop() {
	logrus.Info("Stopping remote write controller")
	close(c.stopCh)
	
	// Stop all active jobs
	c.stopAllJobs()
	
	// Wait for reconciliation loop to finish
	<-c.doneCh
}

// reconcileLoop periodically reconciles remote write jobs
func (c *Controller) reconcileLoop(ctx context.Context) {
	defer close(c.doneCh)
	
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.stopCh:
			return
		case <-ticker.C:
			if err := c.reconcileRemoteWriteJobs(ctx); err != nil {
				logrus.Errorf("Failed to reconcile remote write jobs: %v", err)
			}
		}
	}
}

// loadExistingRemoteWriteJobs loads existing MetricAccess resources with remote write enabled
func (c *Controller) loadExistingRemoteWriteJobs(ctx context.Context) error {
	var metricAccessList v1alpha1.MetricAccessList
	if err := c.client.List(ctx, &metricAccessList); err != nil {
		return fmt.Errorf("failed to list MetricAccess resources: %w", err)
	}
	
	for _, metricAccess := range metricAccessList.Items {
		if metricAccess.Spec.RemoteWrite != nil && metricAccess.Spec.RemoteWrite.Enabled {
			if err := c.createRemoteWriteJob(ctx, &metricAccess); err != nil {
				logrus.Errorf("Failed to create remote write job for %s/%s: %v", 
					metricAccess.Namespace, metricAccess.Name, err)
			}
		}
	}
	
	logrus.Infof("Loaded %d remote write jobs", len(c.jobs))
	return nil
}

// reconcileRemoteWriteJobs reconciles the current state with desired state
func (c *Controller) reconcileRemoteWriteJobs(ctx context.Context) error {
	var metricAccessList v1alpha1.MetricAccessList
	if err := c.client.List(ctx, &metricAccessList); err != nil {
		return fmt.Errorf("failed to list MetricAccess resources: %w", err)
	}
	
	c.mu.Lock()
	defer c.mu.Unlock()
	
	// Track which jobs should exist
	desiredJobs := make(map[string]bool)
	
	for _, metricAccess := range metricAccessList.Items {
		key := fmt.Sprintf("%s/%s", metricAccess.Namespace, metricAccess.Name)
		
		if metricAccess.Spec.RemoteWrite != nil && metricAccess.Spec.RemoteWrite.Enabled {
			desiredJobs[key] = true
			
			// Create job if it doesn't exist
			if _, exists := c.jobs[key]; !exists {
				if err := c.createRemoteWriteJobLocked(ctx, &metricAccess); err != nil {
					logrus.Errorf("Failed to create remote write job for %s: %v", key, err)
				}
			}
		}
	}
	
	// Stop jobs that should no longer exist
	for key, job := range c.jobs {
		if !desiredJobs[key] {
			c.stopRemoteWriteJobLocked(key, job)
		}
	}
	
	return nil
}

// createRemoteWriteJob creates a new remote write job
func (c *Controller) createRemoteWriteJob(ctx context.Context, metricAccess *v1alpha1.MetricAccess) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.createRemoteWriteJobLocked(ctx, metricAccess)
}

// createRemoteWriteJobLocked creates a new remote write job (must be called with lock held)
func (c *Controller) createRemoteWriteJobLocked(ctx context.Context, metricAccess *v1alpha1.MetricAccess) error {
	key := fmt.Sprintf("%s/%s", metricAccess.Namespace, metricAccess.Name)
	
	// Validate target type
	targetType := metricAccess.Spec.RemoteWrite.Target.Type
	if targetType != "prometheus" && targetType != "pushgateway" && targetType != "remote_write" {
		return fmt.Errorf("unsupported remote write target type: %s", targetType)
	}
	
	// Discover targets for this tenant
	targets := c.serviceDiscovery.GetTargets()
	
	job := &RemoteWriteJob{
		MetricAccess: metricAccess.DeepCopy(),
		Targets:      targets,
		StopCh:       make(chan struct{}),
	}
	
	c.jobs[key] = job
	
	// Start the remote write job
	go c.runRemoteWriteJob(job)
	
	logrus.Infof("Created remote write job for %s", key)
	return nil
}

// stopRemoteWriteJobLocked stops a remote write job (must be called with lock held)
func (c *Controller) stopRemoteWriteJobLocked(key string, job *RemoteWriteJob) {
	close(job.StopCh)
	delete(c.jobs, key)
	logrus.Infof("Stopped remote write job for %s", key)
}

// stopAllJobs stops all active remote write jobs
func (c *Controller) stopAllJobs() {
	c.mu.Lock()
	defer c.mu.Unlock()
	
	for key, job := range c.jobs {
		c.stopRemoteWriteJobLocked(key, job)
	}
}

// runRemoteWriteJob runs a remote write job until stopped
func (c *Controller) runRemoteWriteJob(job *RemoteWriteJob) {
	logrus.WithFields(logrus.Fields{
		"namespace": job.MetricAccess.Namespace,
		"name":     job.MetricAccess.Name,
	}).Info("Starting remote write job")

	// Get collection interval
	interval := 30 * time.Second
	if job.MetricAccess.Spec.RemoteWrite.Interval.Duration > 0 {
		interval = job.MetricAccess.Spec.RemoteWrite.Interval.Duration
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Initial collection
	c.collectMetrics(job)

	for {
		select {
		case <-job.StopCh:
			logrus.WithFields(logrus.Fields{
				"namespace": job.MetricAccess.Namespace,
				"name":     job.MetricAccess.Name,
			}).Info("Stopping remote write job")
			return
		case <-ticker.C:
			c.collectMetrics(job)
		}
	}
}

// collectMetrics collects metrics from all targets for a job
func (c *Controller) collectMetrics(job *RemoteWriteJob) {
	logrus.WithFields(logrus.Fields{
		"namespace": job.MetricAccess.Namespace,
		"name":     job.MetricAccess.Name,
	}).Debug("Collecting metrics for remote write job")

	var allMetrics []Metric
	for _, target := range job.Targets {
		metrics, err := c.collectFromTarget(job, target)
		if err != nil {
			logrus.WithError(err).Errorf("Failed to collect metrics from target %s", target.URL)
			job.LastError = err
			job.FailureCount++
			continue
		}
		allMetrics = append(allMetrics, metrics...)
	}

	// Store the latest collected metrics for this job
	key := fmt.Sprintf("%s/%s", job.MetricAccess.Namespace, job.MetricAccess.Name)
	c.mu.Lock()
	c.collectedMetrics[key] = allMetrics
	c.mu.Unlock()

	logrus.WithFields(logrus.Fields{
		"namespace": job.MetricAccess.Namespace,
		"name":     job.MetricAccess.Name,
		"count":    len(allMetrics),
	}).Debug("Stored collected metrics")

	if len(allMetrics) > 0 {
		if err := c.sendMetrics(job, allMetrics); err != nil {
			logrus.WithError(err).Error("Failed to send metrics")
			job.LastError = err
			job.FailureCount++
			return
		}
		job.SuccessCount++
		job.MetricsCount += int64(len(allMetrics))
	}

	job.LastRun = time.Now()
	c.updateJobStatus(job)
}

// collectFromTarget collects metrics from a single target
func (c *Controller) collectFromTarget(job *RemoteWriteJob, target discovery.Target) ([]Metric, error) {
	// Use the highest log level to make sure this appears in the logs
	logrus.WithFields(logrus.Fields{
		"target": target.URL,
		"job":    fmt.Sprintf("%s/%s", job.MetricAccess.Namespace, job.MetricAccess.Name),
	}).Error("DIAGNOSTIC: Starting metric collection from target") 

	var metrics []Metric
	client := &http.Client{
		Timeout: 30 * time.Second,
	}

	// Query each metric pattern
	for _, pattern := range job.MetricAccess.Spec.Metrics {
		logrus.WithFields(logrus.Fields{
			"pattern": pattern,
			"target":  target.URL,
		}).Error("DIAGNOSTIC: Processing metric pattern") 

		// Handle label selector patterns
		var queryPattern string
		if strings.HasPrefix(pattern, "{") && strings.HasSuffix(pattern, "}") {
			// This is already a PromQL selector pattern
			queryPattern = pattern
		} else {
			// Simple metric name pattern
			// First try with just the metric name
			queryPattern = pattern
			
			// Check if we have any label selectors to apply
			if len(job.MetricAccess.Spec.LabelSelectors) > 0 {
				// Add label selectors
				labels := []string{}
				for k, v := range job.MetricAccess.Spec.LabelSelectors {
					labels = append(labels, fmt.Sprintf("%s=\"%s\"", k, v))
				}
				queryPattern = fmt.Sprintf("{__name__=\"%s\",%s}", pattern, strings.Join(labels, ","))
			}
		}

		// Try the regular query first
		logrus.WithFields(logrus.Fields{
			"query_pattern": queryPattern,
			"target":        target.URL,
		}).Error("DIAGNOSTIC: Querying with standard pattern")
		
		metricResults, err := c.queryPromQL(client, target.URL, queryPattern)
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"error":   err,
				"pattern": queryPattern,
				"target":  target.URL,
			}).Error("Failed to query metrics")
			continue
		}

		// If we got metrics, add them to the result
		if len(metricResults) > 0 {
			metrics = append(metrics, metricResults...)
			logrus.WithFields(logrus.Fields{
				"count":   len(metricResults),
				"pattern": queryPattern,
				"target":  target.URL,
			}).Error("DIAGNOSTIC: Found metrics with standard pattern")
			continue
		}

		// If no metrics found and this is a simple metric name, try different strategies
		if !strings.HasPrefix(pattern, "{") && strings.Count(pattern, "\"") == 0 {
			// For node metrics (starting with "node_"), try with job="node-exporter"
			if strings.HasPrefix(pattern, "node_") {
				nodeQueryPattern := fmt.Sprintf("{__name__=\"%s\",job=\"node-exporter\"}", pattern)
				logrus.WithFields(logrus.Fields{
					"query_pattern": nodeQueryPattern,
					"target":        target.URL,
				}).Error("DIAGNOSTIC: Trying node-exporter specific pattern")
				
				nodeMetrics, err := c.queryPromQL(client, target.URL, nodeQueryPattern)
				if err != nil {
					logrus.WithFields(logrus.Fields{
						"error":   err,
						"pattern": nodeQueryPattern,
						"target":  target.URL,
					}).Error("Failed to query with node-exporter pattern")
				} else if len(nodeMetrics) > 0 {
					metrics = append(metrics, nodeMetrics...)
					logrus.WithFields(logrus.Fields{
						"count":   len(nodeMetrics),
						"pattern": nodeQueryPattern,
						"target":  target.URL,
					}).Error("DIAGNOSTIC: Found metrics with node-exporter pattern")
					continue
				}
			}
			
			// Try with no label selectors as last resort for any metric type
			flexibleQueryPattern := fmt.Sprintf("{__name__=\"%s\"}", pattern)
			logrus.WithFields(logrus.Fields{
				"query_pattern": flexibleQueryPattern,
				"target":        target.URL,
			}).Error("DIAGNOSTIC: Trying generic pattern without selectors")
			
			flexibleMetrics, err := c.queryPromQL(client, target.URL, flexibleQueryPattern)
			if err != nil {
				logrus.WithFields(logrus.Fields{
					"error":   err,
					"pattern": flexibleQueryPattern,
					"target":  target.URL,
				}).Error("Failed to query with generic pattern")
			} else if len(flexibleMetrics) > 0 {
				metrics = append(metrics, flexibleMetrics...)
				logrus.WithFields(logrus.Fields{
					"count":   len(flexibleMetrics),
					"pattern": flexibleQueryPattern,
					"target":  target.URL,
				}).Error("DIAGNOSTIC: Found metrics with generic pattern")
			} else {
				logrus.WithFields(logrus.Fields{
					"pattern": pattern,
					"target":  target.URL,
				}).Error("DIAGNOSTIC: No metrics found for pattern using any query strategy")
			}
		}
	}

	logrus.WithFields(logrus.Fields{
		"count":  len(metrics),
		"target": target.URL,
		"job":    fmt.Sprintf("%s/%s", job.MetricAccess.Namespace, job.MetricAccess.Name),
	}).Error("DIAGNOSTIC: Completed metric collection from target") 

	return metrics, nil
}

// queryPromQL queries the Prometheus instance for the given PromQL query
func (c *Controller) queryPromQL(client *http.Client, targetURL, query string) ([]Metric, error) {
	// Construct the full URL for the query
	queryURL := fmt.Sprintf("%s/api/v1/query?query=%s", targetURL, url.QueryEscape(query))
	
	logrus.WithFields(logrus.Fields{
		"query_url": queryURL,
		"query": query,
	}).Error("DIAGNOSTIC: Making Prometheus query") // Use ERROR level for diagnostic visibility
	
	// Make the request
	resp, err := client.Get(queryURL)
	if err != nil {
		logrus.WithFields(logrus.Fields{
			"error": err,
			"url":   queryURL,
		}).Error("Failed to query Prometheus")
		return nil, err
	}
	defer resp.Body.Close()
	
	// Parse the response
	if resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		logrus.WithFields(logrus.Fields{
			"status_code": resp.StatusCode,
			"body":        string(bodyBytes),
			"url":         queryURL,
		}).Error("Non-200 response from Prometheus")
		return nil, fmt.Errorf("non-200 response: %d", resp.StatusCode)
	}
	
	// Parse the response body
	var promResp PrometheusResponse
	if err := json.NewDecoder(resp.Body).Decode(&promResp); err != nil {
		logrus.WithFields(logrus.Fields{
			"error": err,
			"url":   queryURL,
		}).Error("Failed to decode Prometheus response")
		return nil, err
	}
	
	// Check if the status is success
	if promResp.Status != "success" {
		logrus.WithFields(logrus.Fields{
			"status": promResp.Status,
			"url":    queryURL,
		}).Error("Prometheus query returned non-success status")
		return nil, fmt.Errorf("prometheus query failed: %s", promResp.Status)
	}
	
	// Check if we got any data
	resultCount := len(promResp.Data.Result)
	logrus.WithFields(logrus.Fields{
		"result_count": resultCount,
		"query":        query,
		"url":          queryURL,
	}).Error("DIAGNOSTIC: Prometheus query result count") 
	
	// Extract metrics from the result
	var metrics []Metric
	for _, result := range promResp.Data.Result {
		// Extract metric name
		metricName, ok := result.Metric["__name__"]
		if !ok {
			logrus.WithFields(logrus.Fields{
				"metric": result.Metric,
			}).Warn("Metric doesn't have a __name__ label")
			continue
		}
		
		// Extract labels
		labels := model.LabelSet{}
		for k, v := range result.Metric {
			if k != "__name__" {
				labels[model.LabelName(k)] = model.LabelValue(v)
			}
		}
		
		// Extract value
		if len(result.Value) < 2 {
			logrus.WithFields(logrus.Fields{
				"metric": metricName,
				"value":  result.Value,
			}).Warn("Metric value is invalid")
			continue
		}
		
		// Convert value to float64
		strVal := result.Value[1].(string)
		val, err := strconv.ParseFloat(strVal, 64)
		if err != nil {
			logrus.WithFields(logrus.Fields{
				"metric": metricName,
				"value":  strVal,
				"error":  err,
			}).Warn("Failed to parse metric value")
			continue
		}
		
		logrus.WithFields(logrus.Fields{
			"metric": metricName,
			"labels": labels,
			"value":  val,
		}).Error("DIAGNOSTIC: Extracted metric")
		
		// Create metric
		metrics = append(metrics, Metric{
			Name:      metricName,
			Labels:    labels,
			Value:     val,
			Timestamp: time.Now(), // Use current time as timestamp
		})
	}
	
	return metrics, nil
}

// sendMetrics sends collected metrics to the remote write target
func (c *Controller) sendMetrics(job *RemoteWriteJob, metrics []Metric) error {
	// Create appropriate collector based on target type
	var collector Collector
	switch job.MetricAccess.Spec.RemoteWrite.Target.Type {
	case "prometheus":
		collector = NewPrometheusCollector(c.client)
	case "pushgateway":
		collector = NewPushgatewayCollector(c.client)
	case "remote_write":
		collector = NewRemoteWriteCollector(c.client)
	default:
		return fmt.Errorf("unsupported remote write target type: %s", 
			job.MetricAccess.Spec.RemoteWrite.Target.Type)
	}

	// Create context with timeout
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Send metrics using the collector
	if err := collector.Send(ctx, job.MetricAccess, metrics); err != nil {
		return fmt.Errorf("failed to send metrics: %w", err)
	}

	return nil
}

// updateJobStatus updates the status of a remote write job
func (c *Controller) updateJobStatus(job *RemoteWriteJob) {
	// Update the MetricAccess status
	status := &v1alpha1.RemoteWriteStatus{
		Active:           true,
		LastCollection:   metav1.NewTime(job.LastRun),
		MetricsCollected: int32(job.MetricsCount),
	}
	
	if job.LastError != nil {
		status.LastError = job.LastError.Error()
	}
	
	job.MetricAccess.Status.RemoteWrite = status
	
	// TODO: Update the status in Kubernetes
	// This would require using the client to patch the MetricAccess resource
}

// GetActiveJobs returns information about active remote write jobs
func (c *Controller) GetActiveJobs() map[string]*RemoteWriteJob {
	c.mu.RLock()
	defer c.mu.RUnlock()
	
	jobs := make(map[string]*RemoteWriteJob)
	for k, v := range c.jobs {
		jobs[k] = v
	}
	
	return jobs
}

// GetAllCollectedMetrics returns all collected metrics for all jobs
func (c *Controller) GetAllCollectedMetrics() map[string][]Metric {
	c.mu.RLock()
	defer c.mu.RUnlock()
	
	// Log diagnostic information if no metrics are collected
	if len(c.collectedMetrics) == 0 {
		logrus.WithFields(logrus.Fields{
			"active_jobs": len(c.jobs),
		}).Warning("No collected metrics found in remote write controller")
		
		// Log information about active jobs
		if len(c.jobs) > 0 {
			for key, job := range c.jobs {
				logrus.WithFields(logrus.Fields{
					"job_key":       key,
					"metrics_count": job.MetricsCount,
					"last_run":      job.LastRun,
					"has_error":     job.LastError != nil,
				}).Info("Active job status")
				
				if job.LastError != nil {
					logrus.WithFields(logrus.Fields{
						"job_key": key,
						"error":   job.LastError.Error(),
					}).Error("Job encountered an error")
				}
			}
		} else {
			logrus.Warning("No active remote write jobs found")
		}
	}
	
	// Make a copy to avoid concurrent map access
	metrics := make(map[string][]Metric)
	for k, v := range c.collectedMetrics {
		metrics[k] = append([]Metric{}, v...)
	}
	
	return metrics
} 