#!/bin/bash
set -euo pipefail

# Change to the repository root directory
cd "$(git rev-parse --show-toplevel)"

# Check if gh command is available
if ! command -v gh &> /dev/null; then
    exit 0
fi

BRANCH_NAME=$(git rev-parse --abbrev-ref HEAD)

BASE_BRANCH=$(gh pr view "$BRANCH_NAME" --json baseRefName --jq '.baseRefName' || echo "")
if [[ -z "$BASE_BRANCH" ]]; then
    echo "Error: Unable to determine base branch. Ensure the current branch corresponds to a pull request." >&2
    exit 1
fi

export BRANCH_NAME

git rebase "$BASE_BRANCH" --exec ./scripts/git-pr-number-single.sh
