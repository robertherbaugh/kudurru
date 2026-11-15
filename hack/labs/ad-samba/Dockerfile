# Test-only aggregation of unmodified Samba, MIT Kerberos and BIND tools.
# Nothing from this image is linked into or shipped with Kudurru.
FROM debian:trixie-slim@sha256:f6b08b09a05725f9431502d3b5714f5c4576e82c1a84fa1debe0c24a7e0c3ec4
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      samba-ad-dc samba-ad-provision samba-common-bin samba-vfs-modules \
      krb5-user bind9-dnsutils ldb-tools python3 \
 && rm -rf /var/lib/apt/lists/*
COPY container.py /opt/lab/container.py
# Samba AD provisioning and its DC processes require root for domain-file
# ownership and identity changes. This disposable test image is never shipped;
# hack/test-ad-samba.py uses private tmpfs and an internal network, with no
# published ports, host realm, privileged mode or additional capabilities.
# nosemgrep: missing-user-entrypoint -- Samba AD lab requires root; see above.
ENTRYPOINT ["python3", "/opt/lab/container.py"]
