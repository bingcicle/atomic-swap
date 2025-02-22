#!/usr/bin/env bash
set -e

IMAGE_NAME="atomic-bootnode"

# VERSION can be "latest", a release tag, a hash or a branch name that does not
# contain slashes. The version must be pushed to github, local changes are not
# seen. The variable both defines which version of the tools is go install'ed
# inside the container, as well as the docker image tag.
VERSION="latest"

# Run docker build from the directory of this script
cd "$(dirname "$0")"

docker build \
	--build-arg "VERSION=${VERSION}" \
	--build-arg "USER_UID=$(id -u)" \
	--build-arg "USER_GID=$(id -g)" \
	. -t "${IMAGE_NAME}:${VERSION}"

echo "built ${IMAGE_NAME}:${VERSION}"
