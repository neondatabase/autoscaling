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
  * [Config changes](#config-changes)

## File descriptions

* `ARCHITECTURE.md` — this file :)
* [`config.go`] — definition of the `config` type, plus entrypoints for setting up update
  watching/handling and config validation.
* [`dumpstate.go`] — HTTP server, types, and conversions for dumping all internal state
* [`plugin.go`] — scheduler plugin interface implementations, plus type definition for
  `AutoscaleEnforcer`, the type implementing the `framework.*Plugin` interfaces.
* [`queue.go`] — implementation of a metrics-based priority queue to select migration targets. Uses
  `container/heap` internally.
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
* **[Score]** — allows us to rank nodes based on available resources. It's called once for
  each pod-node pair, but we don't _actually_ use the pod.
* **[Reserve]** — gives us a chance to approve (or deny) putting a pod on a node, setting aside the
  resources for it in the process.

[Filter]: https://kubernetes.io/docs/concepts/scheduling-eviction/scheduling-framework/#filter
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
strict about this. If a resource is not present in `resources.requests`, we fallback on
`resources.limits`. If any container in a pod has no value for one of its resources, the pod will be
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
    pods      map[PodName]*podState
    otherPods map[PodName]*otherPodState

    vCPU     nodeResourceState[uint16]
    memSlots nodeResourceState[uint16

    otherResources nodeOtherResourceState

    // -- other fields omitted --
}

// Total resources from *all* pods - both VM and non-VM
type nodeResourceState[T any] struct {
    total    T
    system   T
    reserved T
    buffer   T

    capacityPressure     T
    pressureAccountedFor T
}

// Total resources from non-VM pods
type nodeOtherResourceState struct {
	rawCpu    resource.Quantity
	rawMemory resource.Quantity

	reservedCpu      uint16
	reservedMemSlots uint16
}

type podState struct {
}

// Resources for a VM pod
type podResourceState[T any] struct {
    reserved T
    buffer   T

    capacityPressure T
}

