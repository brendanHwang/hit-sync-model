#!/bin/sh
set -eu

skill_dir=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT
package="$stage/hit-sync-model"
mkdir -p "$skill_dir/dist" "$package/bin" "$package/scripts"
cp "$skill_dir/README.md" "$skill_dir/README.ko.md" "$skill_dir/SKILL.md" "$package/"
cp -R "$skill_dir/agents" "$skill_dir/app" "$package/"
cp "$skill_dir/scripts/build.sh" "$skill_dir/scripts/sync_models.py" "$package/scripts/"

for target_os in darwin linux windows; do
    binary=hit-sync-model
    if [ "$target_os" = windows ]; then
        binary=hit-sync-model.exe
    fi
    for target_arch in amd64 arm64; do
        GOOS="$target_os" GOARCH="$target_arch" CGO_ENABLED=0 \
            go -C "$skill_dir/app" build -trimpath -o "$package/bin/$binary" .
        archive="hit-sync-model-$target_os-$target_arch.zip"
        (cd "$stage" && zip -q -r "$archive" hit-sync-model)
        mv "$stage/$archive" "$skill_dir/dist/"
    done
    rm "$package/bin/$binary"
done
