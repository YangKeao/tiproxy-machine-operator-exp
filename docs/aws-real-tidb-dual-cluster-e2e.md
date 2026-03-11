# TiProxy Machine Operator: 跨 VPC Peering 的真实 TiDB 双集群联调计划

本文档是“操作计划”，用于你确认后再执行。  
目标拓扑如下：

- 现有 `TiProxy EKS` 继续放在原 VPC（称为 `VPC-TIPROXY`）。
- 新建两个 TiDB 环境：
  - `VPC-TIDB-A` + `EKS-TIDB-A` + 1 套 TiDB（`1 tidb + 1 pd + 3 tikv`）
  - `VPC-TIDB-B` + `EKS-TIDB-B` + 1 套 TiDB（`1 tidb + 1 pd + 3 tikv`）
- 建立两条 Peering：
  - `VPC-TIPROXY <-> VPC-TIDB-A`
  - `VPC-TIPROXY <-> VPC-TIDB-B`
- 在两个 TiDB EKS 中分别用 internal NLB 暴露 CoreDNS（53/udp+tcp）。
- PD 不暴露 NLB，统一通过 headless service（`<tc>-pd-peer.<ns>.svc.cluster.local`）解析并直连 `2379`。
- 在 TiProxy 侧创建两个 `TiDBInstanceLink`，分别指向两个 TiDB 集群的 PD headless DNS 和对应 DNS NLB。
- TiProxy 镜像使用 `yangkeao/tiproxy:support-multiple-pd-clusters`。

## 0. 关键前提和边界

1. 这份文档不在原 TiProxy VPC/EKS 内安装 `tidb-operator`。  
2. 两个 TiDB 集群是两套独立 EKS（不同 VPC）。  
3. TiProxy 与 TiDB 全部通过 VPC Peering 连通。  
4. 你需要确保三个 VPC 的 CIDR 不重叠。  
5. 下面命令默认同账号、同 region（`us-east-1`）执行。

## 1. 环境变量

```bash
export AWS_REGION=us-east-1

# 已有 TiProxy EKS（沿用现有环境）
export TIPROXY_CLUSTER=tiproxye2e20260310181313
export TIPROXY_NS=default
export MG_NAME=tiproxye2e20260310181313-mg

# 新建两个 TiDB EKS/VPC
export TIDB_A_CLUSTER=tidb-a-$(date +%m%d%H%M)
export TIDB_B_CLUSTER=tidb-b-$(date +%m%d%H%M)
export TIDB_A_VPC_CIDR=10.50.0.0/16
export TIDB_B_VPC_CIDR=10.60.0.0/16

# TiDB K8s 命名（两个 EKS 内都使用该名称，无冲突）
export TIDB_NS=tidb
export TC_A=tc-a
export TC_B=tc-b

# TiProxy Link 名称（创建在 TIPROXY_CLUSTER）
export LINK_A=tc-a-link
export LINK_B=tc-b-link

export TIPROXY_TEST_USER=tiproxy_test
export TIPROXY_TEST_PASSWORD='Tiproxy#Pass123'
```

## 2. 识别现有 TiProxy VPC 信息

```bash
export TIPROXY_VPC_ID=$(aws eks describe-cluster \
  --region "${AWS_REGION}" \
  --name "${TIPROXY_CLUSTER}" \
  --query 'cluster.resourcesVpcConfig.vpcId' \
  --output text)

export TIPROXY_VPC_CIDR=$(aws ec2 describe-vpcs \
  --region "${AWS_REGION}" \
  --vpc-ids "${TIPROXY_VPC_ID}" \
  --query 'Vpcs[0].CidrBlock' \
  --output text)

echo "TIPROXY_VPC_ID=${TIPROXY_VPC_ID} CIDR=${TIPROXY_VPC_CIDR}"
```

## 3. 新建两个 EKS（各自新 VPC）

