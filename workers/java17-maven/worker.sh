#!/bin/bash
set -euo pipefail
umask 077
export PATH=/opt/java/openjdk/bin:/usr/share/maven/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin
export JAVA_HOME=/opt/java/openjdk HOME=/work/home TMPDIR=/work/tmp LANG=C.UTF-8
export GIT_CONFIG_NOSYSTEM=1 GIT_CONFIG_GLOBAL=/dev/null GIT_TERMINAL_PROMPT=0
export MAVEN_OPTS='-Xmx512m -XX:MaxMetaspaceSize=256m -Djava.io.tmpdir=/work/tmp -Duser.home=/work/home'
test_network_mode="${ATG_TEST_NETWORK_MODE:-online}"
surefire_provider="${ATG_SUREFIRE_PROVIDER:-}"
case "$test_network_mode" in offline|online) ;; *) exit 64 ;; esac
if test "$test_network_mode" = offline; then
  [[ "$surefire_provider" == org.apache.maven.surefire:surefire-junit-platform:2.22.2 ]]
fi
unset ATG_TEST_NETWORK_MODE ATG_SUREFIRE_PROVIDER BASH_ENV ENV CDPATH JAVA_TOOL_OPTIONS JDK_JAVA_OPTIONS _JAVA_OPTIONS

case "${1:-}" in
  probe)
    test "$(id -u)" = 65532
    test "$(cat /sys/fs/cgroup/memory.max)" = 3221225472
    test "$(cat /sys/fs/cgroup/pids.max)" = 256
    test "$(cat /sys/fs/cgroup/cpu.max)" = '200000 100000'
    test "$(ulimit -f)" = 524288
    test ! -w /opt/atg/worker.sh
    touch /work/probe
    java_version="$(java -version 2>&1)"
    [[ "$java_version" == *'version "17.'* ]]
    printf '%s\n' "$java_version"
    mvn -version
    if test "$test_network_mode" = offline; then
      unshare --user --map-root-user --net /bin/bash -c 'shopt -s nullglob; interfaces=(/sys/class/net/*); test "${#interfaces[@]}" -eq 1 && test "${interfaces[0]##*/}" = lo' >/dev/null
    fi
    exit 0 ;;
  status)
    if test -f /work/out/exit; then cat /work/out/exit
    elif test -f /work/out/toolchain; then printf 'input\n'
    else printf 'pending\n'; fi
    exit 0 ;;
  collect)
    # No directory traversal or host extraction. The driver bounds tar bytes and
    # admits only regular files with explicit output names.
    # Partial collection can race a growing log; GNU tar still emits a complete
    # size-bounded entry and reports status 1 for this observation.
    if test "${2:-}" = partial; then
      mkdir -p /work/out/reports
      shopt -s nullglob
      for f in /work/repo/target/surefire-reports/TEST-*.xml; do
        test -f "$f" && test ! -L "$f" || continue
        cp "$f" /work/out/reports/
      done
    fi
    set +e
    tar -C /work/out -cf - -- .
    result=$?
    test "$result" -eq 0 || { test "${2:-}" = partial && test "$result" -eq 1; }
    exit 0 ;;
  admit)
    digest="$2"
    if test -f /work/admitted; then test "$(cat /work/admitted)" = "$digest"; exit; fi
    test -p /work/input
    test -f /work/out/toolchain
    mkdir -p /work/staged
    tar -xf - -C /work/staged --no-same-owner --no-same-permissions
    # The driver constructs this archive from validated Java paths, never from
    # uploaded tar bytes. Reject links in BOTH trees before copying any content.
    test -z "$(find /work/repo /work/staged -type l -print -quit)"
    while IFS= read -r -d '' f; do
      rel="${f#/work/staged/}"
      test ! -e "/work/repo/$rel"
    done < <(find /work/staged -type f -print0)
    while IFS= read -r -d '' f; do
      rel="${f#/work/staged/}"
      mkdir -p "/work/repo/$(dirname "$rel")"
      cp --no-clobber "$f" "/work/repo/$rel"
    done < <(find /work/staged -type f -print0)
    printf '%s' "$digest" > /work/admitted
    printf 'ready\n' > /work/input
    exit 0 ;;
  seal|cancel)
    action="$1"
    IFS= read -r receipt
    [[ "$receipt" =~ ^sha256:[a-f0-9]{64}$ ]]
    printf '%s\n' "$receipt" > /work/receipt
    if test "$action" = cancel; then kill -USR1 1; else printf 'collected\n' > /work/collect; fi
    exit 0 ;;
  run) ;;
  *) exit 64 ;;
