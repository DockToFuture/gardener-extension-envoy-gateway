// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package validator

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	gardencoreinstall "github.com/gardener/gardener/pkg/apis/core/install"
	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	admissionv1 "k8s.io/api/admission/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	configinstall "github.com/gardener/gardener-extension-envoy-gateway/pkg/apis/config/install"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	gardencoreinstall.Install(scheme)
	configinstall.Install(scheme)

	return scheme
}

// shootExtension builds a shoot extension entry, embedding the given raw
// providerConfig (may be nil).
func shootExtension(extType string, providerConfig []byte) gardencorev1beta1.Extension {
	ext := gardencorev1beta1.Extension{Type: extType}
	if providerConfig != nil {
		ext.ProviderConfig = &runtime.RawExtension{Raw: providerConfig}
	}

	return ext
}

// handleShoot marshals the shoot into an admission.Request and runs it through
// the handler.
func handleShoot(t *testing.T, v *shootValidator, shoot *gardencorev1beta1.Shoot) admission.Response {
	t.Helper()
	shoot.APIVersion = "core.gardener.cloud/v1beta1"
	shoot.Kind = "Shoot"
	raw, err := json.Marshal(shoot)
	if err != nil {
		t.Fatalf("failed to marshal shoot: %v", err)
	}

	return v.Handle(context.Background(), admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			Kind:      metav1.GroupVersionKind(schema.FromAPIVersionAndKind("core.gardener.cloud/v1beta1", "Shoot")),
			Operation: admissionv1.Create,
			Object:    runtime.RawExtension{Raw: raw},
		},
	})
}

func evaluation() *gardencorev1beta1.ShootPurpose {
	p := gardencorev1beta1.ShootPurposeEvaluation

	return &p
}

func TestHandle_ExtensionNotEnabled_Allowed(t *testing.T) {
	v := newValidatorForScheme(testScheme(t))
	shoot := &gardencorev1beta1.Shoot{
		Spec: gardencorev1beta1.ShootSpec{
			Purpose:    ptr.To(gardencorev1beta1.ShootPurposeDevelopment),
			Extensions: []gardencorev1beta1.Extension{shootExtension("some-other-extension", nil)},
		},
	}
	resp := handleShoot(t, v, shoot)
	if !resp.Allowed {
		t.Fatalf("expected allowed when extension not enabled, got denied: %s", resp.Result.Message)
	}
	if len(resp.Warnings) != 0 {
		t.Fatalf("expected no warnings, got %v", resp.Warnings)
	}
}

func TestHandle_WrongPurpose_Denied(t *testing.T) {
	v := newValidatorForScheme(testScheme(t))
	shoot := &gardencorev1beta1.Shoot{
		Spec: gardencorev1beta1.ShootSpec{
			Purpose:    ptr.To(gardencorev1beta1.ShootPurposeProduction),
			Extensions: []gardencorev1beta1.Extension{shootExtension(ExtensionType, nil)},
		},
	}
	resp := handleShoot(t, v, shoot)
	if resp.Allowed {
		t.Fatal("expected denial for non-evaluation purpose")
	}
	if !strings.Contains(resp.Result.Message, "purpose 'evaluation'") {
		t.Fatalf("unexpected denial message: %s", resp.Result.Message)
	}
}

func TestHandle_EvaluationNoConfig_AllowedNoWarnings(t *testing.T) {
	v := newValidatorForScheme(testScheme(t))
	shoot := &gardencorev1beta1.Shoot{
		Spec: gardencorev1beta1.ShootSpec{
			Purpose:    evaluation(),
			Extensions: []gardencorev1beta1.Extension{shootExtension(ExtensionType, nil)},
		},
	}
	resp := handleShoot(t, v, shoot)
	if !resp.Allowed {
		t.Fatalf("expected allowed, got: %s", resp.Result.Message)
	}
	if len(resp.Warnings) != 0 {
		t.Fatalf("expected no warnings, got %v", resp.Warnings)
	}
}

func TestHandle_ExperimentalChannel_WarnsButAllowed(t *testing.T) {
	v := newValidatorForScheme(testScheme(t))
	cfg := []byte(`{"apiVersion":"envoy-gateway.extensions.gardener.cloud/v1alpha1","kind":"EnvoyGatewayConfig","channel":"experimental"}`)
	shoot := &gardencorev1beta1.Shoot{
		Spec: gardencorev1beta1.ShootSpec{
			Purpose:    evaluation(),
			Extensions: []gardencorev1beta1.Extension{shootExtension(ExtensionType, cfg)},
		},
	}
	resp := handleShoot(t, v, shoot)
	if !resp.Allowed {
		t.Fatalf("experimental channel must be allowed (non-fatal), got: %s", resp.Result.Message)
	}
	if !warningsContain(resp.Warnings, "experimental") {
		t.Fatalf("expected an experimental-channel warning, got %v", resp.Warnings)
	}
}

