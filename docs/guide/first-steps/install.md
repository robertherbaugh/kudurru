# Start a node

**Before you start**

- You need Docker and a fixed address for the host.
- You need free UDP and TCP port 53, and TCP port 8080.

## 1. Prepare the host

1. Give the host a fixed IPv4 address, outside the dynamic pool of your DHCP server.
2. Check that no other DNS server uses port 53.

If `systemd-resolved` uses port 53, follow the
[host preparation steps](../admin/02-install-and-sign-in.md#prepare-the-host).

## 2. Start kudurru

1. Create a directory and the secret files:

   ```sh
   mkdir -p kudurru/secrets && cd kudurru
   openssl rand -base64 32 > secrets/encryption_key
   openssl rand -base64 18 > secrets/admin_password
   ```

2. Start the container:

   ```sh
   docker run -d --name kudurru --restart unless-stopped \
     -p 53:53/udp -p 53:53/tcp -p 8080:8080 \
     -v kudurru-data:/var/lib/kudurru \
     -v "$PWD/secrets:/run/secrets:ro" \
     -e KUDURRU_ENCRYPTION_KEY_FILE=/run/secrets/encryption_key \
     -e KUDURRU_ADMIN_PASSWORD_FILE=/run/secrets/admin_password \
     ghcr.io/robertherbaugh/kudurru:0.2.0
   ```

3. Keep a safe copy of both secret files.

The volume `kudurru-data` keeps the configuration and logs between restarts.
The encryption key protects stored secrets. Keep it with your backups.

## Check the result

Run `docker ps --filter name=kudurru`. The container appears with a status of `Up`.
Open `https://<host>:8080/`, with the host address in place of `<host>`.
The sign-in page opens.

NOTE: This container serves DNS. For DHCP, complete the separate
[host preparation](../dhcp/prepare.md) before you enable the service.
