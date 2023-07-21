# Autoscaling — prod branch

This branch exists only to track what's currently deployed to various production clusters, in a
similar fashion to the staging branch.

We don't *quite* use the release yaml files directly, because there are some config differences that
we want to preserve.

Currently these are:

```js
// Agent:
config.billing = {
      "url": "https://vector-usage-tracking.service.us-east-2.internal.aws.neon.tech/v1",
      "cpuMetricName": "effective_compute_seconds",
      "activeTimeMetricName": "active_time_seconds",
      "collectEverySeconds": 4,
      "accumulateEverySeconds": 24,
      "pushEverySeconds": 30,
      "pushRequestTimeoutSeconds": 30,
      "maxBatchSize": 200
}

// Scheduler:
config.k8sNodeGroupLabel = "eks.amazonaws.com/nodegroup"
config.ignoreNamespaces = ["overprovisioning"]
```

The following production clusters have autoscaling deployed with the config represented in this
branch.

- prod-ap-southeast-1-epsilon
- prod-eu-central-1-gamma
- prod-us-east-2-delta
- prod-us-east-1-theta
- prod-us-west-2-eta
