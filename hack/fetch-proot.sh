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

# Fetches a statically linked proot binary for the given Go arch and writes it
# to pkg/proot/assets/proot-linux-<arch> so it is embedded into the kaniko
# executable at build time. Run this before `make out/executor` for release
# builds. From a clean checkout the asset is a text placeholder and kaniko falls
# back to its stub /proc behaviour.

set -euo pipefail

ARCH="${1:-$(go env GOARCH)}"
case "${ARCH}" in
  amd64) APK_ARCH=x86_64 ;;
  arm64) APK_ARCH=aarch64 ;;
  *) echo "fetch-proot: unsupported arch '${ARCH}' (want amd64 or arm64)" >&2; exit 1 ;;
esac

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
DEST="${SCRIPT_DIR}/../pkg/proot/assets/proot-linux-${ARCH}"

base="https://dl-cdn.alpinelinux.org/alpine/edge/community/${APK_ARCH}"
apk="$(curl -fsSL "${base}/" | grep -oE 'proot-static-[0-9][^"]*\.apk' | sort -V | tail -1)"
if [[ -z "${apk}" ]]; then
  echo "fetch-proot: could not locate a proot-static apk under ${base}" >&2
  exit 1
fi

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT

echo "fetch-proot: downloading ${base}/${apk}"
curl -fsSL -o "${tmp}/proot-static.apk" "${base}/${apk}"
# .apk packages are gzip-compressed tarballs; extract just the static binary.
tar -xzf "${tmp}/proot-static.apk" -C "${tmp}" usr/bin/proot.static
install -m 0755 "${tmp}/usr/bin/proot.static" "${DEST}"

echo "fetch-proot: embedded proot for ${ARCH} -> ${DEST}"
