# kube-mutator

A mutating admission webhook written in Go that automatically injects resource limits into Kubernetes Deployments that don't define them.

When you apply a Deployment with no `resources` block, the Kubernetes API server calls this webhook before saving anything to etcd. The webhook inspects the Deployment, detects missing resource limits, injects sensible defaults, and returns the patched object. The Deployment lands in your cluster with limits already set — without the user having to write them.

---

## What Is a Mutating Admission Webhook?

Most people understand the Kubernetes flow as: `kubectl apply` → API server → etcd. What actually happens is more nuanced.

Before the API server persists any resource, it runs it through a pipeline of **admission controllers**. These controllers can inspect, reject, or mutate the incoming object. At the end of that pipeline sit two extensible hooks: `ValidatingAdmissionWebhook` and `MutatingAdmissionWebhook`.

This project implements a `MutatingAdmissionWebhook`. Here is the full flow:

```
kubectl apply -f deployment.yaml
        │
        ▼
  Kubernetes API Server
        │
        ▼
  Mutating Admission Phase
        │
        ├──▶ kube-mutator webhook (this project)
        │         │
        │         │  receives AdmissionReview (the full Deployment as JSON)
        │         │  checks each container for resource limits
        │         │  builds a JSON Patch if limits are missing
        │         │  returns AdmissionReview response with the patch
        │         │
        ◀─────────┘
        │
        ▼
  API Server applies the patch
        │
        ▼
       etcd  ←  Deployment saved with resource limits injected
```

The user never sees this happen. They apply a Deployment with no limits. They describe it and the limits are there.

---

## Prerequisites

Make sure you have the following installed before you begin:

