# TiProxy Machine Operator: 从空 AWS 网络开始的最小可跑验证

这份文档给一条最小路径，用来验证下面这条链路是否完整可用:

1. `tiproxy-machine-operator` 监听 `TiProxyMachineGroup` 和 `TiDBInstanceLink`
2. operator 在 AWS 创建 Launch Template + ASG
3. ASG 起机后运行 `tiproxy-machine-agent` (非 k8s 内)
4. agent 从 Kubernetes 拉取 MachineGroup 状态，渲染 TiProxy 配置，启动 TiProxy，并通过 `PUT /api/admin/config` 动态更新

为了最小可跑，本流程采用:

- `eksctl` 自动创建 EKS (也会创建 VPC 和子网)
- agent kubeconfig 放到 SSM Parameter Store (先用 `String`，不加密，便于快速打通)
- EC2 实例通过 S3 拉取 agent 二进制和 systemd unit
- EKS API 先走公共访问，后续你可以改为 private endpoint

## 0. 前置条件

本机需要:

- `aws` CLI v2
- `eksctl`
- `kubectl`
- `go` (>= 1.24)
- 已配置好 AWS 凭证 (`aws sts get-caller-identity` 能成功)

仓库路径:

```bash
cd /home/yangkeao/Project/github.com/YangKeao/tiproxy-machine-operator
```

准备环境变量:

```bash
export AWS_REGION=us-west-2
export CLUSTER_NAME=tiproxy-e2e
export K8S_NAMESPACE=default
export MG_NAME=demo-aws
export LINK_NAME=demo-link
export INSTANCE_ROLE_NAME=tiproxy-machine-agent-role
export INSTANCE_PROFILE_NAME=tiproxy-machine-agent-profile
export ARTIFACT_BUCKET=tiproxy-agent-artifacts-$(date +%s)-$RANDOM
export KUBECONFIG_PARAM=/tiproxy/${CLUSTER_NAME}/agent-kubeconfig
export AWS_ACCOUNT_ID=$(aws sts get-caller-identity --query Account --output text)
```

## 1. 创建 EKS (自动创建 VPC)

```bash
eksctl create cluster \
  --name "${CLUSTER_NAME}" \
  --region "${AWS_REGION}" \
  --version 1.31 \
  --managed \
  --nodes 2 \
  --node-type t3.large
```

拉取 kubeconfig 到本机:

```bash
aws eks update-kubeconfig --region "${AWS_REGION}" --name "${CLUSTER_NAME}"
```

取出 VPC/Subnet 信息给后续 ASG 使用:

```bash
export VPC_ID=$(aws eks describe-cluster \
  --name "${CLUSTER_NAME}" \
  --region "${AWS_REGION}" \
  --query 'cluster.resourcesVpcConfig.vpcId' \
  --output text)

read -r SUBNET_A SUBNET_B _ <<<"$(aws eks describe-cluster \
  --name "${CLUSTER_NAME}" \
  --region "${AWS_REGION}" \
  --query 'cluster.resourcesVpcConfig.subnetIds' \
  --output text)"
```

## 2. 创建 TiProxy 机器的 Security Group

最小验证先只放开你当前公网 IP 到 `6000-6099` 和 `3080`:

```bash
export MY_CIDR="$(curl -fsSL https://checkip.amazonaws.com | tr -d '\n')/32"
export TIPROXY_SG_ID=$(aws ec2 create-security-group \
  --group-name "${CLUSTER_NAME}-tiproxy-sg" \
  --description "TiProxy machine SG" \
  --vpc-id "${VPC_ID}" \
  --region "${AWS_REGION}" \
  --query 'GroupId' \
  --output text)

aws ec2 authorize-security-group-ingress \
  --group-id "${TIPROXY_SG_ID}" \
  --region "${AWS_REGION}" \
  --ip-permissions "[
    {\"IpProtocol\":\"tcp\",\"FromPort\":6000,\"ToPort\":6099,\"IpRanges\":[{\"CidrIp\":\"${MY_CIDR}\",\"Description\":\"TiProxy SQL ports\"}]},
    {\"IpProtocol\":\"tcp\",\"FromPort\":3080,\"ToPort\":3080,\"IpRanges\":[{\"CidrIp\":\"${MY_CIDR}\",\"Description\":\"TiProxy API\"}]}
  ]"
```

