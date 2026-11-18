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
shopt -s nullglob

# Preserve the baked models in older images until the image is rebuilt.
if [[ -d /usr/src/baked_models ]]; then
	for model in /usr/src/custom_models/*.tflite; do
		# Existing Compose overrides can bind-mount an individual detector.
		if ! mountpoint -q -- "$model"; then
			rm -f -- "$model"
		fi
	done
fi

# Baked files are restored first, followed by local files from the directory mount.
for model in /usr/src/baked_models/*.tflite /usr/src/local_models/*.tflite; do
	target="/usr/src/custom_models/${model##*/}"
	if ! mountpoint -q -- "$target"; then
		cp -f -- "$model" "$target"
	fi
done

exec bash /usr/src/docker_run.sh "$@"
