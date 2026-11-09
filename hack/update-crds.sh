#!/usr/bin/env bash
#
# SPDX-FileCopyrightText: Contributors to the Gardener project
# SPDX-License-Identifier: Apache-2.0
#
# Refreshes the embedded CRD YAML assets used by pkg/envoygateway/deployer.go.
# Run this script after bumping the versions below. Commit the resulting files.
#
# The fetched artifacts are pinned for integrity: the two Gateway API CRD YAMLs
# are verified against sha256 checksums and the Envoy Gateway chart against its
# OCI digest, all recorded in hack/crd-assets.sha256. A normal run verifies and
# aborts on any mismatch without touching the embedded assets. An intentional
# version bump is adopted by re-running with REGENERATE_CHECKSUMS=1, which
# re-records the manifest and skips verification.
#
# Required tools: curl, helm, yq (mikefarah/yq v4+).

set -euo pipefail

for tool in curl helm yq; do
  if ! command -v "${tool}" >/dev/null 2>&1; then
    echo "error: required tool '${tool}' not found in PATH" >&2
    exit 1
  fi
done

GATEWAY_API_VERSION="${GATEWAY_API_VERSION:-v1.6.3}"
ENVOY_GATEWAY_VERSION="${ENVOY_GATEWAY_VERSION:-v1.9.3}"

SCRIPT_DIR="$( cd "$( dirname "${BASH_SOURCE[0]}" )" && pwd )"
REPO_ROOT="$( cd "${SCRIPT_DIR}/.." && pwd )"
ASSETS_DIR="${REPO_ROOT}/pkg/envoygateway/assets"
MANIFEST="${SCRIPT_DIR}/crd-assets.sha256"

STANDARD_YAML="gateway-api-standard.yaml"
EXPERIMENTAL_YAML="gateway-api-experimental.yaml"
ENVOY_CRDS_YAML="envoy-gateway-crds.yaml"
CHART_REF="envoyproxy/gateway-helm"

# sha256_hex <path> -- prints the 64-char lowercase hex digest only.
# Prefers sha256sum (Linux/CI), falls back to shasum -a 256 (macOS).
sha256_hex() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  else
    shasum -a 256 "$1" | awk '{print $1}'
  fi
}

# mismatch_hint -- common tail for every verification failure.
mismatch_hint() {
  echo "  if this is an intentional version bump, re-run with REGENERATE_CHECKSUMS=1" >&2
}

REGENERATE="${REGENERATE_CHECKSUMS:-}"

mkdir -p "${ASSETS_DIR}"
TMP_DIR="$(mktemp -d)"
trap 'rm -rf "${TMP_DIR}"' EXIT

# --- Load pinned values (normal mode) or prepare to record them (regen mode). ---
declare -A EXPECTED=()
declare -A NEW_HASH=()
CHART_DIGEST_EXPECTED=""

