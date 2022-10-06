# Scheduler Information

For now, this is a collection of miscellaneous notes that should help explain what's going on.

References:

 - [K8S - Creating a kube-scheduler plugin]
 - [How to import 'any' kubernetes package into your project]
 - [github: Scheduler Plugins]
     - [Install Scheduler-plugins]
 - [k8s docs: Scheduling Framework]
 

[K8S - Creating a kube-scheduler plugin]: https://medium.com/@juliorenner123/k8s-creating-a-kube-scheduler-plugin-8a826c486a1
[How to import 'any' kubernetes package into your project]: https://suraj.io/post/2021/05/k8s-import/
[github: Scheduler Plugins]: https://github.com/kubernetes-sigs/scheduler-plugins
[Install Scheduler-plugins]: https://github.com/kubernetes-sigs/scheduler-plugins/blob/master/doc/install.md
[k8s docs: Scheduling Framework]: https://kubernetes.io/docs/concepts/scheduling-eviction/scheduling-framework/

## Summary of ideas

The basic issue with integrating VM migration and autoscaling into the scheduler is that we can't
*directly* add to the scheduling queue from within the plugin (i.e., there isn't anywhere we can
just modify the scheduling queue directly, so we have to make an API call).

What we *can* do is some kind of filtering / binding modification based on VM states. Here's a
hypothetical series of actions, within & outside of the scheduler, modelling what should happen as
we migrate a VM to make room for another's upscaling:

 1. Label the upscaling VM as `vm-A` and the one we're kicking out to make room as `vm-B`. VMs A and
    B are on the same node.
 2. Create a new pod for the receiving pseudo-VM that we're migrating into (call it `vm-C`)
     * This also includes waiting for the pod to get bound to a node.
 3. Transfer the VM state from `vm-B` to `vm-C` (i.e., perform the migration)
 4. Delete the pod for `vm-B` and *at the same time* upscale the pod for `vm-A`, using up some of
    the space freed by removing `vm-B` from the node
     * After upscaling the *pod*, we can do the same for the VM itself

Edge cases to consider:

 * Step 2 can bind `vm-C` to the same node as `vm-B`, if something was removed between the creation
   request and the scheduler's binding
 * \[optimization\]: In between steps 2 and 4, there might be other unrelated pods removed from
   `vm-A/B`'s node, freeing up space for `vm-A` to expand *without* migrating `vm-B`
   * Note: we need to be careful not to take advantage of the removal of a pod removed from *some
       other* migration to make room for a *different* scale-up.

Finishing thoughts:

 * Basically, it seems pretty clear to me that this can't really be done without patching or
     extending kubernetes itself. Thankfully, there's a kubernetes PR for this:
     https://github.com/kubernetes/kubernetes/pull/102884
 * The "pseudo-VM we're migrating into" is the `VirtualMachineMigration` object from
     virtink AFAICT?
   * Related PR from virtink: https://github.com/smartxworks/virtink/pull/59

## Information about plugins (see below)

[K8S - Creating a kube-scheduler plugin]\:

> Saying it in a few words, the K8S scheduler is responsible for assigning *Pods* to *Nodes*. Once a
> new pod is created it gets in the scheduling queue. The attempt to schedule a pod is split in two
> phases: the *Scheduling* and the *Binding* cycle

