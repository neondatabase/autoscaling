#!/bin/bash
set -euo pipefail

# Script to transform Go error handling from sugared to verbose form
# Transforms:
#   ... := ... handle err {
#   ...
#   }
# To:
#   ..., err := ...
#   if err != nil {
#   ...
#   }

find pkg -name "*.go" -type f | while read -r file; do
    sed -i '
    /.*:=.*handle err {.*/{
        s/^\(\t*\)\(.*\) := \(.*\) handle err {/\1\2, err := \3\
\1if err != nil {/
    }' "$file"

    echo "Processed: $file"
done

echo "Desugared all Go files. Don't forget to 'make resugar' before commit!"
