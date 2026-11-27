#!/usr/bin/env bash
# SPDX-FileCopyrightText: 2026 Google LLC
#
# SPDX-License-Identifier: Apache-2.0

# Tests for the trust boundary implemented in scripts/ci-common.sh: who may run
# the integration test, which commit a manual trigger tests, where the trusted
# scripts come from, and how the bootstrap downloads verify TLS.
#
# The integration test job loads its scripts from main, so a pull request that
# changes them does not exercise its own changes in that job. These tests do;
# they run with the regular CI checks (make test-ci-scripts).
#
# Requirements: bash, git, jq and standard POSIX tools. gh, go, wget and curl
# are replaced by stubs and every git operation runs in a throwaway repository
# under a temporary directory, so nothing talks to the network or touches the
# checkout this script lives in.
#
# The script stops at the first failed assertion and prints the failed case.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TEST_TMP="$(cd "$(mktemp -d)" && pwd -P)"
trap 'rm -rf "${TEST_TMP}"' EXIT

# The tests may themselves run inside GitHub Actions or from a git hook: drop
# everything that git and the functions under test read from the environment.
for var in $(compgen -v | grep -E '^(GIT_|GITHUB_|GH_|CI_|PR_|GDC_)' || true); do
  unset "${var}"
done
unset EXPECTED_HEAD_SHA COMMIT_HASH CHECK_NAME

export HOME="${TEST_TMP}/home"
export XDG_CONFIG_HOME="${HOME}/.config"
mkdir -p "${XDG_CONFIG_HOME}"
export GIT_CONFIG_NOSYSTEM=1
export GIT_TERMINAL_PROMPT=0
export GIT_AUTHOR_NAME="CI Script Test"
export GIT_AUTHOR_EMAIL="ci-script-test@example.invalid"
export GIT_COMMITTER_NAME="${GIT_AUTHOR_NAME}"
export GIT_COMMITTER_EMAIL="${GIT_AUTHOR_EMAIL}"

# A fake repository name and token: even if the real GitHub CLI were reached by
# mistake, it could not authenticate.
export GITHUB_REPOSITORY="example-org/example-repo"
export GH_TOKEN="not-a-real-token"

# shellcheck source=scripts/ci-common.sh
source "${SCRIPT_DIR}/ci-common.sh"

PR=7           # number of the fixture pull request
MODE=strict    # shell options the code under test runs with, see run_case
CASE=""
CASES=0
STATUS=0
OUTPUT=""
WORK=""        # throwaway checkout of the current case, see new_checkout

###############################################################################
# Test helpers
###############################################################################

begin() {
  CASE="$1"
}

pass() {
  CASES=$((CASES + 1))
  echo "ok - [${MODE}] ${CASE}"
}

fail() {
  {
    echo "FAIL - [${MODE}] ${CASE}"
    echo "  $*"
    echo "  exit status: ${STATUS}"
    echo "  --- output ---"
    echo "  ${OUTPUT//$'\n'/$'\n'  }"
  } >&2
  exit 1
}

# run_case <dir> <command...> runs the command inside <dir> in a subshell and
# records its exit status and output. The subshell uses the shell options of
# the real callers: "errexit" is the GitHub Actions step that sources
# ci-common.sh (bash -e), "strict" is ci-integration-test.sh
# (set -euo pipefail). It must not be called from a conditional context, which
# would switch errexit off inside the subshell.
run_case() {
  local dir="$1"
  shift
  set +e
  (
    if [[ "${MODE}" == "strict" ]]; then
      set -euo pipefail
    else
      set -e +u +o pipefail
    fi
    cd "${dir}"
    "$@"
  ) > "${TEST_TMP}/output" 2>&1 < /dev/null
  STATUS=$?
  set -e
  OUTPUT="$(cat "${TEST_TMP}/output")"
}

# with_env VAR=value... -- command... exports the assignments and runs the
# command. It is used inside run_case, so nothing leaks into other cases.
with_env() {
  while [[ "$1" != "--" ]]; do
    export "${1?}"
    shift
  done
  shift
  "$@"
}

expect_success() {
  [[ "${STATUS}" -eq 0 ]] || fail "expected success"
}

expect_failure() {
  [[ "${STATUS}" -ne 0 ]] || fail "expected a non-zero exit status"
}

expect_output_contains() {
  [[ "${OUTPUT}" == *"$1"* ]] || fail "expected the output to contain: $1"
}

expect_eq() {
  [[ "$1" == "$2" ]] || fail "$3: got '$1', want '$2'"
}

expect_file_contains() {
  grep -qF -- "$2" "$1" 2>/dev/null || fail "expected $1 to contain: $2"
}

# expect_untouched asserts that the checkout is still the clean main checkout:
# nothing from the pull request was checked out.
expect_untouched() {
  expect_eq "$(git -C "${WORK}" rev-parse HEAD)" "${MAIN_SHA}" "checked-out commit"
  expect_eq "$(git -C "${WORK}" status --porcelain)" "" "working tree changes"
  expect_eq "$(cat "${WORK}/app.txt")" "base" "content of app.txt"
}

# expect_not_fetched asserts that the pull request was not even fetched.
expect_not_fetched() {
  if git -C "${WORK}" rev-parse --verify --quiet "refs/remotes/pull/${PR}/head" >/dev/null; then
    fail "the pull request head was fetched"
  fi
}

expect_no_api_call() {
  [[ ! -e "${GH_STUB_DIR}/calls" ]] || fail "unexpected GitHub API calls: $(tr '\n' ';' < "${GH_STUB_DIR}/calls")"
}

# expect_tls_verification fails if the recorded arguments of a wget or curl
# call (one per line) switch certificate verification off.
expect_tls_verification() {
  if grep -Eqx -- '--no-check-certificate|--insecure|-[A-Za-z]*k[A-Za-z]*' "$1"; then
    fail "certificate verification is switched off: $(tr '\n' ' ' < "$1")"
  fi
}

###############################################################################
# Stubs
###############################################################################

STUB_BIN="${TEST_TMP}/bin"
mkdir -p "${STUB_BIN}"

# gh: answers from files in GH_STUB_DIR and records every call.
cat > "${STUB_BIN}/gh" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$*" >> "${GH_STUB_DIR}/calls"
if [[ -e "${GH_STUB_DIR}/fail" ]]; then
  echo "gh stub: simulated API failure" >&2
  exit 1
