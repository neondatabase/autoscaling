#!/bin/sh

if [ "$#" -ne 1 ]; then
    echo "Usage: $0 <VM-IP>"
    exit 1
fi
host="$1"

set -eu -o pipefail

# USAGE: log <STRING>
log () {
    # busybox date doesn't have --rfc-3339, but we can emulate it.
    echo "[$(date "+%F %T %z")] $1"
}

# Usage: set_cpu_count <COUNT>
set_cpu_count () {
    log "set_cpu_count cpu_count=$1"
    ch-remote --api-socket /var/run/virtink/ch.sock resize --cpus "$1"
}

metrics_url="http://$host:9100/metrics"

current_cpu_count=1
set_cpu_count "$current_cpu_count"
pid="$$"
echo "PID: $pid"
echo "$pid" > autoscaler.pid

while true; do
    # sleep at the start so that we give the VM a moment to start up the metrics
    sleep 5s

    current_load="$(curl -sS "$metrics_url" | grep -oE '^node_load1 [[:digit:].]+' | cut -d' ' -f2)"

    log "cpu_count = $current_cpu_count, load = $current_load"
    if [ 1 -eq "$(echo "$current_load > $current_cpu_count * 0.9" | bc)" ]; then
        log "doubling CPU count"
        current_cpu_count="$(( $current_cpu_count * 2 ))"
        set_cpu_count "$current_cpu_count"
    elif [ 1 -eq "$(echo "$current_load < $current_cpu_count * 0.4" | bc)" ]; then
        log "reducing CPU count, min 1"
        half="$(( $current_cpu_count / 2 ))"
        current_cpu_count="$(( $half > 1 ? $half : 1 ))"
        set_cpu_count "$current_cpu_count"
    fi
done
