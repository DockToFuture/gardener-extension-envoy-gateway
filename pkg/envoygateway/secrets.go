// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package envoygateway

import (
	"crypto/hmac"
	"crypto/sha256"

	extensionssecretmanager "github.com/gardener/gardener/extensions/pkg/util/secret/manager"
	secretsutils "github.com/gardener/gardener/pkg/utils/secrets"
	secretsmanager "github.com/gardener/gardener/pkg/utils/secrets/manager"
)

const (
	// CASecretName is the secrets-manager name for the envoy-gateway CA.
	CASecretName = "ca-envoy-gateway"
	// XDSServerCertSecretName is the secrets-manager name for the xDS server cert
	// served by envoy-gateway to its data-plane Envoy proxies.
	XDSServerCertSecretName = "envoy-gateway-xds"
	// EnvoyClientCertSecretName is the secrets-manager name for the client cert
	// the data-plane Envoy proxies present to the xDS server (and to the
	// optional rate-limit service). Signed by the same CA as the server cert.
	EnvoyClientCertSecretName = "envoy-client"

	// TLSSecretName is the name of the Secret materialized into the shoot at
	// /certs of the envoy-gateway control-plane pod. The xDS runner reads
	// tls.crt, tls.key, and ca.crt from that mount.
	TLSSecretName = "envoy-gateway"
	// EnvoySecretName is the name of the Secret carrying the data-plane
	// envoy client cert (ca.crt, tls.crt, tls.key). envoy-gateway looks for
	// it up in its ControllerNamespace on every reconcile.
	EnvoySecretName = "envoy"
	// HMACSecretName is the name of the Secret carrying the OIDC HMAC key
	// used by SecurityPolicy's OAuth2 filter to sign state cookies. Only
	// consumed when a SecurityPolicy uses OIDC; harmless when unused, but
	// envoy-gateway logs an error every reconcile if it's missing.
	HMACSecretName = "envoy-oidc-hmac" // #nosec G101 -- this is a Secret's object name, not a hard-coded credential.
	// HMACSecretKey is the data key inside HMACSecretName.
	HMACSecretKey = "hmac-secret"
)

// SecretConfigs returns the secret configs the actuator must hand to the
// secrets-manager: a dedicated CA for envoy-gateway plus the xDS server cert
// and the envoy client cert, both signed by that CA.
func SecretConfigs() []extensionssecretmanager.SecretConfigWithOptions {
	return []extensionssecretmanager.SecretConfigWithOptions{
		{
			Config: &secretsutils.CertificateSecretConfig{
				Name:       CASecretName,
				CommonName: CASecretName,
				CertType:   secretsutils.CACert,
			},
			Options: []secretsmanager.GenerateOption{secretsmanager.Persist()},
		},
		{
			Config: &secretsutils.CertificateSecretConfig{
				Name:                        XDSServerCertSecretName,
				CommonName:                  DeploymentName,
				DNSNames:                    []string{DeploymentName, DeploymentName + "." + Namespace, DeploymentName + "." + Namespace + ".svc"},
				CertType:                    secretsutils.ServerCert,
				SkipPublishingCACertificate: true,
			},
			Options: []secretsmanager.GenerateOption{secretsmanager.SignedByCA(CASecretName)},
		},
		{
			Config: &secretsutils.CertificateSecretConfig{
				Name:                        EnvoyClientCertSecretName,
				CommonName:                  EnvoySecretName,
				CertType:                    secretsutils.ClientCert,
				SkipPublishingCACertificate: true,
			},
			Options: []secretsmanager.GenerateOption{secretsmanager.SignedByCA(CASecretName)},
		},
	}
}

// HMACSecretBytes returns 32 deterministic bytes derived from the given
// cluster identifier, suitable for the OIDC HMAC key. Determinism keeps the
// key stable across reconciles without persisting extra state; the input is
// the per-shoot Cluster name, so different shoots get different keys.
func HMACSecretBytes(clusterUID string) []byte {
	mac := hmac.New(sha256.New, []byte("envoy-gateway-oidc-hmac"))
	// hash.Hash.Write never returns an error.
	_, _ = mac.Write([]byte(clusterUID))

	return mac.Sum(nil)
}
