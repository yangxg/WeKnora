#!/bin/bash
set -euo pipefail

# Generate Python bindings from docreader.proto. Pass --go in a fork
# development/CI environment to also regenerate Go bindings. The docreader
# image only needs Python bindings; it intentionally does not include a Go
# toolchain.
#
# Python: bash docreader/scripts/generate_proto.sh
# Python + Go: PATH="$HOME/go/bin:$PATH" bash docreader/scripts/generate_proto.sh --go
# Alternate Python: PYTHON_BIN=/path/to/python bash docreader/scripts/generate_proto.sh --go

PYTHON_BIN="${PYTHON_BIN:-python3}"
GENERATE_GO=false

if [[ "${1:-}" == "--go" ]]; then
  GENERATE_GO=true
elif [[ $# -gt 0 ]]; then
  echo "usage: $0 [--go]" >&2
  exit 2
fi

PROTO_DIR="docreader/proto"
PYTHON_OUT="docreader/proto"
GO_OUT="docreader/proto"

if ! "${PYTHON_BIN}" -c 'import grpc_tools.protoc' >/dev/null 2>&1; then
  echo "error: grpcio-tools unavailable; set PYTHON_BIN to a compatible venv" >&2
  exit 1
fi

"${PYTHON_BIN}" -m grpc_tools.protoc \
  -I"${PROTO_DIR}" \
  --python_out="${PYTHON_OUT}" \
  --pyi_out="${PYTHON_OUT}" \
  --grpc_python_out="${PYTHON_OUT}" \
  "${PROTO_DIR}/docreader.proto"

# grpc_tools emits a top-level Python import; docreader is a package.
sed -i 's/import docreader_pb2/from docreader.proto import docreader_pb2/' \
  "${PYTHON_OUT}/docreader_pb2_grpc.py"

if [[ "${GENERATE_GO}" == true ]]; then
  if ! command -v protoc-gen-go >/dev/null 2>&1 || ! command -v protoc-gen-go-grpc >/dev/null 2>&1; then
    echo "error: --go requires protoc-gen-go and protoc-gen-go-grpc on PATH" >&2
    exit 1
  fi
  "${PYTHON_BIN}" -m grpc_tools.protoc \
    -I"${PROTO_DIR}" \
    --go_out="${GO_OUT}" \
    --go_opt=paths=source_relative \
    --go-grpc_out="${GO_OUT}" \
    --go-grpc_opt=paths=source_relative \
    "${PROTO_DIR}/docreader.proto"
fi

echo "Proto files generated successfully (go=${GENERATE_GO})"
