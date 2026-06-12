#!/usr/bin/env bash
# Copyright 2018 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Builds stand-alone kaniko executor binaries with an embedded static proot,
# mirroring the release pipeline (release-binaries.yaml) for local use.
#
# Usage:
#   scripts/build-binaries.sh [arch ...]
#
# Arguments:
#   arch...   one or more GOARCH values to build (default: amd64 arm64)
#
# Environment:
#   GOOS         target OS                        (default: linux)
#   PLATFORMS    space-separated GOARCH list       (default: "amd64 arm64")
#   UPX          "true" to upx-compress binaries   (default: false)
#   EMBED_PROOT  "true" to fetch+embed real proot  (default: true)
#
# Output: out/executor-<os>-<arch> (+ .sha256) for each requested arch.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"

GOOS="${GOOS:-linux}"
UPX="${UPX:-false}"
EMBED_PROOT="${EMBED_PROOT:-true}"

if [[ "$#" -gt 0 ]]; then
  ARCHES=("$@")
else
  read -r -a ARCHES <<< "${PLATFORMS:-amd64 arm64}"
fi

# Restore the proot assets to their pre-build contents on exit, so embedding a
# real binary for a release-style build never dirties the working tree.
ASSET_BACKUP="$(mktemp -d)"
cp -a pkg/proot/assets/. "${ASSET_BACKUP}/"
restore_assets() {
  cp -a "${ASSET_BACKUP}/." pkg/proot/assets/
  rm -rf "${ASSET_BACKUP}"
}
trap restore_assets EXIT

sha256() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" > "$1.sha256"
  else
    shasum -a 256 "$1" > "$1.sha256"
  fi
}

mkdir -p out

for arch in "${ARCHES[@]}"; do
  echo "==> Building kaniko executor for ${GOOS}/${arch}"

  if [[ "${EMBED_PROOT}" == "true" ]]; then
    ./hack/fetch-proot.sh "${arch}"
  fi

  make out/executor GOOS="${GOOS}" GOARCH="${arch}"

  out="out/executor-${GOOS}-${arch}"
  mv out/executor "${out}"

  if [[ "${UPX}" == "true" ]]; then
    echo "==> Compressing ${out} with upx"
    upx -fq --ultra-brute "${out}"
  fi

  sha256 "${out}"
  echo "==> Built ${out}"
done

echo
echo "Done. Artifacts in out/:"
ls -1 out/executor-"${GOOS}"-* 2>/dev/null || true
