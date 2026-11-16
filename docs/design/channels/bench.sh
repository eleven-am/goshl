#!/usr/bin/env bash
set -u

SS=${SS:-300}
DUR=${DUR:-60}
PRESET=${PRESET:-superfast}
BITRATE=${BITRATE:-3000000}
JOBS=${JOBS:-1}
HDR_CHAINS=${HDR_CHAINS:-current scalefirst}
VF=${VF:-}
THREADS=${THREADS:-}
REPEAT=${REPEAT:-1}
HEADROOM=${HEADROOM:-70}
LONG=${LONG:-1280}
SHORT=${SHORT:-720}

if [ "$#" -eq 0 ]; then
  cat >&2 <<'USAGE'
usage: bench.sh FILE [FILE...]

Run inside the goshl-ffmpeg image, sources mounted read-only:

  docker run --rm -v /path/to/media:/m:ro -v "$PWD":/w --entrypoint bash \
    zot.maix.ovh/goshl-ffmpeg:9.0.2-1 /w/bench.sh /m/a.mkv /m/b.mp4

Environment:
  SS=300            seek into each source, seconds
  DUR=60            seconds of content to transcode
  PRESET=superfast  libx264 preset
  BITRATE=3000000   video bitrate, peak is 1.5x, buffer is 3x
  JOBS=1            concurrent copies of each transcode
  HDR_CHAINS="current scalefirst"   chains for PQ and HLG sources; also zscalefirst, sourceres
  VF=               custom filter chain, %W and %H are replaced
  THREADS=          value for -threads on the encoder, empty for auto
  REPEAT=1          runs per case, the fastest is reported
  HEADROOM=70       percent of the CPU budget the channel estimate may use
  LONG=1280 SHORT=720   bounding box of the output

Columns:
  avg     content seconds per wall second, slowest job
  min5s   slowest five second window of the slowest job
  cores   CPU seconds burned per content second, per job
  wall    wall seconds for DUR seconds of content, slowest job
A channel holds real time only while min5s stays above 1.00.
The closing table divides the CPU budget (the smaller of nproc and the cgroup
limit, times HEADROOM) by cores. Confirm any figure you rely on by rerunning
with JOBS set to it and checking min5s.
USAGE
  exit 2
fi

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

SDR_TAGS="setparams=color_primaries=bt709:color_trc=bt709:colorspace=bt709:range=tv"
CHAIN_SDR="scale=%W:%H:out_color_matrix=bt709:out_range=tv,setsar=1,format=yuv420p,$SDR_TAGS"
CHAIN_CURRENT="zscale=w=%W:h=%H:t=linear:npl=100,format=gbrpf32le,zscale=p=bt709,tonemap=tonemap=hable:desat=0,zscale=t=bt709:m=bt709:r=tv,setsar=1,format=yuv420p,$SDR_TAGS"
CHAIN_SCALEFIRST="scale=%W:%H,zscale=t=linear:npl=100,format=gbrpf32le,zscale=p=bt709,tonemap=tonemap=hable:desat=0,zscale=t=bt709:m=bt709:r=tv,setsar=1,format=yuv420p,$SDR_TAGS"

CHAIN_SOURCERES="zscale=t=linear:npl=100,format=gbrpf32le,zscale=p=bt709,tonemap=tonemap=hable:desat=0,zscale=w=%W:h=%H:t=bt709:m=bt709:r=tv,setsar=1,format=yuv420p,$SDR_TAGS"

chain_for() {
  case "$1" in
    sdr) echo "$CHAIN_SDR" ;;
    current) echo "$CHAIN_CURRENT" ;;
    scalefirst) echo "$CHAIN_SCALEFIRST" ;;
    zscalefirst) echo "zscale=w=%W:h=%H,${CHAIN_SCALEFIRST#*,}" ;;
    sourceres) echo "$CHAIN_SOURCERES" ;;
    custom) echo "$VF" ;;
  esac
}

stamp() {
  local line
  while IFS= read -r line; do
    case "$line" in
      out_time_us=[0-9]*) echo "$EPOCHREALTIME ${line#*=}" ;;
    esac
  done
}

run_case() {
  local attempt row best_row="" best_avg="" avg
  for attempt in $(seq 1 "$REPEAT"); do
    row=$(run_once "$@")
    avg=$(echo "$row" | awk -F'|' '{ gsub(/[ x]/, "", $6); print $6 + 0 }')
    if [ -z "$best_row" ] || awk -v a="$avg" -v b="$best_avg" 'BEGIN { exit !(a > b) }'; then best_row=$row; best_avg=$avg; fi
  done
  echo "$best_row"
}

