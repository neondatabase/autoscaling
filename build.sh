#!/usr/bin/env bash
#
# Helper script to build various YAML files.
#
# Usage: ./build.sh <CLUSTER> <TARGET>

set -eu -o pipefail

PATCHES_FILE='patches.json'

main () {
    if [ "$#" -ne 2 ]; then
        echo "Expected exactly two arguments" >&2
        exit 1
    fi

    cluster="$1"
    target="$2"

    # Check that '$target' exists:
    if [ "$(cat "$PATCHES_FILE" | jq --arg t "$target" '.[$t] != null')" != 'true' ]; then
        echo "Unexpected target $target" >&2
        exit 1
    fi
    filename="$target.yaml"
    if [ ! -f "$filename" ]; then
        echo "Could not find $filename" >&2
        exit 1
    fi

    original_content="$(cat "$filename")"
    OLD_IFS="$IFS"; IFS=$'\n'
    change_sets=($(cat "$PATCHES_FILE" | jq -ec --arg t "$target" '.[$t][]'))
    IFS="$OLD_IFS"

    new_content="$original_content"

    # Apply each change set
    for i in "${!change_sets[@]}"; do
        echo "build.sh: Applying '$target' change set $(( i+1 )) of ${#change_sets[@]} ..." >&2

        new_content="$(apply-change_set "$cluster" "${change_sets[$i]}" "$new_content")"
    done

    # done!
    echo "$new_content"
}

# Usage: new_content=$(apply-change_set <cluster> <change set> <content>)
apply-change_set () {
    cluster="$1"
    change_set="$2"
    content="$3"

    change_type="$(echo "$change_set" | jq -re '.change_type')"
    k8s_kind="$(echo "$change_set" | jq -re '.k8s_kind')"
    k8s_name="$(echo "$change_set" | jq -re '.k8s_name')"
    obj_subpath="$(echo "$change_set" | jq -re '.obj_subpath')"

    # Check that .changes has what we expect:
    if [ "$(echo "$change_set" | jq '.changes["@base"] != null')" != 'true' ]; then
        echo '.changes missing "@base" field' >&2
        echo "change_set: $change_set" >&2
        exit 1
    elif [ "$(echo "$change_set" | jq --arg c "$cluster" '.changes[$c] != null')" != 'true' ]; then
        echo ".changes missing \"$cluster\" field" >&2
        exit 1
    fi

    OLD_IFS="$IFS"; IFS=$'\n'
    changes=($(echo "$change_set" | jq -erc --arg c "$cluster" '.changes | .["@base"] + .[$c] | .[]'))
    IFS="$OLD_IFS"

    case "$change_type" in
        nested_json)
            extractor=("yq" "-r")
            patch_argtype="arg"
            changer="apply-nested_json-change"
            ;;
        naive_yq)
            extractor=("yq")
            patch_argtype="argjson"
            changer="apply-naive_yq-change"
            ;;
        *)
            echo "Unknown change_type \"$change_type\"" >&2
            exit 1
    esac

    # Even if we're operating yaml (via 'naive_yq' change_type), we'll still use JSON as an
    # intermediate form because jq is just a bit easier to work with.
    json_to_modify="$( \
        echo "$content" \
            | ${extractor[@]} -ec --arg k "$k8s_kind" --arg n "$k8s_name" "select(.kind == \$k and .metadata.name == \$n) | $obj_subpath"
    )"

    if [ -z "$json_to_modify" ]; then
        echo "json selector returned nothing" >&2
        exit 1
    fi

    for change in "${changes[@]}"; do
        json_to_modify="$($changer "$change" "$json_to_modify")"
    done

    # Write back into the original location and output that:
    echo "$content" | \
        yq --yaml-output --indentless-lists \
            --arg k "$k8s_kind" \
            --arg n "$k8s_name" \
            "--$patch_argtype" new "$json_to_modify" \
            "if .kind == \$k and .metadata.name == \$n then $obj_subpath = \$new else . end"
}

# Usage: apply-naive_yq-change <change> <input>
apply-naive_yq-change () {
    change="$1"
    input="$2"

    echo "$input" | jq "$change"
}

# Usage: apply-nested_json-change <change> <input>
apply-nested_json-change () {
    change="$1"
    input="$2"

    path="$(echo "$change" | jq -re '.path')"
    value="$(echo "$change" | jq -e '.value')"

    echo "$input" | jq --argjson value "$value" "$path = \$value"
}

main "$@"