fi
case "$*" in
  "api repos/example-org/example-repo/pulls/"*)
    cat "${GH_STUB_DIR}/pull.json"
    ;;
  "api repos/example-org/example-repo/actions/workflows/integration-test.yaml/runs?"*)
    ;;
  "api --method POST repos/example-org/example-repo/check-runs "*)
    echo 4242
    ;;
  "api --method POST repos/example-org/example-repo/issues/comments/"*"/reactions "*)
    ;;
  *)
    echo "gh stub: unexpected call: $*" >&2
    exit 1
    ;;
esac
EOF

# go: "builds" a package by copying its main.go, so a test can tell which
# source tree a binary was built from.
cat > "${STUB_BIN}/go" <<'EOF'
#!/usr/bin/env bash
if [[ "$1" != "build" || "$2" != "-o" || $# -ne 4 ]]; then
  echo "go stub: unexpected call: $*" >&2
  exit 1
fi
cp "$4/main.go" "$3" && chmod +x "$3"
EOF

# wget: records its arguments and writes a dummy trust bundle to the -O file.
cat > "${STUB_BIN}/wget" <<'EOF'
#!/usr/bin/env bash
printf '%s\n' "$@" > "${STUB_LOG}"
out=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    -O)
      out="$2"
      shift
      ;;
    --ca-certificate=*)
      cat "${1#--ca-certificate=}" > "${STUB_LOG}.ca"
      echo "${1#--ca-certificate=}" > "${STUB_LOG}.ca-path"
      ;;
  esac
  shift
done
if [[ -n "${WGET_STUB_FAIL:-}" ]]; then
  exit 4
fi
echo "dummy trust bundle" > "${out}"
EOF

# curl: records the arguments of each call. A call with -o is the tarball
# download and fails, which stops install_gdcloud_cli before it installs
# anything; every other call is answered with CLI bundle metadata.
cat > "${STUB_BIN}/curl" <<'EOF'
#!/usr/bin/env bash
count=$(( $(cat "${STUB_LOG}.count" 2>/dev/null || echo 0) + 1 ))
echo "${count}" > "${STUB_LOG}.count"
printf '%s\n' "$@" > "${STUB_LOG}.${count}"
for arg in "$@"; do
  if [[ "${arg}" == "-o" ]]; then
    exit 22
  fi
done
cat "${CURL_STUB_METADATA}"
EOF

