## Starting computer use as a regular pod

pod.yml
```yaml
apiVersion: v1
kind: Pod
metadata:
  name: anthropic-demo
spec:
  containers:
    - name: anthropic-quickstart
      image: ghcr.io/anthropics/anthropic-quickstarts:computer-use-demo-latest
      env:
        - name: ANTHROPIC_API_KEY
          valueFrom:
            secretKeyRef:
              name: anthropic-api-key
              key: ANTHROPIC_API_KEY
      ports:
        - containerPort: 5900
        - containerPort: 8501
        - containerPort: 6080
        - containerPort: 8080
```

```bash
# importing a key
kubectl create secret generic anthropic-api-key --from-literal=ANTHROPIC_API_KEY=your_api_key_here

# creating a pod
kubectl apply -f ./pod.yml

# exposing ports to localhost
kubectl port-forward pod/anthropic-demo 5900:5900 8501:8501 6080:6080 8080:8080

# open http://localhost:8080/
```

## Trying to do the same in NeonVM

Building the VM image:
```bash
../../bin/vm-builder \
            -spec=./image-spec.yaml \
            --size=10G \
            -src=ghcr.io/anthropics/anthropic-quickstarts:computer-use-demo-latest \
            -dst=vm-computer-use:latest
```

Importing it into local kind:

```bash
../../bin/kind load docker-image vm-computer-use:latest --name neonvm-arthur
```


Creating a VM, look at `./vm.yaml`.

```bash
kubectl apply -f ./vm.yaml
```

Forwarding a port:
```bash
kubectl port-forward pod/anthopic-vm1-j5z7p 5900:5900 8501:8501 6080:6080 8080:8080
```