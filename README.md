# zonecheck

Kubernetes operator that keeps asking "what if this zone died?" -> and tells you which workloads would have nowhere to go.

**Status: early WIP.** It works out which pods each failure knocks loose, whether the remaining nodes have room for them, and whether PDBs would stall the drain, and writes that to the `ZoneCheck` status. No metrics yet. Don't install this anywhere you care about.

## What it does

Every few minutes it simulates losing:

- each zone (one at a time)
- your busiest node (the one whose pods would need the most room elsewhere)
- a node pool you name

...then runs kube-scheduler's own plugins against what's left. Pods that can't be placed show up in the `ZoneCheck` status (and later as Prometheus metrics), with the same message the scheduler would put on the pod:

```
0/6 nodes are available: 2 node(s) were unschedulable, 4 node(s) didn't match PersistentVolume's node affinity.
```

That's kube-scheduler's default rule set: CPU/memory, taints, node selectors + affinity, host ports, volumes (zonal disks, attach limits, ReadWriteOncePod), topology spread, and pod (anti-)affinity. Pods go highest priority first, each one using up room for the next. Pods that are shutting down still take room till they're gone.

Lost nodes stay in the picture, cordoned, like they would after a real drain or outage. That matters for zone spread: with `whenUnsatisfiable: DoNotSchedule`, kube-scheduler still counts the dead zone (as 0 pods), so it won't put more pods anywhere else, and the replacements sit pending. zonecheck catches that. The fix is usually `nodeTaintsPolicy: Honor` on the constraint.

Differences from the real thing:

- nodes get checked in name order where the scheduler races its workers (big clusters still only check a sample, same size as the scheduler's), and score ties go to the first node by name, not a random one -> same answer every run
- no preemption, so a high-priority pod that would bump others shows up as unschedulable
- no DRA (`ResourceClaim`s for GPUs etc)
- pods already waiting to be scheduled aren't counted as competing for the room
- your scheduler's own config isn't read: custom plugin weights or scoring (like GKE's "optimize utilization") -> pods may land elsewhere than zonecheck says
- pods using a different scheduler (`schedulerName`) get the default scheduler's rules

A drain and a real outage don't play out the same, so you get both:

- **drain** -> everything with a controller gets evicted + recreated somewhere
- **outage** -> some pods sit on the dead node until someone steps in: StatefulSet pods, Jobs that only replace failed pods, anything tolerating an unreachable node forever
- in an outage, pods on dead nodes still count for zone spread till they're evicted (with the default `nodeTaintsPolicy`) -> spread can play out a bit differently than zonecheck's drain-based numbers
- pods with no controller are just gone either way, so they're reported as lost. Same for Job pods when the evictions would push the Job past its `backoffLimit`

A drain also goes through PodDisruptionBudgets. If a PDB allows no disruptions, or more of its pods have nowhere to go than its budget covers, the drain stalls and those pods show up as `DrainBlocked`. Same if more than one PDB covers a pod (the Eviction API refuses those). zonecheck assumes the best eviction order, so it's a lower bound.

Kubernetes also throttles evictions when lots of nodes go down at once. If most of a small zone dies (or every zone does), it stops evicting entirely, and every pod there is stuck. zonecheck models that using the same thresholds as the node controller, so set `nodeEviction` if your platform changed them.

Pods owned by other controllers (Argo Rollouts, CloneSets, ...) are assumed to get recreated like a ReplicaSet's.

Why not cluster-capacity or kube-scheduler-simulator? They're one-off/interactive. This runs continuously. Gremlin's Detected Risks checks whether you're spread across zones. It doesn't check whether the surviving zones have room.

## Example

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

## Dev

You'll need Go 1.26+ and a cluster (kind's fine).

```sh
make test     # unit + API validation tests
make install  # CRDs into your current kubectl context
make run      # controller, locally
```

## License

Apache-2.0
