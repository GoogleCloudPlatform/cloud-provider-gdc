#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Google LLC
#
# SPDX-License-Identifier: Apache-2.0

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="${GITHUB_WORKSPACE:-$(cd "${SCRIPT_DIR}/.." && pwd)}"
cd "${REPO_ROOT}"

# shellcheck source=scripts/ci-common.sh
source "${SCRIPT_DIR}/ci-common.sh"

check_ci_preconditions

COMMIT_HASH="${COMMIT_HASH:-$(git rev-parse --short HEAD)}"
REGION="${GDC_REGION:-us-west6}"
ZONE="${GDC_ZONE:-us-west6-a}"
GDCH_PROJECT="${GDC_PROJECT:-sapbtp}"
VUC="${GDC_VUC:-gardener-github-ci}"
ORG="${GDC_ORG:-gdc1}"
LAB_URL="${GDC_LAB_URL:-staging.gpcdemolabs.com}"
GDCLOUD_VERSION="${GDCLOUD_VERSION:-1.16.2}"
VCLUSTER_VERSION="${VCLUSTER_VERSION:-0.32.1}"
VCLUSTER_K8S_TAG="${VCLUSTER_K8S_TAG:-v1.35.0}"
REPO_LC=$(printf '%s' "${GITHUB_REPOSITORY:-googlecloudplatform/cloud-provider-gdc}" | tr '[:upper:]' '[:lower:]')
IMAGE_REPOSITORY="${IMAGE_REPOSITORY:-ghcr.io/${REPO_LC}/cloud-controller-manager-gdch}"
IMAGE_REPOSITORY=$(printf '%s' "${IMAGE_REPOSITORY}" | tr '[:upper:]' '[:lower:]')
IMAGE_TAG="${IMAGE_TAG:-pr-${COMMIT_HASH}}"
IMAGE_WITHTAG="${IMAGE_REPOSITORY}:${IMAGE_TAG}"

WORK_DIR="$(mktemp -d)"
CA_FILE="${WORK_DIR}/cafile"
SA_FILE="${CCM_SERVICE_ACCOUNT_FILE:-${WORK_DIR}/ccm_service_account.json}"
MGMT_URL="https://management-kube.apiserver.${ORG}.${ZONE}.${LAB_URL}"
IMAGE_PUSHED=false

cleanup() {
  local exit_code=$?
  if [[ "${IMAGE_PUSHED}" == "true" ]]; then
    delete_ghcr_image_tag "${IMAGE_REPOSITORY}" "${IMAGE_TAG}"
  fi
  rm -rf "${WORK_DIR}"
  complete_pr_check_run "${exit_code}"
}
trap cleanup EXIT INT TERM

setup_gdc_credentials "${SA_FILE}" "${CA_FILE}" "${ORG}" "${ZONE}" "${LAB_URL}" "${GDC_CCM_SERVICE_ACCOUNT_KEY:-}"
unset GDC_CCM_SERVICE_ACCOUNT_KEY
install_gdcloud_cli "${SA_FILE}" "${CA_FILE}" "${MGMT_URL}" "${GDCLOUD_VERSION}" "./integration/cmd/token-helper"

echo "Pulling vcluster Helm chart (version: ${VCLUSTER_VERSION})..."
retry_cmd helm pull vcluster --repo https://charts.loft.sh --version "${VCLUSTER_VERSION}" --destination "${WORK_DIR}"
VCLUSTER_CHART_PACKAGE="${WORK_DIR}/vcluster-${VCLUSTER_VERSION}.tgz"

echo "Building and pushing CCM image ${IMAGE_WITHTAG}..."
retry_cmd make docker-images IMAGE_REPOSITORY="${IMAGE_REPOSITORY}" IMAGE_TAG="${IMAGE_TAG}"
retry_cmd docker push "${IMAGE_WITHTAG}"
IMAGE_PUSHED=true

echo "Running CCM presubmit integration test..."
CGO_ENABLED=0 go test -v -timeout=25m ./integration/presubmit/cloud-controller-manager \
  -args \
  --commit_hash="${COMMIT_HASH}" \
  --zone="${ZONE}" \
  --region="${REGION}" \
  --project="${GDCH_PROJECT}" \
  --vuc="${VUC}" \
  --org="${ORG}" \
  --lab_url="${LAB_URL}" \
  --cafile="${CA_FILE}" \
  --service_account="${SA_FILE}" \
  --ccm_image_tag="${IMAGE_WITHTAG}" \
  --vcluster_chart="${VCLUSTER_CHART_PACKAGE}" \
  --vcluster_k8s_tag="${VCLUSTER_K8S_TAG}"