func TestHandle_TraefikCoexistence_WarnsButAllowed(t *testing.T) {
	v := newValidatorForScheme(testScheme(t))
	shoot := &gardencorev1beta1.Shoot{
		Spec: gardencorev1beta1.ShootSpec{
			Purpose: evaluation(),
			Extensions: []gardencorev1beta1.Extension{
				shootExtension(ExtensionType, nil),
				shootExtension(TraefikExtensionType, nil),
			},
		},
	}
	resp := handleShoot(t, v, shoot)
	if !resp.Allowed {
		t.Fatalf("traefik coexistence must be allowed (non-fatal), got: %s", resp.Result.Message)
	}
	if !warningsContain(resp.Warnings, "shoot-traefik") {
		t.Fatalf("expected a shoot-traefik coexistence warning, got %v", resp.Warnings)
	}
}

func TestHandle_DisabledTraefik_NoWarning(t *testing.T) {
	v := newValidatorForScheme(testScheme(t))
	traefik := shootExtension(TraefikExtensionType, nil)
	traefik.Disabled = new(true)
	shoot := &gardencorev1beta1.Shoot{
		Spec: gardencorev1beta1.ShootSpec{
			Purpose: evaluation(),
			Extensions: []gardencorev1beta1.Extension{
				shootExtension(ExtensionType, nil),
				traefik,
			},
		},
	}
	resp := handleShoot(t, v, shoot)
	if !resp.Allowed {
		t.Fatalf("expected allowed, got: %s", resp.Result.Message)
	}
	if warningsContain(resp.Warnings, "shoot-traefik") {
		t.Fatalf("did not expect a traefik warning when traefik is disabled, got %v", resp.Warnings)
	}
}

func TestHandle_InvalidChannel_Denied(t *testing.T) {
	v := newValidatorForScheme(testScheme(t))
	cfg := []byte(`{"apiVersion":"envoy-gateway.extensions.gardener.cloud/v1alpha1","kind":"EnvoyGatewayConfig","channel":"bogus"}`)
	shoot := &gardencorev1beta1.Shoot{
		Spec: gardencorev1beta1.ShootSpec{
			Purpose:    evaluation(),
			Extensions: []gardencorev1beta1.Extension{shootExtension(ExtensionType, cfg)},
		},
	}
	resp := handleShoot(t, v, shoot)
	if resp.Allowed {
		t.Fatal("expected denial for invalid channel")
	}
	if !strings.Contains(resp.Result.Message, "channel") {
		t.Fatalf("unexpected denial message: %s", resp.Result.Message)
	}
}

func TestHandle_InvalidLogLevel_Denied(t *testing.T) {
	v := newValidatorForScheme(testScheme(t))
	cfg := []byte(`{"apiVersion":"envoy-gateway.extensions.gardener.cloud/v1alpha1","kind":"EnvoyGatewayConfig","controlPlane":{"logLevel":"loud"}}`)
	shoot := &gardencorev1beta1.Shoot{
		Spec: gardencorev1beta1.ShootSpec{
			Purpose:    evaluation(),
			Extensions: []gardencorev1beta1.Extension{shootExtension(ExtensionType, cfg)},
		},
	}
	resp := handleShoot(t, v, shoot)
	if resp.Allowed {
		t.Fatal("expected denial for invalid controlPlane.logLevel")
	}
	if !strings.Contains(resp.Result.Message, "logLevel") {
		t.Fatalf("unexpected denial message: %s", resp.Result.Message)
	}
}

func TestHandle_UnknownField_Denied(t *testing.T) {
	// Strict decoding must reject unknown fields in the providerConfig.
	v := newValidatorForScheme(testScheme(t))
	cfg := []byte(`{"apiVersion":"envoy-gateway.extensions.gardener.cloud/v1alpha1","kind":"EnvoyGatewayConfig","bogusField":true}`)
	shoot := &gardencorev1beta1.Shoot{
		Spec: gardencorev1beta1.ShootSpec{
			Purpose:    evaluation(),
			Extensions: []gardencorev1beta1.Extension{shootExtension(ExtensionType, cfg)},
		},
	}
	resp := handleShoot(t, v, shoot)
	if resp.Allowed {
		t.Fatal("expected denial for unknown providerConfig field")
	}
}

func warningsContain(warnings []string, substr string) bool {
	for _, w := range warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}

	return false
}
