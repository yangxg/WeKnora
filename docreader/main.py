import logging
import os
import re
import sys
import traceback
import uuid
from concurrent import futures
from typing import Optional

import grpc
from grpc_health.v1 import health_pb2_grpc
from grpc_health.v1.health import HealthServicer

from docreader.auth import AuthInterceptor, TLSConfigError, load_tls_credentials
from docreader import config
from docreader.config import CONFIG
from docreader.parser import Parser
from docreader.proto import docreader_pb2_grpc
from docreader.parser.registry import registry
from docreader.proto.docreader_pb2 import (
    ReadRequest,
    ReadResponse,
    ImageRef,
    ReadStreamMeta,
    ReadStreamResponse,
    ListEnginesResponse,
    ParserEngineInfo,
    RuntimeInfoResponse,
)
from docreader.utils.request import init_logging_request_id, request_id_context

_SURROGATE_RE = re.compile(r"[\ud800-\udfff]")

# Only parser-output knobs are safe to disclose to an external materialization
# adapter. Do not turn config.dump_config() into an RPC: it contains proxy URLs,
# internal ODL URLs, gRPC binding and image-output paths, none of which explains
# document body bytes and several of which disclose deployment topology.
_RUNTIME_CONFIG_KEYS = (
    "DOCREADER_DOCX_MAX_PAGES",
    "DOCREADER_MARKITDOWN_MAX_WORKERS",
    "DOCREADER_ODL_MAX_WORKERS",
    "DOCREADER_ODL_HYBRID",
    "DOCREADER_ODL_HYBRID_MODE",
    "DOCREADER_ODL_HYBRID_FALLBACK",
    "DOCREADER_ODL_MARKDOWN_WITH_HTML",
    "DOCREADER_PDF_RENDER_MAX_WORKERS",
    "DOCREADER_PDF_RENDER_PARALLELISM",
    "DOCREADER_PDF_RENDER_DPI",
    "DOCREADER_PDF_JPEG_QUALITY",
    "DOCREADER_PDF_RENDER_MAX_EDGE",
)
_RUNTIME_LIBRARIES = (
    "markitdown",
    "pypdf",
    "pypdfium2",
    "python-docx",
    "opendataloader-pdf",
    "openpyxl",
)
_RUNTIME_IMAGE_TAG = os.environ.get("DOCREADER_IMAGE_TAG", "")


def _runtime_info() -> tuple[dict[str, str], dict[str, str], str]:
    """Return safe parser identity only; never endpoint, proxy or secret config."""

    import importlib.metadata

    effective = config.dump_config()
    parsing_config = {
        key: str(effective[key]) for key in _RUNTIME_CONFIG_KEYS if key in effective
    }
    library_versions: dict[str, str] = {}
    for name in _RUNTIME_LIBRARIES:
        try:
            library_versions[name] = importlib.metadata.version(name)
        except importlib.metadata.PackageNotFoundError:
            continue
    return parsing_config, library_versions, _RUNTIME_IMAGE_TAG


def _image_ref_provenance(ref_path: str, provenance: dict | None) -> dict:
    """Return v2 facts for this exact parser target; empty means v1 semantics."""

    facts = (provenance or {}).get(ref_path, {})
    return {
        "page_number": int(facts.get("page_number", 0) or 0),
        "source_type": str(facts.get("source_type", "") or ""),
        "markdown_target": str(facts.get("markdown_target", "") or ""),
    }


def to_valid_utf8_text(s: Optional[str]) -> str:
    if not s:
        return ""
    s = _SURROGATE_RE.sub("\ufffd", s)
    return s.encode("utf-8", errors="replace").decode("utf-8")


for handler in logging.root.handlers[:]:
    logging.root.removeHandler(handler)

handler = logging.StreamHandler(sys.stdout)
logging.root.addHandler(handler)

_level_name = (os.environ.get("LOG_LEVEL") or "INFO").upper()
_level = getattr(logging, _level_name, logging.INFO)
logging.root.setLevel(_level)

logger = logging.getLogger(__name__)
logger.info("Initializing server logging, level=%s", _level_name)

init_logging_request_id()


