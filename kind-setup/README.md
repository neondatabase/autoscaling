# kind-setup

Instructions for setting up [`kind`] running locally to use the virtink stuff. Mostly just notes for
myself.

Note: some of the steps here might not be *strictly* necessary. I haven't thinned the list out to
check.

[`kind`]: https://github.com/kubernetes-sigs/kind

0. Make sure you have hardware virtualization enabled on the host machine! This is most likely a
   BIOS setting. You can see by checking if `/dev/kvm` exists.

1. Install `kind`

  On Arch Linux, this looks like `$AUR-HELPER -S kind docker`. (`docker` is there in case it's not
  already installed, because `kind` requires either Docker or Podman, but Podman support is
  experimental).

2. Download the CNI binaries:
   ```console
   $ ./download-cni.sh
   Fetching latest release from https://github.com/containernetworking/plugins...
     % Total    % Received % Xferd  Average Speed   Time    Time     Time  Current
                                    Dload  Upload   Total   Spent    Left  Speed
   100 51940  100 51940    0     0  95095      0 --:--:-- --:--:-- --:--:-- 95128
   Downloading 'https://github.com/containernetworking/plugins/releases/download/v1.1.1/cni-plugins-linux-amd64-v1.1.1.tgz'...
     % Total    % Received % Xferd  Average Speed   Time    Time     Time  Current
                                    Dload  Upload   Total   Spent    Left  Speed
     0     0    0     0    0     0      0      0 --:--:-- --:--:-- --:--:--     0
   100 34.6M  100 34.6M    0     0  31.7M      0  0:00:01  0:00:01 --:--:-- 48.8M
   Downloading sha256 for 'cni-plugins-linux-amd64-v1.1.1.tgz'...
     % Total    % Received % Xferd  Average Speed   Time    Time     Time  Current
                                    Dload  Upload   Total   Spent    Left  Speed
     0     0    0     0    0     0      0      0 --:--:-- --:--:-- --:--:--     0
   100   101  100   101    0     0    236      0 --:--:-- --:--:-- --:--:--   236
   Unpacking into cni-bin...
   ```

2. Start a cluster (using local config, which requires CNI binaries):
  ```console
  $ sudo kind create cluster --name vmtest --config=kind-config.yaml
  Creating cluster "vmtest" ...
   ✓ Ensuring node image (kindest/node:v1.25.2) 🖼
   ✓ Preparing nodes 📦
   ✓ Writing configuration 📜
   ✓ Starting control-plane 🕹️
   ✓ Installing StorageClass 💾
  Set kubectl context to "kind-vmtest"
  You can now use your cluster with:
  
  kubectl cluster-info --context kind-vmtest
  
  Have a nice day! 👋
  ```

3. Check in on it:
  ```console
  $ sudo kubectl cluster-info --context kind-vmtest
  Kubernetes control plane is running at https://127.0.0.1:34421
  CoreDNS is running at https://127.0.0.1:34421/api/v1/namespaces/kube-system/services/kube-dns:dns/proxy
  
  To further debug and diagnose cluster problems, use 'kubectl cluster-info dump'.
  ```

4. So that we don't have to keep typing things, let's leave a root shell running and set the
  `kubectl` context:
  ```console
  $ sudo su
  $ kubectl config set-context kind-vmtest
  Context "kind-vmtest" modified.
  ```

5. We can see the currently-running cluster:
  ```console
  $ kind get clusters
  vmtest
  ```
  By default, the name is `kind`, but in this example we've named ours `vmtest`. To do this, we have
  to pass `--name vmtest` in all of the `kind` commands (e.g., creation, deletion, etc.). If you
  change it, make sure to change the `kubectl config set-context` above to use `kind-<NAME>`.

6. And we can also see the things running in it:
  ```console
  $ kubectl get pods --all-namespaces
  NAMESPACE            NAME                                           READY   STATUS    RESTARTS   AGE
  kube-system          coredns-565d847f94-dd6np                       0/1     Pending   0          17s
  kube-system          coredns-565d847f94-lcz8t                       0/1     Pending   0          17s
  kube-system          etcd-vmtest-control-plane                      1/1     Running   0          31s
  kube-system          kube-apiserver-vmtest-control-plane            1/1     Running   0          34s
  kube-system          kube-controller-manager-vmtest-control-plane   1/1     Running   0          34s
  kube-system          kube-proxy-stls9                               1/1     Running   0          17s
  kube-system          kube-scheduler-vmtest-control-plane            1/1     Running   0          31s
  local-path-storage   local-path-provisioner-684f458cdd-fcs5g        0/1     Pending   0          17s
  ```
  You might notice that the `coredns` and `local-path-provisioner` pods are currently pending.
  They'll stay that way until we enable `flannel`, because we have the default networking (via
  `kindnet`) disabled in the config file.

