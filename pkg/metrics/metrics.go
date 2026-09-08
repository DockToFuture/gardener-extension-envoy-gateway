// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

// Package metrics specifies various metrics provided by the extension.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Namespace is the namespace component of the fully qualified metric name.
const Namespace = "gardener_extension_envoy_gateway"

// Metric label keys.
const (
	// LabelCluster is the metric label carrying the shoot control-plane namespace.
	LabelCluster = "cluster"
	// LabelOperation is the metric label carrying the actuator operation name.
	LabelOperation = "operation"
	// LabelReason is the metric label carrying the classified error reason.
	LabelReason = "reason"
)

// Classified error reasons used as the "reason" label on
// ActuatorOperationErrorsTotal. Kept as a bounded, low-cardinality set.
const (
	// ReasonGatewaysInUse is set when the delete guard refused because user Gateways still exist.
	ReasonGatewaysInUse = "gateways_in_use"
	// ReasonInvalidProviderConfig is set when the providerConfig failed to decode or validate.
	ReasonInvalidProviderConfig = "invalid_provider_config"
	// ReasonSecrets is set for secrets-manager / TLS secret failures.
	ReasonSecrets = "secrets"
	// ReasonManagedResource is set for ManagedResource deploy/delete failures.
	ReasonManagedResource = "managed_resource"
	// ReasonClusterLookup is set when the Cluster resource could not be fetched.
	ReasonClusterLookup = "cluster_lookup"
	// ReasonOther is the catch-all for unclassified errors.
	ReasonOther = "other"
)

var (
	// ActuatorOperationTotal counts each invocation of the envoy-gateway extension actuator.
	ActuatorOperationTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: Namespace,
			Name:      "actuator_operation_total",
			Help:      "Total number of actuator operations for the envoy-gateway extension",
		},
		[]string{LabelCluster, LabelOperation},
	)

	// ActuatorOperationDurationSeconds tracks the duration of each actuator operation.
	ActuatorOperationDurationSeconds = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: Namespace,
			Name:      "actuator_operation_duration_seconds",
			Help:      "Duration in seconds of actuator operations for the envoy-gateway extension",
		},
		[]string{LabelCluster, LabelOperation},
	)

	// ActuatorOperationErrorsTotal counts actuator operations that terminated with a non-nil error.
	// Split from ActuatorOperationTotal so alerting on the failure rate (errors / total) is trivial;
	// keeping the "total" counter as the denominator means retries and one-off errors don't skew a
	// simple failure-count alert.
	ActuatorOperationErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: Namespace,
			Name:      "actuator_operation_errors_total",
			Help:      "Total number of actuator operations that returned an error, by operation and reason",
		},
		[]string{LabelCluster, LabelOperation, LabelReason},
	)

	// DeleteGuardRejectionsTotal counts how many times the live-Gateway delete guard refused a
	// delete because user-owned Gateway objects still exist in the shoot. Useful for spotting
	// operators who repeatedly try to disable the extension on a live shoot without first
	// cleaning up Gateways.
	DeleteGuardRejectionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: Namespace,
			Name:      "delete_guard_rejections_total",
			Help:      "Number of times the delete-guard refused to detach the extension because user Gateways still exist",
		},
		[]string{LabelCluster},
	)
)

// init registers our custom metrics with the default controller-runtime registry.
func init() {
	ctrlmetrics.Registry.MustRegister(
		ActuatorOperationTotal,
		ActuatorOperationDurationSeconds,
		ActuatorOperationErrorsTotal,
		DeleteGuardRejectionsTotal,
	)
}
