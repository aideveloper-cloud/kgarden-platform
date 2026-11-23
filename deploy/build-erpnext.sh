#!/usr/bin/env bash
# Build the K Garden ERPNext image (linux/arm64, for ServerKG) from the committed HEAD of the local
# frappe, erpnext and kgarden checkouts. Uncommitted changes are NOT included.
#
#   deploy/build-erpnext.sh            -> image kgarden/erpnext:<date>-<erpnext sha> and :latest
#
# Paths can be overridden: FRAPPE_SRC, ERPNEXT_SRC, KGARDEN_SRC.
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
FRAPPE_SRC=${FRAPPE_SRC:-$HOME/frappe-bench/apps/frappe}
ERPNEXT_SRC=${ERPNEXT_SRC:-$HOME/Desktop/erpnext}
KGARDEN_SRC=${KGARDEN_SRC:-$HOME/frappe-bench/apps/kgarden}

ctx=$(mktemp -d)
trap 'rm -rf "$ctx"' EXIT
cp "$here"/erpnext/{Containerfile,main-entrypoint.sh,start.sh} "$ctx"/
commits=""
for pair in "frappe:$FRAPPE_SRC" "erpnext:$ERPNEXT_SRC" "kgarden:$KGARDEN_SRC"; do
	name=${pair%%:*}; src=${pair#*:}
	sha=$(git -C "$src" rev-parse HEAD)
	# bench needs a git repo on branch "develop"; a one-commit clone of HEAD keeps the context small.
	git init -q -b develop "$ctx/src/$name"
	git -C "$ctx/src/$name" fetch -q --depth 1 "file://$src" "$sha"
	git -C "$ctx/src/$name" reset -q --hard FETCH_HEAD
	commits+="$name=${sha:0:10} "
	echo "  $name ${sha:0:10} ($(git -C "$src" log -1 --format=%s "$sha" | cut -c1-60))"
done
tag=$(date +%Y%m%d)-$(git -C "$ERPNEXT_SRC" rev-parse --short HEAD)-$(git -C "$KGARDEN_SRC" rev-parse --short HEAD)
docker build --platform linux/arm64 -f "$ctx/Containerfile" --build-arg COMMITS="$commits" \
	-t "kgarden/erpnext:$tag" -t kgarden/erpnext:latest "$ctx"
echo "built kgarden/erpnext:$tag"
# A build leaves tens of GB of BuildKit cache and the Colima disk is finite: keep the two newest
# ERPNext images and drop the build cache (it rebuilds from the base images next time anyway).
docker images kgarden/erpnext --format '{{.CreatedAt}}\t{{.Tag}}' | grep -v $'\tlatest$' | sort -r | tail -n +3 \
	| cut -f2 | xargs -I{} docker rmi "kgarden/erpnext:{}" >/dev/null 2>&1 || true
docker builder prune -af >/dev/null 2>&1 || true
