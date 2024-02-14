#!/usr/bin/env bash
#
# Helper program to diff two k8s objects, called by deploy.sh via KUBECTL_EXTERNAL_DIFF 'kubectl diff'
#
# Usage: ./deploy-diff-helper.sh <file 1> <file 2> <DIFF_PROGRAM>
#
# NB: if we use KUBECTL_EXTERNAL_DIFF='./deploy-diff-helper.sh delta', then we get called like
# above.

set -eu -o pipefail

# Check dependencies
which yq sponge >/dev/null

if [ "$#" -ne '3' ]; then
    echo "$0: expected 3 args, got $#"
    exit 1
fi

file1="$1"
file2="$2"
diff_program="$3"

# Remove some fields from the files to trim output and remove "nothing-actually-changed" diffs
JQ_FILTER='
.
  | del(.metadata.annotations["kubectl.kubernetes.io/last-applied-configuration"])
  | del(.metadata.annotations["deprecated.daemonset.template.generation"])
  | del(.metadata.generation)
  | del(.metadata.resourceVersion)
  | del(.status)
'

for file in $(find "$file1" -type f); do
    yq "$JQ_FILTER" "$file" | sponge "$file"
done
for file in $(find "$file2" -type f); do
    yq "$JQ_FILTER" "$file" | sponge "$file"
done

"$diff_program" "$file1" "$file2"