if [[ -z "${REGENERATE}" ]]; then
  if [[ ! -f "${MANIFEST}" ]]; then
    echo "error: pin manifest ${MANIFEST} not found" >&2
    mismatch_hint
    exit 1
  fi
  # File redirection, not a pipe: a pipe runs the loop in a subshell and the
  # arrays populated here would be discarded.
  while read -r key val; do
    [[ -z "${key}" || "${key}" == \#* ]] && continue
    if [[ "${key}" == chart:* ]]; then
      CHART_DIGEST_EXPECTED="${key##*@}"   # sha256:...
    else
      EXPECTED["${key}"]="${val}"
    fi
  done < "${MANIFEST}"
fi

# fetch_and_verify <url> <asset-filename>
# Downloads to the temp dir and either records (regen) or verifies (normal) the
# checksum. Nothing is written to ASSETS_DIR here: all artifacts are staged in
# the temp dir and copied over in one step only after every check has passed,
# so a failure partway through never leaves the embedded assets half-updated.
fetch_and_verify() {
  local url="$1" name="$2" tmp="${TMP_DIR}/$2" got want
  echo "Downloading ${name} ..."
  curl -fsSL "${url}" -o "${tmp}"
  got="$(sha256_hex "${tmp}")"

  if [[ -n "${REGENERATE}" ]]; then
    NEW_HASH["${name}"]="${got}"
    echo "  recorded ${name} ${got}"
    return 0
  fi

  want="${EXPECTED[${name}]:-}"
  if [[ -z "${want}" ]]; then
    echo "error: no recorded checksum for ${name} in ${MANIFEST}" >&2
    mismatch_hint
    exit 1
  fi
  if [[ "${got}" != "${want}" ]]; then
    echo "error: checksum mismatch for ${name}" >&2
    echo "  expected: ${want}" >&2
    echo "  actual:   ${got}" >&2
    mismatch_hint
    exit 1
  fi
  echo "  verified ${name}"
}

echo "Gateway API ${GATEWAY_API_VERSION} / Envoy Gateway ${ENVOY_GATEWAY_VERSION}"

fetch_and_verify \
  "https://github.com/kubernetes-sigs/gateway-api/releases/download/${GATEWAY_API_VERSION}/standard-install.yaml" \
  "${STANDARD_YAML}"

fetch_and_verify \
  "https://github.com/kubernetes-sigs/gateway-api/releases/download/${GATEWAY_API_VERSION}/experimental-install.yaml" \
  "${EXPERIMENTAL_YAML}"

# --- Envoy Gateway chart: pull by version, capture the reported digest, verify
# it against the pin, then render from the local tarball we just downloaded.
# This renders the exact verified bytes and avoids depending on helm accepting
# an OCI digest reference directly. ---
echo "Pulling Envoy Gateway chart ${ENVOY_GATEWAY_VERSION} ..."
pull_out="$(helm pull "oci://docker.io/${CHART_REF}" \
  --version "${ENVOY_GATEWAY_VERSION}" \
  --destination "${TMP_DIR}" 2>&1)"
# sed -n '...p' exits 0 even on no match; grep would abort under set -e.
chart_digest="$(printf '%s\n' "${pull_out}" | sed -n 's/^Digest:[[:space:]]*\(sha256:[0-9a-f]*\).*/\1/p')"
if [[ -z "${chart_digest}" ]]; then
  echo "error: could not parse chart digest from 'helm pull' output:" >&2
  printf '%s\n' "${pull_out}" >&2
  exit 1
fi

if [[ -n "${REGENERATE}" ]]; then
  echo "  recorded chart digest ${chart_digest}"
else
  if [[ -z "${CHART_DIGEST_EXPECTED}" ]]; then
    echo "error: no recorded chart digest in ${MANIFEST}" >&2
    mismatch_hint
    exit 1
  fi
  if [[ "${chart_digest}" != "${CHART_DIGEST_EXPECTED}" ]]; then
    echo "error: chart digest mismatch for ${CHART_REF}" >&2
    echo "  expected: ${CHART_DIGEST_EXPECTED}" >&2
    echo "  actual:   ${chart_digest}" >&2
    mismatch_hint
    exit 1
  fi
  echo "  verified chart digest ${chart_digest}"
fi

echo "Rendering Envoy Gateway CRDs via helm template ..."
chart_tgz="$(ls "${TMP_DIR}"/gateway-helm-*.tgz)"   # fresh TMP_DIR => exactly one match
helm template envoy-gateway "${chart_tgz}" \
  --namespace envoy-gateway-system \
  --include-crds \
  > "${TMP_DIR}/envoy-gateway.yaml"
# Keep only CustomResourceDefinition documents for the gateway.envoyproxy.io
# group and emit them as a proper multi-document YAML stream with '---'
# separators, ready for parsing by k8s.io/apimachinery's YAML decoder. The
# helm chart bundles Gateway API CRDs too; those are shipped separately via
# the gateway-api-*.yaml assets and must not be duplicated here.
yq eval-all 'select(.kind == "CustomResourceDefinition" and .spec.group == "gateway.envoyproxy.io")' \
  "${TMP_DIR}/envoy-gateway.yaml" > "${TMP_DIR}/${ENVOY_CRDS_YAML}"

# --- All downloads verified (or recorded) and the chart rendered. Only now,
# after every check has passed, copy the staged artifacts into ASSETS_DIR in
# one step. A failure in any earlier step exits before this point, leaving the
# embedded assets untouched rather than half-updated. ---
cp "${TMP_DIR}/${STANDARD_YAML}"     "${ASSETS_DIR}/${STANDARD_YAML}"
cp "${TMP_DIR}/${EXPERIMENTAL_YAML}" "${ASSETS_DIR}/${EXPERIMENTAL_YAML}"
cp "${TMP_DIR}/${ENVOY_CRDS_YAML}"   "${ASSETS_DIR}/${ENVOY_CRDS_YAML}"

# --- Regen mode: rewrite the pin manifest atomically. ---
if [[ -n "${REGENERATE}" ]]; then
  {
    echo "# Pinned integrity manifest for hack/update-crds.sh assets."
    echo "# Regenerate with: REGENERATE_CHECKSUMS=1 hack/update-crds.sh"
    echo "# gateway-api <version> : ${GATEWAY_API_VERSION}"
    echo "# envoy-gateway <version> : ${ENVOY_GATEWAY_VERSION}"
    echo "${STANDARD_YAML}     ${NEW_HASH[${STANDARD_YAML}]}"
    echo "${EXPERIMENTAL_YAML} ${NEW_HASH[${EXPERIMENTAL_YAML}]}"
    echo "chart:${CHART_REF}@${chart_digest}"
  } > "${MANIFEST}.tmp" && mv "${MANIFEST}.tmp" "${MANIFEST}"
  echo "Recorded new pins to ${MANIFEST} (verification skipped)."
fi

echo "Done. Refreshed assets:"
ls -la "${ASSETS_DIR}"
