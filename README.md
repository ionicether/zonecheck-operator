# zonecheck

Kubernetes operator that keeps asking "what if this zone died?" -> and tells you which workloads would have nowhere to go.

**Status: early WIP.** The API's defined, the controller doesn't do anything yet. Don't install this anywhere you care about.

## What it'll do

Every few minutes it simulates losing:

- each zone (one at a time)
- your busiest node (the one whose pods would need the most room elsewhere)
- a node pool you name

...then runs the real kube-scheduler logic against what's left. Pods that can't be placed show up in the `ZoneCheck` status + as Prometheus metrics.

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
