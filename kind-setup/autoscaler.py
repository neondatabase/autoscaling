import subprocess
import time
import sys

def set_cpu_count(cpu_count):
   print(f'set_cpu_count {cpu_count=}')
   print(subprocess.getoutput(f"ch-remote --api-socket /var/run/virtink/ch.sock resize --cpus {cpu_count}"))


host=sys.argv[1]
current_cpu_count = 1
set_cpu_count(current_cpu_count)

while True:
    current_load = float(subprocess.getoutput("curl --silent " + host + ":9100/metrics  | grep '^node_load1 '  | awk '{print $2}'"))
    print(f'{current_load=}, {current_cpu_count=}')
    if current_load > current_cpu_count * 0.9:
        current_cpu_count *= 2
        set_cpu_count(current_cpu_count)
    elif current_load < current_cpu_count * 0.4: 
        current_cpu_count = max(1, current_cpu_count // 2)
        set_cpu_count(current_cpu_count)
    time.sleep(5)

