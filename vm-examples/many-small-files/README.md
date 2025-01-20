# many-small-files

This is a reproducer for OOM inside the VM.

There is a small rust app in this dir that creates 1mln+ of 144 bytes with random content. This workload thrashes kernel memory and triggers oom-killer to kill random userspace processes.

