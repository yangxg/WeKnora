"""ImageRef v2 and RuntimeInfo contract tests.

These use no parser binary or network. They pin the protocol facts before a
consumer sees them: exact Markdown target, 1-based page/source provenance, and
an intentionally narrow RuntimeInfo disclosure surface.
"""

from __future__ import annotations

import base64
import importlib.metadata
import unittest
from unittest import mock

from docreader.main import (
    DocReaderServicer,
    _iter_image_refs,
    _resolve_images,
    _runtime_info,
)
from docreader.proto.docreader_pb2 import RuntimeInfoRequest
from docreader import main as main_module


class ImageRefV2Tests(unittest.TestCase):
    def test_unary_image_ref_carries_exact_parser_provenance(self) -> None:
        images = {"images/report_page_2.jpg": base64.b64encode(b"jpg").decode()}
        provenance = {
            "images/report_page_2.jpg": {
                "page_number": 2,
                "source_type": "scanned_page",
                "markdown_target": "images/report_page_2.jpg",
            }
        }

        _dir, refs = _resolve_images(images, "test", provenance=provenance)

        self.assertEqual(1, len(refs))
        ref = refs[0]
        self.assertEqual(2, ref.page_number)
        self.assertEqual("scanned_page", ref.source_type)
        self.assertEqual("images/report_page_2.jpg", ref.markdown_target)

    def test_streaming_image_ref_carries_exact_parser_provenance(self) -> None:
        images = {"images/report_p3_img1.jpg": base64.b64encode(b"jpg").decode()}
        provenance = {
            "images/report_p3_img1.jpg": {
                "page_number": 3,
                "source_type": "embedded_image",
                "markdown_target": "images/report_p3_img1.jpg",
            }
        }

        refs = list(_iter_image_refs(images, provenance=provenance))

        self.assertEqual(1, len(refs))
        self.assertEqual(3, refs[0].page_number)
        self.assertEqual("embedded_image", refs[0].source_type)
        self.assertEqual("images/report_p3_img1.jpg", refs[0].markdown_target)

    def test_missing_v2_provenance_remains_v1_empty_without_inference(self) -> None:
        images = {"images/opaque-uuid.png": base64.b64encode(b"png").decode()}

        _dir, refs = _resolve_images(images, "test")

        self.assertEqual(0, refs[0].page_number)
        self.assertEqual("", refs[0].source_type)
        self.assertEqual("", refs[0].markdown_target)


class RuntimeInfoTests(unittest.TestCase):
    def test_runtime_info_only_exports_output_relevant_allowlist(self) -> None:
        effective = {
            "DOCREADER_PDF_RENDER_DPI": 200,
            "DOCREADER_DOCX_MAX_PAGES": 0,
            "DOCREADER_EXTERNAL_HTTP_PROXY": "http://private.proxy",
            "DOCREADER_ODL_HYBRID_URL": "http://internal-odl:5002",
            "DOCREADER_GRPC_PORT": 50051,
            "DOCREADER_IMAGE_OUTPUT_DIR": "/tmp/docreader",
        }
        def version(name: str) -> str:
            if name == "pypdf":
                return "1.0"
            raise importlib.metadata.PackageNotFoundError(name)

        with mock.patch("docreader.main.config.dump_config", return_value=effective), mock.patch(
            "importlib.metadata.version", side_effect=version
        ):
            config, versions, tag = _runtime_info()

        self.assertEqual({"DOCREADER_PDF_RENDER_DPI": "200", "DOCREADER_DOCX_MAX_PAGES": "0"}, config)
        self.assertEqual({"pypdf": "1.0"}, versions)
        self.assertIsInstance(tag, str)
        rendered = repr((config, versions, tag))
        self.assertNotIn("private.proxy", rendered)
        self.assertNotIn("internal-odl", rendered)
        self.assertNotIn("50051", rendered)
        self.assertNotIn("/tmp/docreader", rendered)

    def test_grpc_runtime_info_returns_only_safe_identity(self) -> None:
        with mock.patch(
            "docreader.main._runtime_info",
            return_value=({"DOCREADER_PDF_RENDER_DPI": "200"}, {"pypdf": "6.14.2"}, "v2-test"),
        ):
            response = DocReaderServicer().GetRuntimeInfo(RuntimeInfoRequest(), None)

        self.assertEqual({"DOCREADER_PDF_RENDER_DPI": "200"}, dict(response.parsing_config))
        self.assertEqual({"pypdf": "6.14.2"}, dict(response.library_versions))
        self.assertEqual("v2-test", response.image_tag)

    def test_runtime_info_uses_explicit_image_tag_not_container_identity(self) -> None:
        with mock.patch.object(main_module, "_RUNTIME_IMAGE_TAG", "pinned-v2"):
            _config, _versions, tag = _runtime_info()
        self.assertEqual("pinned-v2", tag)
