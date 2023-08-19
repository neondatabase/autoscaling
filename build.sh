#!/usr/bin/env bash
#
# Helper script to build various YAML files.

set -eu -o pipefail

main () {
    if [ "$#" -ne 1 ]; then
        echo "Expected exactly one argument" >&2
        exit 1
    fi

    case "$1" in
        autoscaler-agent)
            do-build autoscaler-agent 'autoscaler-agent-config' 'config.json'
            ;;
        autoscale-scheduler)
            do-build autoscale-scheduler 'scheduler-plugin-config' 'autoscale-enforcer-config.json'
            ;;
        *)
            echo "Unexpected target $1" >&2
            exit 1
            ;;
    esac
}

context () {
    if ! ( set -u; echo "$CONTEXT" ) 2>/dev/null >/dev/null; then
        CONTEXT="$(kubectl config current-context)"
    fi
    echo "$CONTEXT"
}

# Usage: do-build <target> <config k8s name> <config filename>
do-build () {
    target="$1"
    config_k8s_name="$2"
    config_filename="$3"

    obj_selector=".kind == \"ConfigMap\" and .metadata.name == \"$config_k8s_name\""
    cfg_selector=".data[\"$config_filename\"]"

    base_changes="$(cat patches.json | jq -e --arg target "$target" '.[$target]["@base"]')"
    ctx_changes="$(cat patches.json | jq -e --arg ctx "$(context)" --arg target "$target" '.[$target][$ctx]' || {
        echo "context not found $(context)" >/dev/tty;
        exit 1
    })"
    changes="$(echo "[$base_changes, $ctx_changes]" | jq '.[0] + .[1]')"

    config_filter="select($obj_selector) | $cfg_selector"
    changes="$(cat patches.json | jq --arg ctx "$(context)" --arg target "$target" '.[$target] | .["@base"] + .[$ctx]')"
    new_config="$(cat $target.yaml | yq -r "$config_filter" | apply-changes "$changes")"
    cat $target.yaml | yq --yaml-output --indentless-lists --arg new "$new_config" "if $obj_selector then $cfg_selector = \$new else . end"
}

# Usage: ... generate config | apply-changes <change spec>
apply-changes () {
    changes="$1"
    config="$(cat)"

    count="$(echo "$changes" | jq 'length')"

    for ((i=0; i<$count; i++)); do
        path="$(echo "$changes" | jq -r ".[$i].path")"
        value="$(echo "$changes" | jq ".[$i].value")"

        config="$(echo "$config" | jq --argjson value "$value" "$path = \$value")"
    done

    echo "$config"
}

main "$@"
