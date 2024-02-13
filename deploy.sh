#!/usr/bin/env bash
#
# Script for deploying to various regions.
#
# Usage: ./deploy.sh <CLUSTER>

set -eu -o pipefail

ENVIRONMENT='dev'

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

    if [ "$(check_deploy multus)" = 'yes' ]; then
        log 'Deploying multus...'
        cmd kubectl --context="$cluster" apply -f multus-eks.yaml
        cmd kubectl --context="$cluster" -n kube-system rollout status daemonset kube-multus-ds
        log 'Multus deploy finished.'
    else
        log 'Skipping.'
    fi
    
    if [ "$(check_deploy whereabouts)" = 'yes' ]; then
        log 'Deploying whereabouts...'
        cmd kubectl --context="$cluster" apply -f whereabouts.yaml
        cmd kubectl --context="$cluster" -n kube-system rollout status daemonset whereabouts
        log 'Whereabouts deploy finished.'
    else
        log 'Skipping.'
    fi
    
    if [ "$(check_deploy vmscrape)" = 'yes' ]; then
        log 'Deploying vmscrape...'
        cmd kubectl --context="$cluster" apply -f vmscrape.yaml
        log 'vmscrape deploy finished.'
    else
        log 'Skipping.'
    fi
    
    if [ "$(check_deploy neonvm)" = 'yes' ]; then
        log 'Deploying neonvm...'
        cmd kubectl apply -f neonvm.yaml
        cmd kubectl --context="$cluster" -n neonvm-system rollout status daemonset  neonvm-device-plugin
        cmd kubectl --context="$cluster" -n neonvm-system rollout status deployment neonvm-controller
        cmd kubectl --context="$cluster" -n neonvm-system rollout status daemonset  neonvm-vxlan-controller
        log 'Whereabouts deploy finished.'
    else
        log 'Skipping.'
    fi
    
    if [ "$(check_deploy autoscale-scheduler)" = 'yes' ]; then
        log 'Baking scheduler...'
        cmd bash -e -o pipefail -c "./build.sh autoscale-scheduler > $cluster/autoscale-scheduler.yaml"
        log 'Deploying scheduler...'
        cmd kubectl --context="$cluster" apply -f "$cluster/autoscale-scheduler.yaml"
        cmd kubectl --context="$cluster" -n kube-system rollout status deployment autoscale-scheduler
        log 'Scheduler deploy finished.'
    else
        log 'Skipping.'
    fi
    
    if [ "$(check_deploy autoscaler-agent)" = 'yes' ]; then
        log 'Baking autoscaler-agent...'
        cmd bash -e -o pipefail -c "./build.sh autoscaler-agent > $cluster/autoscaler-agent.yaml"
        log 'Deploying autoscaler-agent...'
        cmd kubectl --context="$cluster" apply -f "$cluster/autoscaler-agent.yaml"
        cmd kubectl --context="$cluster" -n kube-system rollout status daemonset autoscaler-agent
        log 'Autoscaler-agent deploy finished.'
    else
        log 'Skipping.'
    fi
}

check_deps () {
    log 'check deps...'

    ( set -u; echo "$EDITOR" ) 2>/dev/null >/dev/null || echo "missing env var EDITOR"
    which bash grep date sed jq yq kubectl >/dev/null

    # Check that 'yq' is the type we expect. There's a couple variants.
    # On arch linux, this is the 'yq' package (nb: not 'go-yq')
    ( yq --help | grep -q 'https://github.com/kislyuk/yq' ) 2>/dev/null 1>/dev/null || echo "must have 'yq' installed, from 'https://github.com/kislyuk/yq'"
}

# Usage: check_deploy <name>
check_deploy () {
    while true; do
        echo -en "\e[1m$(date -u '+%F %T') \e[32m::\e[39m Deploy $1? ([Y]es/[S]kip): \e[0m" >/dev/tty
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

main "$@"
