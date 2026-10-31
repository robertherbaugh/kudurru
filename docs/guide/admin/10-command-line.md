# 10. Command line

The program `kudurru` is the server and the command line client in one
file. This chapter shows how to connect the client to a server and lists
the commands that you use most.

**Before you start:**

- You have the `kudurru` program. It is in the container image and in the
  Linux packages. You can also download it from a release.
- You have a token (Chapter 9) or the password of an account.

## Two kinds of command

| Kind | Where it runs | Examples |
|---|---|---|
| Client commands | On any computer. They use the API of a server. | `get`, `apply`, `lists`, `logs`, `query`, `settings` |
| Host commands | On the host of the node. They read the local files or the local socket. | `serve`, `doctor`, `health`, `admin` |

In Docker, run a host command with `docker exec`:

```sh
docker exec kudurru kudurru health --ready
```

## Connect to a server

A *context* holds the address of a server and a token. Use `login` to make
one.

1. Sign in with a password:

   ```sh
   kudurru login --server https://dns.example.net:8080 --username admin
   ```

   Enter the password when the command asks for it. The command makes a
   token for the command line and stores it.

2. Or use a token that you made in the dashboard:

   ```sh
   kudurru login --server https://dns.example.net:8080 --token-stdin < token.txt
   ```

3. Test the connection:

   ```sh
   kudurru nodes
   ```

The command stores the token in the keyring of your system. If there is no
keyring, it stores the token in a file that only you can read. To skip the
keyring, set `KUDURRU_KEYRING=file`.

If your local account has an authenticator, add `--totp`:

```sh
kudurru login --server https://dns.example.net:8080 --username admin --totp
```

Enter the password and current six-digit code at the hidden prompts. For piped
input, use `--password-stdin --totp-stdin` and provide the password line followed
by the code line. Leading zeros matter. The code is never saved; there is no
flag that takes its value or environment variable for it.

Login and token creation do not follow redirects or retry automatically. Use
the final server address. If a response is lost, inspect your sessions and tokens
in the dashboard before starting a fresh attempt: the server may have completed
the request even though the command could not confirm it.

NOTE: The default certificate of kudurru is self-signed. Use `--ca-file`
with the certificate to trust it. For a test only, use
`--insecure-skip-tls-verify`. Do not use this option in production.

### Use more than one server

| Task | Command |
|---|---|
| List the contexts | `kudurru config get-contexts` |
| Show the current context | `kudurru config current-context` |
| Change the context | `kudurru config use-context <name>` |
| Use a context for one command | `kudurru --context <name> nodes` |
| Delete a context | `kudurru config delete-context <name>` |

### Use a Kubernetes context directly

`--context` continues to select a saved Kudurru HTTP context by default. Add
`--transport kubernetes` to select that name from kubeconfig for generic CRD
commands:

```sh
kudurru --transport kubernetes --kubeconfig /path/to/config --context lab get zones
kudurru --transport kubernetes --context lab -n dns apply -f objects.yaml
```

