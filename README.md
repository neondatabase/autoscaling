# Autoscaling — dev branch

This branch exists only to track what's currently deployed to the dev-us-east-2-beta cluster.

We don't *quite* use the release yaml files directly, because there are some config differences that
we want to preserve.

Currently these are:

```js
// Scheduler:
config.nodeDefaults.computeUnit = { "vCPUs": 1, "mem": 4 }
```
