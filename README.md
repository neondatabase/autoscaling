# Autoscaling — prod branch

This branch exists only to track what's currently deployed to these production clusters, in a
similar fashion to the staging branch:

- prod-ap-southeast-1-epsilon
- prod-eu-central-1-gamma
- prod-il-central-1-iota
- prod-us-east-1-theta
- prod-us-east-2-delta
- prod-us-west-2-eta

We don't *quite* use the release yaml files directly, because there are some config differences that
we want to preserve.

These are represented in `patches.json`, which are then applied by running `./build.sh <target>`,
where `<target>` is either `autoscaler-agent` or `autoscale-scheduler`.

So, a typical release flow _for each region_ looks like the following:

```sh
# set the context to the name of the cluster, e.g. dev-us-east-2-beta.
# This is used later on, by build.sh
kubectl config set-context <CLUSTER>

# deploy NeonVM
kubectl apply -f neonvm.yaml && \
    kubectl -n neonvm-system rollout status daemonset neonvm-device-plugin && \
    kubectl -n neonvm-system rollout status daemonset neonvm-vxlan-controller && \
    kubectl -n neonvm-system rollout status deployment neonvm-controller

# deploy the scheduler:
./build.sh autoscale-scheduler | tee <CLUSTER>/autoscale-scheduler.yaml | kubectl apply -f -

# deploy the autoscaler-agents:
./build.sh autoscaler-agent | tee <CLUSTER>/autoscaler-agent.yaml | kubectl apply -f - && \
    kubectl -n kube-system rollout status daemonset autoscaler-agent
```

**Note:** `build.sh` requires the `yq` command-line tool ([link](https://github.com/kislyuk/yq)).
There's more than one tool named `yq`; make sure you have the right one.

### Other regions

No other prod regions have autoscaling deployed.
