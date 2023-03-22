## NeonVM migration prototype

- implement new API and CRDs for VM migrations
- implement new controller or add migration flow to current neonvm controller

### Minimal migration specification

```yaml
apiVersion: vm.neon.tech/v1
kind: VirtualMachineMigration
metadata:
  name: example
spec:
  vmName: example-vm
```

### Full migration specification

```yaml
apiVersion: vm.neon.tech/v1
kind: VirtualMachineMigration
metadata:
  name: example
  labels: {}
  annotations: {}
spec:
  vmName: example-vm
  nodeSelector: {}
  nodeAffinity: {}
  completionTimeout: 3600
  incremental: true
  compress: true
  autoConverge: true
  allowPostCopy: true
  maxBandwidth: 10Gi
  xbzrleCache:
    enabled: true
    size: 256Mi
  zeroBlocks: true
  multifdCompression: zstd
```

### State of migration

```yaml
status:
  migrationStatus: completed
  completionTimestamp: 2022-12-22T01:01:03Z
  totaltime: 128212 ms
  downtime: 63 ms
  throughput: 1087Mi
  sourceNode: node1
  targetNode: node2
  targetVmName: example-abc
  targetPodName: example-abc-def-12345
  memory:
    transferred: 228391Ki
    remaining: 0Ki
    total: 8Gi
    precopy: 202371Ki
    downtime: 26020Ki
  pages:
    pageSize: 4Ki
    duplicate: 2045389
    skipped: 0
    normal: 52501
    pagesPerSecond: 32710
```

### Questions

- how generate name for target VM (name prefix/sufix/other) ?
- how long migration resource should be available (`kubectl get neonvmmigrate`) after migration finished ?
- how to cancel migration ? as variant - just delete it (`kubectl delete neonvmmigrate example`)
- should migration controller delete source VM after migration ?
- metrics ?
