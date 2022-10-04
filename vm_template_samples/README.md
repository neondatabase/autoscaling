## VM templates for Virtink playground

- [postgres14-tmpfs.yaml](postgres14-tmpfs.yaml) - VM with postgres14, read-only root (1Gb size), 1 vcpu, 8Gb ram, `PGDATA` mounted to memmory (tmpfs 8Gb size)
- [postgres14-nfs.yaml](postgres14-nfs.yaml) - VM with postgres14, read-only root (1Gb size), 1 vcpu, 1Gb ram, `PGDATA` mounted to NFS server (`nfs-server:/share`)
- [postgres14-disk.yaml](postgres14-disk.yaml) - VM with postgres14, read-write root (8Gb size), 1 vcpu, 1Gb ram
- [postgres14-cdi.yaml](postgres14-cdi.yaml) - VM with postgres14, read-write root in shared datavolume (8Gb size), 1 vcpu, 1Gb ram
- [postgres14-cdi-nfs.yaml](postgres14-cdi-nfs.yaml) - VM with postgres14, read-write root in shared datavolume (1Gb size), 1 vcpu, 1Gb ram, PGDATA mounted to NFS server (`nfs-server:/share`)
- [postgres14-migration.yaml](postgres14-migration.yaml) - VirtualMachineMigrations, used to trigger VM migration

### access to VM's console

```console
kubectl apply -f postgres14-tmpfs.yaml
VM_POD=$(kubectl get vm postgres14-tmpfs -ojsonpath='{.status.vmPodName}')
kubectl exec $VM_POD -it -- screen /dev/pts/0
```

then press `[ENTER]` and you will autologin to VM

```console
cloud-hypervisor login: root (automatic login)

Welcome to Alpine!

The Alpine Wiki contains a large amount of how-to guides and general
information about administrating Alpine systems.
See <http://wiki.alpinelinux.org/>.

You can setup the system with the command: setup-alpine

You may change this message by editing /etc/motd.

cloud-hypervisor:~#
```

to exit from screen session press `CTRL-A` and then `K` (kill session)
