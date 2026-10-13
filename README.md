# zonecheck

Kubernetes operator that keeps asking "what if this zone died?" and tells you which workloads would have nowhere to go.

>Early WIP. Results land in the `ZoneCheck` status, no metrics yet. Don't install it anywhere you care about.

Snapshot the cluster -> take a zone (or node, or pool) away -> run kube-scheduler's own plugins against what's left. Every few minutes, for every zone. Pods that can't be placed get listed with the same message the scheduler would put on them:

```
0/6 nodes are available: 2 node(s) were unschedulable, 4 node(s) didn't match PersistentVolume's node affinity.
```

It only reads. Nothing gets drained, cordoned, or evicted, so it's safe to leave running in a cluster you're nervous about. The cost is that it's a prediction, and a prediction is only as good as the rules it copies. So it copies the scheduler's, rather than writing its own.

## Doesn't something already do this?

Parts of it. cluster-capacity and kube-scheduler-simulator both run the real scheduler against a cluster, but you run them by hand, once, and read the answer. Nobody does that every week, and capacity drifts every deploy.

Gremlin's Detected Risks checks whether you're spread across zones. It doesn't check whether the surviving zones have room for everything that moves, and that gets less likely as a cluster fills up.

## Usage

You'll need Go 1.26+ and a cluster (kind's fine).

```sh
make install   # CRDs into your current kubectl context
make run       # controller, locally
kubectl apply -f config/samples/checks_v1alpha1_zonecheck.yaml
```

```yaml
apiVersion: checks.zonecheck.dev/v1alpha1
kind: ZoneCheck
metadata:
  name: cluster
spec:
  interval: 5m
  scenarios:
  - type: Zone
  - type: BusiestNode
  - type: NodePool
    nodePool:
      labelKey: <your provider's pool label>
      name: gpu
  # only if your control plane changed these flags (defaults shown)
  nodeEviction:
    unhealthyZoneThresholdPercent: 55  # --unhealthy-zone-threshold
    largeClusterSizeThreshold: 50      # --large-cluster-size-threshold
```

`Zone` loses each zone one at a time. `BusiestNode` loses the node whose pods would need the most room elsewhere. `NodePool` loses every node with that label.

Then:

```sh
kubectl get zonecheck cluster -o yaml
```

One result per scenario, with counts and up to 25 named pods. Check the `ZoneLabelsComplete` condition first. Nodes without a zone label belong to no zone, so they never get taken away and the results look better than they are. Clusters built by hand (kind included) usually need the labels added.

## What it checks

kube-scheduler's default rule set: CPU/memory, taints, node selectors + affinity, host ports, volumes (zonal disks, attach limits, ReadWriteOncePod), topology spread, and pod (anti-)affinity. Pods go highest priority first, each one using up room for the next. Pods already shutting down still take room till they're gone.

Lost nodes stay in the picture, cordoned, like they would after a real drain. That's on purpose. With `whenUnsatisfiable: DoNotSchedule`, the scheduler still counts the dead zone (as 0 pods), so it won't put more pods anywhere else, and the replacements sit pending. zonecheck catches that one. The fix is usually `nodeTaintsPolicy: Honor` on the constraint.

A drain also goes through PodDisruptionBudgets. A PDB that allows no disruptions, or one with more homeless pods than budget, stalls the drain, and those pods come back as `DrainBlocked`. Same if more than one PDB covers a pod (the Eviction API refuses those). zonecheck assumes the best eviction order, so treat it as a lower bound.

Pods with ephemeral volumes get a fresh claim, like a real replacement would, so they aren't stuck to the old pod's disk.

## Drain vs outage

They don't play out the same, so each result covers both.

In a drain, everything with a controller gets evicted and recreated somewhere. In an outage, some pods sit on the dead node until someone steps in: StatefulSet pods, Jobs that only replace failed pods, anything tolerating an unreachable node forever. Those come back as `OutageStuck`.

Pods with no controller are gone either way (`Lost`). Same for Job pods when the evictions would push the Job past its `backoffLimit`.

Kubernetes also throttles evictions when lots of nodes go down at once. If most of a small zone dies (or every zone does), it stops evicting entirely, and every pod there is stuck. zonecheck uses the node controller's thresholds for that, which is what `nodeEviction` is for if your platform changed them.

## Limitations

Your scheduler's config isn't read. Custom plugin weights or scoring (GKE's "optimize utilization", say) can put pods somewhere else than zonecheck expects, and pods using a different `schedulerName` get the default rules anyway.

No preemption. A high-priority pod that would bump others shows up as unschedulable.

No DRA (`ResourceClaim`s for GPUs and the like).

Pods already waiting to be scheduled aren't counted as competing for the room.

Nodes get checked in name order where the scheduler races its workers, and score ties go to the first node by name instead of a random one. Same answer every run, at the cost of sometimes differing from the real pick. Big clusters only check a sample, same size as the scheduler's.

In an outage, pods on dead nodes still count for zone spread till they're evicted (with the default `nodeTaintsPolicy`), so spread can play out a bit differently than the drain-based numbers.

Pods owned by other controllers (Argo Rollouts, CloneSets, ...) are assumed to get recreated like a ReplicaSet's.

## Development

```sh
make test
make lint
```

Unit tests run the scheduler plugins directly: resources, priority order, spread with and without `Honor`, a volume pinned to a lost zone, two pods fighting over one disk, the PDB and eviction rules, and a 1000-node benchmark.

The controller tests run against a real API server (envtest), no cluster needed. They cover a full run with a zonal disk, a PDB, an ephemeral volume, and an unlabeled node, plus the CRD's validation. One test runs the controller with only the permissions in `config/rbac/role.yaml`, so a missing RBAC rule fails there and not in your cluster.

None of it checks a prediction against a real drain yet. That's the kind e2e test, which is next.

## License

Apache-2.0
