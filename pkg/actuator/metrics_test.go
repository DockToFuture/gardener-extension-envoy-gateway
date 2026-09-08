// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package actuator

import (
	"errors"
	"fmt"
	"testing"

	"github.com/gardener/gardener-extension-envoy-gateway/pkg/metrics"
)

func TestClassifyError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{name: "nil", err: nil, want: ""},
		{name: "gateways-in-use guard", err: &gatewaysInUseError{names: []string{"a/b"}}, want: metrics.ReasonGatewaysInUse},
		{name: "wrapped gateways-in-use", err: fmt.Errorf("wrap: %w", &gatewaysInUseError{names: []string{"a/b"}}), want: metrics.ReasonGatewaysInUse},
		{name: "decode provider config", err: errors.New("failed to decode envoy-gateway providerConfig: unknown field"), want: metrics.ReasonInvalidProviderConfig},
		{name: "invalid log level", err: errors.New(`invalid envoy-gateway log level "verbose": must be one of debug, info, warn, error`), want: metrics.ReasonInvalidProviderConfig},
		{name: "secrets tls", err: errors.New("failed to reconcile envoy-gateway TLS secrets: something"), want: metrics.ReasonSecrets},
		{name: "secrets manager", err: errors.New("failed to construct secrets-manager: no cluster"), want: metrics.ReasonSecrets},
		{name: "managed resource deploy", err: errors.New("failed to deploy envoy-gateway: mr timed out"), want: metrics.ReasonManagedResource},
		{name: "managed resource delete", err: errors.New("failed to delete envoy-gateway: mr timed out"), want: metrics.ReasonManagedResource},
		{name: "managed resource force-delete", err: errors.New("failed to force-delete envoy-gateway: mr timed out"), want: metrics.ReasonManagedResource},
		{name: "cluster lookup", err: errors.New("failed to get cluster: not found"), want: metrics.ReasonClusterLookup},
		{name: "unknown", err: errors.New("something else entirely"), want: metrics.ReasonOther},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyError(tc.err)
			if got != tc.want {
				t.Errorf("classifyError(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}
