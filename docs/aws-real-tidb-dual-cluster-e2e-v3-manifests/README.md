# AWS Real TiDB Dual Cluster E2E V3 Manifests

这些文件对应本轮 `aws-real-tidb-dual-cluster-e2e-v3` 测试中实际使用过的临时 YAML，现已整理进仓库。

说明：

- 原始临时文件来源：`/tmp/tc-a-v3.yaml`、`/tmp/tc-b-v3.yaml`、`/tmp/coredns-a-v3.yaml`、`/tmp/coredns-b-v3.yaml`、`/tmp/tiproxy-manager-v3.yaml`、`/tmp/tiproxy-machinegroup-and-ports-v3.yaml`、`/tmp/tiproxy-links-v3.yaml`
- 为了方便复用，目录内版本做了两类归一化：
  - 把运行时专属值改成 `${VAR}` 形式的占位符
  - 按调查结论修正了 CoreDNS internal NLB 注解，以及 Link 中 `nsServerAddress` 的推荐写法
- 建议通过 `envsubst` 渲染后再 `kubectl apply -f -`

最少需要替换的变量：

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

按当前文档中的推荐顺序：

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
