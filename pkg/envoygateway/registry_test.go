// SPDX-FileCopyrightText: Contributors to the Gardener project
//
// SPDX-License-Identifier: Apache-2.0

package envoygateway

import (
	"testing"

	"github.com/gardener/gardener/pkg/client/kubernetes"
	"github.com/gardener/gardener/pkg/utils/managedresources"
	"github.com/go-logr/logr"
)

func TestRegistrySerializedObjects_BelowSecretLimit(t *testing.T) {
	d := NewDeployer(nil, logr.Discard(), DefaultConfig(), newTestImageVector(t))
	resources, err := d.GenerateResources()
	if err != nil {
		t.Fatalf("GenerateResources returned error: %v", err)
	}

	registry := managedresources.NewRegistry(kubernetes.ShootScheme, kubernetes.ShootCodec, kubernetes.ShootSerializer)
	for name, data := range resources {
		registry.AddSerialized(name, data)
	}
	serialized, err := registry.SerializedObjects()
	if err != nil {
		t.Fatalf("SerializedObjects returned error: %v", err)
	}

	const secretLimit = 1 << 20 // 1 MiB
	total := 0
	for _, v := range serialized {
		total += len(v)
	}
	if total >= secretLimit {
		t.Fatalf("serialized manifest size %d bytes exceeds Secret limit of %d bytes", total, secretLimit)
	}
	t.Logf("serialized manifest size: %d bytes (raw: ~5 MiB)", total)
}