esac

phase="$2"; selector="${3:-}"; repository_url="${4:-}"; source_commit="${5:-}"
case "$phase" in source|package|focused_tests|verify) ;; *) exit 64 ;; esac
[[ "$repository_url" =~ ^https://[A-Za-z0-9.-]+/[A-Za-z0-9_./-]+$ || "$repository_url" =~ ^http://(host\.containers\.internal|localhost|127\.0\.0\.1)(:[0-9]+)?/[A-Za-z0-9_./-]+$ ]]
[[ "$repository_url" != *'..'* ]]
[[ "$source_commit" =~ ^([a-f0-9]{40}|[a-f0-9]{64})$ ]]
mkdir -p /work/{home,tmp,out,repo}
mkfifo /work/input /work/collect
finish_cancel() { if test -f /work/receipt; then printf 'ATG_RECEIPT='; cat /work/receipt; fi; exit 130; }
trap finish_cancel USR1

set +e
(
  set -euo pipefail
  ulimit -f 524288
  cd /work/repo
  object_format=sha1
  if test "${#source_commit}" = 64; then object_format=sha256; fi
  git -c init.templateDir= init -q --object-format="$object_format"
  git -c credential.helper= -c core.hooksPath=/dev/null -c protocol.allow=never -c protocol.https.allow=always -c protocol.http.allow=always -c http.followRedirects=false fetch -q --depth=1 "$repository_url" "$source_commit"
  git -c core.hooksPath=/dev/null checkout -q --detach FETCH_HEAD
  test "$(git rev-parse HEAD)" = "$source_commit"
  git rev-parse HEAD HEAD^{tree} > /work/out/source-identity
  git archive --format=tar.gz HEAD > /work/out/source.tar.gz
  { java -version; mvn -version; git --version; uname -sm; } > /work/out/toolchain 2>&1
  printf '<settings xmlns="http://maven.apache.org/SETTINGS/1.0.0"><localRepository>/work/m2</localRepository><mirrors><mirror><id>public-central</id><mirrorOf>*</mirrorOf><url>https://repo.maven.apache.org/maven2</url></mirror></mirrors></settings>\n' > /work/settings.xml
  if test "$phase" != source; then
    # Fresh checkout every operation, with no Maven cache or shared volume.
    if test "$phase" != package; then IFS= read -r _ < /work/input; fi
    common=(-B -ntp -s /work/settings.xml -gs /work/settings.xml -Dstyle.color=never -DfailIfNoTests=true '-DargLine=-Xmx512m -XX:MaxMetaspaceSize=256m' -DforkCount=1 -DreuseForks=true)
    run_selected_unit_tests() {
      [[ "$selector" =~ ^[A-Za-z0-9_.,#+]+$ ]]
      if test "$test_network_mode" = offline; then
        # Resolve the pinned source's Maven artifacts before isolating test execution.
        mvn "${common[@]}" -DincludeScope=test \
          -Dartifact="$surefire_provider" \
          org.apache.maven.plugins:maven-dependency-plugin:3.6.1:go-offline \
          org.apache.maven.plugins:maven-dependency-plugin:3.6.1:resolve \
          org.apache.maven.plugins:maven-dependency-plugin:3.6.1:resolve-plugins \
          org.apache.maven.plugins:maven-dependency-plugin:3.6.1:get
        unshare --user --map-root-user --net mvn -o "${common[@]}" clean test "-Dtest=$selector" -Dsurefire.failIfNoSpecifiedTests=true
      else
        mvn "${common[@]}" clean test "-Dtest=$selector" -Dsurefire.failIfNoSpecifiedTests=true
      fi
    }
    case "$phase" in
      package) if test -n "$selector"; then run_selected_unit_tests; else mvn "${common[@]}" clean package; fi ;;
      focused_tests) run_selected_unit_tests ;;
      verify) if test -n "$selector"; then run_selected_unit_tests; else mvn "${common[@]}" clean verify; fi ;;
    esac
  fi
) > /work/out/command.log 2>&1 &
job=$!
wait "$job"
code=$?
set -e
# Preserve raw XML from the fresh target only, never checked-in/stale reports.
mkdir -p /work/out/reports
shopt -s nullglob
for f in /work/repo/target/surefire-reports/TEST-*.xml; do
  test -f "$f" && test ! -L "$f" || exit 65
  cp "$f" /work/out/reports/
done
printf '%s\n' "$code" > /work/out/exit
IFS= read -r _ < /work/collect
printf 'ATG_RECEIPT='
cat /work/receipt
exit "$code"
