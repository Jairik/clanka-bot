#!/usr/bin/env bash
# Copy try-out detectors into the directory the wakeword server already reads,
# then start that server.
#
# wakeword/models/local/ is gitignored, so a model dropped there does not
# dirty the checkout or stop auto-deploy. Compose mounts that directory at
# /usr/src/local_models and mounts this script over the image entrypoint.
# The published image has no copy of these files; they are picked up here,
# at container start, which is why a restart is enough and a rebuild is not.
#
# A local file with the same name as a baked model replaces it. Arguments
# from Compose (--custom-model-dir, --threshold) are forwarded unchanged.

set -euo pipefail

if [[ -d /usr/src/local_models ]]; then
	shopt -s nullglob
	for model in /usr/src/local_models/*.tflite; do
		cp -f "$model" /usr/src/custom_models/
	done
fi

exec bash /usr/src/docker_run.sh "$@"
