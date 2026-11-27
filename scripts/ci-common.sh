#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Google LLC
#
# SPDX-License-Identifier: Apache-2.0

# retry_cmd retries a command up to 3 times with exponential backoff.
retry_cmd() {
  local max_attempts=3
  local attempt=1
  local delay=5
  while true; do
    if "$@"; then
      return 0
    fi
    if (( attempt >= max_attempts )); then
      echo "::error::Command failed after ${max_attempts} attempts: $*" >&2
      return 1
    fi
    echo "Command failed (attempt ${attempt}/${max_attempts}): $*. Retrying in ${delay}s..." >&2
    sleep "${delay}"
    attempt=$(( attempt + 1 ))
    delay=$(( delay * 2 ))
  done
}

# CI_TRUSTED_REF is the ref that every trust decision below reads from. It is
# spelled out in full so that a branch or tag named "origin/main" cannot shadow
# the remote-tracking ref of the main branch.
CI_TRUSTED_REF="refs/remotes/origin/main"

# authorized_logins prints the GitHub logins (lower case, one per line) listed
# in OWNERS_ALIASES and CODEOWNERS on origin/main. It fails if origin/main
# cannot be resolved or if neither file can be read from it. Files in the
# checked-out tree are never consulted because a pull request controls them.
authorized_logins() {
  if ! git rev-parse --verify --quiet "${CI_TRUSTED_REF}^{commit}" >/dev/null 2>&1; then
    echo "Cannot resolve origin/main; nobody is authorized." >&2
    return 1
  fi

  local content found=false

  # OWNERS_ALIASES: plain YAML list items ("- login"). Bot entries match the
  # exact bot login.
  if content=$(git show "${CI_TRUSTED_REF}:OWNERS_ALIASES" 2>/dev/null); then
    found=true
    printf '%s\n' "${content}" | tr -d '\r' | LC_ALL=C awk '
      { sub(/#.*/, "") }
      NF == 2 && $1 == "-" && $2 ~ /^[A-Za-z0-9_-]+(\[bot\])?$/ { print tolower($2) }'
  fi

  # CODEOWNERS: "@login" owners only. The path pattern, team references
  # ("@org/team"), e-mail addresses and comments never yield a login.
  if content=$(git show "${CI_TRUSTED_REF}:CODEOWNERS" 2>/dev/null); then
    found=true
    printf '%s\n' "${content}" | tr -d '\r' | LC_ALL=C awk '
      { sub(/#.*/, "") }
      { for (i = 2; i <= NF; i++) if ($i ~ /^@[A-Za-z0-9_-]+$/) print tolower(substr($i, 2)) }'
  fi

  if [[ "${found}" != "true" ]]; then
    echo "Neither OWNERS_ALIASES nor CODEOWNERS can be read from origin/main; nobody is authorized." >&2
    return 1
  fi
}

# is_authorized_maintainer succeeds only if the given GitHub login equals,
# case-insensitively, a login listed in OWNERS_ALIASES or CODEOWNERS on
# origin/main. Repository or organization membership (author_association) does
# not authorize anything, and bot entries match the exact bot login.
is_authorized_maintainer() {
  local actor="${1:-}"

  # Empty and malformed logins are denied before any comparison.
  if [[ ! "${actor}" =~ ^[A-Za-z0-9_-]+(\[bot\])?$ ]]; then
    return 1
  fi

  local actor_lc owners_list login
  actor_lc=$(printf '%s' "${actor}" | tr '[:upper:]' '[:lower:]')
  if ! owners_list=$(authorized_logins); then
    return 1
  fi

  while IFS= read -r login; do
    if [[ -n "${login}" && "${login}" == "${actor_lc}" ]]; then
      return 0
    fi
  done <<< "${owners_list}"

  return 1
}

# prepare_trusted_ci_files stages the CI runner scripts and builds the
# token-helper binary in an isolated directory (/tmp/ci-trusted) outside the
# repository working tree before any untrusted PR ref is checked out. Everything
# is taken from origin/main. There is no fallback to the checked-out tree, which
# a pull request controls: if anything is missing, the run fails.
prepare_trusted_ci_files() {
  CI_TRUSTED_DIR="${1:-/tmp/ci-trusted}"
  rm -rf "${CI_TRUSTED_DIR}"
  mkdir -p "${CI_TRUSTED_DIR}/scripts" "${CI_TRUSTED_DIR}/bin" "${CI_TRUSTED_DIR}/src"

  if ! git rev-parse --verify --quiet "${CI_TRUSTED_REF}^{commit}" >/dev/null 2>&1; then
    echo "::error::Cannot resolve origin/main; refusing to stage CI scripts from the checked-out tree."
    exit 1
  fi

  echo "Staging trusted CI scripts and token-helper from origin/main..."
  local script
  for script in ci-common.sh ci-integration-test.sh; do
    if ! git show "${CI_TRUSTED_REF}:scripts/${script}" > "${CI_TRUSTED_DIR}/scripts/${script}" ||
       [[ ! -s "${CI_TRUSTED_DIR}/scripts/${script}" ]]; then
      echo "::error::Cannot read scripts/${script} from origin/main."
      exit 1
    fi
  done

  if ! git archive --format=tar --output="${CI_TRUSTED_DIR}/src.tar" "${CI_TRUSTED_REF}" ||
     ! tar -xf "${CI_TRUSTED_DIR}/src.tar" -C "${CI_TRUSTED_DIR}/src" ||
     ! (cd "${CI_TRUSTED_DIR}/src" && go build -o "${CI_TRUSTED_DIR}/bin/token-helper" ./integration/cmd/token-helper) ||
     [[ ! -x "${CI_TRUSTED_DIR}/bin/token-helper" ]]; then
    echo "::error::Cannot build the token-helper from origin/main."
    exit 1
  fi
  rm -rf "${CI_TRUSTED_DIR}/src" "${CI_TRUSTED_DIR}/src.tar"

  chmod +x "${CI_TRUSTED_DIR}/scripts/ci-common.sh" "${CI_TRUSTED_DIR}/scripts/ci-integration-test.sh"
  export CI_TRUSTED_DIR
}

# normalize_commit_sha prints the lower-case form of a full 40-character
# hexadecimal commit SHA and fails for anything else. Abbreviated SHAs are
# rejected: a short prefix does not identify one commit and can be collided on
# purpose.
normalize_commit_sha() {
  local sha="${1:-}"
  if [[ ! "${sha}" =~ ^[0-9A-Fa-f]{40}$ ]]; then
    return 1
  fi
  printf '%s\n' "${sha}" | tr 'A-F' 'a-f'
}

# find_test_integration_command prints the first '/test-integration' command
# line of a comment body, or nothing if the comment has none.
find_test_integration_command() {
  printf '%s\n' "${1:-}" | tr -d '\r' | grep -E '^[[:space:]]*/test-integration([[:space:]]|$)' | head -n 1 || true
}

# parse_test_integration_command parses a '/test-integration' command line of
# the form
#
#   /test-integration [<sha>]
#
# and prints <sha> in lower case (or an empty line when omitted). When <sha> is
# given, it must be the full 40-character SHA of the PR head commit; abbreviated
# SHAs or trailing arguments are rejected. Omitting <sha> is allowed only for
# same-repository PRs authored by an OWNERS login (enforced later in
# fetch_reviewed_pr_head).
parse_test_integration_command() {
  local -a words=()
  read -r -a words <<< "${1:-}"

  if [[ "${#words[@]}" -gt 2 ]]; then
    echo "::error::'/test-integration' accepts at most one argument (the full 40-character commit SHA). Required form: '/test-integration [sha]'." >&2
    return 1
  fi

  local sha=""
  if [[ "${#words[@]}" -eq 2 ]]; then
    if ! sha=$(normalize_commit_sha "${words[1]}"); then
      echo "::error::'/test-integration' argument '${words[1]}' must be the full 40-character commit SHA. Required form: '/test-integration <sha>' (or '/test-integration' on same-repository OWNERS PRs)." >&2
      return 1
    fi
  fi

  printf '%s\n' "${sha}"
}

# print_manual_trigger_hint explains how a maintainer starts the test by hand
# for a fork or non-OWNERS PR, printing the ready-to-copy command for the
# current head commit when known.
print_manual_trigger_hint() {
  local pr_number="${1:-}"
  local pr_head_sha="${2:-}"
  local sha_hint="<sha>" normalized
  if normalized=$(normalize_commit_sha "${pr_head_sha}"); then
    sha_hint="${normalized}"
  fi
  echo "A maintainer listed in OWNERS_ALIASES or CODEOWNERS on main must review commit ${sha_hint} and trigger this test: comment '/test-integration ${sha_hint}' on the PR, or use workflow_dispatch with pr_number=${pr_number} and head_sha=${sha_hint}."
}

# fetch_reviewed_pr_head fetches the head of pull request PR_NUMBER and verifies
# it matches the commit to test. For fork PRs and PRs authored by users not
# listed in OWNERS_ALIASES/CODEOWNERS on main, EXPECTED_HEAD_SHA is required so
# the run is bound to the exact commit a maintainer reviewed (even if the PR
# head moves while waiting for environment approval). For same-repository PRs
# authored by a listed OWNERS login (which already run automatically on every
# push), omitting EXPECTED_HEAD_SHA tests the current PR head. Every failure
# aborts; nothing from the pull request is checked out here. On success
# PR_HEAD_SHA and COMMIT_HASH are exported.
fetch_reviewed_pr_head() {
  if [[ ! "${PR_NUMBER:-}" =~ ^[0-9]+$ ]]; then
    echo "::error::The pull request number must consist of digits only."
    exit 1
  fi

  local expected_sha=""
  if [[ -n "${EXPECTED_HEAD_SHA:-}" ]]; then
    if ! expected_sha=$(normalize_commit_sha "${EXPECTED_HEAD_SHA}"); then
      echo "::error::Invalid commit SHA '${EXPECTED_HEAD_SHA}' for PR #${PR_NUMBER}. When naming a commit, use the full 40-character SHA."
      exit 1
    fi
  fi

  if [[ -z "${GITHUB_REPOSITORY:-}" ]]; then
    echo "::error::GITHUB_REPOSITORY is not set; cannot read PR #${PR_NUMBER}."
    exit 1
  fi

  local pr_json pr_state pr_base_ref pr_head_sha pr_head_repo pr_user
  if ! pr_json=$(gh api "repos/${GITHUB_REPOSITORY}/pulls/${PR_NUMBER}"); then
    echo "::error::Failed to read PR #${PR_NUMBER} from the GitHub API; refusing to test it."
    exit 1
  fi
  if ! pr_state=$(jq -r '.state // empty' <<< "${pr_json}") ||
     ! pr_base_ref=$(jq -r '.base.ref // empty' <<< "${pr_json}") ||
     ! pr_head_sha=$(jq -r '.head.sha // empty' <<< "${pr_json}") ||
     ! pr_head_repo=$(jq -r '.head.repo.full_name // empty' <<< "${pr_json}") ||
     ! pr_user=$(jq -r '.user.login // empty' <<< "${pr_json}"); then
    echo "::error::Unexpected GitHub API response for PR #${PR_NUMBER}; refusing to test it."
    exit 1
  fi
  if [[ "${pr_state}" != "open" ]]; then
    echo "::error::PR #${PR_NUMBER} is not open (state: '${pr_state}'); refusing to test it."
    exit 1
  fi
  if [[ "${pr_base_ref}" != "main" ]]; then
    echo "::error::PR #${PR_NUMBER} does not target main (base: '${pr_base_ref}'); refusing to test it."
    exit 1
  fi
  if ! pr_head_sha=$(normalize_commit_sha "${pr_head_sha}"); then
    echo "::error::Unexpected GitHub API response for PR #${PR_NUMBER} (missing or invalid head.sha); refusing to test it."
    exit 1
  fi

  if [[ -z "${expected_sha}" ]]; then
    if [[ "${pr_head_repo}" == "${GITHUB_REPOSITORY}" ]] && is_authorized_maintainer "${pr_user}"; then
      expected_sha="${pr_head_sha}"
      echo "PR #${PR_NUMBER} is a same-repository PR by authorized maintainer '${pr_user}'; using current head commit ${expected_sha}."
    else
      echo "::error::PR #${PR_NUMBER} is from a fork (${pr_head_repo:-unknown}) or an author ('${pr_user:-unknown}') not listed in OWNERS_ALIASES/CODEOWNERS on main, so a manual run must name the reviewed head commit. After reviewing commit ${pr_head_sha}, comment '/test-integration ${pr_head_sha}' on the PR, or set head_sha=${pr_head_sha} with pr_number=${PR_NUMBER} in workflow_dispatch."
      exit 1
    fi
  fi

  echo "Fetching PR #${PR_NUMBER}..."
  local pr_ref="refs/remotes/pull/${PR_NUMBER}/head" fetched_sha
  if ! git fetch --no-tags origin "+refs/pull/${PR_NUMBER}/head:${pr_ref}"; then
    echo "::error::Failed to fetch the head of PR #${PR_NUMBER}."
    exit 1
  fi
  if ! fetched_sha=$(git rev-parse --verify --quiet "${pr_ref}^{commit}"); then
    echo "::error::Failed to resolve the fetched head of PR #${PR_NUMBER}."
    exit 1
  fi
  if [[ "${fetched_sha}" != "${expected_sha}" ]]; then
    echo "::error::PR #${PR_NUMBER} head changed: it is ${fetched_sha}, but this run was requested for ${expected_sha}. Nothing from the PR was checked out. After reviewing commit ${fetched_sha}, trigger the test again with '/test-integration ${fetched_sha}' (or head_sha=${fetched_sha})."
    exit 1
  fi

  PR_HEAD_SHA="${fetched_sha}"
  export PR_HEAD_SHA
  COMMIT_HASH="${PR_HEAD_SHA:0:7}"
  export COMMIT_HASH
  echo "Verified that the head of PR #${PR_NUMBER} is the reviewed commit ${PR_HEAD_SHA}."
}

# checkout_reviewed_pr_head checks out exactly the commit that
# fetch_reviewed_pr_head verified. The pull request is not fetched again.
checkout_reviewed_pr_head() {
  if ! git checkout -B "pr-${PR_NUMBER}" "${PR_HEAD_SHA}" ||
     [[ "$(git rev-parse HEAD)" != "${PR_HEAD_SHA}" ]]; then
    echo "::error::Failed to check out the reviewed commit ${PR_HEAD_SHA} of PR #${PR_NUMBER}."
    exit 1
  fi
}

# should_skip_non_code_changes returns 0 (true) on pull_request events when:
# 1. All files modified in the PR relative to origin/main are non-code files
#    (e.g. README.md, docs, OWNERS, Makefile, VERSION, etc.), OR
# 2. The PR branch already has a previous commit where the 'Run CCM Integration Test'
#    step succeeded, and only non-runtime files changed between that tested commit
#    and the current PR head (ignoring upstream origin/main changes brought in by
#    a rebase or merge).
should_skip_non_code_changes() {
  if [[ "${GITHUB_EVENT_NAME:-}" != "pull_request" ]]; then
    return 1
  fi

  local non_code_regex='^(\.gitignore|\.dockerignore|\.golangci\.yaml|OWNERS.*|CODEOWNERS|LICENSE.*|NOTICE.*|VERSION|Makefile|.*\.md|docs/.*|\.github/.*|scripts/ci-common(_test)?\.sh)$'
  local pr_head_sha=""
  if [[ -f "${GITHUB_EVENT_PATH:-}" ]]; then
    pr_head_sha=$(jq -r '.pull_request.head.sha // empty' "${GITHUB_EVENT_PATH}")
  fi
  if [[ -z "${pr_head_sha}" ]]; then
    pr_head_sha=$(git rev-parse HEAD)
  fi

  # 1. Check if the entire PR diff against origin/main only touches non-code files
  #    (excluding runtime code, Dockerfile, and ci-integration-test.sh).
  local pr_files
  pr_files=$(git diff --name-only "origin/main...${pr_head_sha}" 2>/dev/null || true)
  if [[ -n "${pr_files}" ]]; then
    if ! printf '%s\n' "${pr_files}" | grep -qvE "${non_code_regex}"; then
      echo "All files changed in PR (${pr_files//$'\n'/, }) are non-code files. Skipping integration test."
      return 0
    fi
  fi

  # 2. Check if an earlier commit on this PR branch already passed the
  #    integration test step and only non-code files changed since that commit.
  local head_ref="${GITHUB_HEAD_REF:-}"
  if [[ -z "${head_ref}" && -f "${GITHUB_EVENT_PATH:-}" ]]; then
    head_ref=$(jq -r '.pull_request.head.ref // empty' "${GITHUB_EVENT_PATH}")
  fi
  if [[ -z "${head_ref}" || -z "${GH_TOKEN:-}" || -z "${GITHUB_REPOSITORY:-}" ]]; then
    return 1
  fi

  local candidate_runs
  candidate_runs=$(gh api "repos/${GITHUB_REPOSITORY}/actions/workflows/integration-test.yaml/runs?branch=${head_ref}&status=completed&per_page=10" \
    --jq '.workflow_runs[] | select(.conclusion == "success") | "\(.id) \(.head_sha)"' 2>/dev/null || true)
  if [[ -z "${candidate_runs}" ]]; then
    return 1
  fi

  local run_id prev_sha tested_sha=""
  while read -r run_id prev_sha; do
    [[ -z "${run_id}" || -z "${prev_sha}" || "${prev_sha}" == "${pr_head_sha}" ]] && continue
    local step_ok
    step_ok=$(gh api "repos/${GITHUB_REPOSITORY}/actions/runs/${run_id}/jobs" \
      --jq '.jobs[] | select(.name == "CCM Integration Test (GDC Staging)") | .steps[] | select(.name == "Run CCM Integration Test" and .conclusion == "success") | .conclusion' 2>/dev/null | head -n 1 || true)
    if [[ "${step_ok}" == "success" ]]; then
      tested_sha="${prev_sha}"
      break
    fi
  done <<< "${candidate_runs}"

  if [[ -z "${tested_sha}" ]]; then
    return 1
  fi

  if ! git cat-file -e "${tested_sha}^{commit}" 2>/dev/null; then
    git fetch --depth=50 origin "${tested_sha}" >/dev/null 2>&1 || return 1
  fi

  local prev_base curr_base changed_files
  prev_base=$(git merge-base origin/main "${tested_sha}" 2>/dev/null || echo "origin/main")
  curr_base=$(git merge-base origin/main "${pr_head_sha}" 2>/dev/null || echo "origin/main")
  changed_files=$(git diff --name-only "${tested_sha}" "${pr_head_sha}" 2>/dev/null || return 1)

  local file effective_changes=()
  while IFS= read -r file; do
    [[ -z "${file}" ]] && continue
    if [[ "$(git diff "${prev_base}" "${tested_sha}" -- "${file}" 2>/dev/null | git patch-id --stable | awk '{print $1}')" == \
          "$(git diff "${curr_base}" "${pr_head_sha}" -- "${file}" 2>/dev/null | git patch-id --stable | awk '{print $1}')" ]]; then
      continue
    fi
    effective_changes+=("${file}")
  done <<< "${changed_files}"

  if [[ "${#effective_changes[@]}" -eq 0 ]]; then
    echo "Integration test already passed on ${tested_sha:0:7} and no PR files changed since then (rebase only). Skipping integration test rerun."
    return 0
  fi

  for file in "${effective_changes[@]}"; do
    if ! [[ "${file}" =~ ${non_code_regex} ]]; then
      echo "Code file '${file}' changed since last passing commit ${tested_sha:0:7}; integration test will run."
      return 1
    fi
  done

  echo "Integration test already passed on ${tested_sha:0:7} and only non-code files changed since then (${effective_changes[*]}). Skipping integration test rerun."
  return 0
}

# check_ci_preconditions verifies whether the integration test should run when
# triggered by GitHub Actions, and checks out the reviewed head commit of the
# target PR on manual dispatch or '/test-integration <sha>' PR comment.
check_ci_preconditions() {
  if [[ "${GITHUB_ACTIONS:-}" != "true" ]]; then
    return 0
  fi

  if [[ "${CI_STAGE_PRECONDITIONS:-}" != "true" && -z "${CI_LOG_FILE:-}" ]]; then
    CI_LOG_FILE="$(mktemp /tmp/ci-integration-XXXXXX.log)"
    export CI_LOG_FILE
    exec > >(tee -a "${CI_LOG_FILE}") 2>&1
    CI_TEE_PID=$!
    export CI_TEE_PID
  fi

  if [[ "${CI_PRECONDITIONS_VERIFIED:-}" == "true" ]]; then
    return 0
  fi

  # pull_request and issue_comment runs are authorized from the event payload;
  # without it nothing can be authorized.
  if [[ "${GITHUB_EVENT_NAME:-}" == "pull_request" || "${GITHUB_EVENT_NAME:-}" == "issue_comment" ]] &&
     [[ ! -f "${GITHUB_EVENT_PATH:-}" ]]; then
    echo "::error::The ${GITHUB_EVENT_NAME} event payload is missing; cannot authorize this run."
    exit 1
  fi

  if [[ "${GITHUB_EVENT_NAME:-}" == "pull_request" && -f "${GITHUB_EVENT_PATH:-}" ]]; then
    local head_repo author_association pr_user pr_number pr_head_sha
    head_repo=$(jq -r '.pull_request.head.repo.full_name // empty' "${GITHUB_EVENT_PATH}")
    author_association=$(jq -r '.pull_request.author_association // empty' "${GITHUB_EVENT_PATH}")
    pr_user=$(jq -r '.pull_request.user.login // empty' "${GITHUB_EVENT_PATH}")
    pr_number=$(jq -r '.pull_request.number // empty' "${GITHUB_EVENT_PATH}")
    pr_head_sha=$(jq -r '.pull_request.head.sha // empty' "${GITHUB_EVENT_PATH}")

    if [[ "${head_repo}" != "${GITHUB_REPOSITORY:-}" ]]; then
      echo "Skipping automatic integration test for forked PR (${head_repo})."
      print_manual_trigger_hint "${pr_number}" "${pr_head_sha}"
      exit 1
    fi

    if ! is_authorized_maintainer "${pr_user}"; then
      echo "Skipping automatic integration test: PR author '${pr_user}' (author_association='${author_association}') is not listed in OWNERS_ALIASES or CODEOWNERS on main. Repository or organization membership does not start this test."
      print_manual_trigger_hint "${pr_number}" "${pr_head_sha}"
      exit 1
    fi

    if should_skip_non_code_changes; then
      if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
        echo "should_run=false" >> "${GITHUB_OUTPUT}"
      fi
      exit 0
    fi
  fi

  local comment_id=""

  if [[ "${GITHUB_EVENT_NAME:-}" == "issue_comment" && -f "${GITHUB_EVENT_PATH:-}" ]]; then
    local is_pr comment_body comment_user comment_assoc cmd_line
    is_pr=$(jq -r '.issue.pull_request.url // empty' "${GITHUB_EVENT_PATH}")
    comment_body=$(jq -r '.comment.body // empty' "${GITHUB_EVENT_PATH}")
    comment_user=$(jq -r '.comment.user.login // empty' "${GITHUB_EVENT_PATH}")
    comment_assoc=$(jq -r '.comment.author_association // empty' "${GITHUB_EVENT_PATH}")
    comment_id=$(jq -r '.comment.id // empty' "${GITHUB_EVENT_PATH}")
    PR_NUMBER=$(jq -r '.issue.number // empty' "${GITHUB_EVENT_PATH}")
    export PR_NUMBER

    cmd_line=$(find_test_integration_command "${comment_body}")
    if [[ -z "${is_pr}" || -z "${cmd_line}" ]]; then
      echo "Comment does not contain a '/test-integration' command line on a pull request. Skipping."
      if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
        echo "should_run=false" >> "${GITHUB_OUTPUT}"
      fi
      exit 0
    fi

    if ! is_authorized_maintainer "${comment_user}"; then
      echo "::error::User '${comment_user}' (author_association='${comment_assoc}') is not listed in OWNERS_ALIASES or CODEOWNERS on main and cannot trigger '/test-integration'."
      exit 1
    fi

    # The SHA (if provided) always comes from the comment, never from the environment.
    if ! EXPECTED_HEAD_SHA=$(parse_test_integration_command "${cmd_line}"); then
      exit 1
    fi
    export EXPECTED_HEAD_SHA
  fi

  if [[ "${GITHUB_EVENT_NAME:-}" == "workflow_dispatch" ]]; then
    if ! is_authorized_maintainer "${GITHUB_ACTOR:-}"; then
      echo "::error::User '${GITHUB_ACTOR:-}' is not listed in OWNERS_ALIASES or CODEOWNERS on main and cannot trigger workflow_dispatch."
      exit 1
    fi
    if [[ -z "${PR_NUMBER:-}" && -n "${EXPECTED_HEAD_SHA:-}" ]]; then
      echo "::error::head_sha was given without pr_number. Set pr_number to test a pull request, or leave both empty to test the selected branch."
      exit 1
    fi
  fi

  # Stage trusted CI scripts and compile token-helper before checking out any PR branch.
  prepare_trusted_ci_files

  # When triggered manually via '/test-integration' comment or
  # workflow_dispatch with pr_number, fetch the PR head, verify it matches the
  # reviewed commit (required for fork and non-OWNERS PRs), check out that
  # commit, merge the current base branch, and attach a check-run to the PR's
  # head SHA.
  if [[ "${GITHUB_EVENT_NAME:-}" == "issue_comment" ]] ||
     [[ "${GITHUB_EVENT_NAME:-}" == "workflow_dispatch" && -n "${PR_NUMBER:-}" ]]; then
    local base_branch_sha
    if ! base_branch_sha="$(git rev-parse --verify --quiet "${CI_TRUSTED_REF}^{commit}")"; then
      echo "::error::Cannot resolve origin/main; refusing to test PR #${PR_NUMBER:-}."
      exit 1
    fi
    fetch_reviewed_pr_head
    if [[ -n "${comment_id}" && -n "${GH_TOKEN:-}" && -n "${GITHUB_REPOSITORY:-}" ]]; then
      gh api --method POST "repos/${GITHUB_REPOSITORY}/issues/comments/${comment_id}/reactions" \
        -f content='rocket' >/dev/null 2>&1 || true
    fi
    start_pr_check_run "${CHECK_NAME:-CCM Integration Test (GDC Staging)}"
    checkout_reviewed_pr_head
    echo "Merging base branch (${base_branch_sha:0:7}) into PR #${PR_NUMBER} (${COMMIT_HASH})..."
    if ! git -c user.name="github-actions[bot]" -c user.email="github-actions[bot]@users.noreply.github.com" \
      merge --no-edit "${base_branch_sha}"; then
      echo "::error::Failed to merge base branch (${base_branch_sha:0:7}) into PR #${PR_NUMBER} (${COMMIT_HASH}). Please rebase the PR onto main."
      complete_pr_check_run 1
      exit 1
    fi
  fi
}

# verify_and_prepare_ci runs in the dedicated secret-less precondition step in
# GitHub Actions and exports verified state to GITHUB_ENV and GITHUB_OUTPUT.
verify_and_prepare_ci() {
  CI_STAGE_PRECONDITIONS=true check_ci_preconditions

  if [[ -n "${GITHUB_ENV:-}" ]]; then
    {
      echo "CI_PRECONDITIONS_VERIFIED=true"
      echo "CI_TRUSTED_DIR=${CI_TRUSTED_DIR}"
      [[ -n "${PR_NUMBER:-}" ]] && echo "PR_NUMBER=${PR_NUMBER}"
      [[ -n "${PR_HEAD_SHA:-}" ]] && echo "PR_HEAD_SHA=${PR_HEAD_SHA}"
      [[ -n "${PR_CHECK_RUN_ID:-}" ]] && echo "PR_CHECK_RUN_ID=${PR_CHECK_RUN_ID}"
      [[ -n "${COMMIT_HASH:-}" ]] && echo "COMMIT_HASH=${COMMIT_HASH}"
    } >> "${GITHUB_ENV}"
  fi

  if [[ -n "${GITHUB_OUTPUT:-}" ]]; then
    echo "should_run=true" >> "${GITHUB_OUTPUT}"
  fi
}

# start_pr_check_run creates an in_progress GitHub Check Run on PR_HEAD_SHA when
# triggered via '/test-integration' or workflow_dispatch so the manual run
# satisfies required status checks on the PR.
start_pr_check_run() {
  local check_name="$1"
  if [[ -z "${PR_HEAD_SHA:-}" || -z "${GH_TOKEN:-}" || -z "${GITHUB_REPOSITORY:-}" ]]; then
    return 0
  fi

  local run_url="${GITHUB_SERVER_URL:-https://github.com}/${GITHUB_REPOSITORY}/actions/runs/${GITHUB_RUN_ID:-}"
  echo "Creating in_progress check-run '${check_name}' on PR #${PR_NUMBER} commit ${PR_HEAD_SHA}..."
  PR_CHECK_RUN_ID=$(gh api --method POST "repos/${GITHUB_REPOSITORY}/check-runs" \
    -f name="${check_name}" \
    -f head_sha="${PR_HEAD_SHA}" \
    -f status="in_progress" \
    -f details_url="${run_url}" \
    -f "output[title]=Running via ${GITHUB_EVENT_NAME:-manual trigger}" \
    -f "output[summary]=Triggered by @${GITHUB_ACTOR:-maintainer} (${GITHUB_EVENT_NAME:-manual}) for PR #${PR_NUMBER} (${run_url})." \
    --jq '.id' 2>/dev/null || true)
  export PR_CHECK_RUN_ID
}

# complete_pr_check_run emits a GitHub Actions failure annotation (if non-zero)
# and updates the manual check-run on PR_HEAD_SHA with the final conclusion.
complete_pr_check_run() {
  local exit_code="$1"
  if [[ -n "${PR_CHECK_RUN_ID:-}" && -n "${GH_TOKEN:-}" && -n "${GITHUB_REPOSITORY:-}" ]]; then
    local conclusion="failure"
    local title="Integration test failed"
    if [[ "${exit_code}" -eq 0 ]]; then
      conclusion="success"
      title="Integration test passed"
    fi

    local run_url="${GITHUB_SERVER_URL:-https://github.com}/${GITHUB_REPOSITORY}/actions/runs/${GITHUB_RUN_ID:-}"
    echo "Updating check-run ${PR_CHECK_RUN_ID} on PR #${PR_NUMBER} (${PR_HEAD_SHA}) with conclusion=${conclusion}..."
    gh api --method PATCH "repos/${GITHUB_REPOSITORY}/check-runs/${PR_CHECK_RUN_ID}" \
      -f status="completed" \
      -f conclusion="${conclusion}" \
      -f details_url="${run_url}" \
      -f "output[title]=${title}" \
      -f "output[summary]=Manual ${GITHUB_EVENT_NAME:-workflow_dispatch} run completed with ${conclusion} (${run_url})." >/dev/null 2>&1 || \
      echo "Warning: Failed to update check-run ${PR_CHECK_RUN_ID} on PR #${PR_NUMBER}."
  fi

  if [[ "${GITHUB_ACTIONS:-}" == "true" && -n "${CI_LOG_FILE:-}" ]]; then
    sleep 1
    if [[ "${exit_code}" -ne 0 && -s "${CI_LOG_FILE}" ]]; then
      local err_lines tail_log combined_log
      err_lines=$(grep -v '^::add-mask::' "${CI_LOG_FILE}" | grep -E '(--- FAIL:|FAIL[[:space:]]|Error:|fatalf|Fatalf|panic:|timed out|Back-off|ErrImagePull)' | tail -n 20 || true)
      tail_log=$(grep -v '^::add-mask::' "${CI_LOG_FILE}" | tail -n 35 || true)
      if [[ -n "${err_lines}" ]]; then
        combined_log=$(printf '=== Failure Summary ===\n%s\n=== Log Tail ===\n%s' "${err_lines}" "${tail_log}" | sed ':a;N;$!ba;s/%/%25/g;s/\r/%0D/g;s/\n/%0A/g')
      else
        combined_log=$(printf '%s' "${tail_log}" | sed ':a;N;$!ba;s/%/%25/g;s/\r/%0D/g;s/\n/%0A/g')
      fi
      echo "::error title=${CHECK_NAME:-Integration Test} Failed::${combined_log}"
    fi
    sleep 1
    rm -f "${CI_LOG_FILE}"
    exec >/dev/null 2>&1 || true
    if [[ -n "${CI_TEE_PID:-}" ]]; then
      pkill -P "${CI_TEE_PID}" 2>/dev/null || true
      kill "${CI_TEE_PID}" 2>/dev/null || true
    fi
  fi
}

# setup_gdc_credentials writes the GDC service account key to disk (if provided
# via environment variable), masks all lines in GitHub Actions logs, and
# downloads the GDC Root CA certificate over verified TLS.
setup_gdc_credentials() {
  local xtrace_was_set=false
  if [[ "$-" == *x* ]]; then
    xtrace_was_set=true
    set +x
  fi

  local sa_file="$1"
  local ca_file="$2"
  local org="$3"
  local zone="$4"
  local lab_url="$5"
  local sa_key="${6:-}"

  if [[ -n "${sa_key}" ]]; then
    if [[ "${GITHUB_ACTIONS:-}" == "true" ]]; then
      while IFS= read -r line; do
        line="${line#"${line%%[![:space:]]*}"}"
        line="${line%"${line##*[![:space:]]}"}"
        if [[ -n "${line}" && "${line}" != "{" && "${line}" != "}" ]]; then
          echo "::add-mask::${line}"
        fi
      done <<< "${sa_key}"
    fi

    local normalized_sa=""
    if normalized_sa=$(printf '%s' "${sa_key}" | jq . 2>/dev/null) && [[ -n "${normalized_sa}" ]]; then
      sa_key="${normalized_sa}"
    elif normalized_sa=$(printf '%s' "${sa_key}" | sed 's/^[[:space:]]*//; s/[[:space:]]*$//' | tr -d '\r\n' | sed 's/^[^{]*//; s/[^}]*$//; s/BEGINECPRIVATEKEY/BEGIN EC PRIVATE KEY/g; s/ENDECPRIVATEKEY/END EC PRIVATE KEY/g; s/BEGINPRIVATEKEY/BEGIN PRIVATE KEY/g; s/ENDPRIVATEKEY/END PRIVATE KEY/g' | jq . 2>/dev/null) && [[ -n "${normalized_sa}" ]]; then
      sa_key="${normalized_sa}"
    fi

    if [[ "${GITHUB_ACTIONS:-}" == "true" ]]; then
      while IFS= read -r line; do
        line="${line#"${line%%[![:space:]]*}"}"
        line="${line%"${line##*[![:space:]]}"}"
        if [[ -n "${line}" && "${line}" != "{" && "${line}" != "}" ]]; then
          echo "::add-mask::${line}"
        fi
      done <<< "${sa_key}"
    fi

    (
      umask 077
      printf '%s\n' "${sa_key}" > "${sa_file}"
    )
  fi

  if [[ "${xtrace_was_set}" == "true" ]]; then
    set -x
  fi

  if [[ ! -s "${sa_file}" ]]; then
    echo "::error::GDC Service Account key is required (file '${sa_file}' is missing or empty). Verify that GDC_CCM_SERVICE_ACCOUNT_KEY is configured in the gdc-staging environment secrets."
    exit 1
  fi

  # The console certificate is verified against the system trust store. Labs
  # whose console certificate is issued by a private CA provide that CA as PEM
  # text in GDC_BOOTSTRAP_CA_PEM.
  local bootstrap_ca_file=""
  if [[ -n "${GDC_BOOTSTRAP_CA_PEM:-}" ]]; then
    bootstrap_ca_file="$(mktemp)"
    printf '%s\n' "${GDC_BOOTSTRAP_CA_PEM}" > "${bootstrap_ca_file}"
  fi

  echo "Fetching GDC Root CA from console.${org}.${zone}.${lab_url}..."
  # Retry Root CA download with a short timeout in case an upstream ingress backend drops the connection.
  if ! retry_cmd wget "https://console.${org}.${zone}.${lab_url}/.well-known/certificate-authority" \
    ${bootstrap_ca_file:+"--ca-certificate=${bootstrap_ca_file}"} --timeout=10 --tries=2 -q -O "${ca_file}"; then
    rm -f "${bootstrap_ca_file}"
    echo "::error::Failed to fetch GDC Root CA from console.${org}.${zone}.${lab_url}"
    exit 1
  fi
  rm -f "${bootstrap_ca_file}"
}

# install_gdcloud_cli downloads and installs the gdcloud CLI and
# gdcloud-k8s-auth-plugin from the GDC management API server's CLIBundleMetadata.
# Both requests verify the server certificate against the GDC trust bundle in
# ca_file (downloaded by setup_gdc_credentials).
install_gdcloud_cli() {
  local sa_file="$1"
  local ca_file="$2"
  local mgmt_url="$3"
  local gdcloud_version="$4"
  local token_helper_pkg="${5:-./integration/cmd/token-helper}"

  local gdcloud_root="/usr/local/bin/google-distributed-cloud-hosted-cli"
  if [[ -x "${gdcloud_root}/bin/gdcloud" && -x "${gdcloud_root}/bin/gdcloud-k8s-auth-plugin" ]]; then
    echo "gdcloud CLI already installed at ${gdcloud_root}/bin/gdcloud"
    export PATH="${gdcloud_root}/bin:${PATH}"
    export GDCLOUD_PATH="${gdcloud_root}/bin/gdcloud"
    return 0
  fi

  echo "Minting STS token using GDC ServiceAccount to query CLIBundleMetadata..."
  local sts_token
  local -a token_helper_cmd=("go" "run" "${token_helper_pkg}")
  if [[ -n "${CI_TRUSTED_DIR:-}" && -x "${CI_TRUSTED_DIR}/bin/token-helper" ]]; then
    token_helper_cmd=("${CI_TRUSTED_DIR}/bin/token-helper")
  elif [[ "${GITHUB_ACTIONS:-}" == "true" ]]; then
    # In CI the service account key is only handed to the token-helper built
    # from origin/main, never to one built from the checked-out tree.
    echo "::error::The token-helper staged from origin/main is missing; refusing to build it from the checked-out tree."
    exit 1
  elif [[ -x "${token_helper_pkg}" ]]; then
    token_helper_cmd=("${token_helper_pkg}")
  fi
  # Retry token-helper invocation across transient STS ingress resets.
  if ! sts_token=$(retry_cmd "${token_helper_cmd[@]}" \
    --service-account-file="${sa_file}" \
    --ca-cert-file="${ca_file}" \
    --audience="${mgmt_url}"); then
    echo "::error::Failed to mint STS token using token-helper against ${mgmt_url}"
    exit 1
  fi
  if [[ "${GITHUB_ACTIONS:-}" == "true" && -n "${sts_token}" ]]; then
    echo "::add-mask::${sts_token}"
  fi

  echo "Resolving gdcloud CLI bundle URL (version: ${gdcloud_version}) from ${mgmt_url}..."
  local auth_hdr_file clibundle_json serving_url
  auth_hdr_file="$(mktemp /tmp/gdc-auth-hdr-XXXXXX)"
  (
    umask 077
    printf 'Authorization: Bearer %s\n' "${sts_token}" > "${auth_hdr_file}"
  )
  # Capture the curl response separately with --fail before piping to jq so HTTP/TCP
  # failures trigger retry_cmd instead of being masked by jq's exit status.
  # Listing CLIBundleMetadata on GDC staging routinely takes ~45-50s, so allow 120s
  # for response transfer while keeping a fast 5s TCP connect timeout.
  if ! clibundle_json=$(retry_cmd curl --cacert "${ca_file}" --proto '=https' --proto-redir '=https' -sSL --fail --connect-timeout 5 --max-time 120 \
    -H "@${auth_hdr_file}" \
    "${mgmt_url}/apis/artifactview.private.gdc.goog/v1alpha1/namespaces/ui-system/clibundlemetadata"); then
    rm -f "${auth_hdr_file}"
    echo "::error::Failed to query CLIBundleMetadata from ${mgmt_url}"
    exit 1
  fi
  rm -f "${auth_hdr_file}"
  serving_url=$(printf '%s' "${clibundle_json}" | \
    jq -r --arg ver "${gdcloud_version}" '
      [.items[]
       | select(.platform.os == "linux" and .platform.architecture == "amd64")
       | select($ver == "" or (.commonMetadata.artifactVersion | contains($ver)))
      ]
      | sort_by(.metadata.creationTimestamp)
       | last
       | .commonMetadata.servingURL // empty
    ')

  if [[ -z "${serving_url}" ]]; then
    echo "::error::Failed to resolve gdcloud CLI servingURL for version '${gdcloud_version}'."
    exit 1
  fi

  local tarball="/tmp/gdcloud_cli_linux.tar.gz"
  echo "Downloading gdcloud CLI from ${serving_url}..."
  # Wrap tarball download in retry_cmd with a 5s connect timeout to recover from dead VIP backends.
  retry_cmd curl --cacert "${ca_file}" --proto '=https' --proto-redir '=https' -L --fail --connect-timeout 5 --retry 3 "${serving_url}?uncompressed=false" -o "${tarball}"

  local sudo_cmd=""
  if [[ "${EUID:-$(id -u)}" -ne 0 ]] && command -v sudo >/dev/null 2>&1; then
    sudo_cmd="sudo"
  fi

  echo "Extracting gdcloud CLI to /usr/local/bin and installing gdcloud-k8s-auth-plugin..."
  ${sudo_cmd} tar -xzf "${tarball}" -C /usr/local/bin/
  ${sudo_cmd} "${gdcloud_root}/bin/gdcloud" components install gdcloud-k8s-auth-plugin
  rm -f "${tarball}"

  export PATH="${gdcloud_root}/bin:${PATH}"
  export GDCLOUD_PATH="${gdcloud_root}/bin/gdcloud"
  "${GDCLOUD_PATH}" version
}

# delete_ghcr_image_tag removes the temporary PR container image version from
# ghcr.io via the GitHub Packages REST API after the test finishes.
delete_ghcr_image_tag() {
  local image_repo="$1"
  local image_tag="$2"

  if [[ "${GITHUB_ACTIONS:-}" != "true" || -z "${GH_TOKEN:-}" ]]; then
    return 0
  fi

  local owner="${GITHUB_REPOSITORY_OWNER:-}"
  local owner_lc
  owner_lc=$(printf '%s' "${owner}" | tr '[:upper:]' '[:lower:]')
  local pkg_name="${image_repo#ghcr.io/${owner_lc}/}"
  pkg_name="${pkg_name#ghcr.io/${owner}/}"
  local encoded_pkg="${pkg_name//\//%2F}"

  echo "Cleaning up temporary container image ${image_repo}:${image_tag} from ghcr.io..."
  local scope
  for scope in "orgs/${owner}" "users/${owner}" "orgs/${owner_lc}" "users/${owner_lc}"; do
    local version_id
    version_id=$(gh api "/${scope}/packages/container/${encoded_pkg}/versions" \
      --jq ".[] | select(.metadata.container.tags[]? == \"${image_tag}\") | .id" 2>/dev/null | head -n 1 || true)

    if [[ -n "${version_id}" ]]; then
      if ! gh api --method DELETE "/${scope}/packages/container/${encoded_pkg}/versions/${version_id}" >/dev/null 2>&1; then
        echo "Warning: Failed to delete ${image_repo}:${image_tag} (version ${version_id}) from ghcr.io."
      fi
      return 0
    fi
  done
}
