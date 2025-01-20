# many-small-files

This is a reproducer for OOM inside the VM.

There is a small rust app in this dir that creates 1mln+ of 144 bytes with random content. This workload thrashes kernel memory and triggers oom-killer to kill random userspace processes.

## Building a VM

From this directory:
```
docker build -t many-small-files . && \
../../bin/vm-builder \
            -spec=./neon-image-spec.yaml \
            -src=many-small-files:latest \
            -dst=vm-neon-msf:latest \
            -target-arch=linux/amd64 \
            -size 2G && \
../../bin/kind load docker-image vm-neon-msf:latest --name $(../../bin/kind get clusters)
```

## Cheatsheet

To start a VM:
```
kubectl apply -f ./spec.yml
```

To destroy:
```
kubectl delete -f ./spec.yml
```

Logs and stuff:
```
kubectl logs -f $(kubectl get pods -o jsonpath='{.items[0].metadata.name}')

kubectl exec -t -i $(kubectl get pods -o jsonpath='{.items[0].metadata.name}') -- ssh guest-vm
```

## How to reproduce the issue

Start the VM and watch the logs. Usually oom-killer engages at 1e6 created files.