如果你在验证期间切换网络（例如办公网/VPN），公网出口 IP 可能变化，导致访问 `3080/6000` 超时。此时需要更新 SG 白名单。

## 3. 构建并上传 agent 制品到 S3

```bash
go build -o /tmp/tiproxy-machine-agent ./cmd/tiproxy-machine-agent

aws s3 mb "s3://${ARTIFACT_BUCKET}" --region "${AWS_REGION}"
aws s3 cp /tmp/tiproxy-machine-agent "s3://${ARTIFACT_BUCKET}/tiproxy-machine-agent" --region "${AWS_REGION}"
aws s3 cp deploy/systemd/tiproxy-machine-agent.service "s3://${ARTIFACT_BUCKET}/tiproxy-machine-agent.service" --region "${AWS_REGION}"
```

## 4. 创建 EC2 实例角色和 Instance Profile

信任策略:

```bash
cat >/tmp/tiproxy-agent-trust-policy.json <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": { "Service": "ec2.amazonaws.com" },
      "Action": "sts:AssumeRole"
    }
  ]
}
EOF
```

权限策略 (最小集合):

```bash
cat >/tmp/tiproxy-agent-inline-policy.json <<EOF
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["ssm:GetParameter"],
      "Resource": "arn:aws:ssm:${AWS_REGION}:${AWS_ACCOUNT_ID}:parameter${KUBECONFIG_PARAM}"
    },
    {
      "Effect": "Allow",
      "Action": ["eks:DescribeCluster"],
      "Resource": "arn:aws:eks:${AWS_REGION}:${AWS_ACCOUNT_ID}:cluster/${CLUSTER_NAME}"
    },
    {
      "Effect": "Allow",
      "Action": ["sts:GetCallerIdentity"],
      "Resource": "*"
    },
    {
      "Effect": "Allow",
      "Action": ["s3:GetObject"],
      "Resource": "arn:aws:s3:::${ARTIFACT_BUCKET}/*"
    }
  ]
}
EOF
```

创建角色与实例配置:

```bash
aws iam create-role \
  --role-name "${INSTANCE_ROLE_NAME}" \
  --assume-role-policy-document file:///tmp/tiproxy-agent-trust-policy.json

aws iam put-role-policy \
  --role-name "${INSTANCE_ROLE_NAME}" \
  --policy-name tiproxy-agent-inline \
  --policy-document file:///tmp/tiproxy-agent-inline-policy.json

aws iam create-instance-profile \
  --instance-profile-name "${INSTANCE_PROFILE_NAME}"

aws iam add-role-to-instance-profile \
  --instance-profile-name "${INSTANCE_PROFILE_NAME}" \
  --role-name "${INSTANCE_ROLE_NAME}"

sleep 20

export INSTANCE_ROLE_ARN="arn:aws:iam::${AWS_ACCOUNT_ID}:role/${INSTANCE_ROLE_NAME}"
```

## 5. 给实例角色开通 EKS API 访问

确保集群支持 access entry:

```bash
aws eks update-cluster-config \
  --name "${CLUSTER_NAME}" \
  --region "${AWS_REGION}" \
  --access-config authenticationMode=API_AND_CONFIG_MAP
```

等待集群回到 `ACTIVE` 后执行:

```bash
aws eks create-access-entry \
  --cluster-name "${CLUSTER_NAME}" \
  --region "${AWS_REGION}" \
  --principal-arn "${INSTANCE_ROLE_ARN}" \
  --type STANDARD || true

aws eks associate-access-policy \
  --cluster-name "${CLUSTER_NAME}" \
  --region "${AWS_REGION}" \
  --principal-arn "${INSTANCE_ROLE_ARN}" \
  --policy-arn arn:aws:eks::aws:cluster-access-policy/AmazonEKSClusterAdminPolicy \
  --access-scope type=cluster
```