7. Install `flannel` for networking:
  ```console
  $ curl https://raw.githubusercontent.com/coreos/flannel/master/Documentation/kube-flannel.yml \
      -o flannel.yaml
  ... skipped ...
  $ kubectl apply -f flannel.yaml
  namespace/kube-flannel created
  clusterrole.rbac.authorization.k8s.io/flannel created
  clusterrolebinding.rbac.authorization.k8s.io/flannel created
  serviceaccount/flannel created
  configmap/kube-flannel-cfg created
  daemonset.apps/kube-flannel-ds created
  ```
  Now we can see the previously pending `kube-system` (and `local-path-storage`) pods are running,
  and there's a new pod in `kube-flannel` to run our networking:
  ```console
  $ kubect get pods --all-namespaces
  kube-flannel         kube-flannel-ds-slngb                          1/1     Running   0          68s
  kube-system          coredns-565d847f94-dd6np                       1/1     Running   0          2m53s
  kube-system          coredns-565d847f94-lcz8t                       1/1     Running   0          2m53s
  kube-system          etcd-vmtest-control-plane                      1/1     Running   0          3m7s
  kube-system          kube-apiserver-vmtest-control-plane            1/1     Running   0          3m10s
  kube-system          kube-controller-manager-vmtest-control-plane   1/1     Running   0          3m10s
  kube-system          kube-proxy-stls9                               1/1     Running   0          2m53s
  kube-system          kube-scheduler-vmtest-control-plane            1/1     Running   0          3m7s
  local-path-storage   local-path-provisioner-684f458cdd-fcs5g        1/1     Running   0          2m53s
  ```

At this point, it's clear the cluster is up and running ok. Let's continue with the virtink-related
setup:

8. Install `cert-manager`:
  ```sh
  # as user:
  curl -L 'https://github.com/cert-manager/cert-manager/releases/download/v1.8.2/cert-manager.yaml' \
       -o cert-manager.yaml
  # as root:
  kubectl apply -f cert-manager.yaml
  ```
  This should add the following pods:
  ```console
  $ k3s kubectl get pods --all-namespaces
  NAMESPACE            NAME                                         READY   STATUS    RESTARTS   AGE
  cert-manager         cert-manager-7778d64785-hwdkq                1/1     Running   0          32s
  cert-manager         cert-manager-cainjector-5c7b85f464-nqwbr     1/1     Running   0          32s
  cert-manager         cert-manager-webhook-58b97ccf69-4tb4p        1/1     Running   0          32s
  ... skipped ...
  ```

9. Install Virtink:

  We're actually using a modified version of [the original]:
  ```sh
  # as user:
  cp ../virtink_tempo/virtink_sergeyneon.yaml virtink-sergey.yaml
  # as root:
  kubectl apply -f virtink-sergey.yaml
  ```

  [the original]: https://github.com/smartxworks/virtink/releases/download/v0.11.0/virtink.yaml

  **Note:** If you try to run this before `cert-manager` is fully running, pieces of it will fail.
  If that happens, you can just re-run the `kubectl apply` step.

  This adds some more pods as well:
  ```console
  $ kubectl get pods --all-namespaces
  NAMESPACE            NAME                                         READY   STATUS    RESTARTS   AGE
  ... skipped ...
  virtink-system       virt-controller-75cdf594d7-jpmmg             1/1     Running   0          22s
  virtink-system       virt-daemon-hh8bz                            1/1     Running   0          22s
  ```

Before we continue with a Postgres VM image, install the CRD for `NetworkAttachmentDefinition`:

10. This is provided by [`multus-cni`]:
  ```sh
  # as user:
  REPO="k8snetworkplumbingwg/multus-cni"
  PATH="deployments/multus-daemonset-thick.yml"
  curl "https://raw.githubusercontent.com/$REPO/master/$PATH" -o multus-daemonset-thick.yaml
  # as root:
  kubectl apply -f multus-daemonset-thick.yaml
  ```
  This adds a `kube-system` pod:
  ```console
  $ kubectl get pods --all-namespaces
  NAMESPACE            NAME                                            READY   STATUS    RESTARTS   AGE
  ... skipped ...
  kube-system          kube-multus-ds-r2xvp                            1/1     Running   0          21s
  ... skipped ...
  ```

Continue with an existing Postgres VM image:

11. Using the neighboring file `sample-vm-postgres-disk.yaml`, run:
  ```sh
  kubectl apply -f sample-vm-postgres-disk.yaml
  ```
  `sample-vm-postgres-disk.yaml` is mostly copied from our own [`postgres14-disk.yaml`], with the
  internal names changed slightly.

  [`postgres14-disk.yaml`]: https://github.com/neondatabase/autoscaling/blob/main/vm_template_samples/postgres14-disk.yaml

And now the VM should be up and running!

## Future work

Looks like it's probably super feasible to build container/VM images locally & use them in `kind` --
it _does_ involve [creating a local registry] though. For playing around with the scheduler, this
seems pretty crucial to me, because modified scheduler builds are supplied via a container image
containing the scheduler.

[creating a local registry]: https://kind.sigs.k8s.io/docs/user/local-registry/
