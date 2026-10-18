#!/bin/sh
# Writes the Helm plugin manifest (legacy format, works on Helm 3 and Helm 4) for the given version.
set -eu
version="$1"
out="${2:-build/plugin.yaml}"
mkdir -p "$(dirname "$out")"
cat > "$out" <<YAML
name: debug
version: ${version}
usage: Debug Helm releases by reading their records straight from the cluster
description: |-
  Inspect release resources, find which release declares an object,
  show revision history with failure reasons and find stuck releases.
platformCommand:
  - os: windows
    command: \${HELM_PLUGIN_DIR}\\helm-debug.exe
  - command: \${HELM_PLUGIN_DIR}/helm-debug
YAML
