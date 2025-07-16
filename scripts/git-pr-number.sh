#!/bin/bash

# Change to the repository root directory
cd "$(git rev-parse --show-toplevel)" || exit 1 

# Check if gh command is available
if ! command -v gh &> /dev/null; then
    exit 0
fi

# Try to get current branch name
BRANCH_NAME=$(git rev-parse --abbrev-ref HEAD)

BASE_BRANCH=$(gh pr view "$BRANCH_NAME" --json baseRefName --jq '.baseRefName')

export BRANCH_NAME

git rebase "$BASE_BRANCH" --exec ./scripts/git-pr-number-single.sh
