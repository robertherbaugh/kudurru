# Enable the Kubernetes DHCP Service

**Before you start**

- You need cluster administration access and a configured LoadBalancer provider.
- You need the [planned DHCP address](plan.md).
- You need the complete values file for your existing Helm release.

## Update the deployment

Keep client DHCP traffic away from kudurru until the final cutover.
Do not enable the router relay yet.

1. Upgrade the `kudurru-crds` chart to the same version as kudurru.
2. Add this section to your existing values file:

   ```yaml
   dhcp:
     enabled: true
     serverAddress: 10.200.200.67
     service:
       type: LoadBalancer
       externalTrafficPolicy: Local
       annotations:
         metallb.io/loadBalancerIPs: 10.200.200.67
   ```

   The annotation is for MetalLB. Use your provider's address allocation method
   if you use another provider. Keep `externalTrafficPolicy: Local` to preserve relay source addresses.
   Do not enable `hostNetwork` for this mode.

3. Apply the complete values through your existing Helm or GitOps workflow.

   For a Helm-managed release named `kudurru`, the commands are:

   ```sh
   helm upgrade --install kudurru-crds oci://ghcr.io/robertherbaugh/charts/kudurru-crds --version 0.1.0
   helm upgrade kudurru oci://ghcr.io/robertherbaugh/charts/kudurru --version 0.1.0 \
     --namespace kudurru-system --values kudurru-values.yaml
   ```

   Use your release names and namespace. `kudurru-values.yaml` must include
   your current settings, including replica count and any Secret references.
   The installer needs permission to create the chart's admission policy and RBAC.

4. Read the DHCP Service:

   ```sh
   kubectl -n kudurru-system get service kudurru-dhcp
   ```

## Check the result

The Service shows your planned **EXTERNAL-IP** and `67:…/UDP` under **PORT(S)**.
If the address stays `<pending>`, fix the LoadBalancer allocation before you continue.
The elected serving replica becomes a Service endpoint after a scope is ready.
Continue to [Configure the router relay](relay.md).
