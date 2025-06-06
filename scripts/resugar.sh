#!/bin/bash
set -euo pipefail

# Script to transform Go error handling from verbose to sugared form
# Transforms:
#   ..., err := ...
#   if err != nil {
#   ...
#   }
# To:
#   ... := ... handle err {
#   ...
#   }

find pkg -name "*.go" -type f | while read -r file; do
    sed -i '
    /.*, *err[ \t]*:=.*/{
        N
        s/^\(\t*\)\(.*\), err := \([^;]*\)\n\(\t*\)if err != nil {/\1\2 := \3 handle err {/
    }' "$file"

    echo "Processed: $file"
done