```bash
eksctl create cluster \
  --name "${TIDB_A_CLUSTER}" \
  --region "${AWS_REGION}" \
  --version 1.34 \
  --managed \
  --node-type m6i.xlarge \
  --nodes 4 \
  --nodes-min 4 \
  --nodes-max 6 \
  --node-labels dedicated=tidb \
  --with-oidc \
  --vpc-cidr "${TIDB_A_VPC_CIDR}"

eksctl create cluster \
  --name "${TIDB_B_CLUSTER}" \
  --region "${AWS_REGION}" \
  --version 1.34 \
  --managed \
  --node-type m6i.xlarge \
  --nodes 4 \
  --nodes-min 4 \
  --nodes-max 6 \
  --node-labels dedicated=tidb \
  --with-oidc \
  --vpc-cidr "${TIDB_B_VPC_CIDR}"
```

采集两个 TiDB VPC 信息：

```bash
export TIDB_A_VPC_ID=$(aws eks describe-cluster \
  --region "${AWS_REGION}" \
  --name "${TIDB_A_CLUSTER}" \
  --query 'cluster.resourcesVpcConfig.vpcId' \
  --output text)
export TIDB_B_VPC_ID=$(aws eks describe-cluster \
  --region "${AWS_REGION}" \
  --name "${TIDB_B_CLUSTER}" \
  --query 'cluster.resourcesVpcConfig.vpcId' \
  --output text)

export TIDB_A_VPC_CIDR_REAL=$(aws ec2 describe-vpcs --region "${AWS_REGION}" --vpc-ids "${TIDB_A_VPC_ID}" --query 'Vpcs[0].CidrBlock' --output text)
export TIDB_B_VPC_CIDR_REAL=$(aws ec2 describe-vpcs --region "${AWS_REGION}" --vpc-ids "${TIDB_B_VPC_ID}" --query 'Vpcs[0].CidrBlock' --output text)

echo "A: ${TIDB_A_VPC_ID} ${TIDB_A_VPC_CIDR_REAL}"
echo "B: ${TIDB_B_VPC_ID} ${TIDB_B_VPC_CIDR_REAL}"
```

## 4. 建立两条 VPC Peering 并打路由

### 4.1 TIPROXY <-> TIDB-A

```bash
export PCX_A=$(aws ec2 create-vpc-peering-connection \
  --region "${AWS_REGION}" \
  --vpc-id "${TIPROXY_VPC_ID}" \
  --peer-vpc-id "${TIDB_A_VPC_ID}" \
  --query 'VpcPeeringConnection.VpcPeeringConnectionId' \
  --output text)

aws ec2 accept-vpc-peering-connection \
  --region "${AWS_REGION}" \
  --vpc-peering-connection-id "${PCX_A}" >/dev/null

aws ec2 modify-vpc-peering-connection-options \
  --region "${AWS_REGION}" \
  --vpc-peering-connection-id "${PCX_A}" \
  --requester-peering-connection-options AllowDnsResolutionFromRemoteVpc=true \
  --accepter-peering-connection-options AllowDnsResolutionFromRemoteVpc=true
```

### 4.2 TIPROXY <-> TIDB-B

```bash
export PCX_B=$(aws ec2 create-vpc-peering-connection \
  --region "${AWS_REGION}" \
  --vpc-id "${TIPROXY_VPC_ID}" \
  --peer-vpc-id "${TIDB_B_VPC_ID}" \
  --query 'VpcPeeringConnection.VpcPeeringConnectionId' \
  --output text)

aws ec2 accept-vpc-peering-connection \
  --region "${AWS_REGION}" \
  --vpc-peering-connection-id "${PCX_B}" >/dev/null

aws ec2 modify-vpc-peering-connection-options \
  --region "${AWS_REGION}" \
  --vpc-peering-connection-id "${PCX_B}" \
  --requester-peering-connection-options AllowDnsResolutionFromRemoteVpc=true \
  --accepter-peering-connection-options AllowDnsResolutionFromRemoteVpc=true
```

### 4.3 路由表写入（所有 route table）

