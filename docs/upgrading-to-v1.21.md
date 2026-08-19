# Upgrading an existing release to cert-manager v1.21.x

This component's documentation and test fixtures now target the cert-manager Helm chart
`v1.21.1`. If this component is already deployed with an older chart (historically the
docs pinned `v1.5.4`), an in-place `helm upgrade` (i.e. `atmos terraform apply` after
bumping `cert_manager_chart_version`) crosses many minor versions. The upstream project
[recommends upgrading one minor version at a time](https://cert-manager.io/docs/installation/upgrade/)
and reading each version's release notes. A direct jump generally works — the Deployment
label selectors are unchanged between chart v1.5.4 and v1.21.1, so Helm will not hit
immutable-field errors — but read the notes below first.

**Before any upgrade, back up cert-manager resources:**

```shell
kubectl get -o yaml certificates,certificaterequests,issuers,clusterissuers -A > cert-manager-backup.yaml
```

## Pre-flight checks (potentially blocking)

1. **CRD stored API versions.** cert-manager v1.7 removed the legacy
   `v1alpha2`/`v1alpha3`/`v1beta1` API versions from the CRDs. If the CRDs were *ever*
   installed at a version older than v1.0, `status.storedVersions` may still list a
   legacy version, and the API server will reject the CRD update during the upgrade.
   Check with:

   ```shell
   kubectl get crd certificates.cert-manager.io -o jsonpath='{.status.storedVersions}'
   ```

   If anything other than `["v1"]` appears, run
   [`cmctl upgrade migrate-api-version`](https://cert-manager.io/docs/installation/upgrade/remove-deprecated-apis/)
   before upgrading. Clusters first installed at v1.5.4 or later store only `v1` and are fine.

2. **Legacy API clients.** `cert-manager.io/v1alpha2|v1alpha3|v1beta1` are no longer
   served (since v1.6). Any manifests, GitOps pipelines, or operators still applying
   those apiVersions will break. This component's bundled `cert-manager-issuer` chart
   already uses `cert-manager.io/v1`.

3. **Kubernetes version.** The v1.21 chart requires Kubernetes >= 1.22 and is tested
   upstream against Kubernetes 1.33–1.36. Verify your EKS version before upgrading.

## Behavior changes to expect after the upgrade

- **Private key rotation defaults to `Always` (v1.18, permanent since v1.19).** Every
  `Certificate` without an explicit `spec.privateKey.rotationPolicy` now gets a new
  private key at each renewal.

  The self-signed CA Certificate installed by the bundled `cert-manager-issuer` chart
  (`my-selfsigned-ca`) now sets `rotationPolicy` explicitly. It defaults to `Always`,
  matching current cert-manager behavior: the CA private key is regenerated at each
  renewal. That is fine if everything consuming the CA reads it live from the
  `ca-key-pair` secret, but it breaks anything that pins or has a stale copy of the CA
  certificate or public key (TLSA/HPKP-style setups, baked-in trust bundles). To keep
  the pre-v1.18 behavior for the bundled CA (renewals re-sign with the same key), set:

  ```yaml
  cert_manager_issuer_values:
    selfsigned_ca_rotation_policy: Never
  ```

  Certificates you created yourself get the same v1.18+ `Always` default — set
  `spec.privateKey.rotationPolicy` explicitly on each one if consumers of those
  certificates pin the key.

- **ingress-shim default issuer is now opt-in (component change).** Historically this
  component injected malformed `ingressShim` Helm values whenever `letsencrypt_enabled`
  was `true`; charts before v1.16 silently ignored them, so no default issuer was ever
  actually configured. Rather than "fixing" the keys and silently making
  `letsencrypt-staging` (an untrusted CA) the cluster-wide default issuer, the component
  now only configures a default issuer when the new `ingress_shim_default_issuer_name`
  variable is set (e.g. `letsencrypt-prod`). The default (`null`) matches the long-standing
  de facto behavior: Ingresses must name their issuer explicitly.

- **`revisionHistoryLimit` defaults to `1` (v1.18).** Old `CertificateRequest`
  revisions are garbage-collected after the upgrade.

- **ACME HTTP01 solver ingress uses `pathType: Exact` (v1.18).** Incompatible with
  ingress-nginx 1.12.0–1.12.5 and 1.13.0–1.13.1. The issuers bundled with this
  component use DNS01/Route53 only and are unaffected; this matters only for
  HTTP01 issuers you define yourself.

- **Strict Helm values schema (v1.16+).** Unknown keys anywhere in the values (including
  the `cert_manager_values` variable) fail `helm upgrade` with a schema error. v1.21
  additionally removed `prometheus.servicemonitor.targetPort`, `prometheus.servicemonitor.path`,
  and `prometheus.podmonitor.path`. This component's default values are already
  schema-clean; audit any extra values you pass.

- **Controller metrics port renamed (v1.21).** The controller Service port
  `tcp-prometheus-servicemonitor` is now `http-metrics`. The chart-managed
  ServiceMonitor is updated automatically, but custom Prometheus scrape configs or
  NetworkPolicies referencing the old port name must be updated.

- **Token-create RBAC removed (v1.21).** The chart no longer ships a Role/RoleBinding
  granting `serviceaccounts/token create` on the controller ServiceAccount. This
  component uses IRSA (role-ARN annotation + ambient credentials) and is unaffected;
  only the undocumented `serviceAccountRef` Route53 pattern needs replacement RBAC.

- **CRDs are kept on uninstall.** This component now sets `crds.enabled: true` and
  `crds.keep: true` (replacing the deprecated `installCRDs: true`; both render
  identically on v1.21). The CRDs carry `helm.sh/resource-policy: keep`, so destroying
  the component no longer deletes the CRDs — a safety feature, since deleting the CRDs
  deletes every Certificate and its issued secrets. Remove them manually with
  `kubectl delete crd` if you truly want a full teardown. Note `crds.*` requires chart
  >= v1.15; if you pin an older chart, override with `installCRDs: true` via
  `cert_manager_values`.

- **`startupapicheck` post-upgrade hook (since v1.6).** Upgrades now run a Job
  (`cmctl check api`) that waits for the webhook to become ready. With this component's
  `atomic = true`, a failing check rolls the release back instead of leaving a broken
  install.

## References

- [cert-manager v1.21.0 release notes](https://github.com/cert-manager/cert-manager/releases/tag/v1.21.0)
- [cert-manager v1.18 release notes](https://cert-manager.io/docs/releases/release-notes/release-notes-1.18/)
- [Upgrading cert-manager](https://cert-manager.io/docs/installation/upgrade/)
- [Supported releases and Kubernetes versions](https://cert-manager.io/docs/releases/)
