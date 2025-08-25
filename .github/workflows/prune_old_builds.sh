#!/bin/bash

set -euo pipefail

KEEP=10
GITHUB_TOKEN=$1

delete_release() {
    release_id=$1
    tag_name=$2
    set -x
    curl -X DELETE --header "Authorization: Bearer $GITHUB_TOKEN" "https://api.github.com/repos/tailscale/go/releases/$release_id"
    curl -X DELETE --header "Authorization: Bearer $GITHUB_TOKEN" "https://api.github.com/repos/tailscale/go/git/refs/tags/$tag_name"
    set +x
}

curl https://api.github.com/repos/tailscale/go/releases 2>/dev/null |\
    jq -r '.[] | "\(.published_at) \(.id) \(.tag_name)"' |\
    egrep '[^ ]+ [^ ]+ build-[0-9a-f]{40}' |\
    sort |\
    head --lines=-${KEEP}|\
    while read date release_id tag_name; do
        delete_release "$release_id" "$tag_name"
    done
