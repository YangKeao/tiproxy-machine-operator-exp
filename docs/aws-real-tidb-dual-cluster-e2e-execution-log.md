# AWS 真实 TiDB 双集群联调执行记录（实际跑通记录）

> 执行日期：2026-03-10（UTC+8）
>
> 本文档记录的是**实际执行过程**（包含中间失败和修复），不是理想步骤。

## 1. 测试目标

- TiProxy 运行在既有 `TiProxy EKS/VPC`。
- 两套 TiDB 分别运行在两个新建 VPC/EKS。
- 通过 VPC Peering 互通。
- TiProxy 通过 `TiDBInstanceLink`（`pdAddresses + nsServerAddress`）发现两个集群。
- 用不同端口（`assignedPort`）把流量路由到不同 TiDB 集群。

## 2. 本次使用的环境

- AWS Account: `886436925895`
- Region: `us-east-1`
- TiProxy EKS: `tiproxye2e20260310181313`
- TiProxy VPC: `vpc-01e4da17915b8ec4e` (`192.168.0.0/16`)
- TiDB-A EKS: `tidb-a-03102034`
- TiDB-B EKS: `tidb-b-03102034`
- TiDB-A VPC: `vpc-01fc23cab98ef3c00` (`10.50.0.0/16`)
- TiDB-B VPC: `vpc-0c932f7df3596f9ee` (`10.60.0.0/16`)

## 3. 执行过程（含问题与修复）

### 3.1 新建 TiDB 两套 EKS

- 用 `eksctl create cluster` 创建：
  - `tidb-a-03102034`
  - `tidb-b-03102034`
- 两个集群均为 k8s `1.34`，4 个节点，节点标签 `dedicated=tidb`。

### 3.2 建立跨 VPC 网络

- 创建并接受 Peering：
  - `pcx-0bdbe48cb40c667fa` (TiProxy <-> TiDB-A)
  - `pcx-0858f6ff717c4fc1d` (TiProxy <-> TiDB-B)
- 打开 peering DNS resolution 双向选项。
- 在双方所有 route table 写互访路由。

### 3.3 安全组放行

- 先给 TiDB 侧 SG 放行了来自 TiProxy VPC 的：
  - `2379/tcp`, `4000/tcp`, `10080/tcp`, `30000-32767/tcp+udp`
- 之后发现真实生效的是 `ClusterSecurityGroupId`，不是最初抓到的 `SharedNodeSecurityGroup`，于是把同样规则补到 `sg-045438724677980ec`（A）和 `sg-0f6a731b35fef81c7`（B）。

### 3.4 安装 tidb-operator 与部署两套 TiDB

- 在两个 TiDB EKS 内安装 `tidb-operator`。
- `kubectl apply` CRD 首次报注解过长（`Too long`），改为 `kubectl apply --server-side --force-conflicts` 后成功。
- 部署 `tc-a`、`tc-b`（`1 PD + 3 TiKV + 1 TiDB`）。

### 3.5 存储问题修复（EBS CSI）

- 首次部署时 PD PVC 一直 `Pending`。
- 原因：集群仅有 `gp2` in-tree，无 `ebs.csi.aws.com` 驱动。
- 修复：安装 EKS addon `aws-ebs-csi-driver`，并给节点角色挂 `AmazonEBSCSIDriverPolicy`。
- 之后两套 TiDB 均 `Ready`。

### 3.6 CoreDNS 暴露与 ns server 方案

1. 初版 CoreDNS NLB 同时配 TCP+UDP，失败：
   - 错误：`mixed protocol is not supported for LoadBalancer`
2. 改成 UDP-only 后，target health 仍不健康：
   - 原因：NLB health check 默认 TCP 打 UDP NodePort
3. 修复：增加 `coredns-health`（TCP NodePort `32053`），并在 `coredns-nlb` 上指定：
   - `aws-load-balancer-healthcheck-protocol: TCP`
   - `aws-load-balancer-healthcheck-port: "32053"`
4. 另外，必须使用旧注解 `aws-load-balancer-internal: "true"` 才会建 internal NLB（本环境 service-controller 对 `scheme: internal` 不生效）。

最终可用 `nsServerAddress`（IP:53）为：

