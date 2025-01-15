#!/bin/bash

# Change to the repository root directory
cd "$(git rev-parse --show-toplevel)" || exit 1 

git rebase main --exec ./scripts/git-pr-number-single.sh
