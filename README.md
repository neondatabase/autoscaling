# Autoscaling — dev branch

This branch exists only to track what's currently deployed to these regions:

* dev-us-east-2-beta
* dev-eu-central-1-alpha
* dev-eu-west-1-zeta

We don't *quite* use the release yaml files directly, because there are some config differences that
we want to preserve.

These are represented in `patches.json`, which are then applied by running `./build.sh <target>`,
where `<target>` is either `autoscaler-agent` or `autoscale-scheduler`.

This is currently automated with `deploy.sh`, so a typical release flow looks like:

```sh
# Donwload the yaml files locally for a particular version, like v0.24.0
./download.sh <VERSION>

# Run the deploy process for a cluster, using those downloaded files.
# The cluster will be e.g. dev-us-east-2-beta.
#
# You must have a local kubectl context matching that name.
#
# deploy.sh is interactive and has mandatory dry-runs with diff inspection.
./deploy.sh <CLUSTER>
```

**Note:** `build.sh` (and, transitively, `deploy.sh`) requires the `yq` command-line tool ([link](https://github.com/kislyuk/yq)).
There's more than one tool named `yq`; make sure you have the right one.

### Other regions

No other dev regions have autoscaling deployed.
