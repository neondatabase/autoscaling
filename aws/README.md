# VM live-migration Lab

## LXD cluster

### TL;DR

create LXD cluster

- install `terrafrom`, `terragrunt`
- look at [settings.hcl](settings.hcl) (but usually you need not change anything there)
- create AWS VPC - run `terragrant apply` in [vpc](vpc) folder
- create LXD cluster - run `terragrant apply` in [lxd_cluster](lxd_cluster) folder
- ssh into one of LXD node - `ssh -i artifacts/key_name_here ubuntu@ip.addr.here`
- you can configire your local lxc (e.g. on your laptop/MacBook) by `lxc remote add ip.addr.here --password lxd_password_here`

play with cluster

- inspect cluster nodes - `lxc cluster ls`
- launch VM - `lxc launch images:ubuntu/20.04/cloud u1 --vm`
- inspect  VMs - `lxc ls` (wait when it will show IP address - that mean VM fully started)
- go into VM - `lxc exec u1 -- bash`
- move vm to other node - `lxc move u1 --target node-name-here`

Default LXD profile used for VM has cloud-init flow with postgresql installation, you can reach postgresql server by

```console
PGPASSWORD=bench psql -h ip.addr.of.vm -U bench bench
```

>NOTE: you need install postgresql client by `apt install postgresql-client` and if you need `pgbench` on cluster node then `apt install postgresql-contrib`

Look at scripts in [lxd_cluster/scripts](lxd_cluster/scripts) folder to see how all configured.

### Tear down

DO NOT FORGET delete lxd cluster after tests as it a bit expensive

- run `terragrant destroy` in [lxd_cluster](lxd_cluster) folder
- run `terragrant destroy` in [vpc](vpc) folder
