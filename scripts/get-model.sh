#!/usr/bin/env bash
# Downloads the official Google Gemma 4 26B-A4B QAT Q4_0 GGUF and its vision projector
# into ./models (the paths docker-compose.yml and README expect). Resumes partial downloads.
set -euo pipefail
cd "$(git rev-parse --show-toplevel)"
repo="https://huggingface.co/google/gemma-4-26B-A4B-it-qat-q4_0-gguf/resolve/main"
mkdir -p models
for f in gemma-4-26B-it-mmproj.gguf gemma-4-26B_q4_0-it.gguf; do
  echo "→ $f"
  curl -fL --retry 5 -C - -o "models/$f" "$repo/$f"
done
ls -lh models
