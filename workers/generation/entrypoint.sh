#!/bin/sh
set -eu
umask 077
export PATH=/usr/local/bin:/usr/bin:/bin HOME=/work/home TMPDIR=/tmp
export PYTHONDONTWRITEBYTECODE=1 PYTHONNOUSERSITE=1
unset ENV BASH_ENV PYTHONPATH PYTHONSTARTUP GIT_CONFIG SCM_TOKEN GH_TOKEN GITLAB_TOKEN OPENAI_API_KEY

case "${1:-}" in
  probe)
    test "$(id -u)" = 65532
    test "$(cat /sys/fs/cgroup/memory.max)" = 1073741824
    test "$(cat /sys/fs/cgroup/pids.max)" = 128
    test "$(cat /sys/fs/cgroup/cpu.max)" = '100000 100000'
    test ! -w /opt/atg/entrypoint.sh
    test ! -w /opt
    test ! -e /var/run/docker.sock
    test ! -e /run/podman/podman.sock
    touch /work/probe
    exec python -m atg_generation_worker probe ;;
  status)
    if test -f /work/out/exit; then printf 'complete\n'
    elif test ! -f /work/task.json; then printf 'task\n'
    elif test -f /work/launched; then printf 'pending\n'
    elif test ! -f /work/credential; then printf 'credential\n'
    else printf 'pending\n'; fi
    exit 0 ;;
  admit)
    # Only task bytes are accepted here; credentials have a separate stdin path.
    exec python -c '
import hashlib,os,re,sys
d=sys.argv[1]
assert re.fullmatch(r"sha256:[a-f0-9]{64}",d)
raw=sys.stdin.buffer.read(2097153)
assert len(raw)<=2097152 and "sha256:"+hashlib.sha256(raw).hexdigest()==d
target="/work/task.json"
if os.path.exists(target):
    assert open(target,"rb").read()==raw
else:
    with open("/work/task.pending","xb") as out: out.write(raw)
    os.chmod("/work/task.pending",0o400)
    os.replace("/work/task.pending",target)
' "$2" ;;
  credential)
    exec python -c '
import os,sys
raw=sys.stdin.buffer.read(8193)
assert 0<len(raw)<=8192
key=raw.decode("utf-8").strip()
assert 0<len(key)<=4096 and not any(c in key for c in "\r\n\x00")
if not os.path.exists("/work/credential"):
    with open("/work/credential.pending","xb") as out: out.write(key.encode("utf-8"))
    os.chmod("/work/credential.pending",0o600)
    os.replace("/work/credential.pending","/work/credential")
' ;;
  collect)
    # No arbitrary paths, credentials, environment, or host archive extraction.
    partial="${2:-}"
    set --
    for file in task.json out/result.json out/command.log out/exit; do
      if test -f "/work/$file" && test ! -L "/work/$file"; then set -- "$@" "$file"; fi
    done
    # Names are normalized by transform, not sourced from repository/model data.
    set +e
    tar -C /work --transform='s|^out/||' -cf - -- "$@" 2>/dev/null
    code=$?
    test "$code" -eq 0 || { test "$partial" = partial && test "$code" -eq 1; } || exit 65
    exit 0 ;;
  seal|cancel)
    action="$1"
    IFS= read -r receipt
    case "$receipt" in sha256:*) ;; *) exit 65 ;; esac
    test "${#receipt}" -eq 71
    case "${receipt#sha256:}" in *[!a-f0-9]*) exit 65 ;; esac
    if test -f /work/receipt; then test "$(cat /work/receipt)" = "$receipt"
    else printf '%s\n' "$receipt" > /work/receipt.pending; mv /work/receipt.pending /work/receipt; fi
    if test "$action" = cancel; then touch /work/cancel; else test -f /work/out/exit; touch /work/sealed; fi
    exit 0 ;;
  run) ;;
  *) exit 64 ;;
esac

digest="$2"; seconds="$3"; max_output="$4"
case "$digest" in sha256:*) ;; *) exit 64 ;; esac
test "${#digest}" -eq 71
case "${digest#sha256:}" in *[!a-f0-9]*) exit 64 ;; esac
case "$seconds:$max_output" in *[!0-9:]*) exit 64 ;; esac
test "$seconds" -ge 1 && test "$seconds" -le 600
test "$max_output" -ge 4096 && test "$max_output" -le 1048576
mkdir -p /work/home /work/out
job=''
cancel_job() {
  if test -n "$job"; then kill -TERM "$job" 2>/dev/null || :; wait "$job" 2>/dev/null || :; fi
  rm -f /work/credential
}
trap 'cancel_job; exit 130' TERM INT
finish_cancel() {
  cancel_job
  test -f /work/receipt || exit 130
  printf 'ATG_GENERATION_RECEIPT='; cat /work/receipt
  exit 130
}
while test ! -f /work/task.json || test ! -f /work/credential; do
  if test -f /work/cancel; then finish_cancel; fi
  sleep 0.1
done
touch /work/launched
set +e
timeout --signal=TERM --kill-after=5 "$seconds" \
  python -m atg_generation_worker run --task-file /work/task.json \
  --output-file /work/out/result.json --request-digest "$digest" \
  --credential-file /work/credential --allow-model-calls \
  > /work/out/command.log 2>&1 &
job=$!
while kill -0 "$job" 2>/dev/null; do
  if test -f /work/cancel; then finish_cancel; fi
  sleep 0.1
done
wait "$job"
code=$?
set -e
job=''
rm -f /work/credential
printf '%s\n' "$code" > /work/out/exit.pending
mv /work/out/exit.pending /work/out/exit
# A completed result stays in tmpfs until the controller retains and acknowledges
# its content-addressed evidence. stdout contains exactly one stable receipt.
while test ! -f /work/sealed; do
  if test -f /work/cancel; then finish_cancel; fi
  sleep 0.1
done
printf 'ATG_GENERATION_RECEIPT='; cat /work/receipt
exit "$code"