- A: `10.50.66.17:53,10.50.97.75:53`
- B: `10.60.109.184:53,10.60.89.185:53`

### 3.7 TiProxy 镜像、Link 与端口分配

- TiProxy 机器组 ASG 已切到自定义镜像并轮转实例。
- 创建 `TiDBInstanceLink`：
  - `tc-a-link` -> `pdAddresses: tc-a-pd-peer.tidb.svc.cluster.local:2379`
  - `tc-b-link` -> `pdAddresses: tc-b-pd-peer.tidb.svc.cluster.local:2379`
- `assignedPort`：
  - `tc-a-link` -> `6001`
  - `tc-b-link` -> `6002`

### 3.8 TiDB `tiproxy-port` 生效方式修正

- 发现 `spec.tidb.serverLabels` 在当前 tidb-operator 版本里未生效（字段被裁掉，pod/运行时无 label）。
- 修复：改用 `spec.tidb.config` 的 `[labels]`：
  - `tiproxy-port = "6001"` / `"6002"`
- 复查 `SHOW CONFIG ... labels.tiproxy-port` 已生效。

### 3.9 TiProxy 日志定位与 DNS workaround

- 失败日志核心：
  - `resolve host tc-a-pd-0.tc-a-pd-peer.tidb.svc failed ... no such host`
  - `resolve host tc-b-pd-0.tc-b-pd-peer.tidb.svc failed ... no such host`
- 结论：PD autosync 返回 `*.svc` 短域名，实例默认 `search` 不含 `cluster.local`，Go resolver 无法补全。
- 临时 workaround：给 TiProxy 机器加 `search cluster.local`。

## 4. 用户后续要求的调整

- 镜像改为：`yangkeao/tiproxy:support-multiple-pd-clusters-2`
- `pd-addrs` 可以不依赖（本次按用户要求不将其作为主要修复手段）
- 使用 `search domain` workaround。

## 5. 关键结论

1. 跨 VPC + headless PD + 外部 DNS NLB 路线可打通，但 CoreDNS NLB 配置细节必须正确（协议、healthcheck、SG）。
2. `tiproxy-port` 在当前 TiDB 版本应通过 `spec.tidb.config [labels]` 配置，而不是 `serverLabels`。
3. `*.svc` 短域名在 TiProxy 机器上需要 `search cluster.local`（或用 FQDN/其它 DNS 策略）。

## 6. 当前状态（本记录生成时）

- 已切换 TiProxy 镜像到：`yangkeao/tiproxy:support-multiple-pd-clusters-2`
- 已在 bootstrap userData 注入 search-domain workaround（`cluster.local`）
- 已替换 TiProxy 实例并确认新实例已拉起新镜像
- 下一步是用 **in-cluster manager**（而非本地 `go run`）再完成一轮验证

## 7. 在 TiProxy EKS 内运行真实 manager 并复测（本次补充）

### 7.1 manager 镜像构建与推送

- 构建并推送镜像到 ECR：
  - Repo: `tiproxy-machine-operator-manager-test`
  - Image: `886436925895.dkr.ecr.us-east-1.amazonaws.com/tiproxy-machine-operator-manager-test:run-20260310235542`
  - Digest: `sha256:338b7e7610c0f09996e3454c8dc46358bf4bccc7545027d861a8eba6939a3ad7`

### 7.2 在 TiProxy EKS 部署 in-cluster manager

- 新建命名空间与权限（测试环境）：
  - `Namespace`: `tiproxy-system`
  - `ServiceAccount`: `tiproxy-manager`
  - `ClusterRoleBinding`: 绑定 `cluster-admin`（仅用于本轮 e2e）
- 创建 AWS 临时凭据 Secret：
  - `Secret`: `tiproxy-system/manager-aws-creds`
- 创建 Deployment：
  - `Deployment`: `tiproxy-machine-operator-manager`
  - args:
    - `--cloud-provider=aws`
    - `--aws-region=us-east-1`
    - `--leader-elect=false`
- Rollout 结果：
  - `deployment "tiproxy-machine-operator-manager" successfully rolled out`
  - Pod `1/1 Running`

### 7.3 复测结果（真实 manager）

1. manager 运行位置确认

