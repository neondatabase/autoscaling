#!/bin/sh
echo "TODO: ip addresses need to be updated. have to either (a) expose the port to do it from
the host system (very difficult!) or (b) do it from within the pod (not as clean, but doable)."
exit 1
pgbench -h 10.77.1.36 -U postgres -c 20 -T 1000 -P 1 -f factorial.sql 
