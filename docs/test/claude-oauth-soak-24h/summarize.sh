#!/usr/bin/env bash
set -euo pipefail

output_dir=${1:-${SOAK_OUTPUT_DIR:-}}
if [[ -z $output_dir ]]; then
  echo "usage: $0 <run-output-directory>" >&2
  exit 64
fi

manifest="$output_dir/manifest.tsv"
if [[ ! -f $manifest ]]; then
  echo "manifest not found: $manifest" >&2
  exit 66
fi

hit_min=${SOAK_CACHE_HIT_MIN:-4096}

awk -F '\t' -v hit_min="$hit_min" '
  NR == 1 { next }
  {
    total++
    expected[$6]++
    creation[$6] += $11
    read[$6] += $12
    if ($7 ~ /^2[0-9][0-9]$/) ok++; else failed++
    latency_sum += $8
    input_sum += $9
    output_sum += $10
    creation_sum += $11
    read_sum += $12
    if ($6 == "hit" && $12 + 0 < hit_min) {
      suspicious_hit++
      suspicious_hit_ids = suspicious_hit_ids " " $1
    }
    if (($6 == "cold" || $6 == "ttl_miss") && $11 + 0 < hit_min) {
      suspicious_write++
      suspicious_write_ids = suspicious_write_ids " " $1
    }
  }
  END {
    printf "requests=%d ok=%d failed=%d\n", total, ok, failed
    if (total > 0) {
      printf "avg_latency_s=%.3f input_tokens=%d output_tokens=%d cache_creation_tokens=%d cache_read_tokens=%d\n", latency_sum / total, input_sum, output_sum, creation_sum, read_sum
    }
    print "by_expected_cache:"
    for (kind in expected) {
      printf "  %s count=%d avg_cache_create=%.1f avg_cache_read=%.1f\n", kind, expected[kind], creation[kind] / expected[kind], read[kind] / expected[kind]
    }
    printf "suspicious_hit_count=%d threshold=%d\n", suspicious_hit, hit_min
    if (suspicious_hit > 0) print "  ids:" suspicious_hit_ids
    printf "suspicious_write_count=%d threshold=%d\n", suspicious_write, hit_min
    if (suspicious_write > 0) print "  ids:" suspicious_write_ids
  }
' "$manifest"