```bash
add_route_all_tables() {
  local vpc_id="$1"
  local dst_cidr="$2"
  local pcx_id="$3"
  for rtb in $(aws ec2 describe-route-tables --region "${AWS_REGION}" \
      --filters Name=vpc-id,Values="${vpc_id}" \
      --query 'RouteTables[].RouteTableId' --output text); do
    aws ec2 create-route \
      --region "${AWS_REGION}" \
      --route-table-id "${rtb}" \
      --destination-cidr-block "${dst_cidr}" \
      --vpc-peering-connection-id "${pcx_id}" >/dev/null 2>&1 || \
    aws ec2 replace-route \
      --region "${AWS_REGION}" \
      --route-table-id "${rtb}" \
      --destination-cidr-block "${dst_cidr}" \
      --vpc-peering-connection-id "${pcx_id}" >/dev/null
  done
}

add_route_all_tables "${TIPROXY_VPC_ID}" "${TIDB_A_VPC_CIDR_REAL}" "${PCX_A}"
add_route_all_tables "${TIDB_A_VPC_ID}" "${TIPROXY_VPC_CIDR}" "${PCX_A}"

add_route_all_tables "${TIPROXY_VPC_ID}" "${TIDB_B_VPC_CIDR_REAL}" "${PCX_B}"
add_route_all_tables "${TIDB_B_VPC_ID}" "${TIPROXY_VPC_CIDR}" "${PCX_B}"
```

## 5. TiDB 两侧 EKS 安全组放行（从 TiProxy VPC 来流量）

说明：TiProxy 需要访问以下流量：

- TiDB SQL/status（典型 `4000`/`10080`）
- PD client（`2379`，目标是 `pd-peer` 解析出的 PD Pod IP）
- CoreDNS NLB 的 NodePort（通常 `30000-32767`，TCP/UDP）

为避免漏配，直接给两个 TiDB 集群的节点 SG 加规则：

```bash
export NODE_SG_A=$(aws cloudformation describe-stacks \
  --region "${AWS_REGION}" \
  --stack-name "eksctl-${TIDB_A_CLUSTER}-cluster" \
  --query "Stacks[0].Outputs[?OutputKey=='ClusterSharedNodeSecurityGroup'].OutputValue" \
  --output text)

export NODE_SG_B=$(aws cloudformation describe-stacks \
  --region "${AWS_REGION}" \
  --stack-name "eksctl-${TIDB_B_CLUSTER}-cluster" \
  --query "Stacks[0].Outputs[?OutputKey=='ClusterSharedNodeSecurityGroup'].OutputValue" \
  --output text)

allow_from_tiproxy() {
  local sg_id="$1"
  aws ec2 authorize-security-group-ingress \
    --region "${AWS_REGION}" \
    --group-id "${sg_id}" \
    --ip-permissions "[
      {\"IpProtocol\":\"tcp\",\"FromPort\":4000,\"ToPort\":4000,\"IpRanges\":[{\"CidrIp\":\"${TIPROXY_VPC_CIDR}\",\"Description\":\"TiProxy->TiDB SQL\"}]},
      {\"IpProtocol\":\"tcp\",\"FromPort\":10080,\"ToPort\":10080,\"IpRanges\":[{\"CidrIp\":\"${TIPROXY_VPC_CIDR}\",\"Description\":\"TiProxy->TiDB status\"}]},
      {\"IpProtocol\":\"tcp\",\"FromPort\":2379,\"ToPort\":2379,\"IpRanges\":[{\"CidrIp\":\"${TIPROXY_VPC_CIDR}\",\"Description\":\"TiProxy->PD client via headless DNS\"}]},
      {\"IpProtocol\":\"tcp\",\"FromPort\":30000,\"ToPort\":32767,\"IpRanges\":[{\"CidrIp\":\"${TIPROXY_VPC_CIDR}\",\"Description\":\"TiProxy->NLB NodePort\"}]},
      {\"IpProtocol\":\"udp\",\"FromPort\":30000,\"ToPort\":32767,\"IpRanges\":[{\"CidrIp\":\"${TIPROXY_VPC_CIDR}\",\"Description\":\"TiProxy->NLB NodePort UDP\"}]}
    ]" >/dev/null 2>&1 || true
}

allow_from_tiproxy "${NODE_SG_A}"
allow_from_tiproxy "${NODE_SG_B}"
```

