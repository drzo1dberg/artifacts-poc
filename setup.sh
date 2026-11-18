#!/usr/bin/env bash
# Sets up and starts the whole stack on a Linux host.
#
#   ./setup.sh                      model from Hugging Face, GPU detected automatically
#   ./setup.sh --model ollama       model from the Ollama registry instead
#   ./setup.sh --gpu cpu            run the model on the CPU, slow
#
# The script checks the host, downloads the model, writes docker-compose.override.yml for the GPU,
# and runs docker compose. It's safe to run again: finished steps are skipped.
set -euo pipefail

cd "$(dirname "$0")"

# The GGUF file on Hugging Face, pinned to a commit and checked against its SHA-256. Qwen publishes no
# GGUF of this model itself. lmstudio-community, the organization behind LM Studio, converts the
# official weights from Qwen/Qwen3.5-9B with llama.cpp.
HF_REPO="lmstudio-community/Qwen3.5-9B-GGUF"
HF_REVISION="1379f25c6b505a3fc737bd7818cb09389cf807c1"
HF_FILE="Qwen3.5-9B-Q4_K_M.gguf"
HF_SHA256="cd76ec205963b3b33350093e6904d9de16c4e666fd104e1f632d25c7f15f2a13"

APP_URL="http://app.localhost:8080"
OVERRIDE="docker-compose.override.yml"
OVERRIDE_MARK="# Written by setup.sh."

model_source="hf"
gpu="auto"

usage() {
	cat <<EOF
Usage: ./setup.sh [--model hf|ollama] [--gpu auto|vulkan|nvidia|cpu]

  --model hf       download $HF_FILE from huggingface.co/$HF_REPO (default)
  --model ollama   let Ollama download qwen3.5:9b from its own registry
  --gpu auto       NVIDIA if the container toolkit is set up, else Vulkan, else CPU (default)
  --gpu vulkan     AMD or Intel GPU through /dev/dri
  --gpu nvidia     NVIDIA GPU through the NVIDIA Container Toolkit
  --gpu cpu        no GPU, about ten times slower
EOF
}

while [ $# -gt 0 ]; do
	case "$1" in
	--model) model_source="${2:-}"; shift 2 ;;
	--gpu) gpu="${2:-}"; shift 2 ;;
	-h | --help) usage; exit 0 ;;
	*) usage >&2; exit 2 ;;
	esac
done
case "$model_source" in hf | ollama) ;; *) usage >&2; exit 2 ;; esac
case "$gpu" in auto | vulkan | nvidia | cpu) ;; *) usage >&2; exit 2 ;; esac

step() { printf '\n==> %s\n' "$*"; }
info() { printf '    %s\n' "$*"; }
warn() { printf '    WARNING: %s\n' "$*" >&2; }
die() {
	printf '\nERROR: %s\n' "$1" >&2
	shift
	for line in "$@"; do printf '       %s\n' "$line" >&2; done
	exit 1
}

# --- Host checks ---

step "Checking the host"

[ "$(uname -s)" = Linux ] || die "This setup needs Linux." \
	"Firecracker uses KVM, which macOS and Windows don't offer to containers." \
	"On Windows, WSL 2 with nested virtualization can work but is untested."

for cmd in docker curl sha256sum; do
	command -v "$cmd" >/dev/null || die "$cmd is not installed."
done
docker info >/dev/null 2>&1 || die "Docker is not running, or this user can't use it." \
	"Start Docker, or add yourself to the docker group and log in again."

