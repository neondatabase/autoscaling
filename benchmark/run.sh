#!/bin/sh

# This script is compatible with ash shell (more minimal than bash)
duration="15s"

echo "Starting generate tests with various rates (duration: $duration for each)"
echo "-------------------------------------------------------------"

# Use a simple list of rates instead of an array
for rate in 1000 2000 5000 10000 15000 25000 50000 100000 200000 300000 600000; do
    echo "Running with rate: $rate"
    /tmp/generate -rate $rate -duration $duration > /dev/console
    echo "Completed rate: $rate"
    echo "-------------------------------------------------------------"
done

echo "All tests completed!"
