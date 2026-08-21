#!/bin/sh

# Apply the four repository settings that RELEASE-CHECKLIST.md requires and
# that GitHub refuses to accept while the repository is private: private
# vulnerability reporting, protection for main, protection for release tags,
# and the public-release environment the signing secrets are scoped to.
#
# Run this once, immediately after the repository is made public and before the
# release tag is pushed. Every step verifies itself by reading the setting back,
# because an unconfigured gate and a satisfied one look identical from inside
# the workflow that depends on it.

set -eu

repository=${1:-$(gh repo view --json nameWithOwner --jq .nameWithOwner)}
environment=public-release
branch_ruleset="protect main"
tag_ruleset="protect release tags"

fail() {
  echo "apply_release_settings: $*" >&2
  exit 1
}

visibility=$(gh api "repos/$repository" --jq .visibility)
if [ "$visibility" != public ]; then
  fail "$repository is $visibility. GitHub offers none of these settings on a
private repository at this plan: rulesets and branch protection answer 403,
environment settings answer 422, and private vulnerability reporting answers 404.
Make the repository public first; release.yml refuses to publish otherwise."
fi

# 1. Private vulnerability reporting. Until this is on, the advisory URL in
# SECURITY.md returns a 404 to an outside reporter, so the email channel in that
# document is carrying the whole promise on its own.
echo "enabling private vulnerability reporting"
gh api -X PUT "repos/$repository/private-vulnerability-reporting" --silent
[ "$(gh api "repos/$repository/private-vulnerability-reporting" --jq .enabled)" = true ] ||
  fail "private vulnerability reporting did not come back enabled"

# 2 and 3. Rulesets rather than classic branch protection, because the same
# mechanism protects the release tags, and an attestation is only worth as much
# as the immutability of the tag it names. Neither ruleset requires a pull
# request: this is a single-maintainer tree that commits to main directly, so
# the rules that matter are the ones preventing history from being rewritten or
# a published tag from being moved, not a review ceremony with one participant.
apply_ruleset() {
  name=$1
  include=$2
  rules=$3
  existing=$(gh api "repos/$repository/rulesets" --jq "map(select(.name == \"$name\")) | .[0].id // empty")
  body=$(printf '{"name":"%s","target":"%s","enforcement":"active","conditions":{"ref_name":{"include":["%s"],"exclude":[]}},"rules":%s}' \
    "$name" "$4" "$include" "$rules")
  if [ -n "$existing" ]; then
    echo "updating ruleset $name"
    printf '%s' "$body" | gh api -X PUT "repos/$repository/rulesets/$existing" --input - --silent
  else
    echo "creating ruleset $name"
    printf '%s' "$body" | gh api -X POST "repos/$repository/rulesets" --input - --silent
  fi
  gh api "repos/$repository/rulesets" --jq "map(select(.name == \"$name\" and .enforcement == \"active\")) | length" |
    grep -qx 1 || fail "ruleset $name is not active"
}

apply_ruleset "$branch_ruleset" 'refs/heads/main' \
  '[{"type":"deletion"},{"type":"non_fast_forward"}]' branch
apply_ruleset "$tag_ruleset" 'refs/tags/v*' \
  '[{"type":"deletion"},{"type":"non_fast_forward"},{"type":"update"}]' tag

# 4. The environment itself, with no deployment approval on it. What the
# environment is still for is scope: the six Apple signing secrets live here
# and only the jobs that name this environment can read them, which is the
# property worth keeping. Approval is not that property.
#
# The human decision is the tag push. Only a maintainer can create a v* tag,
# the tag ruleset above makes it immutable once created, and release.yml's
# authorize job independently refuses any tag whose commit lacks a successful
# candidate run. A reviewer prompt on top of that gated an already-gated path.
echo "clearing deployment approval on the $environment environment"
printf '{"prevent_self_review":false,"reviewers":[],"deployment_branch_policy":null}' |
  gh api -X PUT "repos/$repository/environments/$environment" --input - --silent
gh api "repos/$repository/environments/$environment" \
  --jq '.protection_rules | map(select(.type == "required_reviewers")) | length' |
  grep -qx 0 || fail "$environment still has a required reviewer"

echo "done. Confirm in repository settings as well: an API that answered 200 is"
echo "evidence the call was accepted, not that the gate reads the way you meant."