chmod +x "${STUB_BIN}"/*
export PATH="${STUB_BIN}:${PATH}"
for tool in gh go wget curl; do
  if [[ "$(command -v "${tool}")" != "${STUB_BIN}/${tool}" ]]; then
    echo "FAIL - ${tool} does not resolve to its stub" >&2
    exit 1
  fi
done

###############################################################################
# Fixture
###############################################################################

# build_fixture creates the bare repository that plays the role of the GitHub
# repository:
#   refs/heads/main      two commits; the first one carries the owners files,
#                        the CI scripts and the token-helper source
#   refs/pull/7/head     the reviewed commit of the pull request; it changes
#                        app.txt and tries to change all trust inputs
#   refs/fixture/moved   a commit pushed to the pull request after the review
build_fixture() {
  REMOTE="${TEST_TMP}/remote.git"
  local seed="${TEST_TMP}/seed"

  git -c init.defaultBranch=main init -q --bare "${REMOTE}"
  git -C "${REMOTE}" symbolic-ref HEAD refs/heads/main
  git -c init.defaultBranch=main init -q "${seed}"
  (
    cd "${seed}"
    git symbolic-ref HEAD refs/heads/main
    git remote add origin "${REMOTE}"
    mkdir -p scripts integration/cmd/token-helper

    cat > OWNERS_ALIASES <<'EOF'
# See the OWNERS docs at https://go.k8s.io/owners

aliases:
  example-reviewers:
  - Alice-Admin
  - bob
  - ci-helper[bot]
  - sync-agent
  # - retired-owner
  example-approvers:
  - Alice-Admin
  - bob   # still an approver
EOF
    cat > CODEOWNERS <<'EOF'
# @commented-out owns nothing
*       @Carol @example-org/core-team dave@example.com   # @inline-comment
/docs/  @erin
EOF
    echo "# ci-common.sh from main" > scripts/ci-common.sh
    echo "# ci-integration-test.sh from main" > scripts/ci-integration-test.sh
    echo "// token-helper from main" > integration/cmd/token-helper/main.go
    echo "base" > app.txt
    git add -A
    git commit -q -m "main: initial commit"

    git checkout -q -b pr
    echo "reviewed change" > app.txt
    echo "  - mallory" >> OWNERS_ALIASES
    echo "* @mallory" >> CODEOWNERS
    echo "# ci-common.sh from the pull request" > scripts/ci-common.sh
    echo "# ci-integration-test.sh from the pull request" > scripts/ci-integration-test.sh
    echo "// token-helper from the pull request" > integration/cmd/token-helper/main.go
    git commit -q -a -m "pr: reviewed commit"
    git push -q origin "HEAD:refs/pull/${PR}/head"

    echo "pushed after the review" > app.txt
    git commit -q -a -m "pr: commit pushed after the review"
    git push -q origin HEAD:refs/fixture/moved

    git checkout -q main
    echo "second commit on main" > base.txt
    git add -A
    git commit -q -m "main: second commit"
    git push -q origin main
  )

  PR_SHA="$(git -C "${REMOTE}" rev-parse "refs/pull/${PR}/head")"
  MOVED_SHA="$(git -C "${REMOTE}" rev-parse refs/fixture/moved)"
  MAIN_SHA="$(git -C "${REMOTE}" rev-parse refs/heads/main)"
}

# new_checkout [pr_head_sha] gives the current case its own copy of the remote
# (with the pull request head at the given commit, by default the reviewed
# one), a fresh clone of it at main, and its own GitHub API stub state.
CHECKOUTS=0
new_checkout() {
  local pr_head="${1:-${PR_SHA}}"
  CHECKOUTS=$((CHECKOUTS + 1))
  local dir="${TEST_TMP}/checkout-${CHECKOUTS}"

  mkdir -p "${dir}/gh"
  cp -R "${REMOTE}" "${dir}/remote.git"
  git -C "${dir}/remote.git" update-ref "refs/pull/${PR}/head" "${pr_head}"
  git clone -q "${dir}/remote.git" "${dir}/work"

  WORK="${dir}/work"
  if [[ "$(git -C "${WORK}" rev-parse --show-toplevel)" != "${WORK}" ]]; then
    echo "FAIL - test checkout ${WORK} is not a repository of its own" >&2
    exit 1
  fi
  GH_STUB_DIR="${dir}/gh"
  export GH_STUB_DIR
  EVENT_FILE="${dir}/event.json"
  GITHUB_ENV_FILE="${dir}/github-env"
  GITHUB_OUTPUT_FILE="${dir}/github-output"
  : > "${GITHUB_ENV_FILE}"
  : > "${GITHUB_OUTPUT_FILE}"
  pull_json open main mallory "mallory/example-repo" "${pr_head}"
}

# pull_json <state> <base ref> [user] [head_repo] [sha] sets the answer of the
# stubbed pulls API. By default the PR is from a fork by an unlisted user.
pull_json() {
  local state="$1" base="$2"
  local user="${3:-mallory}" repo="${4:-mallory/example-repo}" sha="${5:-${PR_SHA}}"
  jq -n --arg state "${state}" --arg base "${base}" --arg user "${user}" --arg repo "${repo}" --arg sha "${sha}" \
    '{number: 7, state: $state, user: {login: $user}, base: {ref: $base}, head: {sha: $sha, repo: {full_name: $repo}}}' > "${GH_STUB_DIR}/pull.json"
}

# checkout_pr_commit puts the checkout on the pull request commit, so that the
# working tree is controlled by the pull request.
checkout_pr_commit() {
  git -C "${WORK}" fetch -q origin "refs/pull/${PR}/head"
  git -C "${WORK}" -c advice.detachedHead=false checkout -q --detach FETCH_HEAD
}

# set_origin_main_without <path...> points origin/main of the checkout at a
# commit that lacks the given paths. The checked-out tree keeps them.
set_origin_main_without() {
  (
    cd "${WORK}"
    git checkout -q -b fixture-main "${MAIN_SHA}"
    git rm -q -r -- "$@"
    git commit -q -m "main: remove $*"
    git update-ref refs/remotes/origin/main HEAD
    git -c advice.detachedHead=false checkout -q --detach "${MAIN_SHA}"
  )
}

###############################################################################
# 1. Authorization: OWNERS only, exact match, fail closed
###############################################################################

sorted_authorized_logins() {
  local logins
  logins="$(authorized_logins)"
  sort -u <<< "${logins}"
}

expect_authorized() {
  begin "$1"
  shift
  run_case "${WORK}" is_authorized_maintainer "$@"
  expect_success
  pass
}

expect_denied() {
  begin "$1"
  shift
  run_case "${WORK}" is_authorized_maintainer "$@"
  expect_failure
  pass
}

test_authorization() {
  new_checkout

  begin "the authorized logins are exactly the logins listed on origin/main"
  run_case "${WORK}" sorted_authorized_logins
  expect_success
  expect_eq "${OUTPUT}" "$(printf '%s\n' 'alice-admin' 'bob' 'carol' 'ci-helper[bot]' 'erin' 'sync-agent')" "authorized logins"
  pass

  expect_authorized "login listed in OWNERS_ALIASES is authorized" "bob"
  expect_authorized "entry 'Alice-Admin' authorizes login 'alice-admin' (case-insensitive)" "alice-admin"
  expect_authorized "entry 'bob' authorizes login 'BOB' (case-insensitive)" "BOB"
  expect_authorized "login listed in CODEOWNERS is authorized" "carol"
  expect_authorized "login listed for a sub-path in CODEOWNERS is authorized" "erin"

  local assoc
  for assoc in MEMBER OWNER COLLABORATOR; do
    expect_denied "unlisted login is denied with author_association ${assoc}" "mallory" "${assoc}"
  done
  expect_denied "unlisted login is denied" "mallory"
  expect_denied "empty login is denied" ""
  expect_denied "missing login is denied"
  expect_denied "login that is a prefix of an entry is denied" "bo"
  expect_denied "login that extends an entry is denied" "bobby"
  expect_denied "pattern characters in a login match nothing" "b.b"
  expect_denied "wildcard login is denied" "*"
  expect_denied "login with an embedded newline is denied" $'mallory\nbob'
  expect_denied "listed login followed by a newline is denied" $'bob\n'

  expect_authorized "bot entry authorizes the exact bot login" "ci-helper[bot]"
  expect_denied "bot entry does not authorize the login without the [bot] suffix" "ci-helper"
  expect_denied "entry without [bot] does not authorize the login with the suffix" "sync-agent[bot]"

  expect_denied "team reference in CODEOWNERS does not authorize the organization name" "example-org"
  expect_denied "team reference in CODEOWNERS does not authorize the team name" "core-team"
  expect_denied "e-mail address in CODEOWNERS does not authorize its local part" "dave"
  expect_denied "e-mail address in CODEOWNERS does not authorize its domain" "example"
  expect_denied "login in a CODEOWNERS comment line is denied" "commented-out"
  expect_denied "login in a trailing CODEOWNERS comment is denied" "inline-comment"
  expect_denied "login in an OWNERS_ALIASES comment is denied" "retired-owner"
  expect_denied "alias name is not a login" "example-reviewers"

  checkout_pr_commit
  begin "owners files of the checked-out pull request do not authorize anyone"
  expect_file_contains "${WORK}/OWNERS_ALIASES" "- mallory"
  expect_file_contains "${WORK}/CODEOWNERS" "@mallory"
  run_case "${WORK}" is_authorized_maintainer "mallory"
  expect_failure
  pass
  expect_authorized "logins listed on origin/main stay authorized on a pull request checkout" "bob"

  git -C "${WORK}" tag origin/main "${PR_SHA}"
  git -C "${WORK}" branch origin/main "${PR_SHA}"
  expect_denied "a tag or branch named origin/main does not replace the remote-tracking ref" "mallory"
  expect_authorized "logins listed on origin/main stay authorized next to such a tag or branch" "bob"

  new_checkout
  git -C "${WORK}" update-ref -d refs/remotes/origin/main
  expect_denied "listed login is denied when origin/main cannot be resolved (no fallback to HEAD)" "bob"

  new_checkout
  set_origin_main_without OWNERS_ALIASES CODEOWNERS
  expect_denied "listed login is denied when origin/main has neither owners file (no fallback to HEAD)" "bob"

  new_checkout
  set_origin_main_without OWNERS_ALIASES
  expect_authorized "CODEOWNERS alone is enough when OWNERS_ALIASES is missing on origin/main" "carol"
  expect_denied "login from the checked-out OWNERS_ALIASES is denied when origin/main lacks the file" "bob"

  new_checkout
  set_origin_main_without CODEOWNERS
  expect_authorized "OWNERS_ALIASES alone is enough when CODEOWNERS is missing on origin/main" "bob"
  expect_denied "login from the checked-out CODEOWNERS is denied when origin/main lacks the file" "carol"
}

###############################################################################
# 2. Comment parsing: /test-integration [<sha>]
###############################################################################

parse_comment() {
  local line
  line="$(find_test_integration_command "$1")"
  if [[ -z "${line}" ]]; then
    echo "no command line"
    return 0
  fi
  parse_test_integration_command "${line}"
}

expect_parsed() {
  begin "$1"
  run_case "${TEST_TMP}" parse_comment "$2"
  expect_success
  expect_eq "${OUTPUT}" "$3" "parsed command"
  pass
}

expect_rejected() {
  begin "$1"
  run_case "${TEST_TMP}" parse_comment "$2"
  expect_failure
  expect_output_contains "$3"
  pass
}

test_comment_parsing() {
  local upper_sha
  upper_sha="$(tr 'a-f' 'A-F' <<< "${PR_SHA}")"

  expect_parsed "'/test-integration <sha>' on its own" \
    "/test-integration ${PR_SHA}" "${PR_SHA}"
  expect_parsed "'/test-integration <sha>' inside a multi-line comment with CRLF line endings" \
    $'Looks good to me.\r\n  /test-integration '"${PR_SHA}"$'  \r\nThanks!' "${PR_SHA}"
  expect_parsed "'/test-integration' without a SHA parses with empty SHA (policy checked later)" \
    "/test-integration" ""
  expect_parsed "upper-case SHA is accepted and normalised to lower case" \
    "/test-integration ${upper_sha}" "${PR_SHA}"
  expect_parsed "tab between command and SHA" \
    $'/test-integration\t'"${PR_SHA}" "${PR_SHA}"
  expect_parsed "the first command line of a comment is used" \
    $'/test-integration '"${PR_SHA}"$'\n/test-integration '"${MOVED_SHA}" "${PR_SHA}"

  expect_parsed "text that only mentions the command is not a command line" \
    "please run /test-integration ${PR_SHA}" "no command line"
  expect_parsed "quoted command is not a command line" \
    "> /test-integration ${PR_SHA}" "no command line"
  expect_parsed "a longer word starting with the command is not a command line" \
    "/test-integrations ${PR_SHA}" "no command line"

  expect_rejected "'/test-integration <word>' without a SHA is rejected" \
    "/test-integration please" "full 40-character commit SHA"
  expect_rejected "abbreviated SHA is rejected" \
    "/test-integration ${PR_SHA:0:12}" "full 40-character commit SHA"
  expect_rejected "SHA that is one character short is rejected" \
    "/test-integration ${PR_SHA:0:39}" "full 40-character commit SHA"
  expect_rejected "SHA followed by further characters is rejected" \
    "/test-integration ${PR_SHA}0" "full 40-character commit SHA"
  expect_rejected "SHA that is not the first argument is rejected" \
    "/test-integration please ${PR_SHA}" "at most one argument"
  expect_rejected "an argument after the SHA is rejected" \
    "/test-integration ${PR_SHA} WorkerController" "at most one argument"
  expect_rejected "a second SHA after the SHA is rejected" \
    "/test-integration ${PR_SHA} ${MOVED_SHA}" "at most one argument"
}

###############################################################################
# 3. Manual triggers are bound to the named commit
###############################################################################

pin_and_checkout() {
  fetch_reviewed_pr_head
  checkout_reviewed_pr_head
}

test_manual_trigger_pinning() {
  begin "the named commit is the PR head: exactly that commit is checked out"
  new_checkout
  run_case "${WORK}" with_env PR_NUMBER="${PR}" EXPECTED_HEAD_SHA="${PR_SHA}" -- pin_and_checkout
  expect_success
  expect_eq "$(git -C "${WORK}" rev-parse HEAD)" "${PR_SHA}" "checked-out commit"
  expect_eq "$(cat "${WORK}/app.txt")" "reviewed change" "content of app.txt"
  expect_file_contains "${GH_STUB_DIR}/calls" "api repos/example-org/example-repo/pulls/${PR}"
  pass

  begin "the named commit in upper case is accepted"
  new_checkout
  run_case "${WORK}" with_env PR_NUMBER="${PR}" EXPECTED_HEAD_SHA="$(tr 'a-f' 'A-F' <<< "${PR_SHA}")" -- pin_and_checkout
  expect_success
  expect_eq "$(git -C "${WORK}" rev-parse HEAD)" "${PR_SHA}" "checked-out commit"
  pass

  begin "the PR head moved after the commit was named: abort, nothing from the PR is checked out"
  new_checkout "${MOVED_SHA}"
  run_case "${WORK}" with_env PR_NUMBER="${PR}" EXPECTED_HEAD_SHA="${PR_SHA}" -- pin_and_checkout
  expect_failure
  expect_output_contains "PR #${PR} head changed"
  expect_output_contains "/test-integration ${MOVED_SHA}"
  expect_untouched
  pass

  begin "missing SHA on a fork PR by an unlisted author: abort before fetching and print ready-to-copy command"
  new_checkout
  run_case "${WORK}" with_env PR_NUMBER="${PR}" -- pin_and_checkout
  expect_failure
  expect_output_contains "/test-integration ${PR_SHA}"
  expect_output_contains "head_sha=${PR_SHA}"
  expect_untouched
  expect_not_fetched
  pass

  begin "missing SHA on a fork PR authored by a listed maintainer: abort before fetching"
  new_checkout
  pull_json open main bob "bob/example-repo" "${PR_SHA}"
  run_case "${WORK}" with_env PR_NUMBER="${PR}" -- pin_and_checkout
  expect_failure
  expect_output_contains "/test-integration ${PR_SHA}"
  expect_untouched
  expect_not_fetched
  pass

  begin "missing SHA on a same-repo PR authored by an unlisted user: abort before fetching"
  new_checkout
  pull_json open main mallory "${GITHUB_REPOSITORY}" "${PR_SHA}"
  run_case "${WORK}" with_env PR_NUMBER="${PR}" -- pin_and_checkout
  expect_failure
  expect_output_contains "/test-integration ${PR_SHA}"
  expect_untouched
  expect_not_fetched
  pass

  begin "missing SHA on a same-repo PR authored by a listed maintainer: uses current head"
  new_checkout
  pull_json open main bob "${GITHUB_REPOSITORY}" "${PR_SHA}"
  run_case "${WORK}" with_env PR_NUMBER="${PR}" -- pin_and_checkout
  expect_success
  expect_eq "$(git -C "${WORK}" rev-parse HEAD)" "${PR_SHA}" "checked-out commit"
  pass

  begin "missing SHA on a same-repo PR authored by a listed bot: uses current head"
  new_checkout
  pull_json open main "ci-helper[bot]" "${GITHUB_REPOSITORY}" "${PR_SHA}"
  run_case "${WORK}" with_env PR_NUMBER="${PR}" -- pin_and_checkout
  expect_success
  expect_eq "$(git -C "${WORK}" rev-parse HEAD)" "${PR_SHA}" "checked-out commit"
  pass

  begin "missing SHA on a same-repo OWNERS PR whose head moves between API read and fetch: abort"
  new_checkout "${MOVED_SHA}"
  pull_json open main bob "${GITHUB_REPOSITORY}" "${PR_SHA}"
  run_case "${WORK}" with_env PR_NUMBER="${PR}" -- pin_and_checkout
  expect_failure
  expect_output_contains "PR #${PR} head changed"
  expect_untouched
  pass

  local bad_sha
  for bad_sha in "${PR_SHA:0:7}" "${PR_SHA:0:39}" "${PR_SHA}0" "refs/pull/${PR}/head" "HEAD"; do
    begin "SHA '${bad_sha}' is not a full commit SHA: abort before any API call or fetch"
    new_checkout
    run_case "${WORK}" with_env PR_NUMBER="${PR}" EXPECTED_HEAD_SHA="${bad_sha}" -- pin_and_checkout
    expect_failure
    expect_output_contains "full 40-character SHA"
    expect_untouched
    expect_not_fetched
    expect_no_api_call
    pass
  done

  local bad_number
  for bad_number in "" "abc" "${PR}a" "${PR} " "-${PR}" "${PR}/../8" "${PR};id"; do
    begin "pr_number '${bad_number}' is not numeric: abort before any API call or fetch"
    new_checkout
    run_case "${WORK}" with_env PR_NUMBER="${bad_number}" EXPECTED_HEAD_SHA="${PR_SHA}" -- pin_and_checkout
    expect_failure
    expect_output_contains "digits only"
    expect_untouched
    expect_not_fetched
    expect_no_api_call
    pass
  done

  begin "closed PR: abort before fetching"
  new_checkout
  pull_json closed main
  run_case "${WORK}" with_env PR_NUMBER="${PR}" EXPECTED_HEAD_SHA="${PR_SHA}" -- pin_and_checkout
  expect_failure
  expect_output_contains "is not open"
  expect_untouched
  expect_not_fetched
  pass

  begin "PR with a base other than main: abort before fetching"
  new_checkout
  pull_json open release-v1.0
  run_case "${WORK}" with_env PR_NUMBER="${PR}" EXPECTED_HEAD_SHA="${PR_SHA}" -- pin_and_checkout
  expect_failure
  expect_output_contains "does not target main"
  expect_untouched
  expect_not_fetched
  pass

  begin "GitHub API call fails: abort before fetching"
  new_checkout
  touch "${GH_STUB_DIR}/fail"
  run_case "${WORK}" with_env PR_NUMBER="${PR}" EXPECTED_HEAD_SHA="${PR_SHA}" -- pin_and_checkout
  expect_failure
  expect_output_contains "Failed to read PR #${PR} from the GitHub API"
  expect_untouched
  expect_not_fetched
  pass

  local answer
  for answer in '' '{}' '[]' 'not json' '{"state": null, "base": {"ref": "main"}}' '{"state": "open"}' '{"state": "open", "base": {"ref": ""}}' '{"state": "open", "base": {"ref": "main"}}' '{"state": "open", "base": {"ref": "main"}, "head": {"sha": "not-a-sha"}}'; do
    begin "GitHub API answers '${answer}': abort before fetching"
    new_checkout
    printf '%s' "${answer}" > "${GH_STUB_DIR}/pull.json"
    run_case "${WORK}" with_env PR_NUMBER="${PR}" EXPECTED_HEAD_SHA="${PR_SHA}" -- pin_and_checkout
    expect_failure
    expect_untouched
    expect_not_fetched
    pass
  done

  begin "GITHUB_REPOSITORY is not set: abort before any API call or fetch"
  new_checkout
  run_case "${WORK}" with_env PR_NUMBER="${PR}" EXPECTED_HEAD_SHA="${PR_SHA}" GITHUB_REPOSITORY= -- pin_and_checkout
  expect_failure
  expect_untouched
  expect_not_fetched
  expect_no_api_call
  pass

  begin "the PR head cannot be fetched: abort"
  new_checkout
  git -C "${WORK}/../remote.git" update-ref -d "refs/pull/${PR}/head"
  run_case "${WORK}" with_env PR_NUMBER="${PR}" EXPECTED_HEAD_SHA="${PR_SHA}" -- pin_and_checkout
  expect_failure
  expect_output_contains "Failed to fetch the head of PR #${PR}"
  expect_untouched
  pass
}

###############################################################################
# 4. Trusted files are staged from origin/main only
###############################################################################

test_trusted_staging() {
  local staged

  begin "scripts and token-helper are staged from origin/main, not from the checked-out tree"
  new_checkout
  checkout_pr_commit
  staged="${WORK}.staged"
  expect_eq "$(cat "${WORK}/scripts/ci-common.sh")" "# ci-common.sh from the pull request" "fixture: checked-out ci-common.sh"
  run_case "${WORK}" prepare_trusted_ci_files "${staged}"
  expect_success
  expect_eq "$(cat "${staged}/scripts/ci-common.sh")" "# ci-common.sh from main" "staged ci-common.sh"
  expect_eq "$(cat "${staged}/scripts/ci-integration-test.sh")" "# ci-integration-test.sh from main" "staged ci-integration-test.sh"
  expect_eq "$(cat "${staged}/bin/token-helper")" "// token-helper from main" "source of the staged token-helper"
  [[ -x "${staged}/bin/token-helper" ]] || fail "the staged token-helper is not executable"
  [[ ! -e "${staged}/src" && ! -e "${staged}/src.tar" ]] || fail "the source tree used for the build was left behind"
  pass

  begin "staging fails when origin/main cannot be resolved (no fallback to the checked-out tree)"
  new_checkout
  checkout_pr_commit
  staged="${WORK}.staged"
  git -C "${WORK}" update-ref -d refs/remotes/origin/main
  run_case "${WORK}" prepare_trusted_ci_files "${staged}"
  expect_failure
  expect_output_contains "Cannot resolve origin/main"
  [[ ! -e "${staged}/scripts/ci-common.sh" && ! -e "${staged}/bin/token-helper" ]] || fail "files were staged"
  pass

  begin "staging fails when a CI script is missing on origin/main (no fallback to the checked-out tree)"
  new_checkout
  staged="${WORK}.staged"
  set_origin_main_without scripts/ci-integration-test.sh
  run_case "${WORK}" prepare_trusted_ci_files "${staged}"
  expect_failure
  expect_output_contains "Cannot read scripts/ci-integration-test.sh from origin/main"
  [[ ! -e "${staged}/bin/token-helper" ]] || fail "the token-helper was staged"
  pass

  begin "staging fails when the token-helper cannot be built from origin/main (no fallback to the checked-out tree)"
  new_checkout
  staged="${WORK}.staged"
  set_origin_main_without integration
  run_case "${WORK}" prepare_trusted_ci_files "${staged}"
  expect_failure
  expect_output_contains "Cannot build the token-helper from origin/main"
  [[ ! -e "${staged}/bin/token-helper" ]] || fail "the token-helper was staged"
  pass
}

###############################################################################
# 5. The complete precondition step for each trigger
###############################################################################

# preconditions runs the entry point of the "Verify CI Preconditions" step.
# Staging the trusted files is replaced here; test_trusted_staging covers it.
preconditions() {
  prepare_trusted_ci_files() {
    CI_TRUSTED_DIR="${WORK}.trusted"
    export CI_TRUSTED_DIR
  }
  verify_and_prepare_ci
}

# run_event <event name> [VAR=value...] runs the precondition step for the
# event payload in EVENT_FILE.
run_event() {
  local event="$1"
  shift
  run_case "${WORK}" with_env GITHUB_ACTIONS=true GITHUB_EVENT_NAME="${event}" GITHUB_EVENT_PATH="${EVENT_FILE}" \
    GITHUB_ENV="${GITHUB_ENV_FILE}" GITHUB_OUTPUT="${GITHUB_OUTPUT_FILE}" CHECK_NAME="Fixture Check" "$@" -- preconditions
}

pull_request_event() {
  jq -n --arg login "$1" --arg assoc "$2" --arg repo "$3" --arg sha "${PR_SHA}" \
    '{pull_request: {number: 7, author_association: $assoc, user: {login: $login},
      head: {sha: $sha, ref: "feature", repo: {full_name: $repo}}}}' > "${EVENT_FILE}"
}

comment_event() {
  jq -n --arg login "$1" --arg assoc "$2" --arg body "$3" \
    '{issue: {number: 7, pull_request: {url: "https://api.github.invalid/pulls/7"}},
      comment: {id: 1001, body: $body, author_association: $assoc, user: {login: $login}}}' > "${EVENT_FILE}"
}

expect_should_run() {
  expect_eq "$(cat "${GITHUB_OUTPUT_FILE}")" "should_run=$1" "step output"
}

expect_not_started() {
  expect_eq "$(cat "${GITHUB_OUTPUT_FILE}")" "" "step output"
  expect_eq "$(cat "${GITHUB_ENV_FILE}")" "" "exported environment"
}

# expect_reviewed_commit_merged asserts that the checkout is the reviewed
# commit with main merged into it.
expect_reviewed_commit_merged() {
  expect_eq "$(git -C "${WORK}" rev-parse HEAD^1)" "${PR_SHA}" "first parent of the tested commit"
  expect_eq "$(git -C "${WORK}" rev-parse HEAD^2)" "${MAIN_SHA}" "second parent of the tested commit"
  expect_eq "$(cat "${WORK}/app.txt")" "reviewed change" "content of app.txt"
  expect_file_contains "${GITHUB_ENV_FILE}" "CI_PRECONDITIONS_VERIFIED=true"
  expect_file_contains "${GITHUB_ENV_FILE}" "PR_NUMBER=${PR}"
  expect_file_contains "${GITHUB_ENV_FILE}" "PR_HEAD_SHA=${PR_SHA}"
  expect_file_contains "${GITHUB_ENV_FILE}" "COMMIT_HASH=${PR_SHA:0:7}"
  expect_file_contains "${GH_STUB_DIR}/calls" "head_sha=${PR_SHA}"
}

test_pull_request_event() {
  begin "pull_request: same-repository PR of a listed author starts the test automatically"
  new_checkout
  git -C "${WORK}" fetch -q origin "refs/pull/${PR}/head"
  pull_request_event "Alice-Admin" "NONE" "${GITHUB_REPOSITORY}"
  run_event pull_request
  expect_success
  expect_should_run true
  expect_file_contains "${GITHUB_ENV_FILE}" "CI_PRECONDITIONS_VERIFIED=true"
  expect_untouched
  pass

  local assoc
  for assoc in MEMBER OWNER COLLABORATOR; do
    begin "pull_request: PR of an unlisted author with author_association ${assoc} is not started"
    new_checkout
    pull_request_event "mallory" "${assoc}" "${GITHUB_REPOSITORY}"
    run_event pull_request
    expect_failure
    expect_output_contains "is not listed in OWNERS_ALIASES or CODEOWNERS on main"
    expect_output_contains "'/test-integration ${PR_SHA}'"
    expect_output_contains "pr_number=${PR} and head_sha=${PR_SHA}"
    expect_not_started
    pass
  done

  begin "pull_request: PR from a fork is not started, even for a listed author"
  new_checkout
  pull_request_event "bob" "MEMBER" "bob/example-repo"
  run_event pull_request
  expect_failure
  expect_output_contains "forked PR"
  expect_output_contains "'/test-integration ${PR_SHA}'"
  expect_output_contains "pr_number=${PR} and head_sha=${PR_SHA}"
  expect_not_started
  pass

  begin "pull_request: missing event payload aborts"
  new_checkout
  rm -f "${EVENT_FILE}"
  run_event pull_request
  expect_failure
  expect_output_contains "event payload is missing"
  expect_not_started
  pass
}

test_issue_comment_event() {
  begin "issue_comment: listed maintainer names the PR head: that commit is tested"
  new_checkout
  comment_event "bob" "NONE" $'Reviewed.\n/test-integration '"${PR_SHA}"
  run_event issue_comment
  expect_success
  expect_should_run true
  expect_reviewed_commit_merged
  pass

  begin "issue_comment: '/test-integration' without a SHA succeeds on a same-repo OWNERS PR"
  new_checkout
  pull_json open main bob "${GITHUB_REPOSITORY}" "${PR_SHA}"
  comment_event "bob" "NONE" "/test-integration"
  run_event issue_comment
  expect_success
  expect_should_run true
  expect_reviewed_commit_merged
  pass

  begin "issue_comment: anything after the SHA is rejected"
  new_checkout
  comment_event "bob" "NONE" "/test-integration ${PR_SHA} WorkerController"
  run_event issue_comment
  expect_failure
  expect_output_contains "at most one argument"
  expect_not_started
  expect_untouched
  expect_not_fetched
  expect_no_api_call
  pass

  local assoc
  for assoc in MEMBER OWNER COLLABORATOR; do
    begin "issue_comment: unlisted commenter with author_association ${assoc} cannot trigger"
    new_checkout
    comment_event "mallory" "${assoc}" "/test-integration ${PR_SHA}"
    run_event issue_comment
    expect_failure
    expect_output_contains "is not listed in OWNERS_ALIASES or CODEOWNERS on main"
    expect_not_started
    expect_untouched
    expect_not_fetched
    expect_no_api_call
    pass
  done

  begin "issue_comment: '/test-integration' without a SHA is rejected on a fork PR with ready-to-copy command"
  new_checkout
  comment_event "bob" "NONE" "/test-integration"
  run_event issue_comment
  expect_failure
  expect_output_contains "::error::"
  expect_output_contains "'/test-integration ${PR_SHA}'"
  expect_not_started
  expect_untouched
  expect_not_fetched
  if grep -qF '/reactions' "${GH_STUB_DIR}/calls" 2>/dev/null; then
    fail "posted a rocket reaction on a rejected comment"
  fi
  pass

  begin "issue_comment: a SHA from the environment does not replace the SHA named in the comment"
  new_checkout "${MOVED_SHA}"
  comment_event "bob" "NONE" "/test-integration ${PR_SHA}"
  run_event issue_comment EXPECTED_HEAD_SHA="${MOVED_SHA}"
  expect_failure
  expect_output_contains "PR #${PR} head changed"
  expect_not_started
  expect_untouched
  pass

  begin "issue_comment: a SHA from the environment does not make up for a comment without SHA on a fork PR"
  new_checkout
  comment_event "bob" "NONE" "/test-integration"
  run_event issue_comment EXPECTED_HEAD_SHA="${PR_SHA}"
  expect_failure
  expect_not_started
  expect_untouched
  expect_not_fetched
  pass

  begin "issue_comment: the PR head moved after the comment: abort, nothing from the PR is checked out"
  new_checkout "${MOVED_SHA}"
  comment_event "bob" "NONE" "/test-integration ${PR_SHA}"
  run_event issue_comment
  expect_failure
  expect_output_contains "PR #${PR} head changed"
  expect_not_started
  expect_untouched
  pass

  begin "issue_comment: comment without a command line is skipped"
  new_checkout
  comment_event "mallory" "NONE" "I would never write /test-integration ${PR_SHA} in the middle of a line."
  run_event issue_comment
  expect_success
  expect_should_run false
  expect_untouched
  expect_not_fetched
  pass

  begin "issue_comment: missing event payload aborts"
  new_checkout
  rm -f "${EVENT_FILE}"
  run_event issue_comment
  expect_failure
  expect_output_contains "event payload is missing"
  expect_not_started
  pass
}

test_workflow_dispatch_event() {
  begin "workflow_dispatch: listed maintainer names pr_number and head_sha: that commit is tested"
  new_checkout
  run_event workflow_dispatch GITHUB_ACTOR=bob PR_NUMBER="${PR}" EXPECTED_HEAD_SHA="${PR_SHA}"
  expect_success
  expect_should_run true
  expect_reviewed_commit_merged
  pass

  begin "workflow_dispatch: pr_number without head_sha is rejected on a fork PR with ready-to-copy SHA"
  new_checkout
  run_event workflow_dispatch GITHUB_ACTOR=bob PR_NUMBER="${PR}" EXPECTED_HEAD_SHA=
  expect_failure
  expect_output_contains "head_sha=${PR_SHA}"
  expect_not_started
  expect_untouched
  expect_not_fetched
  pass

  begin "workflow_dispatch: pr_number without head_sha succeeds on a same-repo OWNERS PR"
  new_checkout
  pull_json open main bob "${GITHUB_REPOSITORY}" "${PR_SHA}"
  run_event workflow_dispatch GITHUB_ACTOR=bob PR_NUMBER="${PR}" EXPECTED_HEAD_SHA=
  expect_success
  expect_should_run true
  expect_reviewed_commit_merged
  pass

  begin "workflow_dispatch: the PR head is not the named commit: abort, nothing from the PR is checked out"
  new_checkout "${MOVED_SHA}"
  run_event workflow_dispatch GITHUB_ACTOR=bob PR_NUMBER="${PR}" EXPECTED_HEAD_SHA="${PR_SHA}"
  expect_failure
  expect_output_contains "PR #${PR} head changed"
  expect_not_started
  expect_untouched
  pass

  begin "workflow_dispatch: unlisted actor cannot trigger"
  new_checkout
  run_event workflow_dispatch GITHUB_ACTOR=mallory PR_NUMBER="${PR}" EXPECTED_HEAD_SHA="${PR_SHA}"
  expect_failure
  expect_output_contains "is not listed in OWNERS_ALIASES or CODEOWNERS on main"
  expect_not_started
  expect_untouched
  expect_not_fetched
  expect_no_api_call
  pass

  begin "workflow_dispatch: without pr_number the selected branch is tested as it is"
  new_checkout
  run_event workflow_dispatch GITHUB_ACTOR=bob PR_NUMBER= EXPECTED_HEAD_SHA=
  expect_success
  expect_should_run true
  expect_untouched
  expect_not_fetched
  expect_no_api_call
  pass

  begin "workflow_dispatch: head_sha without pr_number is rejected"
  new_checkout
  run_event workflow_dispatch GITHUB_ACTOR=bob PR_NUMBER= EXPECTED_HEAD_SHA="${PR_SHA}"
  expect_failure
  expect_output_contains "head_sha was given without pr_number"
  expect_not_started
  expect_untouched
  pass
}

###############################################################################
# 6. Bootstrap downloads verify TLS
###############################################################################

# without_delays runs a command with sleep replaced, so that retry loops
# around deliberately failing downloads do not slow the tests down.
without_delays() {
  # shellcheck disable=SC2329 # called by the retry helper in ci-common.sh
  sleep() { :; }
  "$@"
}

test_tls_bootstrap() {
  local dir="${TEST_TMP}/tls"
  local sa_file="${dir}/sa.json" ca_file="${dir}/ca.pem"
  local url="https://console.org1.zone1.lab.example.invalid/.well-known/certificate-authority"
  mkdir -p "${dir}/trusted/bin"
  echo '{}' > "${sa_file}"

  begin "trust bundle download: the console certificate is verified against the system trust store"
  run_case "${dir}" with_env STUB_LOG="${dir}/wget-1" -- \
    setup_gdc_credentials "${sa_file}" "${ca_file}" org1 zone1 lab.example.invalid ""
  expect_success
  expect_eq "$(head -n 1 "${dir}/wget-1")" "${url}" "download URL"
  expect_tls_verification "${dir}/wget-1"
  [[ ! -e "${dir}/wget-1.ca" ]] || fail "an extra CA file was passed although GDC_BOOTSTRAP_CA_PEM is not set"
  expect_eq "$(cat "${ca_file}")" "dummy trust bundle" "downloaded trust bundle"
  pass

  begin "trust bundle download: GDC_BOOTSTRAP_CA_PEM is passed to wget as CA file and removed afterwards"
  local pem=$'-----BEGIN CERTIFICATE-----\nZml4dHVyZQ==\n-----END CERTIFICATE-----'
  run_case "${dir}" with_env STUB_LOG="${dir}/wget-2" GDC_BOOTSTRAP_CA_PEM="${pem}" -- \
    setup_gdc_credentials "${sa_file}" "${ca_file}" org1 zone1 lab.example.invalid ""
  expect_success
  expect_eq "$(head -n 1 "${dir}/wget-2")" "${url}" "download URL"
  expect_tls_verification "${dir}/wget-2"
  expect_eq "$(cat "${dir}/wget-2.ca")" "${pem}" "CA file passed to wget"
  [[ ! -e "$(cat "${dir}/wget-2.ca-path")" ]] || fail "the temporary CA file was left behind"
  pass

  begin "trust bundle download: a failed download aborts"
  run_case "${dir}" with_env STUB_LOG="${dir}/wget-3" WGET_STUB_FAIL=1 -- \
    without_delays setup_gdc_credentials "${sa_file}" "${ca_file}" org1 zone1 lab.example.invalid ""
  expect_failure
  expect_output_contains "Failed to fetch GDC Root CA"
  expect_tls_verification "${dir}/wget-3"
  pass

  if [[ -x /usr/local/bin/google-distributed-cloud-hosted-cli/bin/gdcloud ]]; then
    echo "skip - [${MODE}] gdcloud CLI download: the gdcloud CLI is already installed on this machine"
    return 0
  fi

  printf '#!/usr/bin/env bash\necho fixture-sts-token\n' > "${dir}/trusted/bin/token-helper"
  chmod +x "${dir}/trusted/bin/token-helper"
  jq -n '{items: [{metadata: {creationTimestamp: "2026-01-01T00:00:00Z"},
    platform: {os: "linux", architecture: "amd64"},
    commonMetadata: {artifactVersion: "1.16.2-fixture", servingURL: "https://artifacts.example.invalid/gdcloud.tar.gz"}}]}' \
    > "${dir}/metadata.json"

  begin "gdcloud CLI download: metadata and tarball requests verify the server against the trust bundle"
  run_case "${dir}" with_env STUB_LOG="${dir}/curl" CURL_STUB_METADATA="${dir}/metadata.json" CI_TRUSTED_DIR="${dir}/trusted" -- \
    without_delays install_gdcloud_cli "${sa_file}" "${ca_file}" "https://mgmt.example.invalid" "1.16.2"
  expect_failure
  local calls downloads=0 log
  calls="$(cat "${dir}/curl.count" 2>/dev/null || echo 0)"
  [[ "${calls}" -ge 2 ]] || fail "expected a metadata request and a tarball download, got ${calls} curl call(s)"
  for log in "${dir}"/curl.[0-9]*; do
    expect_tls_verification "${log}"
    expect_eq "$(grep -x -A 1 -- '--cacert' "${log}" | tail -n 1)" "${ca_file}" "--cacert argument of curl call ${log##*.}"
    expect_eq "$(grep -x -A 1 -- '--proto' "${log}" | tail -n 1)" "=https" "--proto argument of curl call ${log##*.}"
    expect_eq "$(grep -x -A 1 -- '--proto-redir' "${log}" | tail -n 1)" "=https" "--proto-redir argument of curl call ${log##*.}"
    if grep -qxF -- "https://artifacts.example.invalid/gdcloud.tar.gz?uncompressed=false" "${log}"; then
      downloads=$((downloads + 1))
    fi
  done
  [[ "${downloads}" -ge 1 ]] || fail "the tarball was not requested from the serving URL of the metadata"
  pass

  begin "gdcloud CLI download: in GitHub Actions the token-helper is never built from the checked-out tree"
  run_case "${dir}" with_env STUB_LOG="${dir}/curl-untrusted" CURL_STUB_METADATA="${dir}/metadata.json" GITHUB_ACTIONS=true CI_TRUSTED_DIR="${dir}/missing" -- \
    install_gdcloud_cli "${sa_file}" "${ca_file}" "https://mgmt.example.invalid" "1.16.2"
  expect_failure
  expect_output_contains "refusing to build it from the checked-out tree"
  [[ ! -e "${dir}/curl-untrusted.count" ]] || fail "a request was sent although no trusted token-helper exists"
  pass
}

###############################################################################
# Run
###############################################################################

build_fixture

for MODE in errexit strict; do
  test_authorization
  test_comment_parsing
  test_manual_trigger_pinning
  test_trusted_staging
  test_pull_request_event
  test_issue_comment_event
  test_workflow_dispatch_event
done

# The bootstrap downloads only run from ci-integration-test.sh (strict mode).
MODE=strict
test_tls_bootstrap

echo "PASS - ${CASES} cases"
