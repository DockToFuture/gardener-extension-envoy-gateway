#!/usr/bin/env bash
#
# SPDX-FileCopyrightText: Contributors to the Gardener project
# SPDX-License-Identifier: Apache-2.0
#
# Refreshes the embedded CRD YAML assets used by pkg/envoygateway/deployer.go.
# Run this script after bumping the versions below. Commit the resulting files.
#
# Required tools: curl, helm, yq (mikefarah/yq v4+).

set -euo pipefail

for tool in curl helm yq; do
  if ! command -v "${tool}" >/dev/null 2>&1; then
    echo "error: required tool '${tool}' not found in PATH" >&2
    exit 1
  fi
done

GATEWAY_API_VERSION="${GATEWAY_API_VERSION:-v1.5.1}"
ENVOY_GATEWAY_VERSION="${ENVOY_GATEWAY_VERSION:-v1.8.2}"

SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
REPO_ROOT="$( cd "${SCRIPT_DIR}/.." && pwd )"
ASSETS_DIR="${REPO_ROOT}/pkg/envoygateway/assets"

mkdir -p "${ASSETS_DIR}"

echo "Downloading Gateway API standard-channel CRDs ${GATEWAY_API_VERSION} ..."
curl -fsSL "https://github.com/kubernetes-sigs/gateway-api/releases/download/${GATEWAY_API_VERSION}/standard-install.yaml" \
  -o "${ASSETS_DIR}/gateway-api-standard.yaml"

echo "Downloading Gateway API experimental-channel CRDs ${GATEWAY_API_VERSION} ..."
curl -fsSL "https://github.com/kubernetes-sigs/gateway-api/releases/download/${GATEWAY_API_VERSION}/experimental-install.yaml" \
  -o "${ASSETS_DIR}/gateway-api-experimental.yaml"

echo "Rendering Envoy Gateway CRDs ${ENVOY_GATEWAY_VERSION} via helm template ..."
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT
helm template envoy-gateway oci://docker.io/envoyproxy/gateway-helm \
  --version "${ENVOY_GATEWAY_VERSION}" \
  --namespace envoy-gateway-system \
  --include-crds \
  > "${TMP_DIR}/envoy-gateway.yaml"
# Keep only CustomResourceDefinition documents for the gateway.envoyproxy.io
# group and emit them as a proper multi-document YAML stream with '---'
# separators, ready for parsing by k8s.io/apimachinery's YAML decoder. The
# helm chart bundles Gateway API CRDs too; those are shipped separately via
# the gateway-api-*.yaml assets and must not be duplicated here.
yq eval-all 'select(.kind == "CustomResourceDefinition" and .spec.group == "gateway.envoyproxy.io")' \
  "${TMP_DIR}/envoy-gateway.yaml" > "${ASSETS_DIR}/envoy-gateway-crds.yaml"

echo "Done. Refreshed assets:"
ls -la "${ASSETS_DIR}"