def _resolve_images(
    images: dict,
    request_id: str,
    storage_map: dict | None = None,
    provenance: dict | None = None,
) -> tuple[str, list]:
    """Resolve document images into inline bytes for the Go App to persist.

    ``images`` is a dict of {relative_path: raw_data} where raw_data is
    base64-encoded string or raw bytes.

    The Go App is solely responsible for persisting images to the configured
    storage backend (local/minio/cos/tos). This function only decodes images
    and returns them as inline bytes via ImageRef.

    Returns ("", list[ImageRef]).  image_dir_path is always empty.
    """
    import base64

    if not images:
        return "", []

    mime_map = {
        ".png": "image/png",
        ".jpg": "image/jpeg",
        ".jpeg": "image/jpeg",
        ".gif": "image/gif",
        ".webp": "image/webp",
        ".bmp": "image/bmp",
    }

    refs = []
    for ref_path, b64data in images.items():
        try:
            img_bytes = base64.b64decode(b64data)
        except Exception:
            img_bytes = b64data.encode("utf-8") if isinstance(b64data, str) else b64data

        fname = os.path.basename(ref_path) or f"{uuid.uuid4().hex}.png"
        ext = os.path.splitext(fname)[1].lower()
        mime = mime_map.get(ext, "application/octet-stream")

        facts = _image_ref_provenance(ref_path, provenance)
        refs.append(
            ImageRef(
                filename=fname,
                original_ref=ref_path,
                mime_type=mime,
                image_data=img_bytes,
                **facts,
            )
        )

    logger.info("Resolved %d images (mode=inline)", len(refs))
    return "", refs


def _mime_for_ref(ref_path: str) -> tuple[str, str]:
    """Return (filename, mime_type) for an image reference path."""
    mime_map = {
        ".png": "image/png",
        ".jpg": "image/jpeg",
        ".jpeg": "image/jpeg",
        ".gif": "image/gif",
        ".webp": "image/webp",
        ".bmp": "image/bmp",
    }
    fname = os.path.basename(ref_path) or f"{uuid.uuid4().hex}.png"
    ext = os.path.splitext(fname)[1].lower()
    return fname, mime_map.get(ext, "application/octet-stream")


def _iter_image_refs(images: dict, provenance: dict | None = None):
    """Yield ImageRef one at a time, freeing each source entry as we go.

    Used by the streaming RPC so we never hold every decoded image plus its
    base64 source in memory simultaneously (the inline path's peak-memory and
    message-size problem for large scanned PDFs).
    """
    import base64

    for ref_path in list(images.keys()):
        b64data = images.pop(ref_path)
        try:
            img_bytes = base64.b64decode(b64data)
        except Exception:
            img_bytes = b64data.encode("utf-8") if isinstance(b64data, str) else b64data
        del b64data
        fname, mime = _mime_for_ref(ref_path)
        facts = _image_ref_provenance(ref_path, provenance)
        yield ImageRef(
            filename=fname,
            original_ref=ref_path,
            mime_type=mime,
            image_data=img_bytes,
            **facts,
        )


