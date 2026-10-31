# Test-only aggregation of unmodified Samba, MIT Kerberos and BIND tools.
# Nothing from this image is linked into or shipped with Kudurru.
FROM debian:trixie-slim@sha256:f6b08b09a05725f9431502d3b5714f5c4576e82c1a84fa1debe0c24a7e0c3ec4
RUN apt-get update \
 && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
      samba-ad-dc samba-ad-provision samba-common-bin samba-vfs-modules \
      krb5-user bind9-dnsutils ldb-tools python3 \
 && rm -rf /var/lib/apt/lists/*
COPY container.py /opt/lab/container.py
ENTRYPOINT ["python3", "/opt/lab/container.py"]
