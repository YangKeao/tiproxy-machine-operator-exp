# AWS Real TiDB Dual-Cluster E2E V3 Manifests

These files correspond to the temporary YAML files actually used in the `aws-real-tidb-dual-cluster-e2e-v3` test round and are now organized into the repository.

Notes:

- Original temporary file sources: `/tmp/tc-a-v3.yaml`, `/tmp/tc-b-v3.yaml`, `/tmp/coredns-a-v3.yaml`, `/tmp/coredns-b-v3.yaml`, `/tmp/tiproxy-manager-v3.yaml`, `/tmp/tiproxy-machinegroup-and-ports-v3.yaml`, `/tmp/tiproxy-links-v3.yaml`
- For reuse, the versions in this directory were normalized in two ways:
  - runtime-specific values were turned into `${VAR}` placeholders
  - the CoreDNS internal NLB annotation and the recommended `nsServerAddress` format in Link were corrected according to the investigation result
- Recommended usage: render with `envsubst`, then `kubectl apply -f -`

Minimum variables to replace:

- `${AWS_REGION}`
- `${MG_NAME}`
- `${PORT_A_NAME}`
- `${PORT_B_NAME}`
- `${LINK_A}`
- `${LINK_B}`
- `${MANAGER_IMAGE}`
- `${AL2023_AMI_ID}`
- `${TIPROXY_SUBNET_A}`
- `${TIPROXY_SUBNET_B}`
- `${TIPROXY_SG_ID}`
- `${TIPROXY_INSTANCE_PROFILE}`
- `${ARTIFACT_BUCKET}`
- `${KUBECONFIG_PARAM}`
- `${TIPROXY_BASE_IMAGE}`
- `${TIPROXY_VERSION}`
- `${PD_A_ADDRESS}`
- `${PD_B_ADDRESS}`
- `${NS_A_ADDRESS}`
- `${NS_B_ADDRESS}`

Recommended order under the current document set:

1. `00-tidb-namespace.yaml`
2. `01-tc-a.yaml`
3. `02-tc-b.yaml`
4. `03-coredns-nlb.yaml`
5. `10-tiproxy-system-namespace.yaml`
6. `11-tiproxy-manager-rbac.yaml`
7. `12-tiproxy-manager-aws-creds-secret.example.yaml`
8. `13-tiproxy-manager-deployment.yaml`
9. `20-tiproxy-machinegroup-and-ports.yaml`
10. `21-tidb-resource-links.yaml`
11. `30-seed-tc-a-job.yaml`
12. `31-seed-tc-b-job.yaml`
13. `32-mysql-check-pod.yaml`
