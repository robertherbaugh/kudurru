# 11. Maintenance

This chapter shows how to back up and restore kudurru, how to upgrade it,
how to recover a lost password and how to monitor it.

**Before you start:** You can run commands on the host of the node. For
some tasks you have the role Admin.

## What to back up

| Item | Where | Back up with |
|---|---|---|
| The data volume (objects, users, tokens, audit log, query log, lists) | `/var/lib/kudurru` | A copy of the volume |
| The encryption key | `secrets/encryption_key` | A copy in a safe place |
| `kudurru.yaml` and `objects.d/` | `/etc/kudurru` | A copy of the files |
| The objects only | The API | `kudurru backup` |

The command `kudurru backup` saves zones, record sets, lists, client groups
and policies. It does not save users, tokens, the audit log or the query
log. Use the volume copy for these.

## Back up

### Copy the volume (Docker)

The commands use the container and the volume of Chapter 2. For Docker
Compose, use `docker compose stop kudurru` and `docker compose start
kudurru`. The volume name then has the name of the project as a prefix.

1. Stop the container so that the files are consistent:

   ```sh
   docker stop kudurru
   ```

2. Copy the volume to a file:

   ```sh
   docker run --rm -v kudurru-data:/data -v "$PWD:/backup" busybox \
     tar -czf /backup/kudurru-data.tgz -C /data .
   ```

3. Start the container:

   ```sh
   docker start kudurru
   ```

CAUTION: While the container does not run, the network has no DNS. Do this at a
time when this is acceptable, or use more than one node.

### Save the objects

You can do this while kudurru runs.

```sh
kudurru backup --out objects.yaml
```

Add `--safe` to remove passwords and keys from the URLs of lists. Then you
can share the file. Before you restore a file that you saved with `--safe`,
put the secrets back.

To skip the objects that a file or an integration owns, add
`--exclude-managed`.

### Download from the dashboard

The page **Tools** has the section **Backup and export**. It has links for
the objects as JSON files and for each zone as a BIND file. Use the command
`kudurru backup` for a file that you can restore.

## Restore

### Restore the whole volume

1. Stop and remove the container: `docker stop kudurru` and `docker rm
   kudurru`.
2. Put the content of the backup into the volume:

   ```sh
   docker run --rm -v kudurru-data:/data -v "$PWD:/backup" busybox \
     tar -xzf /backup/kudurru-data.tgz -C /data
   ```

3. Start the container again with the command of Chapter 2. Use the same
   encryption key.

### Restore the objects only

1. Start a node (Chapter 2).
2. Sign in with the command line (Chapter 10).
3. Run the command:

   ```sh
   kudurru restore -f objects.yaml
   ```

The command shows the result for each object.

NOTE: Without a license key, kudurru restores no policy other than the
policy `default`. The other policies get the error `license_required`. Put
the key on the node before you restore (Chapter 13).

CAUTION: If the encryption key is not the same, kudurru cannot read the
stored secrets.

## Upgrade

kudurru stores a schema version in its data files. A new program converts
older data when it starts. A program refuses to start with data that a
newer program wrote. You cannot go back to an older version after a
change of the schema. Make a backup first.

1. Read the release notes. Check for changes that need an action.
2. Back up (see above).
3. Pull the new image. Replace `<version>` with the new version:

   ```sh
   docker pull ghcr.io/robertherbaugh/kudurru:<version>
   ```

4. Stop and remove the old container. The data stays in the volume:

   ```sh
   docker stop kudurru && docker rm kudurru
   ```

5. Start a new container with the command of Chapter 2. Use the new
   version in the image name.
6. Check that the node is ready:

   ```sh
   docker exec kudurru kudurru health --ready
   ```

7. Open the dashboard. Open **System** > **Diagnosis**. Check that there are
   no new errors.

For Helm, Compose with more than one node, and packages, see the
[upgrade runbook](../../deploy/runbooks.md#upgrading). With more than one
node, upgrade one node at a time. Wait until the node is ready before you
go to the next.

## Recover a lost admin password

You need access to the host. The command uses a local socket and no
password.

1. Reset the password:

   ```sh
   docker exec kudurru kudurru admin reset-password admin
   ```

2. Read the new password in the output. kudurru shows it one time.
3. Sign in with the new password. The dashboard asks you to change it.

To set a password that you choose, add `--password-stdin` and give the
password on the standard input.

To make a token for a program when no user can sign in:

```sh
docker exec kudurru kudurru admin create-token --expires-in 1h
```

To list the local users:

```sh
docker exec kudurru kudurru admin list-users
```

NOTE: Only `root`, the user of the server, and a group that you set with
`auth.adminSocket.group` can use these commands. Keep this access limited.

## Rotate the encryption key

Do not replace the encryption key without a plan. If you replace it
without the old key, kudurru cannot read the stored secrets. Follow the
[key rotation runbook](../../deploy/runbooks.md#rotating-the-encryption-key).

## Monitor kudurru

| Tool | Use |
|---|---|
| **Dashboard** | The statistics and the status area |
| **Diagnosis** | The messages about the setup |
| `kudurru health --ready` | Readiness of one node |
| Port 8081 (`/healthz/ready`) | Readiness for load balancers. It needs no sign-in. |
| Port 9153 (`/metrics`) | Metrics for Prometheus |
| `deploy/grafana/kudurru-overview.json` | A Grafana dashboard that you can import |
| `deploy/prometheus/rules.yaml` | Alert rules |

To get the metrics in Docker, publish the metrics port. The compose file in
`deploy/compose/single` has a comment that shows how.

### Change the log level

The server log has the level `info` by default. For a problem, set it to
`debug` for a short time:

```sh
kudurru settings set log.level=debug
```

Set it back with `kudurru settings set log.level-`.

Read the log of the container with `docker logs kudurru`.

## Remove data

| Task | How |
|---|---|
| Delete old query log rows | **Query log** > **Purge…** (Chapter 3) |
| Delete an object | The page of the object, or `kudurru delete` |
| Remove all data | Stop the node. Delete the data volume. |

CAUTION: You cannot undo the deletion of a data volume. Back up first.
