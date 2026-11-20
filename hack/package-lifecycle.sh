#!/usr/bin/env bash
set -euo pipefail

readonly release_tag=v0.1.0
readonly release_root="https://github.com/robertherbaugh/kudurru/releases/download/$release_tag"
readonly release_manifest_sha=8383e8b1d0721e82eadff82123e1817a057621494ce5e5f9731d1e6d15b47d67
readonly released_deb=kudurru_0.1.0_amd64.deb released_rpm=kudurru-0.1.0-1.x86_64.rpm
readonly rocky_init_image=quay.io/rockylinux/rockylinux@sha256:f1879ea953baa37f34ede6862c41d471a1e891c77daa4ef5031b9aac37af5aa3
readonly conf=/etc/kudurru yaml=/etc/kudurru/kudurru.yaml env_file=/etc/kudurru/kudurru.env
readonly api_cert=/var/lib/kudurru/keys/api.crt objects_probe="$conf/objects.d/package-lifecycle.yaml"
readonly unit=kudurru.service unit_file=/usr/lib/systemd/system/kudurru.service
readonly enabled_link=/etc/systemd/system/multi-user.target.wants/kudurru.service

die() { echo "package-lifecycle: $*" >&2; exit 1; }

candidate_package() {
  local dir=$1 format=$2
  local -a matches
  shopt -s nullglob
  case "$format" in
    deb) matches=("$dir"/kudurru_*_amd64.deb) ;;
    rpm) matches=("$dir"/kudurru-*.x86_64.rpm) ;;
  esac
  [[ ${#matches[@]} == 1 ]] || die "expected exactly one amd64 $format in $dir"
  printf '%s\n' "${matches[0]}"
}

verify_manifest() (
  cd "$1"
  printf '%s  checksums.txt\n' "$release_manifest_sha" | sha256sum --check --strict
)

package_sha() {
  local path=$1 name=${1##*/} entry
  entry=$(awk -v name="$name" '$2 == name || $2 == "*" name { print }' "${path%/*}/checksums.txt")
  [[ -n $entry && $entry != *$'\n'* ]] || die "expected exactly one checksum for $name"
  if ! (cd "${path%/*}"; printf '%s\n' "$entry" | sha256sum --check --strict --status); then
    die "SHA256 verification failed for $name"
  fi
  printf '%s\n' "${entry%% *}"
}

prepare() {
  local dist=$1 inputs=$2 asset digest deb rpm
  deb=$(candidate_package "$dist" deb)
  rpm=$(candidate_package "$dist" rpm)
  mkdir "$inputs"
  mkdir "$inputs/released" "$inputs/candidate"
  cp "$deb" "$rpm" "$dist/checksums.txt" "$inputs/candidate/"
  curl -fsSL --retry 3 -o "$inputs/released/checksums.txt" "$release_root/checksums.txt"
  verify_manifest "$inputs/released"
  for asset in "$released_deb" "$released_rpm"; do
    curl -fsSL --retry 3 -o "$inputs/released/$asset" "$release_root/$asset"
    digest=$(package_sha "$inputs/released/$asset")
    printf 'Verified %s: %s\n' "$asset" "$digest"
  done
  for asset in "${deb##*/}" "${rpm##*/}"; do
    digest=$(package_sha "$inputs/candidate/$asset")
    printf 'Verified %s: %s\n' "$asset" "$digest"
  done
}

require_systemd() {
  [[ $EUID == 0 && $(< /proc/1/comm) == systemd && $(uname -m) == x86_64 ]] ||
    die 'requires root on a disposable x86_64 guest with systemd as PID 1'
}

check_config_permissions() {
  [[ $(stat -c '%U:%G %a' "$conf") == 'root:kudurru 750' ]] || die 'incorrect config directory permissions'
  local file
  for file in "$yaml" "$env_file"; do
    [[ $(stat -c '%U:%G %a' "$file") == 'root:kudurru 640' ]] || die "incorrect permissions on $file"
  done
}

configure() {
  local file
  sed -i 's/0\.0\.0\.0:/127.0.0.1:/g' "$yaml"
  printf '\n# package-lifecycle upgrade marker\n' >> "$yaml"
  for file in encryption_key admin_password; do
    (umask 027; openssl rand -base64 32 > "$conf/$file")
    chgrp kudurru "$conf/$file"
  done
  printf '\nKUDURRU_ENCRYPTION_KEY_FILE=%s/encryption_key\nKUDURRU_ADMIN_PASSWORD_FILE=%s/admin_password\n' \
    "$conf" "$conf" >> "$env_file"
}

wait_ready() {
  local attempt
  for ((attempt = 0; attempt < 60; attempt++)); do
    if systemctl is-active --quiet "$unit" && kudurru health --ready --timeout 1s >/dev/null 2>&1; then
      return
    fi
    sleep 1
  done
  die 'kudurru did not become ready'
}

probe_service() {
  local invocation protocol answer
  wait_ready
  systemctl is-enabled --quiet "$unit"
  invocation=$(systemctl show --property=InvocationID --value "$unit")
  [[ -n $invocation && $invocation != 00000000000000000000000000000000 && $invocation != "${1:-}" ]] ||
    die 'expected a new systemd service invocation'
  echo "Active systemd invocation: $invocation"
  kudurru doctor --config "$yaml" --server https://127.0.0.1:8080 --ca-file "$api_cert"
  for protocol in +notcp +tcp; do
    answer=$(dig -r @127.0.0.1 +time=2 +tries=1 +short "$protocol" health.kudurru.internal. TXT)
    [[ $answer =~ ^\"[^\"]+\ [0-9]+\"$ ]] || die "unexpected DNS health TXT ($protocol): $answer"
    printf 'DNS %s: %s\n' "$protocol" "$answer"
  done
}

stage_objects_probe() {
  (umask 027; cat > "$objects_probe" <<'YAML'
apiVersion: kudurru.io/v1alpha1
kind: DNSZone
metadata: { name: package-lifecycle }
spec:
  zoneName: package-lifecycle.test.
  type: primary
  soa: { mname: ns1.package-lifecycle.test., rname: hostmaster.package-lifecycle.test. }
  nameservers: [ns1.package-lifecycle.test.]
  policy: { queryAccess: allow }
YAML
  )
  chgrp kudurru "$objects_probe"
}

probe_objects() {
  local attempt answer
  for ((attempt = 0; attempt < 20; attempt++)); do
    if answer=$(dig -r @127.0.0.1 +time=1 +tries=1 +norecurse +short package-lifecycle.test. SOA) &&
      [[ $answer == 'ns1.package-lifecycle.test. hostmaster.package-lifecycle.test. '* ]]; then
      printf 'DNS from objects.d: %s\n' "$answer"
      return
    fi
    sleep 1
  done
  die 'packaged service did not serve the zone from objects.d'
}

rpm_is_newer() {
  local ordering
  ordering=$(KUDURRU_RPM_CANDIDATE_EVR="$1" KUDURRU_RPM_RELEASED_EVR="$2" rpm --eval \
    '%{lua: print(rpm.vercmp(os.getenv("KUDURRU_RPM_CANDIDATE_EVR"), os.getenv("KUDURRU_RPM_RELEASED_EVR")))}') || return
  [[ $ordering == 1 ]]
}

lifecycle() {
  local format=$1 inputs=$2 result=$3 released candidate released_sha candidate_sha
  local old_version new_version account config_sha previous purge=false
  [[ ! -e $conf && ! -e /var/lib/kudurru && ! -e $unit_file && ! -e /usr/bin/kudurru ]] ||
    die 'found an existing kudurru installation or data'
  if getent passwd kudurru >/dev/null || getent group kudurru >/dev/null; then die 'found an existing kudurru account'; fi
  [[ ! -e $result && ! -L $result ]] || die "result already exists: $result"

  verify_manifest "$inputs/released"
  candidate=$(candidate_package "$inputs/candidate" "$format")
  case "$format" in
    deb) released="$inputs/released/$released_deb" ;;
    rpm) released="$inputs/released/$released_rpm" ;;
  esac
  released_sha=$(package_sha "$released")
  candidate_sha=$(package_sha "$candidate")

  echo "Install $release_tag ($format)"
  case "$format" in
    deb)
      old_version=$(dpkg-deb -f "$released" Version)
      new_version=$(dpkg-deb -f "$candidate" Version)
      dpkg --compare-versions "$new_version" gt "$old_version" || die 'candidate is not newer than v0.1.0'
      dpkg -i "$released"
      [[ $(dpkg-query -W -f='${Status} ${Version}' kudurru) == "install ok installed $old_version" ]] || die 'released DEB is not installed'
      ;;
    rpm)
      old_version=$(rpm -qp --qf '%{EVR}' "$released")
      new_version=$(rpm -qp --qf '%{EVR}' "$candidate")
      rpm_is_newer "$new_version" "$old_version" || die 'candidate is not newer than v0.1.0'
      rpm -i "$released"
      [[ $(rpm -q --qf '%{EVR}' kudurru) == "$old_version" ]] || die 'released RPM is not installed'
      ;;
  esac
  account=$(id kudurru)
  check_config_permissions
  [[ $(systemctl show --property=ActiveState --value "$unit") == inactive && ! -L $enabled_link ]] ||
    die 'package started or enabled the unit before configuration'
  configure
  config_sha=$(sha256sum "$yaml" "$env_file")

  echo "Start and restart $release_tag"
  systemctl enable --now "$unit"
  probe_service
  previous=$(systemctl show --property=InvocationID --value "$unit")
  systemctl restart "$unit"
  probe_service "$previous"
  stat -c "${release_tag} objects.d: %U:%G %a" "$conf/objects.d"
  stage_objects_probe

  echo "Upgrade $old_version -> $new_version"
  previous=$(systemctl show --property=InvocationID --value "$unit")
  case "$format" in
    deb)
      DEBIAN_FRONTEND=noninteractive dpkg --force-confold -i "$candidate"
      [[ $(dpkg-query -W -f='${Status} ${Version}' kudurru) == "install ok installed $new_version" ]] || die 'candidate DEB is not installed'
      ;;
    rpm)
      rpm -U "$candidate"
      [[ $(rpm -q --qf '%{EVR}' kudurru) == "$new_version" ]] || die 'candidate RPM is not installed'
      ;;
  esac
  printf '%s\n' "$config_sha" | sha256sum --check --strict
  check_config_permissions
  [[ $(stat -c '%U:%G %a' "$conf/objects.d") == 'root:kudurru 750' ]] || die 'incorrect candidate objects.d permissions'
  probe_service "$previous"
  probe_objects
  previous=$(systemctl show --property=InvocationID --value "$unit")
  systemctl restart "$unit"
  probe_service "$previous"
  rm "$objects_probe"

  echo "Remove $new_version"
  case "$format" in
    deb) DEBIAN_FRONTEND=noninteractive apt-get remove -y kudurru ;;
    rpm) rpm -e kudurru ;;
  esac
  [[ $(systemctl show --property=ActiveState --value "$unit") == inactive && ! -L $enabled_link ]] ||
    die 'removal did not stop and disable the unit'
  [[ ! -e $unit_file && ! -e /usr/bin/kudurru ]] || die 'removal left package files behind'
  case "$format" in
    deb)
      [[ $(dpkg-query -W -f='${db:Status-Status}' kudurru) == config-files ]] || die 'DEB removal did not leave conffiles'
      printf '%s\n' "$config_sha" | sha256sum --check --strict
      echo "Purge $new_version"
      DEBIAN_FRONTEND=noninteractive apt-get purge -y kudurru
      [[ $(dpkg-query -W -f='${db:Status-Status}' kudurru 2>/dev/null || true) =~ ^(not-installed)?$ ]] ||
        die 'DEB is still installed or has conffiles registered'
      purge=true
      ;;
    rpm)
      if rpm -q kudurru; then die 'RPM is still installed'; fi
      printf '%s\n' "$config_sha" | sed 's/$/.rpmsave/' | sha256sum --check --strict
      ;;
  esac
  [[ ! -e $yaml && ! -e $env_file ]] || die 'package-owned config remains at its original paths'
  [[ $(id kudurru) == "$account" && -d /var/lib/kudurru && -s $conf/encryption_key && -s $conf/admin_password ]] ||
    die 'removal deleted the account, state, or unmanaged secrets'

  jq -n --arg format "$format" --arg tag "$release_tag" --arg manifest "$release_manifest_sha" \
    --arg released "${released##*/}" --arg releasedSha "$released_sha" --arg oldVersion "$old_version" \
    --arg candidate "${candidate##*/}" --arg candidateSha "$candidate_sha" --arg newVersion "$new_version" \
    --argjson purge "$purge" '{
      format: $format,
      released: {tag: $tag, manifestSha256: $manifest, name: $released, sha256: $releasedSha, version: $oldVersion},
      candidate: {name: $candidate, sha256: $candidateSha, version: $newVersion},
      maintainerScriptsRun: true,
      systemdLifecycleRun: true,
      checks: {install: true, doctor: true, dnsUdpAndTcp: true, restart: true, upgrade: true, candidateObjectsDirectory: true,
        editedConfigPreserved: true, removeStoppedAndDisabled: true, purgeRun: $purge,
        accountStateAndUnmanagedSecretsRetained: true}
    }' > "$result"
}

