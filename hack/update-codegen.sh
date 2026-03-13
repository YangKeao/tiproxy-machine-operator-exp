#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd)
cd "$ROOT_DIR"

CONTROLLER_GEN_VERSION=${CONTROLLER_GEN_VERSION:-v0.18.0}
CONTROLLER_GEN=(go run "sigs.k8s.io/controller-tools/cmd/controller-gen@${CONTROLLER_GEN_VERSION}")

rm -f config/crd/bases/_.yaml
"${CONTROLLER_GEN[@]}" object paths=./api/v1alpha1
"${CONTROLLER_GEN[@]}" crd paths=./api/v1alpha1 output:crd:dir=config/crd/bases
