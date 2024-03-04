#!/usr/bin/env bash
#
# Script for deploying to various regions.
#
# Usage: ./deploy.sh <CLUSTER>

set -eu -o pipefail

log () {
    echo -en "\e[1m"
    echo -n "$(date -u '+%F %T')"
    echo -en " \e[34m::\e[39m "
    echo -n "$1"
    echo -e "\e[0m"
}

err () {
    echo -en "\e[1m"
    echo -n "$(date -u '+%F %T')"
    echo -en " \e[31m::\e[39m "
    echo -n "$1"
    echo -e "\e[0m"
}

cmd () {
    # Handle quoting properly so that printing the command works as expected.
    cmdline=''
    for arg in "${@}"; do
        if ! ( echo "$arg" | grep -qE '^[A-Za-z0-9_-]*$' ) ; then
            cmdline="$cmdline ${arg@Q}"
        else
            cmdline="$cmdline $arg"
        fi
    done
    echo -en "\e[1m"
    echo -n "$(date -u '+%F %T')"
    echo -en " \e[33m::\e[0m"
    echo "$cmdline"
    "$@" | sed -e 's/^/    /g' # sed is used for indenting here
}

main () {
    check_deps

    ( set -u ; echo "$1" ) 2>/dev/null >/dev/null || {
        err "Missing first argument (cluster)"
        err "Usage: ./deploy.sh <CLUSTER>"
        exit 1
    }

    cluster="$1"

    if ! [ -d "./$cluster" ]; then
        err "Missing directory at './$cluster'"
        exit 1
    fi

    cmd kubectl config set-context "$cluster"

    if [ "$(confirm 'Dry-run deploy multus?')" = 'yes' ]; then
        cmd cp multus-eks.yaml "$cluster/multus-eks.yaml"
        check_diff "$cluster" "$cluster/multus-eks.yaml"
        if [ "$(confirm 'Deploy multus?')" = 'yes' ]; then
            log 'Deploying multus...'
            cmd kubectl --context="$cluster" apply -f multus-eks.yaml
            cmd kubectl --context="$cluster" -n kube-system rollout status daemonset kube-multus-ds
            log 'Multus deploy finished.'
        else
            log 'Skipping.'
        fi
    else
        log 'Skipping.'
    fi
    
    if [ "$(confirm 'Dry-run deploy whereabouts?')" = 'yes' ]; then
        cmd cp whereabouts.yaml "$cluster/whereabouts.yaml"
        check_diff "$cluster" "$cluster/whereabouts.yaml"
        if [ "$(confirm 'Deploy whereabouts?')" = 'yes' ]; then
            log 'Deploying whereabouts...'
            cmd kubectl --context="$cluster" apply -f whereabouts.yaml
            cmd kubectl --context="$cluster" -n kube-system rollout status daemonset whereabouts
            log 'Whereabouts deploy finished.'
        else
            log 'Skipping.'
        fi
    else
        log 'Skipping.'
    fi
    
    if [ "$(confirm 'Dry-run deploy vmscrape?')" = 'yes' ]; then
        cmd cp vmscrape.yaml "$cluster/vmscrape.yaml"
        check_diff "$cluster" "$cluster/vmscrape.yaml"
        if [ "$(confirm 'Deploy vmscrape?')" = 'yes' ]; then
            log 'Deploying vmscrape...'
            cmd kubectl --context="$cluster" apply -f vmscrape.yaml
            log 'vmscrape deploy finished.'
        else
            log 'Skipping.'
        fi
    else
        log 'Skipping.'
    fi
    
    if [ "$(confirm 'Dry-run deploy neonvm?')" = 'yes' ]; then
        cmd cp neonvm.yaml "$cluster/neonvm.yaml"
        check_diff "$cluster" "$cluster/neonvm.yaml"
        if [ "$(confirm 'Deploy neonvm?')" = 'yes' ]; then
            log 'Deploying neonvm...'
            cmd kubectl apply -f neonvm.yaml
            cmd kubectl --context="$cluster" -n neonvm-system rollout status daemonset  neonvm-device-plugin
            cmd kubectl --context="$cluster" -n neonvm-system rollout status deployment neonvm-controller
            cmd kubectl --context="$cluster" -n neonvm-system rollout status daemonset  neonvm-vxlan-controller
            log 'Neonvm deploy finished.'
        else
            log 'Skipping.'
        fi
    else
        log 'Skipping.'
    fi
    
    if [ "$(confirm 'Dry-run deploy autoscale-scheduler?')" = 'yes' ]; then
        log 'Baking scheduler...'
        cmd bash -e -o pipefail -c "./build.sh $cluster autoscale-scheduler > $cluster/autoscale-scheduler.yaml"
        check_diff "$cluster" "$cluster/autoscale-scheduler.yaml"
        if [ "$(confirm 'Deploy autoscale-scheduler?')" = 'yes' ]; then
            log 'Deploying scheduler...'
            cmd kubectl --context="$cluster" apply -f "$cluster/autoscale-scheduler.yaml"
            cmd kubectl --context="$cluster" -n kube-system rollout status deployment autoscale-scheduler
            log 'Scheduler deploy finished.'
        else
            log 'Skipping.'
        fi
    else
        log 'Skipping.'
    fi
    
    if [ "$(confirm 'Dry-run deploy autoscaler-agent?')" = 'yes' ]; then
        log 'Baking autoscaler-agent...'
        cmd bash -e -o pipefail -c "./build.sh $cluster autoscaler-agent > $cluster/autoscaler-agent.yaml"
        check_diff "$cluster" "$cluster/autoscaler-agent.yaml"
        if [ "$(confirm 'Deploy autoscaler-agent?')" = 'yes' ]; then
            log 'Deploying autoscaler-agent...'
            cmd kubectl --context="$cluster" apply -f "$cluster/autoscaler-agent.yaml"
            cmd kubectl --context="$cluster" -n kube-system rollout status daemonset autoscaler-agent
            log 'Autoscaler-agent deploy finished.'
        else
            log 'Skipping.'
        fi
    else
        log 'Skipping.'
    fi

    log 'All done! 😊'
}

