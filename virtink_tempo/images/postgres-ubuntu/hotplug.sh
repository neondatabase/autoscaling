#!/bin/sh
chcpu -r
chcpu -e 0,1,2,3,4,5,6,7,8
#echo 1 | tee /sys/devices/system/cpu/cpu*/online

