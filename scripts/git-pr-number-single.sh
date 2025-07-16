#!/bin/bash
set -euo pipefail

# Check if the remote is neondatabase/autoscaling
if ! gh repo view --json nameWithOwner -q '.nameWithOwner' 2>/dev/null | grep -q "^neondatabase/autoscaling$"; then
    exit 0
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

# Create a temporary file with the new commit message
TMPFILE=$(mktemp)
echo "$COMMIT_SUBJECT (#$PR_NUMBER)" > "$TMPFILE"
echo "$COMMIT_BODY" >> "$TMPFILE"
git commit --amend --file="$TMPFILE"
rm "$TMPFILE"

echo "Appended PR number (#$PR_NUMBER) to the commit subject."

exit 0