// Resources for a non-VM pod
type podOtherResourceState struct {
	rawCpu    resource.Quantity
	rawMemory resource.Quantity
}
```

### Basics: `reserved`, `system`, and `total`

At a high-level, `nodeResourceState.reserved` provides an upper bound on the amount of each resource
that's currently allocated. `total` is the total amount available, and `system` is the amount
reserved for other functions that the scheduler _isn't responsible for_. So, `reserved` is _almost
always_ less than or equal to `total - system`.

During normal operations, we have a strict bound on resource usage in order to keep `reserved ≤
total - system`, but it isn't feasible to guarantee that in _all_ circumstances. In particular, this
condition can be temporarily violated [after startup] or [after config changes].

[after startup]: #startup-uncertainty-buffer
[after config changes]: #config-changes

### Non-VM pods

Let's briefly discuss handling non-VM pods. For a single non-VM pod, we store its CPU and memory
limits in an associated `podOtherResourceState`. The `nodeOtherResourceState` tracks the sum of all
non-VM pods' resource limits in `rawCpu` and `rawMemory`. It rounds `rawCpu` up to the next integer
in `reservedCpu` and `rawMemory` up to the next multiple of `config.MemSlotSize` in
`reservedMemSlots`.

The values of `reservedCpu` and `reservedMemSlots` are incorporated into the `nodeState`'s
`vCPU.reserved` and `memSlots.reserved`.

This setup means that we don't need to over-represent the resources used by each non-VM pod (like we
would if each pod's resources were in integer CPU and memory slots), while still guaranteeing that
we're appropriately factoring in their total usage.

### Pressure and watermarks

<!-- Note: this topic is also discussed in the root-level ARCHITECTURE.md -->

In order to preemptively migrate away VMs _before_ we run out of resources, we have a "watermark"
for each resource. When `reserved > watermark`, we start picking migration targets from the
migration queue (see: `updateMetricsAndCheckMustMigrate` in [`run.go`]). When `reserved >
watermark`, we refer to the amount above the watermark as the _logical pressure_ on the resource.

It's possible, however, that we can't react fast enough and completely run out of resources (i.e.
`reserved == total - system`). In this case, any requests that go beyond the maximum reservable
amount are marked as _capacity pressure_ (both in the node's `capacityPressure` and the pod's).
Roughly speaking, `capacityPressure` represents the amount of additional resources that will be
consumed as soon as they're available — we care about it because migration is slow, so we ideally
don't want to wait to start more migrations.

So: we have two components of resource pressure:

* _Capacity_ pressure — total requested resources we denied because they weren't available
* _Logical_ pressure — the difference `reserved - watermark` (or zero, if `reserved ≤ watermark`).

When a VM migration is started, we mark its `reserved` resources _and_ `capacityPressure` as
`pressureAccountedFor`. We continue migrating away VMs until those migrations account for all of the
resource pressure in the node.

---

In practice, this strategy means that we're probably over-correcting slightly when there's capacity
pressure: when capacity pressure occurs, it's probably the result of a _temporary_,
greater-than-usual increase, so we're likely to have started more migrations than we need in order
to cover it. In future, mechanisms to improve this could be:

1. Making `autoscaler-agent`s prefer more-frequent smaller increments in allocation, so that
   requests are less extreme and more likely to be sustained.
2. Multiplying `capacityPressure` by some fixed ratio (e.g. 0.5) when calculating the total
   pressure to reduce impact — something less than one, but separately guaranteed to be != 0 if
   `capacityPressure != 0`.
3. Artificially slowing down pod `capacityPressure`, so that it only contributes to the node's
   `capacityPressure` when sustained

In general, the idea is that moving slower and correcting later will prevent drastic adjustments.

### Startup uncertainty: `buffer`

In order to stay useful after communication with the scheduler has failed, `autoscaler-agent`s will
continue to make scaling decisions _without_ checking with the plugin. These scaling decisions are
bounded by the last resource permit approved by the scheduler, so that they can still reduce unused
resource usage (we don't want users getting billed extra because of our downtime!).

This presents a problem, however: how does a new scheduler know what the old scheduler last
permitted? Without that, we can't determine an accurate upper bound on resource usage — at least,
until the `autoscaler-agent` reconnects to us. It's actually quite difficult to know what the
previous scheduler last approved, so we don't try! Instead, we work with the uncertainty.

On startup, we _assume_ all existing VM pods may scale — without notifying us — up to the VM's
configured maximum. So each VM pod gets `reserved` equal to the VM's `<resource>.Max`. Alongside
that, we track `buffer` — the expected difference between `reserved` usage and actual usage: equal
to the VM's `<resource>.Max - <resource>.Use`.

As each `autoscaler-agent` reconnects, their first message contains their current resource usage, so
we're able to reduce `reserved` appropriately and begin allowing other pods to be scheduled. When
this happens, we reset the pod's `buffer` to zero.

Eventually, all `autoscaler-agent`s _should_ reconnect, and the node's `buffer` is zero — meaning
that there's no longer any uncertainty about VM resource usage.

---

With `buffer`, we have a more precise guarantee about resource usage:

> Assuming all `autoscaler-agent`s *and* the previous scheduler are well-behaved, then each node
> will always have `reserved - buffer ≤ total`. For each value of `system`, each node will
> eventually guarantee that future `reserved` will satisfy `reserved - buffer ≤ total - system`.

If the value of `system` changes, we can't immediately guarantee that our resource allocation
respects it, but it _eventually_ will. Some more consequences of configuration changes are
[discussed below](#config-changes).

### Config changes

The scheduler plugin supports changing its configuration at runtime, by watching the `ConfigMap`
object to allow faster updates. Because of this, there are some edge cases that are worth
discussing. (**note:** This would also be the case either way, because configs can change between
restarts.)

There are basically three configurable values that we care about, from a resource-management point
of view:

1. `nodeConfig.{Cpu,Memory}.Watermark`,
2. `nodeConfig.{Cpu,Memory}.System`, and
3. `nodeConfig.ComputeUnit`

These values are all configured on a per-node basis, so they can differ per-node and may change
between configurations.

Let's go through them one-by-one:

#### Changing `Watermark`

Here, there's nothing special to note. We don't store any values based on the watermark, so all
that's required is to update the calculated watermark.

#### Changing `System`

Typically, we won't have `reserved > total - system`. However, when `system` increases, this can be
temporarily violated. This will _also_ change the calculated watermark for the node (because the
_reservable_ amount of that resource has changed).

The end effect of this is that we can't assume anywhere that `reserved ≤ total - system`, which is
one of the reasons for having something like [`util.SaturatingSub`](../util/arith.go).

#### Changing `ComputeUnit`

All of the scheduler plugin's responses to `autoscaler-agent`s contain the current value of its
node's `ComputeUnit`, and the `ComputeUnit` may change at any time. So, for each VM pod, we track
the last `ComputeUnit` that we informed the `autoscaler-agent` of.

Whenever we receive a resource request, we check that it's a multiple of the _last_ `ComputeUnit`
that we sent it, not the current one — the `autoscaler-agent` can't have already known. The next
request will then have to be a multiple of the current `ComputeUnit`.
