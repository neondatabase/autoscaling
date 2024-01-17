# Scheduler Plugin: Architecture

The scheduler plugin has some unfortunately annoying design, largely resulting from edge cases that
we can't properly handle otherwise. So, this document serves to explain some of this weirdness, for
people new to this code to have a little more context.

In some places, we assume that you're already familiar with the protocol between the scheduler
plugin and each `autoscaler-agent`. For more information, refer to the [section on the protocol] in
the repo-level architecture doc.

[section on the protocol]: ../../ARCHITECTURE.md#agent-scheduler-protocol-details

This document should be up-to-date. If it isn't, that's a mistake (open an issue!).

**Table of contents:**

* [File descriptions](#file-descriptions)
* [High-level overview](#high-level-overview)
* [Deep dive into resource management](#deep-dive-into-resource-management)
  * [Basics: `reserved`, `system` and `total`](#basics-reserved-system-and-total)
  * [Non-VM pods](#non-vm-pods)
  * [Pressure and watermarks](#pressure-and-watermarks)
  * [Startup uncertainty: `buffer`](#startup-uncertainty-buffer)

## File descriptions

* `ARCHITECTURE.md` — this file :)
* [`config.go`] — definition of the `config` type, plus entrypoints for setting up update
  watching/handling and config validation.
* [`dumpstate.go`] — HTTP server, types, and conversions for dumping all internal state
* [`plugin.go`] — scheduler plugin interface implementations, plus type definition for
  `AutoscaleEnforcer`, the type implementing the `framework.*Plugin` interfaces.
* [`queue.go`] — implementation of a metrics-based priority queue to select migration targets. Uses
  `container/heap` internally.
* [`prommetrics.go`] — prometheus metrics collectors.
* [`run.go`] — handling for `autoscaler-agent` requests, to a point. The nitty-gritty of resource
  handling relies on `trans.go`.
* [`state.go`] — definitions of `pluginState`, `nodeState`, `podState`. Also _many_ functions to
  create and use them. Basically a catch-all file for everything that's not in `plugin.go`,
  `run.go`, or `trans.go`.
* [`trans.go`] — generic handling for resource requests and pod deletion. This is where the meat of
  the code to ensure we don't overcommit resources is.
* [`watch.go`] — setup to watch VM pod (and non-VM pod) deletions. Uses our
  [`util.Watch`](../util/watch.go).

[`config.go`]: ./config.go
[`dumpstate.go`]: ./dumpstate.go
[`plugin.go`]: ./plugin.go
[`queue.go`]: ./queue.go
[`run.go`]: ./run.go
[`state.go`]: ./state.go
[`trans.go`]: ./trans.go
[`watch.go`]: ./watch.go

## High-level overview

The entrypoint for plugin initialization is through the `NewAutoscaleEnforcerPlugin` method in
`plugin.go`, which in turn:

  1. Fetches the scheduler config (and starts watching for changes) (see: [`config.go`])
  2. Starts watching for pod deletion events (see: [`watch.go`])
  3. Loads an initial state from the cluster's resources (see: `readClusterState` in [`state.go`])
  4. Spawns the HTTP server for handling `autoscaler-agent` requests (see: [`run.go`])

The plugins we implement are:

* **[Filter]** — preemptively discard nodes that don't have enough room for the pod
    * **[PreFilter]** and **[PostFilter]** — used for counts of total number of scheduling attempts
        and failures.
* **[Score]** — allows us to rank nodes based on available resources. It's called once for
  each pod-node pair, but we don't _actually_ use the pod.
* **[Reserve]** — gives us a chance to approve (or deny) putting a pod on a node, setting aside the
  resources for it in the process.

[Filter]: https://kubernetes.io/docs/concepts/scheduling-eviction/scheduling-framework/#filter
[PreFilter]: https://kubernetes.io/docs/concepts/scheduling-eviction/scheduling-framework/#pre-filter
[PostFilter]: https://kubernetes.io/docs/concepts/scheduling-eviction/scheduling-framework/#post-filter
[Score]: https://kubernetes.io/docs/concepts/scheduling-eviction/scheduling-framework/#scoring
[Reserve]: https://kubernetes.io/docs/concepts/scheduling-eviction/scheduling-framework/#reserve

For more information on scheduler plugins, see:
<https://kubernetes.io/docs/concepts/scheduling-eviction/scheduling-framework/>.

We support both VM pods and non-VM pods, in order to accommodate mixed deployments. We expect that
_all other resource usage_ is within the bounds of the configured per-node "system" usage, so it's
best to deploy as much as possible through the scheduler.

VM pods have an associated NeonVM `VirtualMachine` object, so we can fetch the resources from there.
For non-VM pods, we use the values from `resources.requests` for compatibility with other systems
(e.g., [cluster-autoscaler]). This can lead to overcommitting, but it isn't _really_ worth being
strict about this. If any container in a pod has no value for one of its resources, the pod will be
rejected; the scheduler doesn't have enough information to make accurate decisions.

[cluster autoscaler]: https://github.com/kubernetes/autoscaler

## Deep dive into resource management

Some basics:

1. **Different resources are handled independently.** This makes the implementation of the scheduler
   simpler, at the cost of relaxing guarantees about always allocating multiples of compute units.
   This is why `autoscaler-agent`s are responsible for making sure their resource requests are a
   multiple of the configured compute unit (although we _do_ check this).
2. **Resources are handled per-node.** This may be obvious, but it's worth stating explicitly.
   Whenever we talk about handling resources, we're only looking at what's available on a single
   node.

With those out of the way, there's a few things to discuss. In `state.go`, the relevant
resource-related types are:

```go
type nodeState struct {
    pods map[util.NamespacedName]*podState

    cpu nodeResourceState[vmapi.MilliCPU]
    mem nodeResourceState[api.Bytes]

    // -- other fields omitted --
}

// Total resources from all pods - both VM and non-VM
type nodeResourceState[T any] struct {
    Total     T
    Watermark T
    Reserved  T
    Buffer    T

    CapacityPressure     T
    PressureAccountedFor T
}

type podState struct {
    name util.NamespacedName

    // -- other fields omitted --
    cpu podResourceState[vmapi.MilliCPU]
    mem podResourceState[api.Bytes]
}

// Resources for a VM pod
type podResourceState[T any] struct {
    Reserved T
    Buffer   T

    CapacityPressure T
    
    Min T
    Max T
}
```

### Basics: `reserved` and `total`

At a high-level, `nodeResourceState.Reserved` provides an upper bound on the amount of each resource
that's currently allocated. `Total` is the total amount available, so, `Reserved` is _almost always_
less than or equal to `Total`.

During normal operations, we have a strict bound on resource usage in order to keep `Reserved ≤
Total`, but it isn't feasible to guarantee that in _all_ circumstances. In particular, this
condition can be temporarily violated [after startup](#startup-uncertainty-buffer).

### Pressure and watermarks

<!-- Note: this topic is also discussed in the root-level ARCHITECTURE.md -->

In order to preemptively migrate away VMs _before_ we run out of resources, we have a "watermark"
for each resource. When `Reserved > Watermark`, we start picking migration targets from the
migration queue (see: `updateMetricsAndCheckMustMigrate` in [`run.go`]). When `Reserved >
Watermark`, we refer to the amount above the watermark as the _logical pressure_ on the resource.

It's possible, however, that we can't react fast enough and completely run out of resources (i.e.
`Reserved == Total`). In this case, any requests that go beyond the maximum reservable
amount are marked as _capacity pressure_ (both in the node's `CapacityPressure` and the pod's).
Roughly speaking, `CapacityPressure` represents the amount of additional resources that will be
consumed as soon as they're available — we care about it because migration is slow, so we ideally
don't want to wait to start more migrations.

So: we have two components of resource pressure:

* _Capacity_ pressure — total requested resources we denied because they weren't available
* _Logical_ pressure — the difference `Reserved - Watermark` (or zero, if `Reserved ≤ Watermark`).

When a VM migration is started, we mark its `Reserved` resources _and_ `CapacityPressure` as
`PressureAccountedFor`. We continue migrating away VMs until those migrations account for all of the
resource pressure in the node.

---

In practice, this strategy means that we're probably over-correcting slightly when there's capacity
pressure: when capacity pressure occurs, it's probably the result of a _temporary_,
greater-than-usual increase, so we're likely to have started more migrations than we need in order
to cover it. In future, mechanisms to improve this could be:

1. Making `autoscaler-agent`s prefer more-frequent smaller increments in allocation, so that
   requests are less extreme and more likely to be sustained.
2. Multiplying `CapacityPressure` by some fixed ratio (e.g. 0.5) when calculating the total
   pressure to reduce impact — something less than one, but separately guaranteed to be != 0 if
   `CapacityPressure != 0`.
3. Artificially slowing down pod `CapacityPressure`, so that it only contributes to the node's
   `CapacityPressure` when sustained

In general, the idea is that moving slower and correcting later will prevent drastic adjustments.

### Startup uncertainty: `Buffer`

In order to stay useful after communication with the scheduler has failed, `autoscaler-agent`s will
continue to make scaling decisions _without_ checking with the plugin. These scaling decisions are
bounded by the last resource permit approved by the scheduler, so that they can still reduce unused
resource usage (we don't want users getting billed extra because of our downtime!).

This presents a problem, however: how does a new scheduler know what the old scheduler last
permitted? Without that, we can't determine an accurate upper bound on resource usage — at least,
until the `autoscaler-agent` reconnects to us. It's actually quite difficult to know what the
previous scheduler last approved, so we don't try! Instead, we work with the uncertainty.

On startup, we _assume_ all existing VM pods may scale — without notifying us — up to the VM's
configured maximum. So each VM pod gets `Reserved` equal to the VM's `<resource>.Max`. Alongside
that, we track `Buffer` — the expected difference between `Reserved` usage and actual usage: equal
to the VM's `<resource>.Max - <resource>.Use`.

As each `autoscaler-agent` reconnects, their first message contains their current resource usage, so
we're able to reduce `Reserved` appropriately and begin allowing other pods to be scheduled. When
this happens, we reset the pod's `Buffer` to zero.

Eventually, all `autoscaler-agent`s _should_ reconnect, and the node's `Buffer` is zero — meaning
that there's no longer any uncertainty about VM resource usage.

---

With `Buffer`, we have a more precise guarantee about resource usage:

> Assuming all `autoscaler-agent`s *and* the previous scheduler are well-behaved, then each node
> will always have `Reserved - Buffer ≤ Total`.
