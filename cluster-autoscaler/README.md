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
`Dockerfile`. There's currently no testing — given the slow rate of change, it's ok _for now_ for
testing to be done manually.

## Version upgrade checklist

- [ ] Update ca.patch
- [ ] Update ca.tag
- [ ] Update the golang builder image in Dockerfile, to match GA's builder (see note at the top of
    that file)

See more below

## How to update the patch

Upgrading has a few steps. Here's how to do it

```sh-session
$ # start in the autoscaling repo
$ pwd
/path/to/neon/autoscaling
$ # set the new tag up-front. We're using 1.25.1 as an example
$ new_version=cluster-autoscaler-1.25.1
$ # create a temporary directory for kubernetes/autoscaler
$ tmpdir=$(mktemp -d | tee /dev/tty)
/tmp/tmp./tmp/tmp.MCMXmnprG0
$ # clone the source repo
$ git clone --quiet --branch $(cat cluster-autoscaler/ca.tag) https://github.com/kubernetes/autsocaler $tmpdir
Cloning into '/tmp/tmp.MCMXmnprG0'...
remote: Enumerating objects: 23946, done.
remote: Counting objects: 100% (23946/23946), done.
remote: Compressing objects: 100% (15379/15379), done.
Receiving objects: 100% (23946/23946), 47.40 MiB | 13.54 MiB/s, done.
remote: Total 23946 (delta 9309), reused 16670 (delta 6852), pack-reused 0
Resolving deltas: 100% (9309/9309), done.
Note: switching to '42876d98d3e8a54f91257fbf6f81b8eab3e04d2f'.
Updating files: 100% (24715/24715), done.
$ # ensure we have the target version's tag around for later
$ git -C $tmpdir fetch --depth 1 origin tag $new_version
remote: Enumerating objects: 9039, done.
remote: Counting objects: 100% (9039/9039), done.
remote: Compressing objects: 100% (2810/2810), done.
remote: Total 5775 (delta 3451), reused 4254 (delta 2385), pack-reused 0
Receiving objects: 100% (5775/5775), 14.68 MiB | 9.78 MiB/s, done.
Resolving deltas: 100% (3451/3451), completed with 1466 local objects.
From https://github.com/kubernetes/autoscaler
 * [new tag]           cluster-autoscaler-1.25.1 -> cluster-autoscaler-1.25.1
$ # apply the patch:
$ git -C $tmpdir apply $(pwd)/cluster-autoscaler/ca.patch
$ # save it to re-apply *within git* after switching branches
$ git -C $tmpdir stash
Saved working directory and index state WIP on (no branch): 42876d98 Merge pull request #5631 from MaciekPytel/ca_1_24_1
$ git -C $tmpdir checkout $new_version
Previous HEAD position was 42876d98d Merge pull request #5631 from MaciekPytel/ca_1_24_1
HEAD is now at da4091177 Merge pull request #5614 from MaciekPytel/ca_1_25_1
$
$ # Apply the original patch, again. If there's no conflicts, then you can skip to the section
$ # marked "AFTER RESOLVING CONFLICTS". Here, we'll look at the case where there are some
$ git -C $tmpdir stash pop
Auto-merging cluster-autoscaler/utils/kubernetes/listers.go
CONFLICT (content): Merge conflict in cluster-autoscaler/utils/kubernetes/listers.go
HEAD detached at cluster-autoscaler-1.25.1
Unmerged paths:
  (use "git restore --staged <file>..." to unstage)
  (use "git add <file>..." to mark resolution)
        both modified:   cluster-autoscaler/utils/kubernetes/listers.go

no changes added to commit (use "git add" and/or "git commit -a")
The stash entry is kept in case you need it again.
$
$ # So there's conflicts. Resolve them with your editor of choice. In this *particular* case, it
$ # happened to be relatively simple - just minor import conflicts:
$ grep -E -B 3 -A 3 '[<=>]{7}' $tmpdir/cluster-autoscaler/utils/kubernetes/listers.go
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	apiv1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
$
$ # As always though, you should manually inspect that the changes are still sensible.
$ <manually resovle conflicts + check the rest of the patch yourself>
$
$ # AFTER RESOLVING CONFLICTS:
$ # If there were any conflicts, reset the repo state (so that `git diff` is correct)
$ git add . && git reset
Unstaged changes after reset:
M   cluster-autoscaler/utils/kubernetes/listers.go
$ # Re-generate the patch. Remember: we need to be in the autoscaling repo
$ pwd
/path/to/neon/autoscaling
$ git -C $tmpdir diff > cluster-autoscaler/ca.patch
$ # And finally, update the stored tag:
$ echo $new_version > cluster-autoscaler/ca.tag
$ # All done! That's it!
$ # Clean up the temporary directory:
$ rm -rf $tmpdir
$
$ # The final changes might look something like:
$ git diff
diff --git a/cluster-autoscaler/ca.patch b/cluster-autoscaler/ca.patch
index 06cc0c0..9b9b497 100644
--- a/cluster-autoscaler/ca.patch
+++ b/cluster-autoscaler/ca.patch
@@ -1,5 +1,5 @@
 diff --git a/cluster-autoscaler/utils/kubernetes/listers.go b/cluster-autoscaler/utils/kubernetes/listers.go
-index b70ca1db..85f1473f 100644
+index d0033550f..fa3c2ec30 100644
 --- a/cluster-autoscaler/utils/kubernetes/listers.go
 +++ b/cluster-autoscaler/utils/kubernetes/listers.go
 @@ -17,14 +17,19 @@ limitations under the License.
@@ -12,7 +12,7 @@ index b70ca1db..85f1473f 100644
  	appsv1 "k8s.io/api/apps/v1"
  	batchv1 "k8s.io/api/batch/v1"
  	apiv1 "k8s.io/api/core/v1"
- 	policyv1 "k8s.io/api/policy/v1beta1"
+ 	policyv1 "k8s.io/api/policy/v1"
 +	"k8s.io/apimachinery/pkg/api/resource"
 +	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
  	"k8s.io/apimachinery/pkg/fields"
diff --git a/cluster-autoscaler/ca.tag b/cluster-autoscaler/ca.tag
index 601b81b..7ac956d 100644
--- a/cluster-autoscaler/ca.tag
+++ b/cluster-autoscaler/ca.tag
@@ -1 +1 @@
-cluster-autoscaler-1.24.1
+cluster-autoscaler-1.25.1
```
