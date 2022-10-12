# Kubernetes cluster for live migration experiments

Simple Bash-script to create K3S cluster in AWS

## Configration

Copy file [settings.local.example](settings.local.example) to `settings.local` and override
necessary variables there (usually AWS_NAME, EC2_TYPE, K3S_CLUSTER_SIZE).

Settings:

- `AWS_PROFILE` - named profile for [AWS CLI](https://docs.aws.amazon.com/cli/latest/userguide/cli-configure-profiles.html)
- `AWS_REGION` - AWS region where cluster will be created
- `AWS_NAME` - name for all AWS resources and kubernrtes cluster
- `EC2_TYPE` - EC2 instace type for kubernetes nodes
- `AWS_VPC_CIDR` - AWS VPC Classless Inter-Domain Routing (CIDR) block.
- `AWS_SUBNET_CIDR` - AWS Subnet CIDR reservation
- `K3S_CLUSTER_SIZE` - how many nodes in cluster
- `K3S_TOKEN` - token used when k3s nodes join to the cluster
- `K3S_MASTER_SCRIPT` - bash-script used for kubernetes master node provision
- `K3S_WORKER_SCRIPT` - bash-script used for kubernetes worker node provision

## Create/destroy/manage cluster

### Script usage

Run script without arguments

```console
% ./aws_k3s.sh 

=== Dev AWS infra and K3S cluster ===

Usage: aws_k3s.sh <command>

Commands:

    help	display this help and exit

    create	create cluster
    destroy	destroy cluster
    status	check current state

```

### Create cluster

Run `./aws_k3s.sh create`

<details><summary>example output</summary>

```console

=== Dev AWS infra and K3S cluster ===

Retrieving data from AWS
Adding VPC resources
   create VPC
   tag VPC
   create subnet
   set subnet as public
   tag subnet
   create  internet gateway
   tag gateway
   attach gateway to VPC
   obtain default route table
   tag route table
   add default route tp gateway
   attach subnet to route table
   create security group
   tag group
   add open rule to group
   create ssh keypair
done
Starting 'andrey-lab-1' instance
   obtain AMI id for Ubuntu 22.04
   run instance
   tag instance
   wait while instance initalized
   .
   instance started (id: i-00aabe3ef8e330ff1)
done
Starting 'andrey-lab-2' instance
   obtain AMI id for Ubuntu 22.04
   run instance
   tag instance
   wait while instance initalized
   .
   instance started (id: i-0de80cb236f426a05)
done
Starting 'andrey-lab-3' instance
   obtain AMI id for Ubuntu 22.04
   run instance
   tag instance
   wait while instance initalized
   .
   instance started (id: i-0fdd5ab8767ce0061)
done
Waiting 'andrey-lab-1' instance become ready
   check ssh connection
   .
   connected
done
Waiting 'andrey-lab-2' instance become ready
   check ssh connection
   ..
   connected
done
Waiting 'andrey-lab-3' instance become ready
   check ssh connection
   ...
   connected
done
Provisioning 'andrey-lab-1' instance
setup hostname

install necessary deps

allow routing
net.ipv4.ip_forward = 1

create linux bridge interface 'vm-bridge0' if not exist

create VXLAN interface 'vm-vxlan0' if not exist
appending 10.10.10.61 to VXLAN FDB
appending 10.10.10.22 to VXLAN FDB

install cni plugins

bootstap k3s master node
[INFO]  Finding release for channel stable
[INFO]  Using v1.24.6+k3s1 as release
[INFO]  Downloading hash https://github.com/k3s-io/k3s/releases/download/v1.24.6+k3s1/sha256sum-amd64.txt
[INFO]  Downloading binary https://github.com/k3s-io/k3s/releases/download/v1.24.6+k3s1/k3s
[INFO]  Verifying binary download
[INFO]  Installing k3s to /usr/local/bin/k3s
[INFO]  Skipping installation of SELinux RPM
[INFO]  Creating /usr/local/bin/kubectl symlink to k3s
[INFO]  Creating /usr/local/bin/crictl symlink to k3s
[INFO]  Creating /usr/local/bin/ctr symlink to k3s
[INFO]  Creating killall script /usr/local/bin/k3s-killall.sh
[INFO]  Creating uninstall script /usr/local/bin/k3s-uninstall.sh
[INFO]  env: Creating environment file /etc/systemd/system/k3s.service.env
[INFO]  systemd: Creating service file /etc/systemd/system/k3s.service
[INFO]  systemd: Enabling k3s unit
Created symlink /etc/systemd/system/multi-user.target.wants/k3s.service → /etc/systemd/system/k3s.service.
[INFO]  systemd: Starting k3s

install some useful k8s addons
waiting for /var/lib/rancher/k3s/server/manifests
poddisruptionbudget.policy/calico-kube-controllers created
serviceaccount/calico-kube-controllers created
serviceaccount/calico-node created
configmap/calico-config created
customresourcedefinition.apiextensions.k8s.io/bgpconfigurations.crd.projectcalico.org created
customresourcedefinition.apiextensions.k8s.io/bgppeers.crd.projectcalico.org created
customresourcedefinition.apiextensions.k8s.io/blockaffinities.crd.projectcalico.org created
customresourcedefinition.apiextensions.k8s.io/caliconodestatuses.crd.projectcalico.org created
customresourcedefinition.apiextensions.k8s.io/clusterinformations.crd.projectcalico.org created
customresourcedefinition.apiextensions.k8s.io/felixconfigurations.crd.projectcalico.org created
customresourcedefinition.apiextensions.k8s.io/globalnetworkpolicies.crd.projectcalico.org created
customresourcedefinition.apiextensions.k8s.io/globalnetworksets.crd.projectcalico.org created
customresourcedefinition.apiextensions.k8s.io/hostendpoints.crd.projectcalico.org created
customresourcedefinition.apiextensions.k8s.io/ipamblocks.crd.projectcalico.org created
customresourcedefinition.apiextensions.k8s.io/ipamconfigs.crd.projectcalico.org created
customresourcedefinition.apiextensions.k8s.io/ipamhandles.crd.projectcalico.org created
customresourcedefinition.apiextensions.k8s.io/ippools.crd.projectcalico.org created
customresourcedefinition.apiextensions.k8s.io/ipreservations.crd.projectcalico.org created
customresourcedefinition.apiextensions.k8s.io/kubecontrollersconfigurations.crd.projectcalico.org created
customresourcedefinition.apiextensions.k8s.io/networkpolicies.crd.projectcalico.org created
customresourcedefinition.apiextensions.k8s.io/networksets.crd.projectcalico.org created
clusterrole.rbac.authorization.k8s.io/calico-kube-controllers created
clusterrole.rbac.authorization.k8s.io/calico-node created
clusterrolebinding.rbac.authorization.k8s.io/calico-kube-controllers created
clusterrolebinding.rbac.authorization.k8s.io/calico-node created
daemonset.apps/calico-node created
deployment.apps/calico-kube-controllers created
customresourcedefinition.apiextensions.k8s.io/network-attachment-definitions.k8s.cni.cncf.io created
clusterrole.rbac.authorization.k8s.io/multus created
clusterrolebinding.rbac.authorization.k8s.io/multus created
serviceaccount/multus created
configmap/multus-cni-config created
daemonset.apps/kube-multus-ds created
serviceaccount/whereabouts created
clusterrolebinding.rbac.authorization.k8s.io/whereabouts created
clusterrole.rbac.authorization.k8s.io/whereabouts-cni created
Warning: spec.template.spec.nodeSelector[beta.kubernetes.io/arch]: deprecated since v1.14; use "kubernetes.io/arch" instead
daemonset.apps/whereabouts created
customresourcedefinition.apiextensions.k8s.io/ippools.whereabouts.cni.cncf.io created
customresourcedefinition.apiextensions.k8s.io/overlappingrangeipreservations.whereabouts.cni.cncf.io created
Warning: batch/v1beta1 CronJob is deprecated in v1.21+, unavailable in v1.25+; use batch/v1 CronJob
cronjob.batch/ip-reconciler created
namespace/cert-manager created
customresourcedefinition.apiextensions.k8s.io/certificaterequests.cert-manager.io created
customresourcedefinition.apiextensions.k8s.io/certificates.cert-manager.io created
customresourcedefinition.apiextensions.k8s.io/challenges.acme.cert-manager.io created
customresourcedefinition.apiextensions.k8s.io/clusterissuers.cert-manager.io created
customresourcedefinition.apiextensions.k8s.io/issuers.cert-manager.io created
customresourcedefinition.apiextensions.k8s.io/orders.acme.cert-manager.io created
serviceaccount/cert-manager-cainjector created
serviceaccount/cert-manager created
serviceaccount/cert-manager-webhook created
configmap/cert-manager-webhook created
clusterrole.rbac.authorization.k8s.io/cert-manager-cainjector created
clusterrole.rbac.authorization.k8s.io/cert-manager-controller-issuers created
clusterrole.rbac.authorization.k8s.io/cert-manager-controller-clusterissuers created
clusterrole.rbac.authorization.k8s.io/cert-manager-controller-certificates created
clusterrole.rbac.authorization.k8s.io/cert-manager-controller-orders created
clusterrole.rbac.authorization.k8s.io/cert-manager-controller-challenges created
clusterrole.rbac.authorization.k8s.io/cert-manager-controller-ingress-shim created
clusterrole.rbac.authorization.k8s.io/cert-manager-view created
clusterrole.rbac.authorization.k8s.io/cert-manager-edit created
clusterrole.rbac.authorization.k8s.io/cert-manager-controller-approve:cert-manager-io created
clusterrole.rbac.authorization.k8s.io/cert-manager-controller-certificatesigningrequests created
clusterrole.rbac.authorization.k8s.io/cert-manager-webhook:subjectaccessreviews created
clusterrolebinding.rbac.authorization.k8s.io/cert-manager-cainjector created
clusterrolebinding.rbac.authorization.k8s.io/cert-manager-controller-issuers created
clusterrolebinding.rbac.authorization.k8s.io/cert-manager-controller-clusterissuers created
clusterrolebinding.rbac.authorization.k8s.io/cert-manager-controller-certificates created
clusterrolebinding.rbac.authorization.k8s.io/cert-manager-controller-orders created
clusterrolebinding.rbac.authorization.k8s.io/cert-manager-controller-challenges created
clusterrolebinding.rbac.authorization.k8s.io/cert-manager-controller-ingress-shim created
clusterrolebinding.rbac.authorization.k8s.io/cert-manager-controller-approve:cert-manager-io created
clusterrolebinding.rbac.authorization.k8s.io/cert-manager-controller-certificatesigningrequests created
clusterrolebinding.rbac.authorization.k8s.io/cert-manager-webhook:subjectaccessreviews created
role.rbac.authorization.k8s.io/cert-manager-cainjector:leaderelection created
role.rbac.authorization.k8s.io/cert-manager:leaderelection created
role.rbac.authorization.k8s.io/cert-manager-webhook:dynamic-serving created
rolebinding.rbac.authorization.k8s.io/cert-manager-cainjector:leaderelection created
rolebinding.rbac.authorization.k8s.io/cert-manager:leaderelection created
rolebinding.rbac.authorization.k8s.io/cert-manager-webhook:dynamic-serving created
service/cert-manager created
service/cert-manager-webhook created
deployment.apps/cert-manager-cainjector created
deployment.apps/cert-manager created
deployment.apps/cert-manager-webhook created
mutatingwebhookconfiguration.admissionregistration.k8s.io/cert-manager-webhook created
validatingwebhookconfiguration.admissionregistration.k8s.io/cert-manager-webhook created
namespace/cdi created
customresourcedefinition.apiextensions.k8s.io/cdis.cdi.kubevirt.io created
clusterrole.rbac.authorization.k8s.io/cdi-operator-cluster created
clusterrolebinding.rbac.authorization.k8s.io/cdi-operator created
serviceaccount/cdi-operator created
role.rbac.authorization.k8s.io/cdi-operator created
rolebinding.rbac.authorization.k8s.io/cdi-operator created
deployment.apps/cdi-operator created
configmap/cdi-operator-leader-election-helper created
cdi.cdi.kubevirt.io/cdi created
namespace/longhorn-system created
serviceaccount/longhorn-service-account created
clusterrole.rbac.authorization.k8s.io/longhorn-role created
clusterrolebinding.rbac.authorization.k8s.io/longhorn-bind created
customresourcedefinition.apiextensions.k8s.io/engines.longhorn.io created
customresourcedefinition.apiextensions.k8s.io/replicas.longhorn.io created
customresourcedefinition.apiextensions.k8s.io/settings.longhorn.io created
customresourcedefinition.apiextensions.k8s.io/volumes.longhorn.io created
customresourcedefinition.apiextensions.k8s.io/engineimages.longhorn.io created
customresourcedefinition.apiextensions.k8s.io/nodes.longhorn.io created
customresourcedefinition.apiextensions.k8s.io/instancemanagers.longhorn.io created
customresourcedefinition.apiextensions.k8s.io/sharemanagers.longhorn.io created
customresourcedefinition.apiextensions.k8s.io/backingimages.longhorn.io created
customresourcedefinition.apiextensions.k8s.io/backingimagemanagers.longhorn.io created
customresourcedefinition.apiextensions.k8s.io/backingimagedatasources.longhorn.io created
customresourcedefinition.apiextensions.k8s.io/backuptargets.longhorn.io created
customresourcedefinition.apiextensions.k8s.io/backupvolumes.longhorn.io created
customresourcedefinition.apiextensions.k8s.io/backups.longhorn.io created
customresourcedefinition.apiextensions.k8s.io/recurringjobs.longhorn.io created
configmap/longhorn-default-setting created
Warning: policy/v1beta1 PodSecurityPolicy is deprecated in v1.21+, unavailable in v1.25+
podsecuritypolicy.policy/longhorn-psp created
role.rbac.authorization.k8s.io/longhorn-psp-role created
rolebinding.rbac.authorization.k8s.io/longhorn-psp-binding created
configmap/longhorn-storageclass created
daemonset.apps/longhorn-manager created
service/longhorn-backend created
service/longhorn-engine-manager created
service/longhorn-replica-manager created
deployment.apps/longhorn-ui created
service/longhorn-frontend created
deployment.apps/longhorn-driver-deployer created

waiting for addons started
deployment.apps/calico-kube-controllers condition met
deployment.apps/cdi-operator condition met
cdi.cdi.kubevirt.io/cdi condition met
daemon set "whereabouts" successfully rolled out
daemon set "longhorn-manager" successfully rolled out
storageclass.storage.k8s.io/longhorn patched

provisioned

Provisioning 'andrey-lab-2' instance
setup hostname

install necessary deps

allow routing
net.ipv4.ip_forward = 1

create linux bridge interface 'vm-bridge0' if not exist

create VXLAN interface 'vm-vxlan0' if not exist
appending 10.10.10.196 to VXLAN FDB
appending 10.10.10.22 to VXLAN FDB

install cni plugins

join to cluster
[INFO]  Finding release for channel stable
[INFO]  Using v1.24.6+k3s1 as release
[INFO]  Downloading hash https://github.com/k3s-io/k3s/releases/download/v1.24.6+k3s1/sha256sum-amd64.txt
[INFO]  Downloading binary https://github.com/k3s-io/k3s/releases/download/v1.24.6+k3s1/k3s
[INFO]  Verifying binary download
[INFO]  Installing k3s to /usr/local/bin/k3s
[INFO]  Skipping installation of SELinux RPM
[INFO]  Creating /usr/local/bin/kubectl symlink to k3s
[INFO]  Creating /usr/local/bin/crictl symlink to k3s
[INFO]  Creating /usr/local/bin/ctr symlink to k3s
[INFO]  Creating killall script /usr/local/bin/k3s-killall.sh
[INFO]  Creating uninstall script /usr/local/bin/k3s-agent-uninstall.sh
[INFO]  env: Creating environment file /etc/systemd/system/k3s-agent.service.env
[INFO]  systemd: Creating service file /etc/systemd/system/k3s-agent.service
[INFO]  systemd: Enabling k3s-agent unit
Created symlink /etc/systemd/system/multi-user.target.wants/k3s-agent.service → /etc/systemd/system/k3s-agent.service.
[INFO]  systemd: Starting k3s-agent

provisioned

Provisioning 'andrey-lab-3' instance
setup hostname

install necessary deps

allow routing
net.ipv4.ip_forward = 1

create linux bridge interface 'vm-bridge0' if not exist

create VXLAN interface 'vm-vxlan0' if not exist
appending 10.10.10.196 to VXLAN FDB
appending 10.10.10.61 to VXLAN FDB

install cni plugins

join to cluster
[INFO]  Finding release for channel stable
[INFO]  Using v1.24.6+k3s1 as release
[INFO]  Downloading hash https://github.com/k3s-io/k3s/releases/download/v1.24.6+k3s1/sha256sum-amd64.txt
[INFO]  Downloading binary https://github.com/k3s-io/k3s/releases/download/v1.24.6+k3s1/k3s
[INFO]  Verifying binary download
[INFO]  Installing k3s to /usr/local/bin/k3s
[INFO]  Skipping installation of SELinux RPM
[INFO]  Creating /usr/local/bin/kubectl symlink to k3s
[INFO]  Creating /usr/local/bin/crictl symlink to k3s
[INFO]  Creating /usr/local/bin/ctr symlink to k3s
[INFO]  Creating killall script /usr/local/bin/k3s-killall.sh
[INFO]  Creating uninstall script /usr/local/bin/k3s-agent-uninstall.sh
[INFO]  env: Creating environment file /etc/systemd/system/k3s-agent.service.env
[INFO]  systemd: Creating service file /etc/systemd/system/k3s-agent.service
[INFO]  systemd: Enabling k3s-agent unit
Created symlink /etc/systemd/system/multi-user.target.wants/k3s-agent.service → /etc/systemd/system/k3s-agent.service.
[INFO]  systemd: Starting k3s-agent

provisioned

Setup k8s context 'andrey-lab' for local kubectl
Cluster "andrey-lab" set.
User "andrey-lab" set.
Context "andrey-lab" created.
Switched to context "andrey-lab".
```

</details>

### Inspect cluster status


Run `./aws_k3s.sh status`

<details><summary>example output</summary>

```console

=== Dev AWS infra and K3S cluster ===

Retrieving data from AWS
Retrieving instances details
VPC has been created
'andrey-lab-1' running, try: ssh -i andrey-lab.pem ubuntu@34.244.37.179
'andrey-lab-2' running, try: ssh -i andrey-lab.pem ubuntu@54.247.60.5
'andrey-lab-3' running, try: ssh -i andrey-lab.pem ubuntu@54.247.9.8
```

</details>

### Destroy cluster

Run `./aws_k3s.sh destroy`

<details><summary>example output</summary>

```console

=== Dev AWS infra and K3S cluster ===

Retrieving data from AWS
Retrieving instances details
Deleting 'andrey-lab-3' instance
Deleting 'andrey-lab-2' instance
Deleting 'andrey-lab-1' instance
Waiting for 'andrey-lab-3' termination
   ....
   instance terminated (id: i-0fdd5ab8767ce0061)
Waiting for 'andrey-lab-2' termination
   ....
   instance terminated (id: i-0de80cb236f426a05)
Waiting for 'andrey-lab-1' termination
   ............................
   instance terminated (id: i-00aabe3ef8e330ff1)
Delete k8s context 'andrey-lab'
Property "current-context" unset.
Property "clusters.andrey-lab" unset.
Property "users.andrey-lab" unset.
Property "contexts.andrey-lab" unset.
Removing VPC resources
   delete security group
   delete subnet
   detach internet gateway from VPC
   delete internet gateway
   delete VPC
   delete ssh keypair
done
```

</details>

### Kubermetes cluster access

After cluster creation kubernetes context configured with name as specified in `AWS_NAME`

```console
% kubectl config current-context
andrey-lab

% kubectl version --short
Client Version: v1.24.3
Kustomize Version: v4.5.4
Server Version: v1.24.6+k3s1

% kubectl get nodes
NAME           STATUS   ROLES                  AGE     VERSION
andrey-lab-1   Ready    control-plane,master   4m13s   v1.24.6+k3s1
andrey-lab-2   Ready    <none>                 85s     v1.24.6+k3s1
andrey-lab-3   Ready    <none>                 43s     v1.24.6+k3s1
```

### Notes

AWS resources script creates/removes:

- AWS VPC
- AWS Subnet
- Internet Gateway
- Association Internet Gateway with VPC
- Subnet attachment with route table in VPC
- Security Group
- Rule for Security group (simple rule full open to World)
- KeyPair and private SSH key (stored locally near script)
- EC2 Instances with public IP address
