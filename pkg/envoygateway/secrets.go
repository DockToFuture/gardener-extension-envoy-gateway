// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package envoygateway

import (
	"crypto/rand"

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
		{
			Config:  &HMACSecretConfig{Name: HMACSecretName},
			Options: []secretsmanager.GenerateOption{secretsmanager.Persist()},
		},
	}
}

// hmacSecretLength is the length in bytes of the OIDC HMAC signing key. 32
// bytes (256 bits) matches the HMAC-SHA256 block the Envoy OAuth2 filter uses
// to sign its state cookies.
const hmacSecretLength = 32

// HMACSecretConfig is a secrets-manager [secretsutils.ConfigInterface] that
// generates a cryptographically random OIDC HMAC signing key.
type HMACSecretConfig struct {
	// Name is the secrets-manager name of the config.
	Name string
}

// GetName returns the name of the config.
func (c *HMACSecretConfig) GetName() string { return c.Name }

// Generate reads hmacSecretLength random bytes and returns them as the HMAC key.
func (c *HMACSecretConfig) Generate() (secretsutils.DataInterface, error) {
	key := make([]byte, hmacSecretLength)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}

	return &hmacKey{key: key}, nil
}

// hmacKey is the generated-secret counterpart of HMACSecretConfig.
type hmacKey struct {
	key []byte
}

// SecretData returns the key under HMACSecretKey, the data key the deployer
// ships into the shoot and the Envoy OAuth2 filter reads.
func (k *hmacKey) SecretData() map[string][]byte {
	return map[string][]byte{
		HMACSecretKey: k.key,
	}
}
