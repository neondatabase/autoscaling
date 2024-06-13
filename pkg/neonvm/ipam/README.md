## IP address management example

### Creat Network Attachment Definition

```console
kubectl apply -f demo/ipam-nad.yaml
```

### Run example

```console
go run demo/ipam.go
```

### Delete Network Attachment Definition

```console
kubectl delete -f ipam-nad.yaml
```
