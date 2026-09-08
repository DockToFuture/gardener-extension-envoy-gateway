// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package config

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// LogLevel defines the log level used by the Envoy Gateway control-plane and
// data-plane proxies.
type LogLevel string

const (
	// LogLevelDebug enables verbose debug logging.
	LogLevelDebug LogLevel = "debug"
	// LogLevelInfo is the default informational log level.
	LogLevelInfo LogLevel = "info"
	// LogLevelWarn logs warnings and errors only.
	LogLevelWarn LogLevel = "warn"
	// LogLevelError logs errors only.
	LogLevelError LogLevel = "error"
)

// Channel selects the Gateway API CRD channel installed by the extension.
type Channel string

const (
	// ChannelStandard installs only the standard-channel Gateway API CRDs
	// (GatewayClass, Gateway, HTTPRoute, GRPCRoute, ReferenceGrant).
	ChannelStandard Channel = "standard"
	// ChannelExperimental additionally installs the experimental-channel CRDs
	// (TCPRoute, TLSRoute, UDPRoute, BackendTLSPolicy). Experimental APIs may
	// change in backwards-incompatible ways between releases.
	ChannelExperimental Channel = "experimental"
)

// ControlPlaneConfig holds the Envoy Gateway control-plane settings.
type ControlPlaneConfig struct {
	// Replicas is the number of control-plane replicas. Defaults to 2.
	Replicas *int32

	// LogLevel sets the control-plane log level. Valid values are: debug,
	// info, warn, error. Defaults to "info".
	LogLevel LogLevel
}

// DataPlaneConfig holds the Envoy data-plane (proxy) settings applied per Gateway.
type DataPlaneConfig struct {
	// Replicas is the number of proxy replicas per Gateway. Defaults to 2.
	Replicas *int32

	// LogLevel sets the data-plane log level. Valid values are: debug, info,
	// warn, error. Defaults to "info".
	LogLevel LogLevel
}

// EnvoyProxyDefaults defines opinionated defaults that the extension applies to every
// Gateway via an EnvoyProxy template reference.
type EnvoyProxyDefaults struct {
	// Resources are the compute resources applied to every data-plane Envoy
	// proxy container.
	Resources *corev1.ResourceRequirements

	// AccessLogging enables access-log emission on the data-plane Envoy proxies.
	AccessLogging bool
}

// +k8s:deepcopy-gen:interfaces=k8s.io/apimachinery/pkg/runtime.Object

// EnvoyGatewayConfig is the configuration schema for the Envoy Gateway extension.
// This extension deploys the Envoy Gateway control plane and the Gateway API CRDs
// to shoot clusters and registers a single GatewayClass named
// "gardener-envoy-gateway". Following the convention of other extension
// providerConfig APIs, the configuration fields live at the top level with no
// spec/status wrapper.
type EnvoyGatewayConfig struct {
	metav1.TypeMeta

	// ControlPlane holds the Envoy Gateway control-plane settings.
	ControlPlane *ControlPlaneConfig

	// DataPlane holds the Envoy data-plane (proxy) settings applied per Gateway.
	DataPlane *DataPlaneConfig

	// Channel selects the Gateway API CRD channel to install. Valid values are
	// "standard" and "experimental". Defaults to "standard".
	Channel Channel

	// ManageCRDs controls whether the extension installs and updates the Gateway
	// API CRDs. Set to false if the CRDs are owned externally. Defaults to true.
	ManageCRDs *bool

	// EnvoyProxyDefaults is an optional, opinionated template applied to every
	// Gateway via the gateway.envoyproxy.io/v1alpha1.EnvoyProxy reference.
	EnvoyProxyDefaults *EnvoyProxyDefaults
}
