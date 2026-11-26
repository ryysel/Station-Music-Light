#!/bin/sh
set -eu
cd "$(dirname "$0")"
: "${YAM_PROFILES:?set YAM_PROFILES to the YAM platforms directory}"
version=$(awk '/^version:/ {print $2}' yam.yaml)
out=dist/$version
mkdir -p "$out"
for pair in 'arm64 aarch64-unknown-linux-musl' 'arm armv7-unknown-linux-musleabihf'; do
    set -- $pair
    mkdir -p "build/$2"
    CGO_ENABLED=0 GOOS=linux GOARCH=$1 GOARM=7 go build -trimpath -buildvcs=false -ldflags='-s -w' -o "build/$2/music-light" .
done
for model in $(sed -n 's/^targets: \[\(.*\)\]/\1/p' yam.yaml | tr -d '\",'); do
    yam-publish check --source . --platform "$model" --profiles "$YAM_PROFILES"
    yam-publish pack --source . --platform "$model" --profiles "$YAM_PROFILES" --output "$out/music-light-$version-$model.yampkg"
done
(cd "$out" && sha256sum ./*.yampkg > SHA256SUMS)
