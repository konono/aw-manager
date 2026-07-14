package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// PodsActive tracks the number of currently active agent pods.
	PodsActive = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "aw_agent_pods_active",
		Help: "Number of currently active agent pods",
	})

	// ExecDuration records the duration of exec operations.
	ExecDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "aw_agent_exec_duration_seconds",
		Help:    "Duration of exec operations in seconds",
		Buckets: prometheus.ExponentialBuckets(1, 2, 10),
	})

	// ExecTotal counts exec operations by status.
	ExecTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "aw_agent_exec_total",
		Help: "Total number of exec operations",
	}, []string{"status"})

	// PodCreateDuration records the time from pod creation to Running state.
	PodCreateDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "aw_agent_pod_create_duration_seconds",
		Help:    "Duration from pod creation to Running state",
		Buckets: prometheus.ExponentialBuckets(1, 2, 8),
	})

	// MessagesTotal counts received chat messages across all adapters.
	MessagesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "aw_agent_messages_total",
		Help: "Total number of received chat messages",
	}, []string{"adapter"})

	// MessagesRejected counts messages rejected due to concurrency limits.
	MessagesRejected = promauto.NewCounter(prometheus.CounterOpts{
		Name: "aw_agent_messages_rejected_total",
		Help: "Total number of messages rejected due to server busy",
	})
)
