// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package v1alpha1

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
)

func init() {
	// register-gen generates generated.register.go which only registers
	// addKnownTypes. Register the defaulting funcs here so defaulter-gen's
	// RegisterDefaults is wired into the shared localSchemeBuilder without
	// having to hand-edit the generated register file.
	localSchemeBuilder.Register(addDefaultingFuncs)
}

// addDefaultingFuncs registers the defaulting functions with the scheme so that
// defaulter-gen wires them into the generated RegisterDefaults.
func addDefaultingFuncs(scheme *runtime.Scheme) error {
	return RegisterDefaults(scheme)
}

// SetDefaults_EnvoyGatewayConfig sets defaults for the top-level config: the
// Gateway API channel and CRD management. The GEP-68 defaults are channel
// "standard" and manageCRDs true.
//
//nolint:revive // SetDefaults_<Type> is the naming convention required by defaulter-gen.
func SetDefaults_EnvoyGatewayConfig(obj *EnvoyGatewayConfig) {
	if obj.Channel == "" {
		obj.Channel = ChannelStandard
	}
	if obj.ManageCRDs == nil {
		obj.ManageCRDs = new(true)
	}
	if obj.ControlPlane == nil {
		obj.ControlPlane = &ControlPlaneConfig{}
	}
	if obj.DataPlane == nil {
		obj.DataPlane = &DataPlaneConfig{}
	}
}

// SetDefaults_ControlPlaneConfig defaults the control-plane replica count and log level.
//
//nolint:revive // SetDefaults_<Type> is the naming convention required by defaulter-gen.
func SetDefaults_ControlPlaneConfig(obj *ControlPlaneConfig) {
	if obj.Replicas == nil {
		obj.Replicas = ptr.To[int32](2)
	}
	if obj.LogLevel == "" {
		obj.LogLevel = LogLevelInfo
	}
}

// SetDefaults_DataPlaneConfig defaults the data-plane replica count and log level.
//
//nolint:revive // SetDefaults_<Type> is the naming convention required by defaulter-gen.
func SetDefaults_DataPlaneConfig(obj *DataPlaneConfig) {
	if obj.Replicas == nil {
		obj.Replicas = ptr.To[int32](2)
	}
	if obj.LogLevel == "" {
		obj.LogLevel = LogLevelInfo
	}
}