## 6. 配置 kube contexts（3 套）

```bash
aws eks update-kubeconfig --region "${AWS_REGION}" --name "${TIPROXY_CLUSTER}"
aws eks update-kubeconfig --region "${AWS_REGION}" --name "${TIDB_A_CLUSTER}"
aws eks update-kubeconfig --region "${AWS_REGION}" --name "${TIDB_B_CLUSTER}"

export CTX_TIPROXY="arn:aws:eks:${AWS_REGION}:$(aws sts get-caller-identity --query Account --output text):cluster/${TIPROXY_CLUSTER}"
export CTX_TIDB_A="arn:aws:eks:${AWS_REGION}:$(aws sts get-caller-identity --query Account --output text):cluster/${TIDB_A_CLUSTER}"
export CTX_TIDB_B="arn:aws:eks:${AWS_REGION}:$(aws sts get-caller-identity --query Account --output text):cluster/${TIDB_B_CLUSTER}"
```

## 7. 在两个 TiDB EKS 各自安装 tidb-operator

```bash
install_tidb_operator() {
  local ctx="$1"
  kubectl --context "${ctx}" create ns tidb-admin --dry-run=client -o yaml | kubectl --context "${ctx}" apply -f -
  kubectl --context "${ctx}" apply -f /home/yangkeao/Project/github.com/YangKeao/tidb-operator/manifests/crd.yaml
  helm upgrade --install tidb-operator \
    /home/yangkeao/Project/github.com/YangKeao/tidb-operator/charts/tidb-operator \
    --kube-context "${ctx}" \
    --namespace tidb-admin \
    --set scheduler.create=false \
    --set controllerManager.replicas=1
  kubectl --context "${ctx}" -n tidb-admin wait --for=condition=Available deploy \
    -l app.kubernetes.io/component=controller-manager --timeout=10m
}

install_tidb_operator "${CTX_TIDB_A}"
install_tidb_operator "${CTX_TIDB_B}"
```

## 8. 在两个 TiDB EKS 各自部署最小 TiDB 集群

### 8.1 `EKS-TIDB-A` 部署 `tc-a`

```bash
cat >/tmp/tc-a.yaml <<'EOF'
apiVersion: pingcap.com/v1alpha1
kind: TidbCluster
metadata:
  name: tc-a
  namespace: tidb
spec:
  version: v8.5.3
  timezone: UTC
  pvReclaimPolicy: Retain
  enableDynamicConfiguration: true
  configUpdateStrategy: RollingUpdate
  discovery: {}
  helper:
    image: alpine:3.16.0
  pd:
    baseImage: pingcap/pd
    replicas: 1
    maxFailoverCount: 0
    requests:
      storage: "10Gi"
    storageClassName: gp2
    nodeSelector:
      dedicated: tidb
    config: {}
  tikv:
    baseImage: pingcap/tikv
    replicas: 3
    maxFailoverCount: 0
    requests:
      storage: "10Gi"
    storageClassName: gp2
    nodeSelector:
      dedicated: tidb
    config:
      storage:
        reserve-space: "0MB"
      rocksdb:
        max-open-files: 256
      raftdb:
        max-open-files: 256
  tidb:
    baseImage: pingcap/tidb
    replicas: 1
    maxFailoverCount: 0
    service:
      type: ClusterIP
    # 先占位，后续按 TiDBInstanceLink assignedPort 回填
    serverLabels:
      tiproxy-port: "6000"
    nodeSelector:
      dedicated: tidb
    config: {}
EOF

kubectl --context "${CTX_TIDB_A}" create ns "${TIDB_NS}" --dry-run=client -o yaml | kubectl --context "${CTX_TIDB_A}" apply -f -
kubectl --context "${CTX_TIDB_A}" apply -f /tmp/tc-a.yaml
kubectl --context "${CTX_TIDB_A}" -n "${TIDB_NS}" wait --for=condition=Ready tidbcluster/"${TC_A}" --timeout=60m
```