![Diagram showing steps in the scheduling & binding cycles, with extensible vs internal components marked](https://d33wubrfki0l68.cloudfront.net/4e9fa4651df31b7810c851b142c793776509e046/61a36/images/docs/scheduling-framework-extensions.png)

> The kube-scheduler is implemented in Golang and *Plugins* are included to it in compilation time.
> Therefore, if you want to have your own plugin, you will need to have your own scheduler image

So it's clear that we're doing some funky stuff with a modified scheduler build. This _is_
explicitly supported, but outside the most common use cases. The post above uses
`k8s.io/kubernetes/cmd/kube-scheduler/app` in the import to create a new scheduler, so that's what
we use, right? It's here that we run into the first problem:

```console
go get k8s.io/kubernetes/cmd/kube-scheduler
go: downloading k8s.io/component-base v0.0.0
... skipping ...
go: downloading k8s.io/cloud-provider v0.0.0
go: downloading k8s.io/mount-utils v0.0.0
k8s.io/kubernetes/cmd/kube-scheduler imports
	k8s.io/component-base/cli: reading k8s.io/component-base/go.mod at revision v0.0.0: unknown revision v0.0.0
k8s.io/kubernetes/cmd/kube-scheduler imports
	k8s.io/component-base/logs/json/register: reading k8s.io/component-base/go.mod at revision v0.0.0: unknown revision v0.0.0
... skipping ...
k8s.io/kubernetes/cmd/kube-scheduler imports
	k8s.io/kubernetes/cmd/kube-scheduler/app imports
	k8s.io/kubernetes/pkg/scheduler imports
	k8s.io/kubernetes/pkg/scheduler/framework/plugins imports
	k8s.io/kubernetes/pkg/scheduler/framework/plugins/nodevolumelimits imports
	k8s.io/kubernetes/pkg/volume/util imports
	k8s.io/kubernetes/pkg/volume imports
	k8s.io/kubernetes/pkg/proxy/util imports
	k8s.io/component-helpers/node/util/sysctl: reading k8s.io/component-helpers/go.mod at revision v0.0.0: unknown revision v0.0.0
k8s.io/kubernetes/cmd/kube-scheduler imports
	k8s.io/kubernetes/cmd/kube-scheduler/app imports
	k8s.io/kubernetes/pkg/scheduler imports
	k8s.io/kubernetes/pkg/scheduler/framework/plugins imports
	k8s.io/kubernetes/pkg/scheduler/framework/plugins/nodevolumelimits imports
	k8s.io/kubernetes/pkg/volume/util imports
	k8s.io/kubernetes/pkg/volume imports
	k8s.io/kubernetes/pkg/volume/util/recyclerclient imports
	k8s.io/apimachinery/pkg/watch: reading k8s.io/apimachinery/go.mod at revision v0.0.0: unknown revision v0.0.0
```

Basically, a simple `go get` won't cut it. Thankfully, [people] have made [bash scripts] to sort this
out for us:

[people]: https://github.com/kubernetes/kubernetes/issues/79384#issuecomment-521493597
[bash scripts]: https://suraj.io/post/2021/05/k8s-import/

## Notes for plugin points

Framework types from <https://pkg.go.dev/k8s.io/kubernetes@v1.25.2/pkg/scheduler/framework>.
Descriptions taken from <https://kubernetes.io/docs/concepts/scheduling-eviction/scheduling-framework/#extension-points>.

| Name | Framework type name | Description |
|------|---------------------|-------------|
| `QueueSort` | [`QueueSortPlugin`] | These plugins are used to sort Pods in the scheduling queue. A queue sort plugin essentially provides a `Less(Pod1, Pod2)` function. Only one queue sort plugin may be enabled at a time. |
| `PreFilter` | [`PreFilterPlugin`] | These plugins are used to pre-process info about the Pod, or to check certain conditions that the cluster or the Pod must meet. If a PreFilter plugin returns an error, the scheduling cycle is aborted. |
| `Filter` | [`FilterPlugin`] | These plugins are used to filter out nodes that cannot run the Pod. For each node, the scheduler will call filter plugins in their configured order. If any filter plugin marks the node as infeasible, the remaining plugins will not be called for that node. Nodes may be evaluated concurrently. |
| `PostFilter` | [`PostFilterPlugin`] | These plugins are called after Filter phase, but only when no feasible nodes were found for the pod. Plugins are called in their configured order. If any postFilter plugin marks the node as `Schedulable`, the remaining plugins will not be called. A typical PostFilter implementation is preemption, which tries to make the pod schedulable by preempting other Pods. |
| `PreScore` | [`PreScorePlugin`] | These plugins are used to perform "pre-scoring" work, which generates a sharable state for Score plugins to use. If a PreScore plugin returns an error, the scheduling cycle is aborted. |
| `Score` | [`ScorePlugin`] | These plugins are used to rank nodes that have passed the filtering phase. The scheduler will call each scoring plugin for each node. There will be a well defined range of integers representing the minimum and maximum scores. After the NormalizeScore phase, the scheduler will combine node scores from all plugins according to the configured plugin weights. |
| `NormalizeScore` | [`NormalizeScorePlugin`] | These plugins are used to modify scores before the scheduler computes a final ranking of Nodes. A plugin that registers for this extension point will be called with the Score results from the same plugin. This is called once per plugin per scheduling cycle. |
| `Reserve` | [`ReservePlugin`] | A plugin that implements the Reserve extension has two methods, namely `Reserve` and `Unreserve`, that back two informational scheduling phases called Reserve and Unreserve, respectively. Plugins which maintain runtime state (aka "stateful plugins") should use these phases to be notified by the scheduler when resources on a node are being reserved and unreserved for a given Pod. |
| `Permit` | [`PermitPlugin`] | *Permit* plugins are invoked at the end of the scheduling cycle for each Pod, to prevent or delay the binding to the candidate node. A permit plugin can do one of the three things: approve, deny, or wait. |
| `PreBind` | [`PreBindPlugin`] | These plugins are used to perform any work required before a Pod is bound. For example, a pre-bind plugin may provision a network volume and mount it on the target node before allowing the Pod to run there. |
| `Bind` | [`BindPlugin`] | These plugins are used to bind a Pod to a Node. Bind plugins will not be called until all PreBind plugins have completed. Each bind plugin is called in the configured order. A bind plugin may choose whether or not to handle the given Pod. |
| `PostBind` | [`PostBindPlugin`] | This is an informational extension point. Post-bind plugins are called after a Pod is successfully bound. This is the end of a binding cycle, and can be used to clean up associated resources. |

[`QueueSortPlugin`]: https://pkg.go.dev/k8s.io/kubernetes@v1.25.2/pkg/scheduler/framework#QueueSortPlugin
[`PreFilterPlugin`]: https://pkg.go.dev/k8s.io/kubernetes@v1.25.2/pkg/scheduler/framework#PreFilterPlugin
[`FilterPlugin`]: https://pkg.go.dev/k8s.io/kubernetes@v1.25.2/pkg/scheduler/framework#FilterPlugin
[`PostFilterPlugin`]: https://pkg.go.dev/k8s.io/kubernetes@v1.25.2/pkg/scheduler/framework#PostFilterPlugin
[`PreScorePlugin`]: https://pkg.go.dev/k8s.io/kubernetes@v1.25.2/pkg/scheduler/framework#PreScorePlugin
[`ScorePlugin`]: https://pkg.go.dev/k8s.io/kubernetes@v1.25.2/pkg/scheduler/framework#ScorePlugin
[`NormalizeScorePlugin`]: https://pkg.go.dev/k8s.io/kubernetes@v1.25.2/pkg/scheduler/framework#NormalizeScorePlugin
[`ReservePlugin`]: https://pkg.go.dev/k8s.io/kubernetes@v1.25.2/pkg/scheduler/framework#ReservePlugin
[`PermitPlugin`]: https://pkg.go.dev/k8s.io/kubernetes@v1.25.2/pkg/scheduler/framework#PermitPlugin
[`PreBindPlugin`]: https://pkg.go.dev/k8s.io/kubernetes@v1.25.2/pkg/scheduler/framework#PreBindPlugin
[`BindPlugin`]: https://pkg.go.dev/k8s.io/kubernetes@v1.25.2/pkg/scheduler/framework#BindPlugin
[`PostBindPlugin`]: https://pkg.go.dev/k8s.io/kubernetes@v1.25.2/pkg/scheduler/framework#PostBindPlugin
