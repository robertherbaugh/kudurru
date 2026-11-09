# Join a new SQLite receiver

This path requires an activated SQLite primary running the enrolled internal
listener and a private invitation issued for the receiver's exact name and
internal address. It is separate from static twins and Kubernetes storage.
Full partition/rejoin qualification and recovery of an incomplete join remain open.

1. On the primary, review `cluster enrollment` and issue `cluster join-token`
   using that exact review, target, absolute expiry and audit reason. Transfer
   the returned invitation or JSON receipt through a private trusted channel.
   The invitation contains a bearer secret and the original primary CA; never
   log it or obtain replacement trust from an unverified server.
2. On the receiver, stop kudurru. Configure the same original
   `KUDURRU_ENCRYPTION_KEY` or `KUDURRU_ENCRYPTION_KEY_FILE` as the primary.
   The invitation does not contain that key, and joining does not create one.
   Use a new data directory that does not already exist. Confirm the target
   name/address independently against the primary's reviewed invitation.
3. Run the offline command as the OS user that will own the receiver data:

   ```sh
   kudurru cluster join --offline \
     --data-dir /var/lib/kudurru/new-receiver \
     --invitation-file /run/private/receiver-invitation.json \
     --confirm-name receiver \
     --confirm-address 10.0.0.12:8082
   ```

4. Require both `primary.committed: true` and a nonempty `installed` generation
   in the successful JSON result. Start the normal server with `node.dataDir`
   set to this directory, the same node name/internal address and encryption
   configuration. Do not configure `cluster.role: static`. Keep the private
   member identity file with this database across restarts.

The command has one original 30-second lifetime, also bounded by invitation
expiry. It sends one proposal and performs one member-authenticated image fetch.
Existing directories and databases are refused. It never starts a daemon or
claims that the newly installed image is published or that all peers converged.

On failure, preserve the directory, database, private identity and diagnostic
result. The primary may have accepted membership even if the receiver could not
confirm the response or install its first image. Do not replay the proposal,
replace the local identity, delete the directory or issue a successor invitation
as an automatic recovery step. Inspect the retained primary and local state
before a separate operator decision. Automated incomplete-join reconciliation is
not implemented by this command.
