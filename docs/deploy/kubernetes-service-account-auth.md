# Kubernetes ServiceAccount tokens for the API

With the Kubernetes store, a Job or controller can call kudurru with a projected
ServiceAccount JWT for audience `kudurru`. The server verifies it with the
cluster's JWKS, or uses TokenReview while issuer discovery is unavailable.
Existing SubjectAccessReview and kudurru role bindings still govern permissions;
a valid token alone grants no role. The server's Kubernetes identity needs the
existing issuer-discovery and `system:auth-delegator` permissions.

Incoming tokens must have an original lifetime of **at most ten minutes**. This
intentionally rejects opaque legacy tokens, ordinary one-hour projections and
cluster-extended lifetimes. Request a dedicated token with exactly
`expirationSeconds: 600` and `audience: kudurru`; using the pod's default API-server
token is insufficient. The following Pod-template fragment adds a dedicated
projection to an existing automation container:

```yaml
spec:
  serviceAccountName: dns-automation
  automountServiceAccountToken: false
  containers:
    - name: automation
      # Keep the existing image and command.
      volumeMounts:
        - name: kudurru-api-token
          mountPath: /var/run/kudurru-credentials
          readOnly: true
  volumes:
    - name: kudurru-api-token
      projected:
        sources:
          - serviceAccountToken:
              path: token
              audience: kudurru
              expirationSeconds: 600
```

The client must reread `/var/run/kudurru-credentials/token` when making requests
so it uses Kubernetes' rotated token. Mount the directory directly; a `subPath`
mount or a one-time copy does not follow rotation. Keep any separate credentials
needed by that client for the Kubernetes API. Do not log or persist token values.
If the issuer returns a token longer than 600 seconds despite the request,
kudurru rejects it; change the projection/issuer policy rather than relying on
an expiry clamp.

Offline verification provides **bounded revocation, not immediate deletion
detection**. Deleting the bound pod or ServiceAccount stops renewal, but an
already issued offline token can remain usable until its original expiry, at
most ten minutes after issuance. TokenReview observes authoritative deletion on
its next review; successful review results last at most two minutes and never
past the token's original expiry. Deletion and expiry do not replay a refused
operation. A client receiving a refusal must reconcile the operation's outcome
before choosing whether to issue a new operation with a new credential.

During an API outage, already known JWKS keys can verify a valid token only
within its original lifetime. Missing verification keys or a failed TokenReview
fail closed. Invalid/future `iat`, malformed dates, future `nbf` and expiry at the
current instant are rejected on both paths. Cache refresh and held requests
cannot acquire a later expiry. These restrictions apply to tokens presented to
kudurru, not the server's own in-cluster Kubernetes client credentials.