- 本地无 `go run ./cmd/manager` 进程。
- TiProxy EKS 中 manager Pod 正常运行，镜像为：
  - `886436925895.dkr.ecr.us-east-1.amazonaws.com/tiproxy-machine-operator-manager-test:run-20260310235542`

2. 控制面状态确认

- `TiProxyMachineGroup` 仍为 `Ready=True`，`resolved 2 link(s)`。
- `TiDBInstanceLink` 端口分配保持：
  - `tc-a-link -> 6001`
  - `tc-b-link -> 6002`
- ASG 健康：
  - `default-tiproxye2e20260310181313-mg-asg`
  - `Desired=1, InService=1, Healthy=1`
- Launch Template 默认版本在本轮 in-cluster reconcile 后推进到 `1800`（说明 in-cluster manager 已执行 AWS ensure）。

3. TiProxy 配置与流量验证

- `GET http://<TIPROXY_IP>:3080/api/admin/config/` 返回两套 backend-clusters：
  - `tidb-a` (`tc-a-pd-peer.tidb.svc.cluster.local:2379`, `ns-servers=10.50.66.17:53,10.50.97.75:53`)
  - `tidb-b` (`tc-b-pd-peer.tidb.svc.cluster.local:2379`, `ns-servers=10.60.109.184:53,10.60.89.185:53`)
- 路由 SQL 实测：
  - `:6001` 查询 `route_check.identity` 返回 `cluster-a`
  - `:6002` 查询 `route_check.identity` 返回 `cluster-b`

结论：在 **TiProxy VPC 的 EKS 内运行真实 manager** 后，双集群路由仍可正常工作，结果与此前本地 manager 调试一致。

## 8. `TiDBInstanceLink.spec.port` 功能实测（2026-03-11 补充）

### 8.1 测试前状态

- manager 升级前，曾出现重建 Link 后端口漂移：
  - `tc-a-link assignedPort=6001`
  - `tc-b-link assignedPort=6000`
- 预期行为是允许 Link 显式指定端口，且冲突时报错。

### 8.2 升级 controller 与 CRD

- 更新 `TiDBInstanceLink` CRD（增加 `spec.port` 字段）。
- 将 TiProxy EKS 的 manager 升级为新镜像：
  - `886436925895.dkr.ecr.us-east-1.amazonaws.com/tiproxy-machine-operator-manager-test:run-20260311004521`
- Deployment 状态：`1/1 Running`。

### 8.3 用例一：指定端口成功

- 设置：
  - `tc-a-link.spec.port=6001`
  - `tc-b-link.spec.port=6002`
- 收敛结果：
  - `tc-a-link assignedPort=6001 phase=Assigned`
  - `tc-b-link assignedPort=6002 phase=Assigned`
  - `TiProxyMachineGroup Ready=True`
- 连通性：
  - `6001 -> cluster-a`
  - `6002 -> cluster-b`
  - `6000` 连接超时（不可用）

### 8.4 用例二：指定端口冲突

- 设置冲突：`tc-a-link.spec.port=6002`（与 `tc-b-link` 冲突）。
- 收敛结果：
  - `TiProxyMachineGroup Ready=False`
  - `Reason=PortAllocationFailed`
  - Message: `requested port 6002 conflicts between default/tc-a-link and default/tc-b-link`
  - 两个 Link 均变为：
    - `phase=Error`
    - `assignedPort` 清空
    - `Condition PortAssigned=False/PortAllocationFailed`

### 8.5 用例三：恢复与回归

- 将配置恢复为：
  - `tc-a-link.spec.port=6001`
  - `tc-b-link.spec.port=6002`
- 收敛结果：
  - `TiProxyMachineGroup Ready=True`
  - 两个 Link 均 `Assigned` 且 `assignedPort` 与 `spec.port` 一致。
- 连通性恢复：
  - `6001 -> cluster-a`
  - `6002 -> cluster-b`

额外回归：

- 临时移除 `tc-a-link.spec.port`（不指定端口），控制器仍可自动分配并保持可用（本次为 `assignedPort=6001`），随后再恢复为显式 `6001`。

### 8.6 本轮结论

1. `TiDBInstanceLink.spec.port` 指定端口能力生效。
2. 端口冲突会被明确拒绝，并写入 `MachineGroup` 与 `Link` 状态错误。
3. 不指定 `spec.port` 时仍保留自动分配能力。