### 8.2 `EKS-TIDB-B` 部署 `tc-b`

```bash
cat >/tmp/tc-b.yaml <<'EOF'
apiVersion: pingcap.com/v1alpha1
kind: TidbCluster
metadata:
  name: tc-b
  namespace: tidb
spec:
  version: v8.5.3
  timezone: UTC
  pvReclaimPolicy: Retain
  enableDynamicConfiguration: true
  configUpdateStrategy: RollingUpdate
  discovery: {}
  helper:
    image: alpine:3.16.0
  pd:
    baseImage: pingcap/pd
    replicas: 1
    maxFailoverCount: 0
    requests:
      storage: "10Gi"
    storageClassName: gp2
    nodeSelector:
      dedicated: tidb
    config: {}
  tikv:
    baseImage: pingcap/tikv
    replicas: 3
    maxFailoverCount: 0
    requests:
      storage: "10Gi"
    storageClassName: gp2
    nodeSelector:
      dedicated: tidb
    config:
      storage:
        reserve-space: "0MB"
      rocksdb:
        max-open-files: 256
      raftdb:
        max-open-files: 256
  tidb:
    baseImage: pingcap/tidb
    replicas: 1
    maxFailoverCount: 0
    service:
      type: ClusterIP
    # 先占位，后续按 TiDBInstanceLink assignedPort 回填
    serverLabels:
      tiproxy-port: "6001"
    nodeSelector:
      dedicated: tidb
    config: {}
EOF

kubectl --context "${CTX_TIDB_B}" create ns "${TIDB_NS}" --dry-run=client -o yaml | kubectl --context "${CTX_TIDB_B}" apply -f -
kubectl --context "${CTX_TIDB_B}" apply -f /tmp/tc-b.yaml
kubectl --context "${CTX_TIDB_B}" -n "${TIDB_NS}" wait --for=condition=Ready tidbcluster/"${TC_B}" --timeout=60m
```

如果你的默认 StorageClass 不是 `gp2`（例如 `gp3`），请把 YAML 中 `storageClassName` 换成真实值。

## 9. 在每个 TiDB EKS 仅暴露 CoreDNS（internal NLB），PD 使用 headless service

### 9.1 EKS-TIDB-A

```bash
cat >/tmp/tidb-a-coredns.yaml <<'EOF'
apiVersion: v1
kind: Service
metadata:
  name: coredns-nlb
  namespace: kube-system
  annotations:
    service.beta.kubernetes.io/aws-load-balancer-type: nlb
    service.beta.kubernetes.io/aws-load-balancer-scheme: internal
spec:
  type: LoadBalancer
  selector:
    k8s-app: kube-dns
  ports:
  - name: dns-udp
    protocol: UDP
    port: 53
    targetPort: 53
  - name: dns-tcp
    protocol: TCP
    port: 53
    targetPort: 53
EOF

kubectl --context "${CTX_TIDB_A}" apply -f /tmp/tidb-a-coredns.yaml
kubectl --context "${CTX_TIDB_A}" -n kube-system get svc coredns-nlb -w

export NS_A=$(kubectl --context "${CTX_TIDB_A}" -n kube-system get svc coredns-nlb -o jsonpath='{.status.loadBalancer.ingress[0].hostname}')
export PD_A_FQDN="${TC_A}-pd-peer.${TIDB_NS}.svc.cluster.local"
kubectl --context "${CTX_TIDB_A}" -n "${TIDB_NS}" get svc "${TC_A}-pd-peer"
```

### 9.2 EKS-TIDB-B

