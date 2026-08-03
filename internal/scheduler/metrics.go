package scheduler

import (
	"github.com/prometheus/client_golang/prometheus"
	controllermetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

type reservationConflictStage string

const (
	reservationConflictStageReserve reservationConflictStage = "reserve"
	reservationConflictStageBind    reservationConflictStage = "bind"
)

type schedulerMetrics struct {
	runsScheduled        *prometheus.CounterVec
	syncDuration         *prometheus.HistogramVec
	noPodsTotal          *prometheus.CounterVec
	runQueueDuration     *prometheus.HistogramVec
	filterRejections     *prometheus.CounterVec
	reservationConflicts *prometheus.CounterVec
}

func newSchedulerMetrics(registerer prometheus.Registerer) *schedulerMetrics {
	metrics := &schedulerMetrics{
		runsScheduled: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "kruntimes_scheduler_sync_total",
				Help: "Total number of tasks processed by the scheduler.",
			},
			[]string{"runtime", "result"},
		),
		syncDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "kruntimes_scheduler_sync_duration_seconds",
				Help:    "Latency of run scheduling.",
				Buckets: prometheus.DefBuckets,
			},
			[]string{"runtime"},
		),
		noPodsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "kruntimes_scheduler_no_pods_total",
				Help: "Total number of tasks that could not find a matching runtime pod.",
			},
			[]string{"runtime"},
		),
		runQueueDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "kruntimes_scheduler_run_queue_duration_seconds",
				Help:    "Time from Run creation until scheduler assignment.",
				Buckets: prometheus.DefBuckets,
			},
			[]string{"runtime"},
		),
		filterRejections: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "kruntimes_scheduler_filter_rejections_total",
				Help: "Total Runtime Pod evaluations rejected by scheduler Filter plugins.",
			},
			[]string{"plugin", "reason"},
		),
		reservationConflicts: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "kruntimes_scheduler_reservation_conflicts_total",
				Help: "Total scheduler reservation conflicts.",
			},
			[]string{"stage"},
		),
	}
	registerer.MustRegister(
		metrics.runsScheduled,
		metrics.syncDuration,
		metrics.noPodsTotal,
		metrics.runQueueDuration,
		metrics.filterRejections,
		metrics.reservationConflicts,
	)
	return metrics
}

var defaultSchedulerMetrics = newSchedulerMetrics(controllermetrics.Registry)

func (r *RunReconciler) metricsRecorder() *schedulerMetrics {
	if r.metrics != nil {
		return r.metrics
	}
	return defaultSchedulerMetrics
}

func (m *schedulerMetrics) observeFilterRejection(plugin string, reason filterReason) {
	m.filterRejections.WithLabelValues(plugin, string(reason)).Inc()
}

func (m *schedulerMetrics) observeReservationConflict(stage reservationConflictStage) {
	m.reservationConflicts.WithLabelValues(string(stage)).Inc()
}
