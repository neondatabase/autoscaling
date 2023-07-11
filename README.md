# Autoscaling — dev branch

This branch exists only to track what's currently deployed to these regions:

* dev-us-east-2-beta
* dev-eu-central-1-alpha
* dev-eu-west-1-zeta

We don't *quite* use the release yaml files directly, because there are some config differences that
we want to preserve.

Currently these are:

```js
// Agent:
config.billing = {
      "url": "https://vector-usage-tracking.beta.us-east-2.internal.aws.neon.build/v1",
      "cpuMetricName": "effective_compute_seconds",
      "activeTimeMetricName": "active_time_seconds",
      "collectEverySeconds": 4,
      "pushEverySeconds": 24,
      "pushTimeoutSeconds": 30
}

// Scheduler:
config.k8sNodeGroupLabel = "eks.amazonaws.com/nodegroup"
config.doMigration = true
```

### Other regions

No other dev regions have autoscaling deployed.