compose_version=$(docker compose version --short 2>/dev/null || true)
[ -n "$compose_version" ] || die "Docker Compose v2 is missing (the 'docker compose' plugin)."
# The GPU overrides use !reset, which Compose understands from version 2.24 on.
if [ "$(printf '%s\n2.24.0\n' "${compose_version#v}" | sort -V | head -n 1)" != 2.24.0 ]; then
	die "Docker Compose $compose_version is too old, 2.24 or later is needed."
fi
info "Docker Compose $compose_version"

if [ ! -c /dev/kvm ]; then
	die "/dev/kvm is missing, so sandboxd can't start microVMs." \
		"Turn on virtualization (Intel VT-x or AMD-V) in the firmware settings." \
		"In a VM, the hypervisor must allow nested virtualization."
fi
info "/dev/kvm is present"

if ss -ltn 2>/dev/null | awk '{print $4}' | grep -qE '(^|:)8080$'; then
	if ! docker compose ps --services --status running 2>/dev/null | grep -qx frontdoor; then
		die "Port 8080 is in use by another program. The front door needs it."
	fi
fi

docker_root=$(docker info --format '{{.DockerRootDir}}' 2>/dev/null || true)
if [ -n "$docker_root" ] && free_kb=$(df -Pk "$docker_root" 2>/dev/null | awk 'NR==2 {print $4}'); then
	if [ "$free_kb" -lt $((20 * 1024 * 1024)) ]; then
		warn "Less than 20 GB free in $docker_root. Images, the model, and VM disks need about that much."
	fi
fi

# --- GPU ---

step "Choosing where the model runs"

if [ "$gpu" = auto ]; then
	if command -v nvidia-smi >/dev/null && nvidia-smi -L >/dev/null 2>&1 &&
		docker info --format '{{json .Runtimes}}' 2>/dev/null | grep -q nvidia; then
		gpu=nvidia
	elif compgen -G '/dev/dri/renderD*' >/dev/null; then
		gpu=vulkan
	else
		gpu=cpu
	fi
fi

if [ -e "$OVERRIDE" ] && ! head -n 1 "$OVERRIDE" | grep -qF "$OVERRIDE_MARK"; then
	die "$OVERRIDE exists and was not written by this script." \
		"Move it away, or add the GPU settings for the ollama service yourself."
fi

case "$gpu" in
vulkan)
	compgen -G '/dev/dri/renderD*' >/dev/null || die "No GPU render node in /dev/dri."
	rm -f "$OVERRIDE"
	info "Vulkan through /dev/dri, the default in docker-compose.yml"
	;;
nvidia)
	cat >"$OVERRIDE" <<EOF
$OVERRIDE_MARK Run ./setup.sh again to change it.
# Ollama uses CUDA through the NVIDIA Container Toolkit instead of Vulkan through /dev/dri.
services:
  ollama:
    devices: !reset []
    deploy:
      resources:
        reservations:
          devices:
            - driver: nvidia
              count: all
              capabilities: [gpu]
EOF
	info "NVIDIA GPU through the NVIDIA Container Toolkit, see $OVERRIDE"
	;;
cpu)
	cat >"$OVERRIDE" <<EOF
$OVERRIDE_MARK Run ./setup.sh again to change it.
# No GPU: Ollama runs the model on the CPU.
services:
  ollama:
    devices: !reset []
EOF
	warn "No GPU found or chosen. The model runs on the CPU, and a page can take several minutes."
	;;
esac

# --- Model ---

step "Getting the model"

gguf_path="models/$HF_FILE"
if [ "$model_source" = hf ]; then
	if [ -f "$gguf_path" ]; then
		info "$gguf_path is already there"
	else
		url="https://huggingface.co/$HF_REPO/resolve/$HF_REVISION/$HF_FILE"
		info "Downloading $HF_FILE (5.6 GB) from huggingface.co/$HF_REPO"
		# -C - resumes an interrupted download. The file gets its real name only after the check.
		curl -fL -C - --retry 3 --progress-bar -o "$gguf_path.part" "$url"
		info "Checking the SHA-256"
		echo "$HF_SHA256  $gguf_path.part" | sha256sum -c --quiet - ||
			die "The download is damaged." "Delete $gguf_path.part and run ./setup.sh again."
		mv "$gguf_path.part" "$gguf_path"
	fi
else
	# The models job uses any GGUF file in models/. Move it aside so the registry model is used.
	if compgen -G 'models/*.gguf' >/dev/null; then
		mkdir -p models/unused
		mv models/*.gguf models/unused/
		info "Moved the GGUF files in models/ to models/unused/"
	fi
	info "Ollama downloads qwen3.5:9b (6.6 GB) from its registry when the stack starts"
fi

# --- Start ---

step "Building and starting the stack, the first time takes a few minutes"

docker compose up -d --build

step "Waiting for the model"
# The models job registers the model with Ollama, which copies a GGUF file into its volume.
if ! docker compose wait models >/dev/null; then
	docker compose logs --tail 20 models >&2
	die "Creating the model failed, see the log above."
fi
info "agent-llm is ready"

step "Waiting for the app"
for _ in $(seq 1 60); do
	if curl -fs -o /dev/null -H 'Host: app.localhost' http://127.0.0.1:8080/; then
		break
	fi
	sleep 2
done
curl -fs -o /dev/null -H 'Host: app.localhost' http://127.0.0.1:8080/ ||
	die "The app doesn't answer on port 8080." "Run 'docker compose ps' and 'docker compose logs api'."

cat <<EOF

Done. Open $APP_URL in Chrome or Firefox and log in with any name.
The Agent tab builds pages in a microVM, the first answer takes a little longer while the model loads.

  docker compose logs -f api sandboxd    watch the agent and the microVMs
  docker compose down                    stop everything
  docker compose down -v                 stop and delete all data, including the model
EOF