## 6. 生成 agent kubeconfig 并写入 SSM

最小验证先用 `String` 参数。注意不要直接把本机完整 kubeconfig 放进 SSM:

- `aws eks update-kubeconfig --dry-run` 在本机 context 较多时会包含大量无关配置
- SSM Parameter Store 限制: `String` 标准参数最大 4KB, 高级参数最大 8KB
- 建议构造“只包含当前 EKS 集群”的最小 kubeconfig

```bash
export CLUSTER_ENDPOINT=$(aws eks describe-cluster \
  --name "${CLUSTER_NAME}" \
  --region "${AWS_REGION}" \
  --query 'cluster.endpoint' \
  --output text)

export CLUSTER_CA=$(aws eks describe-cluster \
  --name "${CLUSTER_NAME}" \
  --region "${AWS_REGION}" \
  --query 'cluster.certificateAuthority.data' \
  --output text)

cat >/tmp/tiproxy-agent.kubeconfig <<EOF
apiVersion: v1
kind: Config
clusters:
- cluster:
    server: ${CLUSTER_ENDPOINT}
    certificate-authority-data: ${CLUSTER_CA}
  name: ${CLUSTER_NAME}
contexts:
- context:
    cluster: ${CLUSTER_NAME}
    user: aws
  name: ${CLUSTER_NAME}
current-context: ${CLUSTER_NAME}
users:
- name: aws
  user:
    exec:
      apiVersion: client.authentication.k8s.io/v1beta1
      command: aws
      args:
      - eks
      - get-token
      - --region
      - ${AWS_REGION}
      - --cluster-name
      - ${CLUSTER_NAME}
EOF

aws ssm put-parameter \
  --name "${KUBECONFIG_PARAM}" \
  --region "${AWS_REGION}" \
  --type String \
  --overwrite \
  --value "$(cat /tmp/tiproxy-agent.kubeconfig)"
```

## 7. 启动 operator 并安装 CRD

```bash
kubectl apply -f config/crd/bases
```

新开一个终端窗口运行 operator:

```bash
cd /home/yangkeao/Project/github.com/YangKeao/tiproxy-machine-operator
go run ./cmd/manager --cloud-provider=aws --aws-region="${AWS_REGION}"
```

## 8. 创建 TiProxyMachineGroup 和 TiDBInstanceLink

获取一个 AL2023 AMI:

```bash
export AL2023_AMI_ID=$(aws ssm get-parameter \
  --name /aws/service/ami-amazon-linux-latest/al2023-ami-kernel-default-x86_64 \
  --region "${AWS_REGION}" \
  --query 'Parameter.Value' \
  --output text)
```

创建 MachineGroup:

```bash
cat >/tmp/tiproxy-mg.yaml <<'EOF'
apiVersion: tiproxy.pingcap.com/v1alpha1
kind: TiProxyMachineGroup
metadata:
  name: ${MG_NAME}
  namespace: ${K8S_NAMESPACE}
spec:
  provider: aws
  replicas: 1
  portRange:
    start: 6000
    end: 6099
  infrastructure:
    region: ${AWS_REGION}
    machineImage: ${AL2023_AMI_ID}
    machineType: t3.large
    subnetIDs:
    - ${SUBNET_A}
    - ${SUBNET_B}
    securityGroupIDs:
    - ${TIPROXY_SG_ID}
    instanceProfile: ${INSTANCE_PROFILE_NAME}
  bootstrap:
    userData: |
      #!/bin/bash
      set -euxo pipefail
      dnf install -y docker awscli
      systemctl enable --now docker

      mkdir -p /usr/local/bin /etc/systemd/system /etc/tiproxy-machine-agent
      aws s3 cp s3://${ARTIFACT_BUCKET}/tiproxy-machine-agent /usr/local/bin/tiproxy-machine-agent --region ${AWS_REGION}
      chmod +x /usr/local/bin/tiproxy-machine-agent
      aws s3 cp s3://${ARTIFACT_BUCKET}/tiproxy-machine-agent.service /etc/systemd/system/tiproxy-machine-agent.service --region ${AWS_REGION}

      cat >/etc/tiproxy-machine-agent/env <<'ENVEOF'
      TIPROXY_MACHINE_NAMESPACE=${K8S_NAMESPACE}
      TIPROXY_MACHINE_GROUP=${MG_NAME}
      TIPROXY_KUBECONFIG=/etc/tiproxy-machine-agent/kubeconfig
      ENVEOF

      aws ssm get-parameter \
        --name "${KUBECONFIG_PARAM}" \
        --with-decryption \
        --query 'Parameter.Value' \
        --output text \
        --region ${AWS_REGION} >/etc/tiproxy-machine-agent/kubeconfig
      chmod 0600 /etc/tiproxy-machine-agent/kubeconfig

      systemctl daemon-reload
      systemctl enable --now tiproxy-machine-agent
  tiproxy:
    baseImage: pingcap/tiproxy
    version: latest
    apiPort: 3080
EOF

envsubst </tmp/tiproxy-mg.yaml | kubectl apply -f -
```