```bash
cat >/tmp/tidb-b-coredns.yaml <<'EOF'
apiVersion: v1
kind: Service
metadata:
  name: coredns-nlb
  namespace: kube-system
  annotations:
    service.beta.kubernetes.io/aws-load-balancer-type: nlb
    service.beta.kubernetes.io/aws-load-balancer-scheme: internal
spec:
  type: LoadBalancer
  selector:
    k8s-app: kube-dns
  ports:
  - name: dns-udp
    protocol: UDP
    port: 53
    targetPort: 53
  - name: dns-tcp
    protocol: TCP
    port: 53
    targetPort: 53
EOF

kubectl --context "${CTX_TIDB_B}" apply -f /tmp/tidb-b-coredns.yaml
kubectl --context "${CTX_TIDB_B}" -n kube-system get svc coredns-nlb -w

export NS_B=$(kubectl --context "${CTX_TIDB_B}" -n kube-system get svc coredns-nlb -o jsonpath='{.status.loadBalancer.ingress[0].hostname}')
export PD_B_FQDN="${TC_B}-pd-peer.${TIDB_NS}.svc.cluster.local"
kubectl --context "${CTX_TIDB_B}" -n "${TIDB_NS}" get svc "${TC_B}-pd-peer"
```

### 9.3 从 TiProxy VPC 验证 nsServerAddress 与 PD 2379 连通

```bash
kubectl --context "${CTX_TIPROXY}" -n "${TIPROXY_NS}" run dns-pd-check \
  --image=nicolaka/netshoot \
  --restart=Never \
  --command -- sh -ec "
    echo 'A DNS:'; dig +short @${NS_A} ${PD_A_FQDN};
    echo 'B DNS:'; dig +short @${NS_B} ${PD_B_FQDN};
    for ip in \$(dig +short @${NS_A} ${PD_A_FQDN}); do nc -vz -w2 \$ip 2379; done;
    for ip in \$(dig +short @${NS_B} ${PD_B_FQDN}); do nc -vz -w2 \$ip 2379; done
  "

kubectl --context "${CTX_TIPROXY}" -n "${TIPROXY_NS}" wait pod/dns-pd-check --for=jsonpath='{.status.phase}'=Succeeded --timeout=180s
kubectl --context "${CTX_TIPROXY}" -n "${TIPROXY_NS}" logs dns-pd-check
kubectl --context "${CTX_TIPROXY}" -n "${TIPROXY_NS}" delete pod dns-pd-check
```

## 10. 在 TiProxy EKS 切换 TiProxy 镜像并轮转实例

```bash
kubectl --context "${CTX_TIPROXY}" patch tiproxymachinegroup "${MG_NAME}" -n "${TIPROXY_NS}" --type merge -p '{
  "spec": {
    "tiproxy": {
      "baseImage": "yangkeao/tiproxy",
      "version": "support-multiple-pd-clusters",
      "apiPort": 3080
    }
  }
}'

export ASG_NAME=$(kubectl --context "${CTX_TIPROXY}" get tiproxymachinegroup "${MG_NAME}" -n "${TIPROXY_NS}" -o jsonpath='{.status.cloud.machineGroupName}')
export OLD_INSTANCE_ID=$(aws autoscaling describe-auto-scaling-groups \
  --region "${AWS_REGION}" \
  --auto-scaling-group-names "${ASG_NAME}" \
  --query 'AutoScalingGroups[0].Instances[?LifecycleState==`InService`].InstanceId | [0]' \
  --output text)

aws autoscaling terminate-instance-in-auto-scaling-group \
  --region "${AWS_REGION}" \
  --instance-id "${OLD_INSTANCE_ID}" \
  --no-should-decrement-desired-capacity
```

## 11. 在 TiProxy EKS 创建两个 TiDBInstanceLink

