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
    if [[ "$EUID" != 0 ]]; then
        echo "Must be running as root (EUID != 0)"
        exit 1
    fi
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

    candidates="$(kubectl get vm -o jsonpath='{.items[*].metadata.name}')"
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