如果你要验证 `/home/yangkeao/Project/github.com/YangKeao/tiproxy` 当前分支的实现，请先把该分支打包推到镜像仓库，然后把上面 `tiproxy.baseImage`/`tiproxy.version` 改成你的镜像信息。

创建 Link (先填演示地址; 你有真实 TiDB 时替换即可):

```bash
cat >/tmp/tiproxy-link.yaml <<'EOF'
apiVersion: tiproxy.pingcap.com/v1alpha1
kind: TiDBInstanceLink
metadata:
  name: ${LINK_NAME}
  namespace: ${K8S_NAMESPACE}
spec:
  machineGroupRef:
    name: ${MG_NAME}
  clusterName: demo-cluster
  pdAddresses:
  - 10.0.0.10:2379
  - 10.0.0.11:2379
  nsServerAddress: 10.0.0.20:53
EOF

envsubst </tmp/tiproxy-link.yaml | kubectl apply -f -
```

## 9. 验证链路

看 MachineGroup 状态:

```bash
kubectl get tiproxymachinegroup "${MG_NAME}" -n "${K8S_NAMESPACE}" -o yaml
kubectl get tidbinstancelink "${LINK_NAME}" -n "${K8S_NAMESPACE}" -o yaml
kubectl get tiproxymachine -n "${K8S_NAMESPACE}" -o wide
```

看 AWS 侧 ASG/LT:

```bash
export ASG_NAME=$(kubectl get tiproxymachinegroup "${MG_NAME}" -n "${K8S_NAMESPACE}" -o jsonpath='{.status.cloud.machineGroupName}')
aws autoscaling describe-auto-scaling-groups --region "${AWS_REGION}" --auto-scaling-group-names "${ASG_NAME}"
```

拿到实例 IP 并检查:

```bash
export INSTANCE_ID=$(aws autoscaling describe-auto-scaling-groups \
  --region "${AWS_REGION}" \
  --auto-scaling-group-names "${ASG_NAME}" \
  --query 'AutoScalingGroups[0].Instances[0].InstanceId' \
  --output text)

export INSTANCE_PUBLIC_IP=$(aws ec2 describe-instances \
  --region "${AWS_REGION}" \
  --instance-ids "${INSTANCE_ID}" \
  --query 'Reservations[0].Instances[0].PublicIpAddress' \
  --output text)

echo "${INSTANCE_ID} ${INSTANCE_PUBLIC_IP}"
```

通过 SSH 验证 agent/tiproxy:

```bash
ssh ec2-user@"${INSTANCE_PUBLIC_IP}" \
  'sudo systemctl status tiproxy-machine-agent --no-pager; \
   sudo journalctl -u tiproxy-machine-agent -n 100 --no-pager; \
   sudo docker ps'
```

如果你在 AL2023 上看到首次检查时 agent/container 还不存在，通常是 cloud-init 首次启动会触发一次自动重启；等待 1-2 分钟后重试即可。

验证 TiProxy API 配置是否包含 backend clusters:

```bash
curl -sS "http://${INSTANCE_PUBLIC_IP}:3080/api/admin/config/" | sed -n '1,160p'
```

可选: 验证动态配置生效（修改 Link 后观察 hash 变化）:

```bash
kubectl patch tidbinstancelink "${LINK_NAME}" -n "${K8S_NAMESPACE}" \
  --type merge \
  -p '{"spec":{"nsServerAddress":"10.0.0.21:53"}}'

kubectl get tiproxymachinegroup "${MG_NAME}" -n "${K8S_NAMESPACE}" \
  -o jsonpath='{.status.resolvedLinks[0].configHash}{"\n"}'

kubectl get tiproxymachine -n "${K8S_NAMESPACE}" \
  -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.status.configHash}{" "}{.status.lastHeartbeatTime}{"\n"}{end}'

# 用完可改回
kubectl patch tidbinstancelink "${LINK_NAME}" -n "${K8S_NAMESPACE}" \
  --type merge \
  -p '{"spec":{"nsServerAddress":"10.0.0.20:53"}}'
```

## 9.1 实测坑点（2026-03-10，us-east-1）

1. `kubeconfig` 体积问题: `update-kubeconfig --dry-run` 在多 context 环境下容易超出 SSM 参数上限，必须使用最小 kubeconfig。
2. agent 启动参数冲突: 早期版本存在 `flag redefined: kubeconfig` 崩溃，仓库当前版本已修复（复用已存在 flag）。
3. TiProxy 旧镜像入口参数不兼容: `pingcap/tiproxy:latest` 镜像内置 `-conf`，与当前 CLI `--config` 不兼容；仓库当前版本通过 `docker run --entrypoint /bin/tiproxy ... --config ...` 已规避。
4. 实例替换后的 `TiProxyMachine` 对象清理: 当前会保留已下线实例对应对象（例如 `i-xxxx`），不影响新实例运行，但会累积历史对象。若需要可手动清理。
5. 本机公网 IP 漂移: 若 SG 只放行单 IP，验证中途网络出口变化会导致 `curl/ssh` 突然超时，需重新放行新 IP。

## 10. 清理资源

先删 CR (需要 operator 仍在运行，便于触发 finalizer 删除 ASG/LT):

```bash
kubectl delete tidbinstancelink "${LINK_NAME}" -n "${K8S_NAMESPACE}" --ignore-not-found
kubectl delete tiproxymachinegroup "${MG_NAME}" -n "${K8S_NAMESPACE}" --ignore-not-found
```

再删集群与其 VPC:

```bash
eksctl delete cluster --name "${CLUSTER_NAME}" --region "${AWS_REGION}"
```

清理其余资源:

```bash
aws ssm delete-parameter --name "${KUBECONFIG_PARAM}" --region "${AWS_REGION}" || true
aws s3 rm "s3://${ARTIFACT_BUCKET}" --recursive --region "${AWS_REGION}" || true
aws s3 rb "s3://${ARTIFACT_BUCKET}" --region "${AWS_REGION}" || true

aws iam remove-role-from-instance-profile --instance-profile-name "${INSTANCE_PROFILE_NAME}" --role-name "${INSTANCE_ROLE_NAME}" || true
aws iam delete-instance-profile --instance-profile-name "${INSTANCE_PROFILE_NAME}" || true
aws iam delete-role-policy --role-name "${INSTANCE_ROLE_NAME}" --policy-name tiproxy-agent-inline || true
aws iam delete-role --role-name "${INSTANCE_ROLE_NAME}" || true

aws ec2 delete-security-group --group-id "${TIPROXY_SG_ID}" --region "${AWS_REGION}" || true
```

## 参考文档

- EKS Access Entries: https://docs.aws.amazon.com/eks/latest/userguide/creating-access-entries.html
- Associate Access Policy: https://docs.aws.amazon.com/cli/latest/reference/eks/associate-access-policy.html
- EKS authentication mode: https://docs.aws.amazon.com/eks/latest/userguide/setting-up-access-entries.html
- AL2023 SSM AMI 参数: https://docs.aws.amazon.com/linux/al2023/ug/ec2.html
