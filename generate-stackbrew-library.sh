#!/usr/bin/env bash
set -Eeuo pipefail

dir="$(dirname "$BASH_SOURCE")"
cd "$dir"

commit="$(git log -1 --format='format:%H' HEAD --)"

from="$(git show "$commit:Dockerfile" | awk '$1 == "FROM" && $2 == "--platform=$TARGETPLATFORM" { from = $3 } END { print from }')" # sweep sweep sweep

arches="$(bashbrew remote arches --json "$from")"
arches="$(jq <<<"$arches" --raw-output '.arches | keys | join(", ")')"

cat <<-EOF
# this file is generated via https://github.com/tianon/containerd-registry/blob/$commit/generate-stackbrew-library.sh

Maintainers: Tianon Gravi <admwiggin@gmail.com> (@tianon)
GitRepo: https://github.com/tianon/containerd-registry.git
GitFetch: refs/heads/main
GitCommit: $commit

Tags: latest
Architectures: $arches
EOF