check_deps () {
    log 'check deps...'

    which bash grep date sed sponge jq yq kubectl >/dev/null

    # Check that 'yq' is the type we expect. There's a couple variants.
    # On arch linux, this is the 'yq' package (nb: not 'go-yq')
    ( yq --help | grep -q 'https://github.com/kislyuk/yq' ) 2>/dev/null 1>/dev/null || echo "must have 'yq' installed, from 'https://github.com/kislyuk/yq'"

    # Check if 'delta' exists
    if ( which delta ) 2>/dev/null >/dev/null ; then
        DIFF_PROGRAM='delta'
    elif ( which diff ) 2>/dev/null >/dev/null ; then
        DIFF_PROGRAM='diff'
    else
        err "expected to find either 'delta' or 'diff' in \$PATH"
        exit 1
    fi
}

# Usage: confirm <message>
#
# Options are [Y]es or [S]kip
confirm () {
    while true; do
        echo -en "\e[1m$(date -u '+%F %T') \e[32m::\e[39m $1 ([Y]es/[S]kip): \e[0m" >/dev/tty
        read response
        case "$response" in
            'Y' | 'y' | 'Yes' | 'yes')
                echo 'yes'
                return 0
                ;;
            'S' | 's' | 'Skip' | 'skip')
                echo 'no'
                return 0
                ;;
            *)
                echo "Unknown answer '$response'" >/dev/tty
                ;;
        esac
    done
}

# Usage: check_diff <cluster> <file>
check_diff () {
    cluster="$1"
    file="$2"
    (
        set +e
        cmd sh -c "KUBECTL_EXTERNAL_DIFF=\"./deploy-diff-helper.sh $DIFF_PROGRAM\" kubectl --context=$cluster diff -f $file"
        status="$?"
        if [ "$status" -eq 0 ] || [ "$status" -eq 1 ]; then
            # 'kubectl diff' says it'll return 0 or 1 depending on the diff; >1 for any other error.
            return 0
        else
            return "$status"
        fi
    )


}

main "$@"