Only `get`, `apply`, `delete` and `describe` use this mode. Credentials, optional
impersonation, TLS trust and the default namespace come from kubeconfig;
Kubernetes RBAC applies. Existing object ownership is enforced without force.
Do not combine API server/token overrides with this mode. The
[CLI start guide](../../../docs-site/content/start/api-cli.md#direct-kubernetes-crd-commands)
describes bounds and supported kinds.

### Output formats

Use `-o` to select the format: `table` (default), `wide`, `json`, `yaml`
or `name`. Use `-n <namespace>` to select a namespace. The default
namespace comes from the selected context, otherwise `default`.

## Common tasks

| Task | Command |
|---|---|
| Show if blocking is on | `kudurru blocking status` |
| Turn blocking off for 5 minutes | `kudurru blocking disable 5m` |
| Turn blocking on | `kudurru blocking enable` |
| Show the queries of a client | `kudurru logs --client 192.168.1.23 --limit 100` |
| Show the blocked queries | `kudurru logs --status-class blocked` |
| Follow new queries | `kudurru logs --follow` |
| Test a name | `kudurru query example.com` |
| Test a name for another client | `kudurru query ads.example.net AAAA --client 192.168.1.23` |
| Show the statistics of 24 hours | `kudurru stats summary` |
| Show the top blocked names | `kudurru stats top --dim domains --blocked` |
| Remove the cache | `kudurru cache flush` |
| Show the nodes | `kudurru nodes` |
| Show the audit log | `kudurru audit --since 24h` |
| Verify the retained audit trail | `kudurru audit verify` |
| Show the settings | `kudurru settings get` |
| Change a setting | `kudurru settings set cache.maxEntries=200000` |
| Set a setting to its default | `kudurru settings set log.level-` |

You cannot change a setting with the label `host only` with
`settings set`. Change it in `kudurru.yaml`.

### Verify the audit trail

Run `kudurru audit verify` to check the server's complete retained audit chain
and available batch seals. It requires `audit.read` and the audit license.
The command prints whether verification passed and when it was checked, plus
an explanation if an integrity check failed. The server shares scans and
caches results for 30 seconds, so `checkedAt` can precede the command.

Use `kudurru audit verify -o json` for scripts. A completed integrity check
prints a result in either format, including `verified: false` on failure.
The exit status is nonzero when integrity fails, verification is unavailable,
access is denied, or the request times out or is canceled. Request failures
print an error to standard error without a verification result. The usual
`--context`, `--server` and `--request-timeout` flags apply. This command
checks the retained trail; it cannot prove that deleted history never existed.

## Work with objects

Zones, record sets, block lists, client groups and policies are objects.
The command line uses the same YAML as Kubernetes.

| Task | Command |
|---|---|
| List objects of a kind | `kudurru get policies` |
| Show one object as YAML | `kudurru get policy kids -o yaml` |
| Show the state and the messages of an object | `kudurru describe policy kids` |
| Create or change objects | `kudurru apply -f policy.yaml` |
| Create or change all files in a directory | `kudurru apply -f objects.d/ -R` |
| Delete an object | `kudurru delete policy kids` |
| Print all objects as YAML | `kudurru export` |

The kinds and their short names are:

| Kind | Short names |
|---|---|
| `dnszones` | `zone`, `zones` |
| `dnsrecordsets` | `rrset`, `rrsets` |
| `blocklists` | `list`, `lists`, `bl` |
| `clientgroups` | `cg` |
| `policies` | `pol`, `policy` |

### Example

This file makes a client group and a policy for it:

```yaml
apiVersion: kudurru.io/v1alpha1
kind: ClientGroup
metadata:
  name: kids
  namespace: default
spec:
  clients:
    - cidr: 10.0.20.0/24
      name: Kids VLAN
---
apiVersion: kudurru.io/v1alpha1
kind: Policy
metadata:
  name: kids
  namespace: default
spec:
  clientGroupRefs: [kids]
  blockListRefs: [starter]
  priority: 10
```

Apply it:

```sh
kudurru apply -f kids.yaml
```

The dashboard then shows the objects with the owner **API**. The folder
`deploy/kustomize/samples/` has more examples.

NOTE: `kudurru apply` refuses an object that a file or Kubernetes owns. The
error says `managed_by` and names the owner.

NOTE: Without a license key, a node has one policy: the policy `default`.
kudurru then makes the group `kids` but refuses the policy `kids` with the
error `license_required`. See [Chapter 13](13-license.md#what-a-key-changes).

## Back up and restore objects

| Task | Command |
|---|---|
| Save all objects to a file | `kudurru backup --out objects.yaml` |
| Save without secrets in URLs | `kudurru backup --out objects.yaml --safe` |
| Load the objects again | `kudurru restore -f objects.yaml` |

Chapter 11 explains what a backup holds.

## Other commands

| Command | Use |
|---|---|
| `kudurru doctor` | Check the configuration, the ports and the API on the host |
| `kudurru health --ready` | Check that a node is ready (for example in a container health check) |
| `kudurru config schema` | List all settings with their scope and flags |
| `kudurru config show` | Show the effective configuration |
| `kudurru completion <shell>` | Print a script for shell completion |
| `kudurru license status` | Show the license that a node uses (state, plan, nodes, features) |
| `kudurru license verify <key>` | Check a license key without a running node |
| `kudurru version` | Show the version |

Use `kudurru <command> --help` for the options of each command. The
[command reference](../../../docs-site/content/reference/cli.md) has all of
them.
