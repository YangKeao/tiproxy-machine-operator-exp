# tiproxy-machine-operator

This repository contains:

- `tiproxy-machine-operator` (controller): manages `TiProxyMachineGroup`, allocates frontend ports via `TiProxyMachinePort`, resolves backend links via `TiDBResourcePoolLink`, and reconciles provider-specific resources (current implementations: `aws`, `none`).
- `tiproxy-machine-agent` (node daemon): runs on each machine, renders TiProxy config, starts TiProxy in host network via Docker, and pushes runtime updates.

Current TiProxy integration model:

- Each `TiDBResourcePoolLink` becomes one `proxy.backend-clusters` entry.
- Each `TiProxyMachinePort` allocates one frontend port from `spec.portRange` (or uses `spec.requestedPort` if specified).
- Link-to-port assignment is not explicit:
  - the controller first prefers a `TiProxyMachinePort` with the same `namespace/name`
  - then assigns remaining links from the remaining allocated ports in ascending port order
- On AWS provider, each `TiProxyMachineGroup.spec.exposure.loadBalancers[]` entry creates one shared NLB.
- All `TiProxyMachinePort` under one MachineGroup are exposed on every declared load balancer; each allocated port maps to one listener per load balancer.
- On AWS provider, `spec.placement.failureDomains` filters `spec.network.subnetIDs` by subnet AZ, and `spec.placement.spreadPolicy` maps to ASG availability-zone distribution.
- `balance.routing-rule` is set to `"port"`.
- `proxy.port-range` is set from `TiProxyMachineGroup.spec.portRange`.
- Agent updates `TiProxyMachineGroup.status.machines[<machine-id>]` with runtime heartbeat/image/config state.
- Controller prunes stale `status.machines` entries after `--machine-status-stale-after` (default `10m`).
- On AWS provider, changing TiProxy image fields in `TiProxyMachineGroup.spec.tiproxy` triggers one ASG instance refresh (rolling replacement), deduplicated by image hash.

Current limitation:

- placement/network changes are fully applied when creating new AWS resources
- if an NLB already exists, the controller does not currently reconcile its subnet attachments to match later placement changes

## CRDs

- `TiProxyMachineGroup`
- `TiProxyMachinePort`
- `TiDBResourcePoolLink`

CRD manifests are in [config/crd/bases](/home/yangkeao/Project/github.com/YangKeao/tiproxy-machine-operator/config/crd/bases).

## Build

```bash
go build ./cmd/manager
go build ./cmd/tiproxy-machine-agent
```

## Regenerate API Artifacts

```bash
./hack/update-codegen.sh
```

## Local test (minikube)

1. Apply CRDs:
```bash
kubectl apply -f config/crd/bases
```

2. Run operator in-cluster or locally (with kubeconfig):
```bash
go run ./cmd/manager --cloud-provider=noop
```

3. Apply samples:
```bash
kubectl apply -f config/samples/tiproxy_v1alpha1_tiproxymachinegroup.yaml
kubectl apply -f config/samples/tiproxy_v1alpha1_tiproxymachineport.yaml
kubectl apply -f config/samples/tiproxy_v1alpha1_tidbresourcepoollink.yaml
```

AWS sample (edit ids first):
```bash
kubectl apply -f config/samples/tiproxy_v1alpha1_tiproxymachinegroup_aws.yaml
```

## AWS notes

`TiProxyMachineGroup` on AWS should include:

- `provider: aws`
- `spec.placement.region`
- `spec.machine.image`
- `spec.machine.class`
- `spec.network.subnetIDs`
- `spec.network.securityGroupIDs`
- `spec.machine.identityRef` (for pulling images and accessing cluster API)
- optional:
  - `spec.scaling.minReplicas`
  - `spec.scaling.maxReplicas`
  - `spec.placement.failureDomains`
  - `spec.placement.spreadPolicy`
  - `spec.resourceNames`
  - `spec.tags`

If `spec.bootstrap.userData` is not set, the operator uses a minimal bootstrap script that enables `tiproxy-machine-agent` systemd service.
