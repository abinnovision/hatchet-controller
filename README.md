# hatchet-controller

App teams declare a Hatchet tenant in their own namespace and get back a Secret their workers can use right away. The controller creates the tenant in an existing [Hatchet](https://github.com/hatchet-dev/hatchet) installation, mints an API token for it, and keeps that token rotated. It does not install or operate Hatchet itself.

## Why

- **Tenants as code.** A team commits one small `HatchetTenant` next to its workers. No dashboard clicks, no tokens copied between systems.
- **Topology, not lifecycle.** The controller connects namespaces to a Hatchet you already run. Installing and upgrading Hatchet stays with its Helm chart.
- **Split ownership, like Gateway API.** The platform team owns the `HatchetInstance` and decides which namespaces may attach. App teams own their `HatchetTenant`.
- **No long-lived admin credential.** The controller generates its own signing key and Hatchet trusts the public half. Every admin call uses an exchange token that expires after 5 minutes.
- **Plain Kubernetes.** CEL validation instead of webhooks, owner references instead of cleanup jobs, and the usual `Accepted` and `Ready` conditions.

## How it works

<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="docs/assets/how-it-works-dark.svg">
    <img src="docs/assets/how-it-works-light.svg" alt="How hatchet-controller works">
  </picture>
</p>

There are two namespaced resources in `hatchet-controller.abigroup.io/v1alpha1`:

- **`HatchetInstance`** lives in the Hatchet namespace. It points at the Hatchet API, names the Hatchet user the controller acts as, and lists which namespaces may use it (`allowedNamespaces`, default `Same`). The controller generates a signing keyset for it and publishes the public half as a Secret, which the Hatchet API reads at startup.
- **`HatchetTenant`** lives in an app namespace and references an instance with `instanceRef`. The controller checks that the instance allows the namespace, creates the Hatchet tenant (or binds an existing one), mints an API token and writes the worker Secret.

The worker Secret is named `<name>-hatchet` unless `secretName` is set, and contains `HATCHET_CLIENT_TOKEN`, `HATCHET_CLIENT_TENANT_ID`, `HATCHET_CLIENT_HOST_PORT`, `HATCHET_CLIENT_SERVER_URL`, `HATCHET_CLIENT_TLS_STRATEGY` and, if configured, `HATCHET_CLIENT_TLS_ROOT_CA`.

## Compatibility

Requires self-hosted Hatchet v0.84.0 or newer. Hatchet Cloud is not supported.

The controller authenticates through Hatchet's control-plane exchange tokens. They are an internal Hatchet feature with no public documentation and no stability promise, so any Hatchet release can change or remove them without notice.

What we test on every change:

- Integration tests against a real Hatchet at v0.84.0, v0.98.12 and the newest version we track (currently v0.112.10, kept current by Dependabot).
- An end-to-end test that deploys Hatchet and the controller into a kind cluster and checks that a tenant becomes Ready and gets its worker Secret.

Versions outside that list may work but are not tested. Pin your Hatchet version and try upgrades outside production first.

## Getting started

With GitOps you can apply all of the steps below in one sync. The Hatchet API waits for the keyset Secret before it starts.

### 1. Install the controller

```yaml
resources:
  - github.com/abinnovision/hatchet-controller//config/default?ref=v0.1.0
```

Images are published for `linux/amd64` and `linux/arm64`. The base installs into `hatchet-controller-system` but does not create that namespace. Create it yourself (Argo CD `CreateNamespace=true`, Flux, or a Namespace resource) and label it `pod-security.kubernetes.io/enforce: restricted`; the manager meets that profile. To run the controller next to Hatchet instead, set `namespace: hatchet` in your kustomization.

### 2. Create a service user in Hatchet

The controller acts as a regular Hatchet user. Seed one with `ADMIN_EMAIL=hatchet-controller@<domain>` and a random `ADMIN_PASSWORD` you throw away. Afterwards remove `ADMIN_PASSWORD` and keep `ADMIN_EMAIL`. Look up the user's UUID:

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

Add these variables to the Hatchet API container. The container name depends on your install; `config/samples/hatchet-api-patch.yaml` has a full patch.

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

Load the resulting `my-app-hatchet` Secret into your workers with `envFrom`.

## Tokens

Tokens rotate at two thirds of `tokenTTL` (default 8760h, allowed range 24h to 8760h). Running workers keep their old token until they restart, so with the default they have about four months after a rotation to pick up the new one. If you lower `tokenTTL`, use a restarter such as Reloader.

To replace a leaked token, delete the worker Secret. The controller mints a new token and revokes the old one.

## Incoming webhooks

`spec.webhooks` manages Hatchet incoming webhooks for the tenant, for sources such as GitHub, Stripe or Slack. Credentials come from Secrets in the tenant's namespace.

```yaml
apiVersion: hatchet-controller.abigroup.io/v1alpha1
kind: HatchetTenant
metadata: {name: my-app, namespace: my-app}
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

The receiver URL is `<public Hatchet URL>/api/v1/stable/tenants/<tenantID>/webhooks/<name>`. The tenant ID is in `status.tenantID`. `auth` takes exactly one of `basic`, `apiKey` or `hmac`. `source` defaults to `GENERIC`.

- **Failures** show as `Ready=False/WebhookFailed`. A missing Secret or key shows `InvalidSpec`. Neither blocks the worker Secret.
- **Expression and payload changes** (`eventKeyExpression`, `scopeExpression`, `staticPayload`) update the webhook in place.
- **Source and credential changes** delete and re-create the webhook, because Hatchet cannot patch them. Requests get a 404 for a moment. A `WebhookRecreated` event is emitted.
- **A failed re-create** leaves the webhook absent until the cause is fixed; `Ready=False` shows why.
- **Pruning** only covers webhooks tracked in `status.webhooks`. Restoring a HatchetTenant without its status while removing an entry from `spec.webhooks` leaves that webhook in Hatchet.
- **Rotating a credential Secret** (a changed value, not other Secret edits) is picked up within 5 minutes, because the controller does not watch these Secrets.
- **Existing webhooks** with the same name that the controller did not create are replaced.
- **Removing an entry** deletes the webhook in Hatchet. Deleting the `HatchetTenant` deletes all of its webhooks.

## Members

`spec.members` invites people to the tenant and manages the role of the people it invited.

```yaml
apiVersion: hatchet-controller.abigroup.io/v1alpha1
kind: HatchetTenant
metadata: {name: my-app, namespace: my-app}
spec:
  instanceRef: {name: hatchet, namespace: hatchet}
  members:
    - email: alice@example.com
      role: ADMIN
    - email: bob@example.com
```

Hatchet has no API to add a member directly, so the controller sends an invite. The person accepts it in the Hatchet UI after signing in or signing up with that email. `status.members` shows `Invited` with `inviteExpiresAt`, and `Member` once the invite was accepted. The controller notices the acceptance within 5 minutes and emits a `MemberInvited` event for each invite it sends.

- **Emails** must be lowercase, because Hatchet compares invite emails exactly.
- **Roles** are `OWNER`, `ADMIN` and `MEMBER` (the default). `VIEWER` needs Hatchet v0.112 or newer.
- **Role changes** update the pending invite or the member.
- **Expired invites** are sent again on a later reconcile.
- **Removal** only covers people tracked in `status.members`. People added in the Hatchet UI and the service user are never touched. Listing an email that is already a member or has a pending invite adopts it, and the spec then sets their role and removes them when the entry goes. Do not list the service user's own email.
- **Removing an entry** deletes the pending invite or removes the member from the tenant. Deleting the `HatchetTenant` does this for all tracked people.
- **Failures** show as `MemberFailed`, or `Unauthorized` when Hatchet denies the call (e.g. pending invite limit, or the service user is not an owner of an inherited tenant). A failure does not block the worker Secret or the webhooks.
- **Inherited tenants** need the service user to be an `ADMIN` there, and an `OWNER` to grant `OWNER` or remove members.

## Existing tenants and the tenant ID annotation

Once a tenant is created, the controller writes its ID to the annotation `hatchet-controller.abigroup.io/tenant-id`. If you restore the `HatchetTenant` from a backup without status, the annotation keeps it bound to the same tenant instead of creating a second one. The ID is also in `status.tenantID` and in the `TenantCreated` event.

After binding, the annotation is fixed. A different value gives `Ready=False/TenantIDConflict` and the controller does nothing, and a removed annotation is restored from status.

To take over a tenant that already exists, set the annotation before the first apply:

```yaml
apiVersion: hatchet-controller.abigroup.io/v1alpha1
kind: HatchetTenant
metadata:
  name: orders
  namespace: team-orders
  annotations:
    hatchet-controller.abigroup.io/tenant-id: 5f0e2c1a-7b3d-4e8f-9a6b-1c2d3e4f5a6b
spec:
  instanceRef: {name: hatchet, namespace: hatchet}
```

The controller accepts it only if the value is a valid UUID and the service user is a member of that tenant. Tenants created by the controller have the slug `<namespace>.<name>-<uid8>` and can only be taken over from the same namespace. Tenants created by hand in Hatchet can be taken over from any namespace the instance allows, once a Hatchet admin adds the service user as a member; their ID is shown in the Hatchet UI. Anything else reports `Ready=False/InheritRejected`.

## Optional components

```yaml
components:
  - github.com/abinnovision/hatchet-controller//config/components/networkpolicy?ref=v0.1.0
  - github.com/abinnovision/hatchet-controller//config/components/tenant-id-guard?ref=v0.1.0
```

- **`networkpolicy`** gives the controller NetworkPolicy permissions and turns on `--enable-network-policy`. Instances that set `spec.networkPolicy` then get an egress policy for worker pods labeled `hatchet-controller.abigroup.io/worker: "true"`. That policy isolates those pods, so all other egress is blocked unless another policy allows it. Without the component, such tenants report `Ready=False/Forbidden`.
- **`tenant-id-guard`** is a ValidatingAdmissionPolicy that blocks changing or removing the tenant ID annotation once `status.tenantID` is set. Setting it for the first time, or changing it while the tenant is unbound, is allowed. The `/status` subresource is not matched. Requires Kubernetes 1.30 or newer.

## RBAC

| ClusterRole | Aggregated to | Grants |
| --- | --- | --- |
| `hatchet-controller-hatchettenant-editor-role` | admin, edit | manage `hatchettenants` |
| `hatchet-controller-hatchet-viewer-role` | view | read both CRDs |
| `hatchet-controller-hatchetinstance-admin-role` | none | manage `hatchetinstances`; bind it per Hatchet namespace |

Managing a `HatchetInstance` is effectively Hatchet root access, so that role is not aggregated. Wildcard namespace roles, Argo CD AppProjects or Capsule can still grant it, so check those.

## Security

- The controller can read and write Secrets cluster-wide, because it writes worker Secrets into tenant namespaces. It never deletes Secrets. A compromised controller means Hatchet admin access and more, which is why the image is distroless and runs as non-root with a read-only root filesystem.
- The private signing key lives in the Hatchet namespace and can impersonate any Hatchet user. Restrict Secret access there and enable etcd encryption.
- Any allowed namespace can take over a manually created tenant the service user is a member of. Keep `allowedNamespaces` tight.
- Cap the number of tenants with a ResourceQuota on `count/hatchettenants.hatchet-controller.abigroup.io`.
- Metrics are off by default. If you enable them with `--metrics-bind-address`, protect the endpoint.
- The DNS rule in the generated NetworkPolicy allows port 53 to any destination.
- The controller does not follow redirects from `spec.apiURL`.

## Deletion and recovery

- **Deleting a `HatchetTenant`** revokes its tokens, deletes its webhooks and removes the worker Secret. The Hatchet tenant stays, because Hatchet has no API to delete tenants.
- **Deleting a `HatchetInstance`** removes the signing key and keyset. On its next restart the Hatchet API waits for the missing Secret until you disable exchange tokens. A recreated instance gets a new key, which needs an API restart. Meanwhile tenants show `Accepted=False/InstanceNotFound`, keep their Secrets until the tokens expire, and drop their finalizers without revoking. Protect GitOps-managed instances with `argocd.argoproj.io/sync-options: Delete=false` or `kustomize.toolkit.fluxcd.io/prune: disabled`.
- **Removing a namespace from `allowedNamespaces`** sets `Accepted=False`, but the existing Secret and token stay.
- **Removing `spec.networkPolicy`** leaves existing policies in place until the tenant is deleted.
- **After a restore**, tokens issued before it expire on their own.
- **If revocation keeps failing**, the tenant shows `RevocationFailed` after 10 minutes.
- **If tenant creation fails halfway**, the tenant can exist without the service user as a member. The `HatchetTenant` then reports `SlugTaken`; recreate it.
- **If you uninstall the controller** while resources remain, clear their finalizers with `kubectl patch <resource> --type merge -p '{"metadata":{"finalizers":null}}'`.

## Releases

Every merge to `main` with unreleased changes publishes a beta image. A stable release happens when the release PR is merged.

| | Beta | Stable |
| --- | --- | --- |
| Trigger | merge to `main` | merge of the release PR |
| Version | `<next>-beta.<N>`, N counts commits since the last stable release | `<version>` |
| Image tags | `<next>-beta.<N>`, `prerelease-<next>-beta.<N>`, `sha-<short>` | `<version>`, `release-<version>`, `sha-<short>`, `latest` |
| Git tag and GitHub release | none | `v<version>` |
| Image tag in `config/manager` | unchanged | bumped to `<version>` |

Betas have no git tag. To use one, pin the base to the commit the image was built from and use the matching `sha-<short>` image tag (first 7 characters of the commit). The CRDs come from the base, so both must point at the same commit:

```yaml
resources:
  - github.com/abinnovision/hatchet-controller//config/default?ref=<short>
images:
  - name: ghcr.io/abinnovision/hatchet-controller
    newTag: sha-<short>
```

Betas pass the full test suite but may change the `v1alpha1` API without notice. Use them for staging, not production.

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
