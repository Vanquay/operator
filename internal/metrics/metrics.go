/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package metrics defines the operator's own Prometheus metrics.
//
// controller-runtime already exports generic metrics such as
// controller_runtime_reconcile_total and workqueue_depth. These add
// domain-specific ones. Registering them with metrics.Registry makes them
// show up on the manager's /metrics endpoint next to the built-in ones.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// NoWindowLabel is the "window" label value used when a schedule's default
// replica count applies.
const NoWindowLabel = "none"

var (
	// WebAppReadyReplicas is a gauge: a value that goes up and down.
	WebAppReadyReplicas = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "platform_webapp_ready_replicas",
		Help: "Number of ready pods for each WebApp.",
	}, []string{"namespace", "name"})

	// ScheduleScaleOperations is a counter: it only ever goes up.
	ScheduleScaleOperations = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "platform_scalingschedule_scale_operations_total",
		Help: "Number of times a ScalingSchedule changed its target's replica count.",
	}, []string{"namespace", "name", "window"})
)

// init runs automatically when the package is imported. Importing this
// package anywhere in the binary is enough to register the metrics.
func init() {
	metrics.Registry.MustRegister(WebAppReadyReplicas, ScheduleScaleOperations)
}
