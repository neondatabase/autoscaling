#!/bin/bash

# Check if gh command is available
if ! command -v gh &> /dev/null; then
    exit 0
fi

# Check if the remote is neondatabase/autoscaling
if ! gh repo view --json nameWithOwner -q '.nameWithOwner' 2>/dev/null | grep -q "^neondatabase/autoscaling$"; then
    exit 0
fi

# Try to get current branch name
BRANCH_NAME=$(git rev-parse --abbrev-ref HEAD)

if [[ "$BRANCH_NAME" == "HEAD" ]]; then
    # We're in a rebase or similar detached HEAD state
    # Try to get the original branch name from the rebase-merge directory
    if [[ -f "$(git rev-parse --git-dir)/rebase-merge/head-name" ]]; then
        BRANCH_NAME=$(cat "$(git rev-parse --git-dir)/rebase-merge/head-name" | sed 's/refs\/heads\///')
    fi
fi

# Get the commit message directly from the current commit
COMMIT_MSG=$(git log -1 --format=%B)

COMMIT_SUBJECT=$(echo "$COMMIT_MSG" | head -n 1)
COMMIT_BODY=$(echo "$COMMIT_MSG" | tail -n +2)

# If commit already has a PR number, leave it unchanged
if [[ $COMMIT_SUBJECT =~ \(#[0-9]+\) ]]; then
    exit 0
fi

PR_NUMBER=$(gh pr view "$BRANCH_NAME" --json number --jq '.number' 2>/dev/null)

# If no PR number is found, proceed without appending
if [[ -z $PR_NUMBER ]]; then
    echo "No Pull Request found for this commit, won't append a number"
    exit 0
fi

# Create the new commit with updated message
git commit --amend -m "$COMMIT_SUBJECT (#$PR_NUMBER)" -m "$COMMIT_BODY"

echo "Appended PR number (#$PR_NUMBER) to the commit subject."

exit 0
