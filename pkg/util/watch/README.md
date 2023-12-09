# `util/watch` explainer

Here's a high-level overview of `.../util/watch` - our abstraction for the Kubernetes Watch API.

Our implementation is similar to the [`client-go` watch implementation], with some notable
differences:

1. client-go's is type-erased; ours is generic
2. client-go's periodically "resyncs" when necessary, but does not provide the external control or
   ability to be notified on resync completion that would be necessary to use it. Ours provides
   explicit control for "relisting" (fundamentally the same as "resyncing", just a different name),
   with notification when relisting has finished.
3. Ours has some additional features:
    1. Support for custom indexes
    2. The ability to choose when the handlers the initial listing are called (before or after
       returning; see `watch.InitMode`)
    3. Metrics describing the API calls, their results, and the current state of the watch (e.g.
       whether it's healthy)

[`client-go` watch implementation]: https://pkg.go.dev/k8s.io/client-go/tools/watch

---

## How watching generally works

At a high level, watching _generally_ works by first calling `List` to fetch the current state of
the items, and then calling `Watch` _with the [resource version] from listing_ to stream the changes
to the objects since the initial fetch.

If the `Watch` stream closes, we create a new one from the latest resource version, and on error, we
call `List` again ("relist") and retry the `Watch`. This commonly occurs when the resource version
is too old.

[resource version]: https://kubernetes.io/docs/reference/using-api/api-concepts/#resource-versions

So, our general flow looks like:

```
for item in List() {
    update store with item
}
stream := Watch()
loop {
    // stream events...
    loop {
        item, ok := <-stream
        if !ok {
            // stream closed; watch ended.
            goto rewatch
        }

        if item is error {
            goto relist
        }

        update store with item
    }

  relist:
    for item in List() {
        update store with item
    }

  rewatch:
    stream = Watch()
}
```

Note that because resource versions are opaque strings (even if they _happen_ to look a lot like
monotonically increasing integers), we must start a new `Watch` every time we call `List`, because
we have no way to tell the ordering of events except in relation to each other.

## The basic interface

Calls to `watch.Watch` internally produce an event stream. These are exposed with
`watch.HandlerFuncs`, a set of callbacks used on every event — whether that comes from updating the
store with `List` or the actual `Watch` events themselves.

## Our changes: Relisting

Relisting was originally motivated by the scheduler: There, we needed the ability to associate an
incoming Pod with the VirtualMachine that owns it. Fundamentally, this is racy if we're only using
watch: it's possible to get an event about the Pod before we get any events about the
VirtualMachine.

In order to provide an explicit ordering here, we have a support for externally triggering a
"relist" — forcing an ordering on the store's contents by calling `List` again.

The details of this implementation can be found in the relisting portion of the `watch.Watch`
function, and in `(*watch.Store[T]).Relist()`.

## Our changes: Custom indexes

In various places in the codebase, it's been useful to have mappings that are updated for each watch
event, so that we can amortize the cost of lookups (rather than incurring a linear scan over all
items every time we want to find a particular one, or set of them).

While indexes _could_ be implemented inside of the callbacks, there's explicit support provided for
them by the `watch.Store`, which allows lookups into the indexes to share the same lock as fetches
from the full list of items in the `watch.Store`.

Some examples of indexes:

- Indexing by namespace+name, for efficient lookup of a single item (`watch.NameIndex`)
- Indexing by the node a VirtualMachine is on, to efficiently fetch all VMs on a node
    (`pkg/agent/billing.VMNodeIndex`)

... and a couple others used elsewhere :)
