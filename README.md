# Autoscaling — prod branch

This branch exists only to track what's currently deployed to various production clusters, in a
similar fashion to the staging branch.

We don't *quite* use the release yaml files directly, because there are some config differences that
we want to preserve.

Currently these are:

```js
// Agent:
config.billing = {
      "url": "http://neon-internal-api.aws.neon.tech/billing/api/v1",
      "cpuMetricName": "effective_compute_seconds",
      "activeTimeMetricName": "active_time_seconds",
      "collectEverySeconds": 4,
      "pushEverySeconds": 24,
      "pushTimeoutSeconds": 2
}
```

... and all of `vmscrape.yaml`.

The following production clusters have autoscaling deployed with the config represented in this
branch.

- prod-ap-southeast-1-epsilon
- prod-eu-central-1-gamma
- prod-us-east-2-delta

### Other regions

The other production regions have the following deployed:

* prod-us-east-1-theta: `autoscale-scheduler-disabled.yaml`
* prod-us-west-2-eta: `autoscale-scheduler-disabled.yaml`