```bash
cat >/tmp/link-a.yaml <<EOF
apiVersion: tiproxy.pingcap.com/v1alpha1
kind: TiDBInstanceLink
metadata:
  name: ${LINK_A}
  namespace: ${TIPROXY_NS}
spec:
  machineGroupRef:
    name: ${MG_NAME}
  clusterName: tidb-a
  pdAddresses:
  - ${PD_A_FQDN}:2379
  nsServerAddress: ${NS_A}:53
EOF

cat >/tmp/link-b.yaml <<EOF
apiVersion: tiproxy.pingcap.com/v1alpha1
kind: TiDBInstanceLink
metadata:
  name: ${LINK_B}
  namespace: ${TIPROXY_NS}
spec:
  machineGroupRef:
    name: ${MG_NAME}
  clusterName: tidb-b
  pdAddresses:
  - ${PD_B_FQDN}:2379
  nsServerAddress: ${NS_B}:53
EOF

kubectl --context "${CTX_TIPROXY}" apply -f /tmp/link-a.yaml
kubectl --context "${CTX_TIPROXY}" apply -f /tmp/link-b.yaml

export PORT_A=$(kubectl --context "${CTX_TIPROXY}" get tidbinstancelink "${LINK_A}" -n "${TIPROXY_NS}" -o jsonpath='{.status.assignedPort}')
export PORT_B=$(kubectl --context "${CTX_TIPROXY}" get tidbinstancelink "${LINK_B}" -n "${TIPROXY_NS}" -o jsonpath='{.status.assignedPort}')
echo "assigned: A=${PORT_A} B=${PORT_B}"
```

## 12. 回填两个 TiDB 集群的 `tiproxy-port` 标签

```bash
kubectl --context "${CTX_TIDB_A}" -n "${TIDB_NS}" patch tidbcluster "${TC_A}" --type merge -p "{
  \"spec\": {\"tidb\": {\"serverLabels\": {\"tiproxy-port\": \"${PORT_A}\"}}}
}"

kubectl --context "${CTX_TIDB_B}" -n "${TIDB_NS}" patch tidbcluster "${TC_B}" --type merge -p "{
  \"spec\": {\"tidb\": {\"serverLabels\": {\"tiproxy-port\": \"${PORT_B}\"}}}
}"
```

## 13. 准备验证数据并做端口路由验证

在两个集群写入同名表不同值：

```bash
export ROOT_A=$(kubectl --context "${CTX_TIDB_A}" -n "${TIDB_NS}" get secret "${TC_A}-tidb-secret" -o jsonpath='{.data.root}' | base64 --decode)
export ROOT_B=$(kubectl --context "${CTX_TIDB_B}" -n "${TIDB_NS}" get secret "${TC_B}-tidb-secret" -o jsonpath='{.data.root}' | base64 --decode)

kubectl --context "${CTX_TIDB_A}" -n "${TIDB_NS}" port-forward svc/${TC_A}-tidb 14000:4000 >/tmp/pf-a.log 2>&1 &
PF_A=$!
kubectl --context "${CTX_TIDB_B}" -n "${TIDB_NS}" port-forward svc/${TC_B}-tidb 14001:4000 >/tmp/pf-b.log 2>&1 &
PF_B=$!
sleep 3

mysql -h127.0.0.1 -P14000 -uroot -p"${ROOT_A}" -e "
CREATE DATABASE IF NOT EXISTS route_check;
CREATE TABLE IF NOT EXISTS route_check.identity(msg VARCHAR(64) PRIMARY KEY);
REPLACE INTO route_check.identity VALUES('cluster-a');
CREATE USER IF NOT EXISTS '${TIPROXY_TEST_USER}'@'%' IDENTIFIED BY '${TIPROXY_TEST_PASSWORD}';
GRANT ALL PRIVILEGES ON route_check.* TO '${TIPROXY_TEST_USER}'@'%';
FLUSH PRIVILEGES;"

mysql -h127.0.0.1 -P14001 -uroot -p"${ROOT_B}" -e "
CREATE DATABASE IF NOT EXISTS route_check;
CREATE TABLE IF NOT EXISTS route_check.identity(msg VARCHAR(64) PRIMARY KEY);
REPLACE INTO route_check.identity VALUES('cluster-b');
CREATE USER IF NOT EXISTS '${TIPROXY_TEST_USER}'@'%' IDENTIFIED BY '${TIPROXY_TEST_PASSWORD}';
GRANT ALL PRIVILEGES ON route_check.* TO '${TIPROXY_TEST_USER}'@'%';
FLUSH PRIVILEGES;"

kill "${PF_A}" "${PF_B}"
```