- [Go 1.22+](https://golang.org/dl/)
- [Docker](https://docs.docker.com/get-docker/)
- [kind](https://kind.sigs.k8s.io/docs/user/quick-start/#installation)
- [kubectl](https://kubernetes.io/docs/tasks/tools/)
- `openssl` (available on most Linux/macOS systems by default)

---

## Project Structure

```
kube-mutator/
├── main.go                  # Go webhook server
├── Dockerfile               # Multi-stage build
├── kind-config.yaml         # kind cluster configuration
├── go.mod
├── go.sum
├── .gitignore
└── manifests/
    ├── rbac.yaml            # ServiceAccount, ClusterRole, ClusterRoleBinding
    ├── deployment.yaml      # Runs the webhook server as a Pod
    ├── service.yaml         # Exposes the webhook server inside the cluster
    ├── webhook.yaml         # Registers the webhook with the API server
    └── demo-deployment.yaml # Test Deployment with no resource limits
```

> `tls/` and `manifests/secret.yaml` are excluded from version control. You generate them locally using the steps below.

---

## Setup

### 1. Clone the repository

```bash
git clone https://github.com/favxlaw/kube-mutator.git
cd kube-mutator
```

### 2. Fix system file descriptor limits (Linux)

kind requires higher inotify limits than most Linux systems set by default. Without this, kube-proxy crashes and cluster networking breaks entirely.

```bash
sudo sysctl fs.inotify.max_user_watches=524288
sudo sysctl fs.inotify.max_user_instances=512
```

To make it permanent across reboots:

```bash
echo "fs.inotify.max_user_watches=524288" | sudo tee -a /etc/sysctl.conf
echo "fs.inotify.max_user_instances=512" | sudo tee -a /etc/sysctl.conf
sudo sysctl -p
```

### 3. Create the kind cluster

```bash
kind create cluster --name kube-mutator --config kind-config.yaml
```

Verify all system pods are healthy before proceeding:

```bash
kubectl get pods -n kube-system
```

Every pod should show `Running` and `1/1 READY`. If `kube-proxy` is in `CrashLoopBackOff`, the file descriptor fix in the previous step was not applied. Apply it and recreate the cluster.

### 4. Generate TLS certificates

The Kubernetes API server will only call webhook servers over HTTPS. You need a Certificate Authority and a server certificate signed by that CA. The API server receives the CA certificate so it can verify your webhook's identity.

```bash
mkdir tls
```

**Step 1 — Create your Certificate Authority:**

```bash
openssl req -x509 -newkey rsa:2048 -keyout tls/ca.key -out tls/ca.crt \
  -days 365 -nodes -subj "/CN=kube-mutator-ca"
```

**Step 2 — Create the SAN config file:**

Subject Alternative Names tell the TLS certificate which DNS names and IP addresses it is valid for. The service DNS name must match exactly what the API server uses to call your webhook.

```bash
cat <<EOF > tls/san.cnf
[req]
req_extensions = v3_req
distinguished_name = req_distinguished_name

[req_distinguished_name]

[v3_req]
basicConstraints = CA:FALSE
keyUsage = nonRepudiation, digitalSignature, keyEncipherment
extendedKeyUsage = serverAuth
subjectAltName = @alt_names

[alt_names]
DNS.1 = kube-mutator-webhook.default.svc
DNS.2 = kube-mutator-webhook.default.svc.cluster.local
IP.1 = $(kubectl get nodes -o jsonpath='{.items[0].status.addresses[0].address}')
EOF
```

**Step 3 — Generate the webhook server key and CSR:**

```bash
openssl req -newkey rsa:2048 -keyout tls/webhook.key -out tls/webhook.csr \
  -nodes -subj "/CN=kube-mutator-webhook.default.svc" \
  -config tls/san.cnf
```

**Step 4 — Sign the webhook cert with your CA:**

```bash
openssl x509 -req -in tls/webhook.csr \
  -CA tls/ca.crt -CAkey tls/ca.key -CAcreateserial \
  -out tls/webhook.crt -days 365 \
  -extensions v3_req -extfile tls/san.cnf
```

Verify the SANs are present in the cert:

```bash
openssl x509 -in tls/webhook.crt -text -noout | grep -A 4 "Subject Alternative Name"
```

You should see both DNS names and the node IP address.

### 5. Create the TLS secret

```bash
kubectl create secret tls kube-mutator-tls \
  --cert=tls/webhook.crt \
  --key=tls/webhook.key \
  --dry-run=client -o yaml > manifests/secret.yaml

kubectl apply -f manifests/secret.yaml
```

### 6. Update the caBundle in webhook.yaml

The `caBundle` field tells the API server which CA to trust when verifying your webhook's certificate. Replace the empty value in `manifests/webhook.yaml`:

```bash
cat tls/ca.crt | base64 | tr -d '\n'
```

Copy the output and paste it as the value of `caBundle` in `manifests/webhook.yaml`.

### 7. Build and load the Docker image

```bash
docker build -t kube-mutator:latest .
kind load docker-image kube-mutator:latest --name kube-mutator
```

`kind load` pushes the image directly into the cluster's internal registry. This is required because `imagePullPolicy: Never` is set in the Deployment — the cluster will not attempt to pull from DockerHub.

---

## Deploying the Webhook

Apply the manifests in order. The webhook registration must come last, after the server is running and ready to receive requests.

```bash
kubectl apply -f manifests/rbac.yaml
kubectl apply -f manifests/deployment.yaml
kubectl apply -f manifests/service.yaml
```

Wait for the webhook pod to be running:

```bash
kubectl get pods
```

Once you see `1/1 Running`, register the webhook with the API server:

```bash
kubectl apply -f manifests/webhook.yaml
```

From this point on, every Deployment creation in the `default` namespace goes through your webhook before it reaches etcd.

---

## Testing It

Apply the demo Deployment, which intentionally defines no resource limits:

```bash
kubectl apply -f manifests/demo-deployment.yaml
```

Describe the Deployment and check the container spec:

```bash
kubectl describe deployment demo-deployment
```

Under the container you will see:

```
Limits:
  cpu:     200m
  memory:  256Mi
Requests:
  cpu:         100m
  memory:      128Mi
```

You never wrote those. The webhook injected them.

Check the webhook logs to see exactly what happened:

```bash
kubectl logs deployment/kube-mutator
```

Output:

```
2026/04/14 18:11:27 Starting kube-mutator webhook server on port 8443...
2026/04/14 18:12:48 Received admission review for: default/demo-deployment
2026/04/14 18:12:48 Container nginx has no resource limits, injecting...
```

---

## How the Code Works

The webhook server is a standard Go HTTP server with two routes:

- `/` — health check
- `/mutate` — admission webhook endpoint

When the API server sends a `POST` request to `/mutate`, the handler does the following:

**1. Reads and deserializes the AdmissionReview**

The API server sends a JSON payload called an `AdmissionReview`. It contains the full Deployment object exactly as the user wrote it, along with metadata about the operation.

**2. Unmarshals the Deployment from the raw object**

Inside the `AdmissionReview` is `request.object.raw` — the raw JSON of the Deployment. This is unmarshaled into a `appsv1.Deployment` struct.

**3. Checks each container for resource limits**

The webhook loops through every container in `spec.template.spec.containers`. If `container.Resources.Limits` is `nil`, limits are missing.

**4. Builds a JSON Patch**

A JSON Patch is a list of operations that tell the API server what to change in the object. Each operation has:

- `op` — the operation (`add`, `replace`, `remove`)
- `path` — where in the object to apply it, written as a JSON pointer (e.g. `/spec/template/spec/containers/0/resources`)
- `value` — what to set

**5. Returns the AdmissionReview response**

The response wraps the patch and sets `allowed: true`. The API server applies the patch to the object and saves the mutated result to etcd.

One important detail: the response must echo back the same `UID` from the request. This is how the API server matches your response to the original request. It must also set `TypeMeta` with `APIVersion` and `Kind` on the response — without this, newer versions of Kubernetes silently ignore the response.

---

## Understanding failurePolicy

The `failurePolicy` field in `webhook.yaml` controls what happens if your webhook is unreachable or times out.

**`failurePolicy: Fail`** — the API server rejects the Deployment if the webhook cannot be reached. Nothing gets created. This is safe for enforcement but dangerous if your webhook goes down.

**`failurePolicy: Ignore`** — the API server allows the Deployment through unchecked if the webhook cannot be reached. Limits will not be injected but the cluster keeps working.

You can see this in action by scaling your webhook to zero replicas and trying to apply a Deployment:

```bash
kubectl scale deployment kube-mutator --replicas=0
kubectl apply -f manifests/demo-deployment.yaml
```

With `failurePolicy: Fail` the deployment is rejected. Change it to `Ignore`, reapply `webhook.yaml`, and try again — the deployment goes through but without injected limits.

This is a real production decision. Security-critical webhooks use `Fail`. Non-critical webhooks use `Ignore`.

Scale back up when done:

```bash
kubectl scale deployment kube-mutator --replicas=1
```

---

## Cleanup

```bash
kind delete cluster --name kube-mutator
```

---
