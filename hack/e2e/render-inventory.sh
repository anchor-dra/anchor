#!/usr/bin/env bash
set -euo pipefail

if [[ $# -ne 4 ]]; then
  echo "usage: $0 <source-inventory> <output-inventory> <private-dns> <private-ip>" >&2
  exit 2
fi

source_inventory=$1
output_inventory=$2
private_dns=$3
private_ip=$4

source_dir=$(cd "$(dirname "$source_inventory")" && pwd)
output_dir=$(dirname "$output_inventory")

mkdir -p "$output_dir"

# Kubespray resolves group_vars and its SSM ssh_config relative to inventory_dir.
# Keep the generated inventory self-contained when it is written elsewhere.
if [[ "$source_dir" != "$(cd "$output_dir" && pwd)" ]]; then
  if [[ -d "$source_dir/group_vars" ]]; then
    cp -R "$source_dir/group_vars" "$output_dir/"
  fi
  if [[ -f "$source_dir/ssh_config" ]]; then
    cp "$source_dir/ssh_config" "$output_dir/ssh_config"
  fi
fi

awk -v host="$private_dns" -v ip="$private_ip" '
  /^\[kube_control_plane\]/ && !host_added {
    print host " ansible_host=" ip " ip=" ip " access_ip=" ip
    print ""
    host_added=1
  }
  /^\[calico_rr\]/ && !worker_added {
    print host
    print ""
    worker_added=1
  }
  { print }
' "$source_inventory" >"$output_inventory"

echo "rendered $output_inventory with worker $private_dns"
