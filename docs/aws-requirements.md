# AWS requirements (ASG/LT/AMI)

## Launch Template required fields

At minimum, set these in `TiProxyMachineGroup.spec.infrastructure`:

- `spec.provider: aws`
- `machineImage`
- `machineType`
- `subnetIDs`
- `securityGroupIDs`
- `instanceProfile`

Optional but recommended:

- `sshKeyName`
- `spec.bootstrap.userData`
- `spec.bootstrap.kubeconfig.path`
- `spec.bootstrap.kubeconfig.remoteRef` (with `backend=parameterStore` on AWS)
- `minReplicas` / `maxReplicas`
- `tags`

The operator creates/updates:

- Launch Template (new version on spec changes)
- Auto Scaling Group

## AMI baseline

AMI should contain:

- Docker engine
- `tiproxy-machine-agent` binary under `/usr/local/bin`
- `tiproxy-machine-agent.service`
- Optional CA/cert toolchain if TiProxy uses TLS secret material

Using a pre-baked AMI is recommended for predictable startup and lower cold-start latency.

## IAM instance profile

Baseline permissions:

- Pull image from registry (e.g. ECR read permissions if using ECR)
- Read cluster authentication metadata (for EKS bootstrap path)
- Optional cloud logging/metrics permissions

If using EKS IAM auth in kubeconfig `exec` mode, add:

- `eks:DescribeCluster`
- `sts:GetCallerIdentity`

## Security group

Inbound:

- Per-link allocated frontend ports from `TiProxyMachineGroup.status.allocatedPorts`
- TiProxy API port (default `3080`) for local ops/health check if needed

Outbound:

- Kubernetes API endpoint
- TiDB PD / NS server endpoints in `TiDBInstanceLink`
- Container registry endpoints
- EKS private API endpoint (or your self-managed apiserver private endpoint)

## Agent kube-apiserver access

`tiproxy-machine-agent` supports out-of-cluster access:

- `--kubeconfig=/path/to/kubeconfig`
- or env `TIPROXY_KUBECONFIG=/path/to/kubeconfig`

Recommended: inject kubeconfig through Launch Template user-data and keep endpoint private.

Built-in default user-data behavior in operator:

- If `spec.bootstrap.kubeconfig.remoteRef.backend=parameterStore` is set, machine bootstrapping fetches kubeconfig from SSM Parameter Store (with decryption).
- It writes kubeconfig to `spec.bootstrap.kubeconfig.path` (default `/etc/tiproxy-machine-agent/kubeconfig`).
- It sets `TIPROXY_KUBECONFIG` in `/etc/tiproxy-machine-agent/env`.

This requires `aws` CLI on AMI and IAM permission:

- `ssm:GetParameter`
- `kms:Decrypt` (if SSM SecureString uses custom KMS key)
