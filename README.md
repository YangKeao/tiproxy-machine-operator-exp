# tiproxy-machine-operator

This repository contains:

- `tiproxy-machine-operator` (controller): manages `TiProxyMachineGroup`, allocates per-link ports, and reconciles provider-specific machine groups/templates (current implementations: `aws`, `none`).
- `tiproxy-machine-agent` (node daemon): runs on each machine, renders TiProxy config, starts TiProxy in host network via Docker, and pushes runtime updates.

Current TiProxy integration model:

- Each `TiDBInstanceLink` becomes one `proxy.backend-clusters` entry.
- `TiDBInstanceLink.spec.port` can pin a fixed frontend port; when omitted, the controller allocates one from `spec.portRange`.
- `balance.routing-rule` is set to `"port"`.
- `proxy.port-range` is set from `TiProxyMachineGroup.spec.portRange`.
- Agent updates `TiProxyMachine.status.tiProxyImage` with the current runtime image.
- On AWS provider, changing TiProxy image fields in `TiProxyMachineGroup.spec.tiproxy` triggers one ASG instance refresh (rolling replacement), deduplicated by image hash.

## CRDs

- `TiProxyMachineGroup`
- `TiDBInstanceLink`
- `TiProxyMachine`

CRD manifests are in [config/crd/bases](/home/yangkeao/Project/github.com/YangKeao/tiproxy-machine-operator/config/crd/bases).

## Build

```bash
go build ./cmd/manager
go build ./cmd/tiproxy-machine-agent
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
kubectl apply -f config/samples/tiproxy_v1alpha1_tidbinstancelink.yaml
```

AWS sample (edit ids first):
```bash
kubectl apply -f config/samples/tiproxy_v1alpha1_tiproxymachinegroup_aws.yaml
```

## AWS notes

`TiProxyMachineGroup.spec.infrastructure` should include:

- `provider: aws`
- `machineImage`
- `machineType`
- `subnetIDs`
- `securityGroupIDs`
- `instanceProfile` (for pulling images and accessing cluster API)

If `spec.bootstrap.userData` is not set, the operator uses a minimal bootstrap script that enables `tiproxy-machine-agent` systemd service.
