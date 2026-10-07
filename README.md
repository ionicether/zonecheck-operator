# zonecheck

Kubernetes operator that keeps asking "what if this zone died?" -> and tells you which workloads would have nowhere to go.

**Status: early WIP.** It snapshots the cluster + writes which pods each failure would knock loose to the `ZoneCheck` status. No placement yet, so it can't tell you whether they'd fit. Don't install this anywhere you care about.

## What it'll do

Every few minutes it simulates losing:

- each zone (one at a time)
- your busiest node (the one whose pods would need the most room elsewhere)
- a node pool you name

...then runs the real kube-scheduler logic against what's left. Pods that can't be placed show up in the `ZoneCheck` status + as Prometheus metrics.

A drain and a real outage don't play out the same, so you get both:

- **drain** -> everything with a controller gets evicted + recreated somewhere
- **outage** -> some pods sit on the dead node until someone steps in: StatefulSet pods, Jobs that only replace failed pods, anything tolerating an unreachable node forever
- pods with no controller are just gone either way, so they're reported as lost. Same for Job pods when the evictions would push the Job past its `backoffLimit`

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
