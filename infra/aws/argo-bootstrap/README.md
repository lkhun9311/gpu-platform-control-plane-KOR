# infra/aws/argo-bootstrap

Installs the Argo CD Helm chart once, in its own Terraform state. Nothing else
lives here. After install, Argo CD owns in-cluster resources via the app-of-apps
in `config/argocd`, and this state is left alone so routine cluster plan/apply
does not fight Argo CD's drift.

This root **has been applied**: its state carries one `helm_release.argocd` and the outputs
`argocd_chart_version` and `argocd_namespace`. The cluster it was installed into is gone, so what the state
describes no longer exists — `destroy.yml` skips the Helm teardown whenever the Kubernetes API is
unreachable from the runner, which is how a state outlives its cluster. Reusing this state against a new
cluster would compare an old Helm record with a new API; it has not been cleaned up, and doing so is a
separate decision from the procedure below.

## Run once

```bash
cd infra/aws/argo-bootstrap

terraform init \
  -backend-config="bucket=<state-bucket>" \
  -backend-config="kms_key_id=<state-kms-key-arn>"

terraform apply \
  -var "cluster_endpoint=<from cluster output>" \
  -var "cluster_ca=<from cluster output>"
```

### Reaching the API from outside the VPC

This root is the one that talks to the Kubernetes API rather than to AWS, through the helm and kubernetes
providers, so it needs the endpoint to answer. The cluster now defaults to a private-only endpoint
(`api_public_access_cidrs = []`), which means running this from a laptop needs one of:

- **A temporary allowance.** Apply the cluster with `-var='api_public_access_cidrs=["<your egress>/32"]'`,
  run this root, then narrow it again. The allowance covers everything behind that router and moves when the
  ISP reassigns the address.
- **A tunnel through SSM.** Nodes carry `AmazonSSMManagedInstanceCore` and no inbound port; forward the API
  through one of them with `aws ssm start-session --document-name AWS-StartPortForwardingSessionToRemoteHost`
  and point `cluster_endpoint` at the local end.

The second is the one that survives a changing home address, and it is the reason there is no bastion host.

Verify the argo-cd chart version against the argo-helm repository before applying:
`helm search repo argo/argo-cd --versions`.
