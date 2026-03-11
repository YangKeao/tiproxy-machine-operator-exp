# Minikube dev flow

## 1. Start minikube (docker driver)

```bash
minikube start --driver=docker
```

## 2. Apply CRDs

```bash
kubectl apply -f config/crd/bases
```

## 3. Run operator locally (noop cloud provider)

```bash
go run ./cmd/manager --cloud-provider=noop
```

## 4. Apply sample CRs

```bash
kubectl apply -f config/samples/tiproxy_v1alpha1_tiproxymachinegroup.yaml
kubectl apply -f config/samples/tiproxy_v1alpha1_tidbinstancelink.yaml
```

Check resolved output:

```bash
kubectl get tiproxymachinegroup demo -o yaml
kubectl get tidbinstancelink demo-link -o yaml
```

