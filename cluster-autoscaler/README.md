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
`Dockerfile`. This is because CA takes a _very long time_ to build, even compared to the rest of the
stuff in this repo. When one of these files changes, we only check that CA builds. Given the slow
rate of change, it's ok for testing to be done manually for now.

## Version upgrade checklist

- [ ] Update ca.tag
- [ ] Update the golang builder image in Dockerfile, to match GA's builder (see note at the top of
    that file)
