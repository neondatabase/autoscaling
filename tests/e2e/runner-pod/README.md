`runner-pod` tests our basic guarantees around runner pod. This includes:
1. Creating the runner with the right spec and status values.
2. Having exactly one runner pod running for each VM.
3. If a pod fails, we create a new pod and start the VM again.