run_once() {
  local file=$1 name=$2 chain=$3 meta=$4 width=$5 height=$6
  local filter level i pids=() ok=1
  filter=$(chain_for "$chain")
  filter=${filter//%W/$width}
  filter=${filter//%H/$height}
  level=3.1
  local threads=()
  [ -n "$THREADS" ] && threads=(-threads "$THREADS")
  for i in $(seq 1 "$JOBS"); do
    ffmpeg -nostdin -hide_banner -nostats -loglevel info -benchmark \
      -progress pipe:1 -stats_period 0.5 \
      -ss "$SS" -i "$file" -t "$DUR" \
      -map 0:v:0 -map '0:a:0?' \
      -c:v libx264 -preset "$PRESET" "${threads[@]}" \
      -vf "$filter" -profile:v high -level:v "$level" \
      -b:v "$BITRATE" -maxrate $((BITRATE + BITRATE / 2)) -bufsize $((BITRATE * 3)) \
      -force_key_frames 'expr:gte(t,n_forced*6)' \
      -c:a aac -ac 2 -b:a 128000 -ar 48000 \
      -f mpegts -y /dev/null 2> "$TMP/log.$i" | stamp > "$TMP/progress.$i" &
    pids+=("$!")
  done
  for i in "${pids[@]}"; do wait "$i" || ok=0; done
  for i in $(seq 1 "$JOBS"); do grep -q 'bench: utime=' "$TMP/log.$i" || ok=0; done
  if [ "$ok" -ne 1 ]; then
    printf '| %s | %s | %s | %s | FAILED: %s | | | |\n' "$name" "$meta" "$chain" "$JOBS" "$(grep -iE 'error|invalid|failed' "$TMP/log.1" | tail -1)"
    return
  fi
  local worst_avg="" worst_min="" worst_wall="" cpu_total=0
  for i in $(seq 1 "$JOBS"); do
    local bench utime stime rtime content avg mininst
    bench=$(grep -o 'bench: utime=[0-9.]*s stime=[0-9.]*s rtime=[0-9.]*s' "$TMP/log.$i" | tail -1)
    utime=$(echo "$bench" | sed -E 's/.*utime=([0-9.]+)s.*/\1/')
    stime=$(echo "$bench" | sed -E 's/.*stime=([0-9.]+)s.*/\1/')
    rtime=$(echo "$bench" | sed -E 's/.*rtime=([0-9.]+)s.*/\1/')
    content=$(tail -1 "$TMP/progress.$i" | cut -d' ' -f2)
    content=$(awk -v c="$content" 'BEGIN { printf "%.3f", c / 1000000 }')
    avg=$(awk -v c="$content" -v r="$rtime" 'BEGIN { printf "%.2f", c / r }')
    mininst=$(awk '
      { t[NR] = $1; v[NR] = $2 }
      END {
        m = -1
        lo = 2
        for (k = 3; k <= NR; k++) {
          if (t[k] - t[lo] < 5) continue
          while (lo + 1 < k && t[k] - t[lo + 1] >= 5) lo++
          d = (v[k] - v[lo]) / 1000000 / (t[k] - t[lo])
          if (m < 0 || d < m) m = d
        }
        if (m < 0) print "n/a"; else printf "%.2fx", m
      }' "$TMP/progress.$i")
    cpu_total=$(awk -v t="$cpu_total" -v u="$utime" -v s="$stime" -v c="$content" 'BEGIN { printf "%.4f", t + (u + s) / c }')
    if [ -z "$worst_avg" ] || awk -v a="$avg" -v b="$worst_avg" 'BEGIN { exit !(a < b) }'; then worst_avg=$avg; worst_wall=$rtime; fi
    if [ "$mininst" != "n/a" ]; then
      if [ -z "$worst_min" ] || awk -v a="$mininst" -v b="$worst_min" 'BEGIN { exit !(a + 0 < b + 0) }'; then worst_min=$mininst; fi
    fi
  done
  local cores
  cores=$(awk -v t="$cpu_total" -v j="$JOBS" 'BEGIN { printf "%.2f", t / j }')
  printf '| %s | %s | %s | %s | %sx | %s | %s | %s |\n' "$name" "$meta" "$chain" "$JOBS" "$worst_avg" "${worst_min:-n/a}" "$cores" "$worst_wall"
}

CPU_MODEL=$(lscpu 2>/dev/null | awk -F: '/^Model name/ { gsub(/^ +/, "", $2); print $2; exit }')
[ -z "$CPU_MODEL" ] && CPU_MODEL=$(awk -F: '/model name/ { gsub(/^ +/, "", $2); print $2; exit }' /proc/cpuinfo)
NPROC=$(nproc)
CGROUP_LIMIT=unlimited
if [ -r /sys/fs/cgroup/cpu.max ]; then
  read -r quota period < /sys/fs/cgroup/cpu.max
  [ "$quota" != "max" ] && CGROUP_LIMIT=$(awk -v q="$quota" -v p="$period" 'BEGIN { printf "%.2f", q / p }')
elif [ -r /sys/fs/cgroup/cpu/cpu.cfs_quota_us ]; then
  quota=$(cat /sys/fs/cgroup/cpu/cpu.cfs_quota_us)
  period=$(cat /sys/fs/cgroup/cpu/cpu.cfs_period_us)
  [ "$quota" -gt 0 ] && CGROUP_LIMIT=$(awk -v q="$quota" -v p="$period" 'BEGIN { printf "%.2f", q / p }')
fi
BUDGET=$(awk -v n="$NPROC" -v l="$CGROUP_LIMIT" 'BEGIN { b = n; if (l != "unlimited" && l + 0 < n) b = l + 0; printf "%.2f", b }')

echo "cpu model: ${CPU_MODEL:-unknown}"
echo "nproc: $NPROC"
echo "cgroup cpu limit: $CGROUP_LIMIT"
echo "cpu budget: $BUDGET"
echo "ffmpeg: $(ffmpeg -hide_banner -version | head -1 | cut -d' ' -f1-3)"
echo "settings: libx264 $PRESET, ${BITRATE} b/s, ${DUR}s from ${SS}s, ${JOBS} concurrent job(s), threads ${THREADS:-auto}, box ${LONG}x${SHORT}"
echo
echo "| source | input | chain | jobs | avg | min5s | cores | wall s |"
echo "|---|---|---|---|---|---|---|---|"

for file in "$@"; do
  info=$(ffprobe -v error -select_streams v:0 -show_entries stream=codec_name,width,height,pix_fmt,color_transfer -of csv=p=0 "$file" 2>/dev/null | head -1)
  if [ -z "$info" ]; then
    printf '| %s | unreadable | | | | | | |\n' "$(basename "$file")"
    continue
  fi
  IFS=, read -r codec src_w src_h pix trc <<< "$info"
  if [ $((src_w * SHORT)) -ge $((src_h * LONG)) ]; then
    width=$LONG
    height=$(( (LONG * src_h * 2 / src_w + 2) / 4 * 2 ))
  else
    height=$SHORT
    width=$(( (SHORT * src_w * 2 / src_h + 2) / 4 * 2 ))
  fi
  name=$(basename "$file")
  name=${name:0:40}
  meta="$codec ${src_w}x${src_h} $pix $trc"
  if [ -n "$VF" ]; then
    run_case "$file" "$name" custom "$meta" "$width" "$height" | tee -a "$TMP/rows"
  elif [ "$trc" = "smpte2084" ] || [ "$trc" = "arib-std-b67" ]; then
    for chain in $HDR_CHAINS; do
      run_case "$file" "$name" "$chain" "$meta" "$width" "$height" | tee -a "$TMP/rows"
    done
  else
    run_case "$file" "$name" sdr "$meta" "$width" "$height" | tee -a "$TMP/rows"
  fi
done

echo
echo "concurrent channels sustainable at ${HEADROOM}% of $BUDGET CPUs"
echo
echo "| source | chain | cores | channels | note |"
echo "|---|---|---|---|---|"
[ -s "$TMP/rows" ] && awk -F'|' -v budget="$BUDGET" -v headroom="$HEADROOM" -v jobs="$JOBS" '
  function trim(x) { gsub(/^ +| +$/, "", x); return x }
  {
    name = trim($2); chain = trim($4); avg = trim($6); low = trim($7); cores = trim($8)
    if (avg ~ /FAILED/ || cores == "") { printf "| %s | %s | | 0 | failed |\n", name, chain; next }
    channels = int(budget * headroom / 100 / cores)
    note = ""
    if (avg + 0 < 1) { channels = 0; note = "slower than real time" }
    else if (low != "n/a" && low + 0 < 1) { channels = 0; note = "dips below real time" }
    else if (avg + 0 < 1.5) note = "under 1.5x, no margin"
    if (jobs > 1) note = note (note == "" ? "" : ", ") "measured with " jobs " jobs"
    printf "| %s | %s | %s | %d | %s |\n", name, chain, cores, channels, note
  }' "$TMP/rows"
