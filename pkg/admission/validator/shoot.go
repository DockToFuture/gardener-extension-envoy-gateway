// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

// Package validator provides admission webhook validators for the Envoy
// Gateway extension.
package validator

import (
	"context"
	"fmt"
	"net/http"

	extensionswebhook "github.com/gardener/gardener/extensions/pkg/webhook"
	gardencorev1beta1 "github.com/gardener/gardener/pkg/apis/core/v1beta1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/gardener/gardener-extension-envoy-gateway/pkg/apis/config"
	"github.com/gardener/gardener-extension-envoy-gateway/pkg/envoygateway"
)

const (
	// Name is the name of the shoot validator webhook.
	Name = "shoot-validator"
	// ExtensionType is the type of extension being validated.
	ExtensionType = "envoy-gateway"
	// TraefikExtensionType is the GEP-57 shoot-traefik extension type. When it
	// is enabled on the same shoot as this extension, the webhook emits a
	// non-fatal warning about the doubled load-balancer cost.
	TraefikExtensionType = "shoot-traefik"
	// WebhookPath is the URL path on which the validator is served.
	WebhookPath = "/webhooks/validate-envoy-gateway"
)

// shootValidator validates Shoot resources for the Envoy Gateway extension.
//
// It implements controller-runtime's admission.Handler directly rather than
// the gardener extensions webhook Validator interface
// (extensions/pkg/webhook.Validator, whose Validate(ctx, new, old) error
// signature and handler discard warnings). Implementing Handle ourselves is
// what lets us return the non-fatal admission warnings GEP-68 requires — one
// for channel==experimental, one for shoot-traefik coexistence — alongside the
// fatal validation errors.
type shootValidator struct {
	decoder    admission.Decoder
	cfgDecoder runtime.Decoder
}

var _ admission.Handler = &shootValidator{}

// NewShootValidatorWebhook creates a new admission webhook for Shoot validation.
// It ensures that the Envoy Gateway extension can only be enabled for shoots
// with purpose "evaluation", that any providerConfig decodes strictly, and it
// surfaces the GEP-68 non-fatal warnings.
//
// The returned extensionswebhook.Webhook is assembled by hand (rather than via
// extensionswebhook.New) so we can plug in our own admission.Handler; the
// framework still owns TLS, ValidatingWebhookConfiguration registration, and
// path routing based on the fields set here.
func NewShootValidatorWebhook(mgr manager.Manager) (*extensionswebhook.Webhook, error) {
	handler, err := NewShootValidator(mgr)
	if err != nil {
		return nil, err
	}

	return &extensionswebhook.Webhook{
		Name:    Name,
		Action:  extensionswebhook.ActionValidating,
		Path:    WebhookPath,
		Target:  extensionswebhook.TargetSeed,
		Types:   []extensionswebhook.Type{{Obj: &gardencorev1beta1.Shoot{}}},
		Webhook: &admission.Webhook{Handler: handler, RecoverPanic: new(true)},
	}, nil
}

// NewShootValidator creates a new shoot validator admission handler bound to
// the manager's scheme.
func NewShootValidator(mgr manager.Manager) (admission.Handler, error) {
	return newValidatorForScheme(mgr.GetScheme()), nil
}

// newValidatorForScheme builds the handler from a scheme. Split out from
// NewShootValidator so tests can construct it without a manager.
func newValidatorForScheme(scheme *runtime.Scheme) *shootValidator {
	return &shootValidator{
		decoder:    admission.NewDecoder(scheme),
		cfgDecoder: serializer.NewCodecFactory(scheme, serializer.EnableStrict).UniversalDecoder(),
	}
}

// Handle decodes the Shoot from the admission request, runs validation, and
// returns an allowed response (optionally carrying non-fatal warnings) or a
// denial on the first fatal error.
func (v *shootValidator) Handle(_ context.Context, req admission.Request) admission.Response {
	shoot := &gardencorev1beta1.Shoot{}
	if err := v.decoder.Decode(req, shoot); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}

	warnings, err := v.validateShoot(shoot)
	if err != nil {
		return admission.Denied(err.Error())
	}

	return admission.Allowed("").WithWarnings(warnings...)
}

