# Autoscaling — dev branch

This branch exists only to track what's currently deployed to the us-east-2 development cluster.

We don't *quite* use the release yaml files directly, because there are some config differences that
we want to preserve.

Currently these are:

```js
// Scheduler:
config.nodeDefaults.computeUnit = { "vCPUs": 1, "mem": 4 }

// autoscaler-agent:
config.metrics.loadMetricPrefix = "host_"
```
