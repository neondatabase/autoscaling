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
  $ curl https://raw.githubusercontent.com/flannel-io/flannel/v0.19.2/Documentation/kube-flannel.yml \
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
  PATH="deployments/multus-daemonset.yml"
  curl "https://raw.githubusercontent.com/$REPO/master/$PATH" -o multus-daemonset.yaml
  # as root:
  kubectl apply -f multus-daemonset.yaml
  ```
  This adds a `kube-system` pod:
  ```console
  $ kubectl get pods --all-namespaces
  NAMESPACE            NAME                                            READY   STATUS    RESTARTS   AGE
  ... skipped ...
  kube-system          kube-multus-ds-r2xvp                            1/1     Running   0          21s
  ... skipped ...
  ```

## Option A: Continue with an existing Postgres VM image

11. Using the neighboring file `sample-vm-postgres-disk.yaml`, run:
  ```sh
  kubectl apply -f sample-vm-postgres-disk.yaml
  ```
  `sample-vm-postgres-disk.yaml` is mostly copied from our own [`postgres14-disk.yaml`], with the
  internal names changed slightly.

  [`postgres14-disk.yaml`]: https://github.com/neondatabase/autoscaling/blob/main/vm_template_samples/postgres14-disk.yaml

And now the VM should be up and running!

## Option B: Continue with a locally built VM image

11. Make sure that the local regsitry is running, with `vm_image/start-local-registry.sh`:
    ```sh
    vm_image/start-local-registry.sh
    ```
    It'll output the hash of the `registry` image it's running. If you're repeating steps, you don't
    need to run this script again.

11. Run `vm_image/build.sh` to create a new VM image. This runs a few different steps, and the
    [README](./vm_image) in there has some more information. Of note: it generates ssh keys, adds
    the public key to the VM's `authorized_keys`, and disables password authentication. It will
    look something like this:
    ```console
    $ vm_image/build.sh
    Generating new keypair...
    Enter passphrase (empty for no passphrase):
    Enter same passphrase again:
        Generating public/private rsa key pair.
        Your identification has been saved in ssh_id_rsa
        Your public key has been saved in ssh_id_rsa.pub
        The key fingerprint is:
        -- omitted --
        The key's randomart image is:
        -- omitted --
    Building 'Dockerfile.vmdata'...
        -- omitted (sha256 hash) --
    Creating disk.raw ext4 filesystem...
    > dd:
        1+0 records in
        1+0 records out
        1 byte copied, 5.0271e-05 s, 19.9 kB/s
    > mkfs.ext4:
        mke2fs 1.46.5 (30-Dec-2021)
        Discarding device blocks: done
        Creating filesystem with 524288 4k blocks and 131072 inodes
        Filesystem UUID: c88a7fb9-d2ee-400b-b293-da87c2df8cba
        Superblock backups stored on blocks:
        	32768, 98304, 163840, 229376, 294912
    
        Allocating group tables: done
        Writing inode tables: done
        Creating journal (16384 blocks): done
        Writing superblocks and filesystem accounting information: done
    
    Mount 'disk.raw' at ./tmp.7U7uiSD0X3
    Exporting vmdata image into disk.raw
    Unmount ./tmp.7U7uiSD0X3
    Clean up temporary docker artifacts
        -- omitted (sha256 hash) --
        Untagged: vmdata-build:latest
        -- omitted (many "Deleted: <hash>") --
    Convert 'disk.raw' -> 'disk.qcow2'
    Clean up 'disk.raw'
    Build final 'Dockerfile.img'...
        -- omitted (sha256 hash) --
    Push completed image
        localhost:5001/pg14-disk-test:latest
    ```

12. \[Optional\] Add the ssh private key as a secret, so that `ssh-into-vm.sh` works. Alternatively,
    you can wait to add the secret until you need it.
    ```console
    $ kubectl create secret generic vm-ssh --from-file=private-key=vm_image/ssh_id_rsa
    secret/vm-ssh created
    ```
    Check that it was created ok:
    ```console
    $ kubectl describe secret vm-ssh
    Name:         vm-ssh
    Namespace:    default
    Labels:       <none>
    Annotations:  <none>
    
    Type:  Opaque
    
    Data
    ====
    private-key:  2602 bytes
    ```
13. With everything built, launch the VM image:
    ```console
    $ kubectl apply -f local-vm-postgres-disk.yaml
    networkattachmentdefinition.k8s.cni.cncf.io/static-disk created
    virtualmachine.virt.virtink.smartx.com/postgres14-disk created
    service/postgres-connect created
    ```
    Everything should now work. Try `ssh-into-vm.sh` to access it directly, `run-bench.sh` to put
    some load on Postgres, or `start-autoscaler.sh` to start the autoscaling script running.
14. Trying `ssh-into-vm.sh` (requires the `vm-ssh` secret from before):
    ```console
    $ ./ssh-into-vm.sh
    get vmPodName (vm_name = postgres14-disk)
    get pod ip (pod = vm-postgres14-disk-wtlwm)
    pod_ip = 10.244.0.41
    If you don't see a command prompt, try pressing enter.
    fetch https://dl-cdn.alpinelinux.org/alpine/v3.16/community/x86_64/APKINDEX.tar.gz
    (1/6) Installing openssh-keygen (9.0_p1-r2)
    (2/6) Installing ncurses-terminfo-base (6.3_p20220521-r0)
    (3/6) Installing ncurses-libs (6.3_p20220521-r0)
    (4/6) Installing libedit (20210910.3.1-r0)
    (5/6) Installing openssh-client-common (9.0_p1-r2)
    (6/6) Installing openssh-client-default (9.0_p1-r2)
    Executing busybox-1.35.0-r17.trigger
    OK: 11 MiB in 20 packages
    Warning: Permanently added '10.244.0.41' (ED25519) to the list of known hosts.
    Welcome to Alpine!
     ~ This is the VM :) ~
    cloud-hypervisor:~#
    ```
    **Note**: The VM's hostname is `cloud-supervisor`. This is set by kubernetes, and we *could*
    change it in the init script if we wanted, but technically speaking the running container *is*
    the `cloud-supervisor` image; it's just mounted the VM data alongside it.
15. Run `run-bench.sh` and `start-autoscaler.sh` in separate sessions to watch it scale up :)
