#!/bin/sh
# Creates the model "agent-llm" that llmgateway calls. Runs once per start in the "models" job.
#
# With a GGUF file in models/, for example from Hugging Face via setup.sh, it builds the model from
# that file. Without one, it downloads qwen3.5:9b-q8_0 from the Ollama registry.
set -eu

gguf=$(find /models -maxdepth 1 -name '*.gguf' | sort | head -n 1)

if [ -n "$gguf" ]; then
	echo "Creating agent-llm from $gguf"
	# A GGUF file carries the weights and a chat template, but Ollama turns tool calls and thinking
	# into API fields only with its own renderer and parser for the model family. The sampling
	# values are the ones the Ollama registry ships with qwen3.5.
	cat >/tmp/Modelfile <<EOF
FROM $gguf
RENDERER qwen3.5
PARSER qwen3.5
PARAMETER temperature 1
PARAMETER top_k 20
PARAMETER top_p 0.95
PARAMETER presence_penalty 1.5
EOF
else
	echo "No GGUF file in models/, using qwen3.5:9b-q8_0 from the Ollama registry"
	ollama show qwen3.5:9b-q8_0 >/dev/null 2>&1 || ollama pull qwen3.5:9b-q8_0
	echo 'FROM qwen3.5:9b-q8_0' >/tmp/Modelfile
fi

# Ollama's OpenAI compatible API cannot set the context size per request, so it goes into the model.
echo 'PARAMETER num_ctx 32768' >>/tmp/Modelfile
ollama create agent-llm -f /tmp/Modelfile
