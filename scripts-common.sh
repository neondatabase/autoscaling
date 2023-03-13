# Common tools for scripts in this directory. Requires bash.

# Usage: <cmd> | indent
#
# Helper function to clarify output a little better by indenting it.
indent () {
    sed -u -e "s/^/    /g"
}

# Usage: require_root
#
# Helper function to require that the script is being run as root
require_root () {
    if [[ "$OSTYPE" != "darwin"* && "$EUID" != 0 ]]; then
        echo "Must be running as root (EUID != 0)"
        exit 1
    fi
}

# Usage: GIT_INFO="$(git_info)"
#
# Generates and outputs some information about the current git status. Normally we'd use Go's
# built-in "version" buildinfo, but with git worktress + docker, the git information isn't available
# without some extra help.
git_info () {
    diff="git diff --exit-code --name-only"

    if $diff >/dev/null && $diff --staged >/dev/null; then
        dirty=''
    else
        dirty='+dirty'
    fi

    # notes on formatting: the combination of TZ=UTC0 and --date=iso-local means that we'll output
    # ISO 8601 (-ish) format, interpreting the timestamp as relative to UTC+0.
    #
    # Looks like: 7b66271+dirty (2022-12-23 17:20:49 +0000) - agent: Handle metrics more gracefully
    TZ=UTC0 git show -s --format=format:"%h$dirty (%cd) - %s" --date=iso-local | \
        # Passing through layers of quotes means that git commits including a single-quote won't be
        # handled correctly 🤦
        sed -e "s:':\":g"
}

# Usage: NEONVM_VERSION="$(neonvm_version)"
#
# Fetches the version of NeonVM we're using as a dependency, directly from go.mod.
neonvm_version () {
    go list -m -f '{{ .Version }}' github.com/neondatabase/neonvm
}

# Usage: VAR_NAME="$(get_var VAR_NAME DEFAULT_VALUE)"
#
# Helper function that
get_var () {
    var_name="$1"
    default_value="$2"

    ( set -u; exec "echo \"\$$var_name\"" ) 2>/dev/null || {
        printf -v default_escaped "%q" "$default_value"
        read -p "Set $var_name [default $default_escaped]: " new_val
        if [ -z "$new_val" ]; then
            echo "$default_value"
        else
            echo "$new_val"
        fi
    }
}

# Usage: VM_NAME="$(get_vm_name)"
#
# Gets the VM name if it the VM_NAME variable isn't already set. Otherwise echo $VM_NAME
get_vm_name() {
    # note: -n means that VM_NAME could actually be set and we wouldn't catch it. That's ok; it
    # allows the name to be reset with just VM_NAME='', instead of unsetting the variable.
    if [ -n "$VM_NAME" ]; then
        echo "$VM_NAME"
        return
    fi

    candidates="$(kubectl get neonvm -o jsonpath='{.items[*].metadata.name}')"
    if [[ "$?" != 0 ]]; then
        echo "Failed to get candidate VM names" >/dev/tty
        return 1
    elif [ -z "$candidates" ]; then
        echo "No candidate VMs. Are there any running?" >/dev/tty
        return 1
    fi

    if [[ "${#candidates[@]}" != 1 ]]; then
        echo "More than one candidate VM, please set VM_NAME" >/dev/tty
        echo "Candidate VMs:" >/dev/tty
        echo "${candidates[@]}" | tr ' ' '\n' | sed -e 's/^/ * /g' >/dev/tty
        return 1
    fi

    # print the VM name
    echo "${candidates[0]}"
    return
}

# USAGE: VM_IP="$(get_vm_ip "$vm_pod")"
#
# Gets the static IP of the VM
get_vm_ip() {
    # This is actually a bit tricky, because we want to use the overlay network so we have a
    # consistent IP across migrations. We can get it from the 'k8s.v1.cni.cncf.io/network-status'
    # annotation.
    #
    # note: the network-status annotation is a JSON string. we need to unpack that with JQ first
    kubectl get pod "$1" -o jsonpath='{.metadata.annotations}' \
        | jq -er '.["k8s.v1.cni.cncf.io/network-status"]' \
        | jq -er '.[] | select(.interface == "net1") | .ips[0]'
}
