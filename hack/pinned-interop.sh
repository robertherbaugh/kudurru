interop_image_pin() {
  local name=$1 source=$2
  sed -n "s/^[[:space:]]*\(const \)\?$name[[:space:]]*=[[:space:]]*\"\([^\" ]\+@sha256:[0-9a-f]\{64\}\)\"$/\2/p" "$source"
}

interop_dnsdist_image() {
  local image
  image=$(interop_image_pin DnsdistImage internal/testutil/kudurrutest/dnsdist.go)
  if [[ -z "$image" ]] || ! grep -Fxq "FROM $image" images/dnsdist/Dockerfile; then
    echo 'The dnsdist Dockerfile and kudurrutest.DnsdistImage must use the same registry digest' >&2
    return 1
  fi
  printf '%s\n' "$image"
}

interop_check_results() {
  local log=$1 expected
  shift
  expected=$(jq -n '$ARGS.positional' --args "$@")
  jq -es --argjson expected "$expected" '
    [.[] | select(.Action == "skip") | (.Test // .Package)] as $skipped |
    [.[] | select(.Test and (.Test | contains("/") | not))] as $top |
    ($top | map(select(.Action == "run").Test) | sort) as $ran |
    ($top | map(select(.Action == "pass").Test) | sort) as $passed |
    if ($skipped | length) != 0 then error("skipped: \($skipped | join(", "))")
    elif $ran != ($expected | sort) or $passed != ($expected | sort) then
      error("expected \($expected); ran \($ran); passed \($passed)")
    elif any(.[]; .Action == "fail") or
      ([.[] | select(.Action == "pass" and .Test == null)] | length) != 1 then
      error("missing successful package result")
    else true end
  ' "$log" >/dev/null
}

interop_test() (
  local suite=$1 package=$2 pattern log
  shift 2
  local -a tests=()
  while [[ $1 != -- ]]; do
    tests+=("$1")
    shift
  done
  shift
  pattern=$(IFS='|'; echo "${tests[*]}")
  if [[ -n ${KUDURRU_INTEROP_RESULTS_DIR:-} ]]; then
    mkdir -p "$KUDURRU_INTEROP_RESULTS_DIR"
    log="$KUDURRU_INTEROP_RESULTS_DIR/$suite.jsonl"
  else
    log=$(mktemp)
    trap 'rm -f -- "$log"' EXIT
  fi
  local status=0
  go test -json -p 2 -count=1 "$package" -run "^($pattern)$" "$@" | tee "$log" || status=1
  interop_check_results "$log" "${tests[@]}" || status=1
  return "$status"
)
