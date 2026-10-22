# hatchet-controller

A Kubernetes controller that lets app teams declare a [Hatchet](https://github.com/hatchet-dev/hatchet) tenant in their own namespace and get back a Secret their workers can use. It creates the tenant in an existing Hatchet installation, mints an API token, and keeps that token rotated. It does not install or operate Hatchet itself.

## Why

- Tenants live in Git next to the workers that use them. No dashboard clicks and no copied tokens.
- The platform team owns the `HatchetInstance` and decides which namespaces may attach. App teams own their `HatchetTenant`.
- There is no long-lived admin credential. The controller signs its own exchange tokens, which expire after 5 minutes.

## How it works

<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/how-it-works-dark.svg">
    <img src="docs/assets/how-it-works-light.svg" alt="How hatchet-controller works">
  </picture>
</p>

The API group `hatchet-controller.abigroup.io/v1alpha1` has two namespaced resources:

- `HatchetInstance` lives in the Hatchet namespace. It points at the Hatchet API, names the Hatchet user the controller acts as, and sets which namespaces may use it (`allowedNamespaces`, default `Same`). The controller publishes a public signing keyset for it as a Secret, which the Hatchet API reads at startup.
- `HatchetTenant` lives in an app namespace and references an instance with `instanceRef`. The controller creates the Hatchet tenant, mints a token and writes the worker Secret.

The worker Secret is named `<name>-hatchet` (override with `secretName`) and holds the `HATCHET_CLIENT_*` variables a Hatchet SDK needs: token, tenant ID, host and port, server URL, and TLS settings.

## Compatibility

Requires self-hosted Hatchet v0.84.0 or newer. Hatchet Cloud is not supported.

The controller relies on Hatchet's control-plane exchange tokens, an undocumented internal feature that any Hatchet release may change. Each change is tested against Hatchet v0.84.0, v0.98.12 and the latest tracked release. Pin your Hatchet version and test upgrades outside production first.

## Getting started

All steps can be applied in a single GitOps sync. The Hatchet API waits for the keyset Secret before it starts.

### 1. Install the controller

```yaml
resources:
  - github.com/abinnovision/hatchet-controller//config/default?ref=v0.2.0
```

The base installs into `hatchet-controller-system` but does not create the namespace. Create it yourself and label it `pod-security.kubernetes.io/enforce: restricted`. To run the controller next to Hatchet instead, set `namespace: hatchet` in your kustomization.

### 2. Create a service user in Hatchet

The controller acts as a regular Hatchet user. Seed one by starting Hatchet with `ADMIN_EMAIL=hatchet-controller@<domain>` and a random `ADMIN_PASSWORD`, then remove `ADMIN_PASSWORD` and keep `ADMIN_EMAIL`. Look up the user's ID:

```sh
psql "$DATABASE_URL" -tAc "SELECT id FROM \"User\" WHERE email='<email>' AND \"deletedAt\" IS NULL"
```

### 3. Create the instance

```yaml
apiVersion: hatchet-controller.abigroup.io/v1alpha1
kind: HatchetInstance
metadata: {name: hatchet, namespace: hatchet}
spec:
  apiURL: http://hatchet-api.hatchet.svc:8080
  serviceUserID: <user uuid>
  worker:
    hostPort: hatchet-engine.hatchet.svc:7070
    tlsStrategy: none
  allowedNamespaces: {from: Selector, selector: {matchLabels: {hatchet-access: "true"}}}
```

The controller creates the Secret `<instance>-exchange-keyset` in the same namespace.

### 4. Let Hatchet trust the controller

Add these variables to the Hatchet API container. See `config/samples/hatchet-api-patch.yaml` for a full patch.

```yaml
env:
  - {name: SERVER_AUTH_CONTROL_PLANE_EXCHANGE_TOKEN_ENABLED, value: "true"}
  - name: SERVER_AUTH_CONTROL_PLANE_EXCHANGE_TOKEN_JWT_PUBLIC_KEYSET
    valueFrom:
      secretKeyRef: {name: <instance>-exchange-keyset, key: keyset}
  - {name: SERVER_AUTH_CONTROL_PLANE_EXCHANGE_TOKEN_ISSUER, value: hatchet-controller}
  - {name: SERVER_AUTH_CONTROL_PLANE_EXCHANGE_TOKEN_AUDIENCE, value: hatchet}
```

### 5. Create a tenant

```yaml
apiVersion: hatchet-controller.abigroup.io/v1alpha1
kind: HatchetTenant
metadata: {name: my-app, namespace: my-app}
spec:
  instanceRef: {name: hatchet, namespace: hatchet}
```

Load the resulting `my-app-hatchet` Secret into your workers with `envFrom`. The tenant name in Hatchet follows `spec.displayName`, or `<namespace>/<name>` if unset.

## Tokens

Tokens rotate at two thirds of `tokenTTL` (default 8760h, range 24h to 8760h). Running workers keep the old token until they restart, so with a short TTL use a restarter such as Reloader. To replace a leaked token, delete the worker Secret. The controller mints a new one and revokes the old one.

## Webhooks

`spec.webhooks` manages Hatchet incoming webhooks for the tenant. Credentials come from Secrets in the tenant's namespace.

```yaml
spec:
  instanceRef: {name: hatchet, namespace: hatchet}
  webhooks:
    - name: github
      source: GITHUB
      eventKeyExpression: "'github:' + headers['x-github-event']"
      auth:
        hmac:
          algorithm: SHA256
          encoding: HEX
          signatureHeaderName: X-Hub-Signature-256
          signingSecretRef: {name: github-webhook, key: secret}
```

Webhooks are served at `<public Hatchet URL>/api/v1/stable/tenants/<tenantID>/webhooks/<name>`, with the tenant ID taken from `status.tenantID`. `auth` takes exactly one of `basic`, `apiKey` or `hmac`, and `source` defaults to `GENERIC`. These `hmac` settings match the presets in the Hatchet UI:

| `source` | `algorithm` | `encoding` | `signatureHeaderName` | `signingSecretRef` holds |
|---|---|---|---|---|
| `GITHUB` | `SHA256` | `HEX` | `X-Hub-Signature-256` | the webhook secret |
| `LINEAR` | `SHA256` | `HEX` | `linear-signature` | the webhook signing secret |
| `STRIPE` | `SHA256` | `HEX` | `Stripe-Signature` | the endpoint signing secret (`whsec_...`) |
| `SLACK` | `SHA256` | `HEX` | `X-Slack-Signature` | the app signing secret |
| `SVIX` | omit | omit | omit | the endpoint secret (`whsec_...`) |

`STRIPE`, `SLACK` and `SVIX` require `hmac`. Changing the source or credentials re-creates the webhook, so requests may briefly get a 404. Removing an entry deletes the webhook in Hatchet. Failures show as `Ready=False/WebhookFailed`, or `InvalidSpec` for a missing Secret or key, and do not block the worker Secret.

## Members

`spec.members` invites people to the tenant and manages their roles.

```yaml
spec:
  instanceRef: {name: hatchet, namespace: hatchet}
  members:
    - email: alice@example.com
      role: ADMIN
    - email: bob@example.com
```

Hatchet has no API to add members directly, so the controller sends invites that people accept in the Hatchet UI. `status.members` shows each person as `Invited` or `Member`. Emails must be lowercase. Roles are `OWNER`, `ADMIN` and `MEMBER` (default), plus `VIEWER` on Hatchet v0.112 or newer. The controller only manages people listed in the spec and never touches anyone added through the Hatchet UI.

## Adopting an existing tenant

The controller records the tenant ID in the annotation `hatchet-controller.abigroup.io/tenant-id`, so a `HatchetTenant` restored from backup binds to the same tenant. To take over a tenant that already exists, set the annotation before the first apply:

```yaml
metadata:
  name: orders
  namespace: team-orders
  annotations:
    hatchet-controller.abigroup.io/tenant-id: 5f0e2c1a-7b3d-4e8f-9a6b-1c2d3e4f5a6b
```

The service user must be a member of that tenant. For tenants created by hand, a Hatchet admin has to add it first. Tenants created by the controller can only be adopted from their original namespace. If adoption is not allowed, the tenant reports `Ready=False/InheritRejected`.

## Optional components

```yaml
components:
  - github.com/abinnovision/hatchet-controller//config/components/networkpolicy?ref=v0.2.0
  - github.com/abinnovision/hatchet-controller//config/components/tenant-id-guard?ref=v0.2.0
```

- `networkpolicy` lets instances that set `spec.networkPolicy` generate an egress policy for worker pods labeled `hatchet-controller.abigroup.io/worker: "true"`. The policy isolates those pods, so other egress is blocked unless another policy allows it.
- `tenant-id-guard` is a ValidatingAdmissionPolicy that blocks changing or removing the tenant ID annotation once a tenant is bound. Requires Kubernetes 1.30 or newer.

## RBAC

| ClusterRole | Aggregated to | Grants |
| --- | --- | --- |
| `hatchet-controller-hatchettenant-editor-role` | admin, edit | manage `hatchettenants` |
| `hatchet-controller-hatchet-viewer-role` | view | read both CRDs |
| `hatchet-controller-hatchetinstance-admin-role` | none | manage `hatchetinstances`; bind it per Hatchet namespace |

Managing a `HatchetInstance` amounts to Hatchet root access, so that role is not aggregated. Check that wildcard roles or tools like Argo CD AppProjects don't grant it anyway.

## Security

- The controller can read and write Secrets cluster-wide to deliver worker Secrets. It never deletes Secrets, and the image is distroless, non-root and uses a read-only root filesystem.
- The private signing key in the Hatchet namespace can impersonate any Hatchet user. Restrict Secret access there and enable etcd encryption.
- Any allowed namespace can adopt a manually created tenant the service user belongs to, so keep `allowedNamespaces` tight.
- Metrics are off by default. If you enable them with `--metrics-bind-address`, protect the endpoint.

## Deletion

- Deleting a `HatchetTenant` revokes its tokens, deletes its webhooks and removes the worker Secret. The tenant itself stays in Hatchet, which has no delete API.
- Deleting a `HatchetInstance` removes its signing key, and the Hatchet API will not start again until exchange tokens are disabled, or a new instance is created and the API restarted. Protect GitOps-managed instances with `argocd.argoproj.io/sync-options: Delete=false` or `kustomize.toolkit.fluxcd.io/prune: disabled`.
- If you uninstall the controller while resources remain, clear their finalizers with `kubectl patch <resource> --type merge -p '{"metadata":{"finalizers":null}}'`.

## Releases

Merging the release PR publishes a stable release, tagged `v<version>` in Git and `<version>` and `latest` on the image. Every other merge to `main` publishes a beta image tagged `sha-<short>` (first 7 characters of the commit). To try a beta, pin both the base and the image to the same commit:

```yaml
resources:
  - github.com/abinnovision/hatchet-controller//config/default?ref=<short>
images:
  - name: ghcr.io/abinnovision/hatchet-controller
    newTag: sha-<short>
```

Betas pass the full test suite but may change the `v1alpha1` API without notice, so keep them out of production.

## Development

```sh
make build            # generate, manifests, compile
make check            # lint, govulncheck, gofmt, generated files up to date
make test-unit
make test-integration # HATCHET_VERSIONS="v0.84.0 v0.98.12 v0.112.10"
make test-e2e         # needs Docker; uses kind
make docker-build
```

## License

Apache License 2.0, see [LICENSE](LICENSE).
