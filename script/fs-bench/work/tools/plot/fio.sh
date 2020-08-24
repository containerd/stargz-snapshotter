#!/bin/bash

#   Copyright The containerd Authors.

#   Licensed under the Apache License, Version 2.0 (the "License");
#   you may not use this file except in compliance with the License.
#   You may obtain a copy of the License at

#       http://www.apache.org/licenses/LICENSE-2.0

#   Unless required by applicable law or agreed to in writing, software
#   distributed under the License is distributed on an "AS IS" BASIS,
#   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
#   See the License for the specific language governing permissions and
#   limitations under the License.

set -euo pipefail

DATA_DIR="${1}"
OUTPUT_DIR="${2}"

processBW() {
  local LOGFILE="$1"
  awk -F ',' '
  BEGIN {
    sum = 0.0
    cnt = 1
    lastMs = 0
  }
  {
    if ($1 != lastMs) {
      printf("%d %g\n", lastMs, sum / 1024.0 / cnt)
      sum = 0.0
      cnt = 1
      lastMs = $1
      next
    }
    sum += $2
    lastMs = $1
    cnt += 1
  }
' "$LOGFILE"
}

processIOPS() {
  local LOGFILE="$1"
  awk -F ',' '
  BEGIN {
    sum = 0.0
    cnt = 1
    lastMs = 0
  }
  {
    if ($1 != lastMs) {
      printf("%d %g\n", lastMs, sum / cnt)
      sum = 0.0
      cnt = 1
      lastMs = $1
      next
    }
    sum += $2
    lastMs = $1
    cnt += 1
  }
' "$LOGFILE"
}

processLAT() {
  local LOGFILE="$1"
  awk -F ',' '
  BEGIN {
    sum = 0.0
    cnt = 1
    lastMs = 0
  }
  {
    if ($1 != lastMs) {
      printf("%d %g\n", lastMs, sum / 1000.0 / cnt)
      sum = 0.0
      cnt = 1
      lastMs = $1
      next
    }
    sum += $2
    lastMs = $1
    cnt += 1
  }
' "$LOGFILE"
}

cd "$DATA_DIR"
LOGS_BW=$(ls *_bw.*.log)
LOGS_IOPS=$(ls *_iops.*.log)
LOGS_LAT=$(ls *_lat.*.log)
LOGS_SLAT=$(ls *_slat.*.log)
LOGS_CLAT=$(ls *_clat.*.log)

for log in $LOGS_BW; do
  processBW "$log" >"$OUTPUT_DIR/$log"
done

for log in $LOGS_IOPS; do
  processIOPS "$log" >"$OUTPUT_DIR/$log"
done

for log in $LOGS_LAT $LOGS_SLAT $LOGS_CLAT; do
  processLAT "$log" >"$OUTPUT_DIR/$log"
done

plotDerivatives() {
  local LOGS="$(echo "$1" | tr '\n' ' ')"
  if echo -n "$LOGS" | grep -q '[^ ][ ][^ ]'; then
    firstLog=$(echo -n "$LOGS" | tr '\n' ' ' | sed 's/ .*$//')
    restLog=$(echo -n "$LOGS" | tr '\n' ' '  | sed 's/^[^ ]* //')
    printf "set key bottom right\n"
    printf 'plot "%s" w l lw 2 title "%s"' "$firstLog" "${firstLog%.log}"
    for log in $restLog; do
      printf ', "%s" w l lw 2 title "%s"' "$log" "${log%.log}"
    done
    printf "\n"
  else
    printf "unset key\n"
    printf 'plot "%s" w l lw 2\n' "$LOGS_BW"
  fi
}

cat >"$OUTPUT_DIR/plot.gpm" <<EOF
set term svg size 1024,1200
set multiplot layout 5,1
set key outside bottom right

set title "I/O Bandwidth"
set xlabel "time (ms)"
set ylabel "bandwidth (MiB/sec)"
$(plotDerivatives "$LOGS_BW")

set title "IOPS"
set xlabel "time (ms)"
unset ylabel
$(plotDerivatives "$LOGS_IOPS")

set title "Total Letency"
set xlabel "time (ms)"
set ylabel "letency (us)"
$(plotDerivatives "$LOGS_LAT")

set title "Completion Latency"
set xlabel "time (ms)"
set ylabel "letency (us)"
$(plotDerivatives "$LOGS_CLAT")
EOF

# TODO(sequix): somehow, fio did not output submission latency log.
#set title "Submission Latency"
#set xlabel "time (s)"
#set ylabel "letency (ms)"
#$(plotDerivatives "$LOGS_SLAT")