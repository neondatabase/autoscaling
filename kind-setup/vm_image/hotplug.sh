#!/bin/sh
#
# This script gets called by virtink whenever CPU hotplugging happens. If we don't have it, the VM
# won't properly make use of new CPUs that have been allocated to it.
chcpu -r
chcpu -e 0,1,2,3,4,5,6,7,8
#echo 1 | tee /sys/devices/system/cpu/cpu*/online