class DocReaderServicer(docreader_pb2_grpc.DocReaderServicer):
    def __init__(self):
        super().__init__()
        self.parser = Parser()

    def _parse_request(self, request: ReadRequest):
        """Run the parser for a ReadRequest, returning (result, source_desc).

        Shared by the unary Read and streaming ReadStream RPCs.
        """
        cfg = request.config
        parser_engine = cfg.parser_engine if cfg else ""
        engine_overrides = dict(cfg.parser_engine_overrides) if cfg else {}

        if request.url:
            logger.info("Read(URL): url=%s", request.url)
            result = self.parser.parse_url(
                request.url,
                request.title,
                parser_engine=parser_engine,
                engine_overrides=engine_overrides,
            )
            return result, request.url

        file_type = request.file_type or os.path.splitext(request.file_name)[1][1:]
        logger.info(
            "Read(File): file=%s, type=%s, size=%d bytes",
            request.file_name,
            file_type,
            len(request.file_content),
        )
        result = self.parser.parse_file(
            request.file_name,
            file_type,
            request.file_content,
            parser_engine=parser_engine,
            engine_overrides=engine_overrides,
        )
        return result, request.file_name

    def Read(self, request: ReadRequest, context):
        """Unified read: file mode (file_content set) or URL mode (url set)."""
        request_id = request.request_id or str(uuid.uuid4())

        with request_id_context(request_id):
            try:
                result, source_desc = self._parse_request(request)

                if not result or not result.content:
                    error_msg = f"Failed to parse: {source_desc}"
                    logger.error(error_msg)
                    return ReadResponse(error=error_msg)

                _c = to_valid_utf8_text
                image_dir, image_refs = _resolve_images(
                    result.images, request_id, provenance=result.image_provenance
                )

                response = ReadResponse(
                    markdown_content=_c(result.content),
                    image_refs=image_refs,
                    image_dir_path=image_dir,
                    metadata={k: _c(str(v)) for k, v in result.metadata.items()}
                    if result.metadata
                    else {},
                )
                logger.info(
                    "Read response: content_len=%d, images=%d",
                    len(result.content),
                    len(image_refs),
                )
                return response

            except Exception as e:
                error_msg = f"Error reading document: {e}"
                logger.error(error_msg)
                logger.info("Traceback: %s", traceback.format_exc())
                return ReadResponse(error=str(e))

    def ReadStream(self, request: ReadRequest, context):
        """Streaming read: yields one meta frame, then one frame per image.

        Each frame is a small, independent gRPC message, so documents with many
        page images (large scanned PDFs) are returned without hitting the unary
        message-size cap, and neither side has to hold the whole payload at once.
        """
        request_id = request.request_id or str(uuid.uuid4())

        with request_id_context(request_id):
            _c = to_valid_utf8_text
            try:
                result, source_desc = self._parse_request(request)
            except Exception as e:
                logger.error("Error reading document: %s", e)
                logger.info("Traceback: %s", traceback.format_exc())
                yield ReadStreamResponse(meta=ReadStreamMeta(error=str(e)))
                return

            if not result or not result.content:
                error_msg = f"Failed to parse: {source_desc}"
                logger.error(error_msg)
                yield ReadStreamResponse(meta=ReadStreamMeta(error=error_msg))
                return

            images = result.images or {}
            image_count = len(images)
            yield ReadStreamResponse(
                meta=ReadStreamMeta(
                    markdown_content=_c(result.content),
                    image_dir_path="",
                    metadata={k: _c(str(v)) for k, v in result.metadata.items()}
                    if result.metadata
                    else {},
                    image_count=image_count,
                )
            )

            sent = 0
            for ref in _iter_image_refs(images, provenance=result.image_provenance):
                yield ReadStreamResponse(image=ref)
                sent += 1

            logger.info(
                "ReadStream response: content_len=%d, images=%d",
                len(result.content),
                sent,
            )

    def ListEngines(self, request, context):
        overrides = dict(getattr(request, "config_overrides", None) or {})
        engines_data = registry.list_engines(overrides=overrides or None)
        engines = [
            ParserEngineInfo(
                name=e["name"],
                description=e["description"],
                file_types=e["file_types"],
                available=e.get("available", True),
                unavailable_reason=e.get("unavailable_reason", ""),
            )
            for e in engines_data
        ]
        return ListEnginesResponse(engines=engines)

    def GetRuntimeInfo(self, request, context):
        """Expose safe parser identity for a remote materialization adapter.

        This deliberately calls `_runtime_info`, not `config.dump_config`
        directly: network/secret/deployment settings never cross this RPC.
        """
        parsing_config, library_versions, image_tag = _runtime_info()
        return RuntimeInfoResponse(
            parsing_config=parsing_config,
            library_versions=library_versions,
            image_tag=image_tag,
        )


def main():
    config.print_config()

    interceptors = [AuthInterceptor()]

    server = grpc.server(
        futures.ThreadPoolExecutor(max_workers=CONFIG.grpc_max_workers),
        options=[
            ("grpc.max_send_message_length", CONFIG.grpc_max_file_size_mb),
            ("grpc.max_receive_message_length", CONFIG.grpc_max_file_size_mb),
        ],
        interceptors=interceptors,
    )

    docreader_pb2_grpc.add_DocReaderServicer_to_server(DocReaderServicer(), server)

    health_servicer = HealthServicer()
    health_pb2_grpc.add_HealthServicer_to_server(health_servicer, server)

    try:
        tls_credentials = load_tls_credentials()
    except TLSConfigError as e:
        logger.error("Refusing to start: %s", e)
        sys.exit(1)

    if tls_credentials:
        server.add_secure_port(f"[::]:{CONFIG.grpc_port}", tls_credentials)
        logger.info("Server starting on port %d with TLS", CONFIG.grpc_port)
    else:
        server.add_insecure_port(f"[::]:{CONFIG.grpc_port}")
        logger.warning(
            "Server starting on port %d WITHOUT TLS (insecure mode)", CONFIG.grpc_port
        )

    server.start()

    logger.info("Server started on port %d", CONFIG.grpc_port)
    logger.info("Server is ready to accept connections")

    try:
        server.wait_for_termination()
    except KeyboardInterrupt:
        logger.info("Received termination signal, shutting down server")
        server.stop(0)


if __name__ == "__main__":
    main()