本地连 TiProxy 前，确认 TiProxy 实例 SG 已放行你当前出口 IP：

```bash
export TIPROXY_SG_ID=$(kubectl --context "${CTX_TIPROXY}" get tiproxymachinegroup "${MG_NAME}" -n "${TIPROXY_NS}" -o jsonpath='{.spec.infrastructure.securityGroupIDs[0]}')
export PORT_START=$(kubectl --context "${CTX_TIPROXY}" get tiproxymachinegroup "${MG_NAME}" -n "${TIPROXY_NS}" -o jsonpath='{.spec.portRange.start}')
export PORT_END=$(kubectl --context "${CTX_TIPROXY}" get tiproxymachinegroup "${MG_NAME}" -n "${TIPROXY_NS}" -o jsonpath='{.spec.portRange.end}')
export MY_CIDR="$(curl -fsSL https://checkip.amazonaws.com | tr -d '\n')/32"

aws ec2 authorize-security-group-ingress \
  --region "${AWS_REGION}" \
  --group-id "${TIPROXY_SG_ID}" \
  --ip-permissions "[
    {\"IpProtocol\":\"tcp\",\"FromPort\":3080,\"ToPort\":3080,\"IpRanges\":[{\"CidrIp\":\"${MY_CIDR}\",\"Description\":\"TiProxy API\"}]},
    {\"IpProtocol\":\"tcp\",\"FromPort\":${PORT_START},\"ToPort\":${PORT_END},\"IpRanges\":[{\"CidrIp\":\"${MY_CIDR}\",\"Description\":\"TiProxy SQL ports\"}]}
  ]" >/dev/null 2>&1 || true
```

通过 TiProxy 验证：

```bash
export INSTANCE_ID=$(aws autoscaling describe-auto-scaling-groups \
  --region "${AWS_REGION}" \
  --auto-scaling-group-names "${ASG_NAME}" \
  --query 'AutoScalingGroups[0].Instances[?LifecycleState==`InService`].InstanceId | [0]' \
  --output text)
export TIPROXY_IP=$(aws ec2 describe-instances \
  --region "${AWS_REGION}" \
  --instance-ids "${INSTANCE_ID}" \
  --query 'Reservations[0].Instances[0].PublicIpAddress' \
  --output text)

curl -sS "http://${TIPROXY_IP}:3080/api/admin/config/" | sed -n '1,220p'

mysql -h"${TIPROXY_IP}" -P"${PORT_A}" -u"${TIPROXY_TEST_USER}" -p"${TIPROXY_TEST_PASSWORD}" -e "SELECT msg FROM route_check.identity;"
mysql -h"${TIPROXY_IP}" -P"${PORT_B}" -u"${TIPROXY_TEST_USER}" -p"${TIPROXY_TEST_PASSWORD}" -e "SELECT msg FROM route_check.identity;"
```

预期：

- `PORT_A` 返回 `cluster-a`
- `PORT_B` 返回 `cluster-b`

## 14. 常见失败点（这版拓扑特有）

1. Peering 建了但不通：通常是 route table 漏了一侧。  
2. 内部 NLB hostname 无法解析：通常是 peering DNS 选项未开启。  
3. `no backend`：通常是 TiDB `serverLabels.tiproxy-port` 未对齐 `assignedPort`。  
4. PD 连通失败：优先检查 `pdAddresses` 是否为 `*-pd-peer.*.svc.cluster.local`，以及节点 SG 是否允许来自 TiProxy VPC 的 `2379/tcp`。  
5. TiProxy 仍旧旧镜像：MG 改镜像后未轮转实例。

## 15. 清理建议（按需）

如果你后续决定删除测试环境，按顺序清：

1. 删除 `TiDBInstanceLink`
2. 删除两套 TiDB CR
3. 删除两个 TiDB EKS（`eksctl delete cluster ...`）
4. 删除两条 Peering
5. 清理新增路由（可选）