// validateShoot runs the fatal validation and collects non-fatal warnings for
// the enabled envoy-gateway extension. A nil error means the Shoot is admitted;
// the returned warnings (possibly empty) are surfaced to the user regardless.
func (v *shootValidator) validateShoot(shoot *gardencorev1beta1.Shoot) ([]string, error) {
	// Locate the envoy-gateway extension entry and check enablement.
	var (
		hasEnvoyGateway bool
		hasTraefik      bool
		providerCfgRaw  []byte
	)
	for _, ext := range shoot.Spec.Extensions {
		if ext.Type == TraefikExtensionType && (ext.Disabled == nil || !*ext.Disabled) {
			hasTraefik = true

			continue
		}
		if ext.Type != ExtensionType {
			continue
		}
		if ext.Disabled != nil && *ext.Disabled {
			return nil, nil
		}
		hasEnvoyGateway = true
		if ext.ProviderConfig != nil {
			providerCfgRaw = ext.ProviderConfig.Raw
		}
	}
	if !hasEnvoyGateway {
		return nil, nil
	}

	// Enforce purpose=evaluation.
	if shoot.Spec.Purpose == nil || *shoot.Spec.Purpose != gardencorev1beta1.ShootPurposeEvaluation {
		purposeStr := "nil"
		if shoot.Spec.Purpose != nil {
			purposeStr = string(*shoot.Spec.Purpose)
		}

		return nil, fmt.Errorf(
			"envoy-gateway extension can only be enabled for shoots with purpose 'evaluation' (current: %s). "+
				"This scope restriction is part of the GEP-68 incubation phase",
			purposeStr,
		)
	}

	var warnings []string

	// GEP-68: warn when shoot-traefik is enabled alongside this extension —
	// running both ingress paths in one shoot pays for both sets of load
	// balancers.
	if hasTraefik {
		warnings = append(warnings,
			"both the shoot-traefik (GEP-57) and envoy-gateway (GEP-68) extensions are enabled on this shoot. "+
				"Running both ingress paths doubles the load-balancer cost; a single ingress path per shoot is recommended in production.")
	}

	// Strict-decode and validate the providerConfig if any.
	if len(providerCfgRaw) > 0 {
		var cfg config.EnvoyGatewayConfig
		if err := runtime.DecodeInto(v.cfgDecoder, providerCfgRaw, &cfg); err != nil {
			return nil, fmt.Errorf("invalid envoy-gateway providerConfig: %w", err)
		}
		if err := validateLogLevel(cfg.ControlPlane); err != nil {
			return nil, err
		}
		if err := validateDataPlaneLogLevel(cfg.DataPlane); err != nil {
			return nil, err
		}
		if cfg.Channel != "" {
			if _, ok := envoygateway.ValidChannels[string(cfg.Channel)]; !ok {
				return nil, fmt.Errorf("invalid envoy-gateway providerConfig: channel %q must be one of standard, experimental", cfg.Channel)
			}
		}

		// GEP-68: warn when the experimental Gateway API channel is selected —
		// experimental APIs may change in backwards-incompatible ways between
		// releases.
		if cfg.Channel == config.ChannelExperimental {
			warnings = append(warnings,
				"envoy-gateway providerConfig selects channel 'experimental'. "+
					"The experimental-channel Gateway API CRDs (TCPRoute, TLSRoute, UDPRoute, BackendTLSPolicy) "+
					"may change in backwards-incompatible ways between releases.")
		}
	}

	return warnings, nil
}

// validateLogLevel checks the control-plane log level, if set.
func validateLogLevel(cp *config.ControlPlaneConfig) error {
	if cp == nil || cp.LogLevel == "" {
		return nil
	}
	if _, ok := envoygateway.ValidLogLevels[string(cp.LogLevel)]; !ok {
		return fmt.Errorf("invalid envoy-gateway providerConfig: controlPlane.logLevel %q must be one of debug, info, warn, error", cp.LogLevel)
	}

	return nil
}

// validateDataPlaneLogLevel checks the data-plane log level, if set.
func validateDataPlaneLogLevel(dp *config.DataPlaneConfig) error {
	if dp == nil || dp.LogLevel == "" {
		return nil
	}
	if _, ok := envoygateway.ValidLogLevels[string(dp.LogLevel)]; !ok {
		return fmt.Errorf("invalid envoy-gateway providerConfig: dataPlane.logLevel %q must be one of debug, info, warn, error", dp.LogLevel)
	}

	return nil
}
