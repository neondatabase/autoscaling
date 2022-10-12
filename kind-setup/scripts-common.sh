# Common tools for scripts in this directory. Requires bash.

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