rocky() (
  local inputs=$1 result=$2 container attempt
  require_systemd
  [[ ! -e $result && ! -L $result ]] || die "result already exists: $result"
  container=$(podman run --detach --privileged --systemd=always --cgroupns=private \
    --volume "$(realpath "$inputs"):/packages:ro" \
    --volume "$(realpath "$0"):/package-lifecycle.sh:ro" "$rocky_init_image" /sbin/init)
  trap 'podman rm -f "$container" >/dev/null' EXIT
  for ((attempt = 0; attempt < 60; attempt++)); do
    if podman exec "$container" systemctl show --property=Version --value >/dev/null 2>&1; then
      break
    fi
    sleep 1
  done
  if (( attempt == 60 )); then
    podman logs "$container" >&2
    die 'Rocky Linux systemd did not come up'
  fi
  podman exec "$container" dnf install -y bind-utils jq openssl shadow-utils
  podman exec "$container" bash /package-lifecycle.sh rpm /packages /tmp/package-result.json
  podman cp "$container:/tmp/package-result.json" "$result"
)

[[ $# == 3 ]] || die 'usage: package-lifecycle.sh prepare DIST INPUTS | deb|rocky INPUTS RESULT.json (disposable systemd VM only)'
export LC_ALL=C
case "$1" in
  prepare) prepare "$2" "$3" ;;
  deb|rpm)
    require_systemd
    trap 'if (( $? != 0 )); then journalctl -u "$unit" --no-pager -n 100 >&2 || true; fi' EXIT
    lifecycle "$1" "$2" "$3"
    ;;
  rocky) rocky "$2" "$3" ;;
  *) die "unknown mode: $1" ;;
esac
