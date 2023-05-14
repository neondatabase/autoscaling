# cluster-autoscaler

This directory contains a patch for [cluster-autoscaler] (CA) that allows it to correctly interpret the
resource usage of VMs. The patch is designed to work with exactly one version of CA, matching the
Kubernetes version used in the rest of this repository.

[cluster-autoscaler]: https://github.com/kubernetes/autoscaler/tree/master/cluster-autoscaler

The git tag for the version of CA we're using is stored in `ca.tag`.

## Building

Any building of cluster-autoscaler is _exclusively_ done with the Dockerfile in this directory. It
checks out CA's git repo at the right tag, applies the patch, and builds the binary.

The produced image is provided for each release _of this repo_, as `neondatabase/cluster-autoscaler:<VERSION>`

CI does not typically check that CA builds, unless there's been a change in `ca.tag`, `ca.patch`, or
`Dockerfile`. There's currently no testing — given the slow rate of change, it's ok _for now_ for
testing to be done manually.

## Version upgrade checklist

- [ ] Update ca.tag
- [ ] Update the golang builder image in Dockerfile, to match GA's builder (see note at the top of
    that file)
