# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.

"""Comprehensive tests for CTV (Connected TV) categorization functionality.

This test suite covers:
- CTVWorkItem dataclass integrity
- CTV input file detection
- CTV column detection
- CTV work item parsing
- OpenRouter CTV methods and prompts
- Response parsing including error handling
- Mock tests for the two-step pipeline
"""

import json

# Add parent directory to path for imports
import sys
from pathlib import Path
from unittest.mock import AsyncMock, MagicMock, patch

import pandas as pd
import pytest

sys.path.insert(0, str(Path(__file__).parent.parent))

from input_detector import (
    detect_ctv_columns,
    is_ctv_input_file,
    parse_ctv_work_items,
)
from shared_types import ContentType, CTVWorkItem, WorkItem


class TestCTVWorkItem:
    """Tests for the CTVWorkItem dataclass."""

    def test_ctv_work_item_creation_minimal(self):
        """Test creating CTVWorkItem with minimal required fields."""
        item = CTVWorkItem(app_name="Netflix")
        assert item.app_name == "Netflix"
        assert item.bundle_id is None
        assert item.platform is None
        assert item.url is None
        assert item.original_row is None

    def test_ctv_work_item_creation_full(self):
        """Test creating CTVWorkItem with all fields."""
        item = CTVWorkItem(
            app_name="Hulu",
            bundle_id="com.hulu.plus",
            platform="Roku",
            url="https://hulu.com",
            original_row={"App_Name": "Hulu", "Platform": "Roku"},
        )
        assert item.app_name == "Hulu"
        assert item.bundle_id == "com.hulu.plus"
        assert item.platform == "Roku"
        assert item.url == "https://hulu.com"
        assert item.original_row == {"App_Name": "Hulu", "Platform": "Roku"}

    def test_ctv_work_item_identifier_with_bundle_id(self):
        """Test identifier property prefers bundle_id."""
        item = CTVWorkItem(app_name="ESPN", bundle_id="com.espn.score")
        assert item.identifier == "com.espn.score"

    def test_ctv_work_item_identifier_without_bundle_id(self):
        """Test identifier property falls back to app_name."""
        item = CTVWorkItem(app_name="ESPN")
        assert item.identifier == "ESPN"

    def test_ctv_work_item_display_name(self):
        """Test display_name returns app_name."""
        item = CTVWorkItem(app_name="Peacock")
        assert item.display_name == "Peacock"


class TestCTVPlatformDetection:
    """Tests for CTV platform detection from bundle ID patterns."""

    def test_detect_roku_numeric_bundle_id(self):
        """Test detection of Roku from numeric-only bundle ID."""
        from ctv_processor import CTVProcessor

        assert CTVProcessor._detect_ctv_platform("54092") == "Roku"
        assert CTVProcessor._detect_ctv_platform("12345") == "Roku"
        assert CTVProcessor._detect_ctv_platform("999999") == "Roku"

    def test_detect_fire_tv_b0_prefix(self):
        """Test detection of Fire TV from B0/b0 prefix."""
        from ctv_processor import CTVProcessor

        assert CTVProcessor._detect_ctv_platform("B091RFCS5V") == "Fire TV"
        assert CTVProcessor._detect_ctv_platform("B0ABCD1234") == "Fire TV"
        assert CTVProcessor._detect_ctv_platform("b0lowercase") == "Fire TV"

    def test_detect_android_tv_com_prefix(self):
        """Test detection of Android TV from com. prefix."""
        from ctv_processor import CTVProcessor

        assert CTVProcessor._detect_ctv_platform("com.foxsports.videogo") == "Android TV"
        assert CTVProcessor._detect_ctv_platform("com.netflix.app") == "Android TV"
        assert CTVProcessor._detect_ctv_platform("COM.UPPERCASE.APP") == "Android TV"

    def test_detect_vizio_prefix(self):
        """Test detection of Vizio from vizio. prefix."""
        from ctv_processor import CTVProcessor

        assert CTVProcessor._detect_ctv_platform("vizio.app.streaming") == "Vizio"
        assert CTVProcessor._detect_ctv_platform("VIZIO.UPPERCASE") == "Vizio"

    def test_detect_apple_tv_prefix(self):
        """Test detection of Apple TV from tv. prefix."""
        from ctv_processor import CTVProcessor

        assert CTVProcessor._detect_ctv_platform("tv.streaming.app") == "Apple TV"
        assert CTVProcessor._detect_ctv_platform("TV.UPPERCASE.APP") == "Apple TV"

    def test_detect_samsung_tv_g_prefix_numeric(self):
        """Test detection of Samsung TV from G followed by numbers."""
        from ctv_processor import CTVProcessor

        assert CTVProcessor._detect_ctv_platform("G00009197740") == "Samsung TV"
        assert CTVProcessor._detect_ctv_platform("G12345") == "Samsung TV"
        assert CTVProcessor._detect_ctv_platform("g99999") == "Samsung TV"

    def test_detect_xbox_9_prefix_alphanumeric(self):
        """Test detection of Xbox from 9 prefix with alphanumeric."""
        from ctv_processor import CTVProcessor

        assert CTVProcessor._detect_ctv_platform("9wzdncrfjv7w") == "Xbox"
        assert CTVProcessor._detect_ctv_platform("9ABCDEF123") == "Xbox"

    def test_detect_unknown_for_unmatched_patterns(self):
        """Test Unknown is returned for unmatched patterns."""
        from ctv_processor import CTVProcessor

        assert CTVProcessor._detect_ctv_platform("randomstring") == "Unknown"
        assert CTVProcessor._detect_ctv_platform("abc123xyz") == "Unknown"
        assert CTVProcessor._detect_ctv_platform("netflix") == "Unknown"

    def test_detect_unknown_for_empty_bundle_id(self):
        """Test Unknown is returned for empty or None bundle ID."""
        from ctv_processor import CTVProcessor

        assert CTVProcessor._detect_ctv_platform("") == "Unknown"
        assert CTVProcessor._detect_ctv_platform("   ") == "Unknown"
        assert CTVProcessor._detect_ctv_platform(None) == "Unknown"

    def test_detect_handles_whitespace(self):
        """Test platform detection handles leading/trailing whitespace."""
        from ctv_processor import CTVProcessor

        assert CTVProcessor._detect_ctv_platform("  54092  ") == "Roku"
        assert CTVProcessor._detect_ctv_platform(" B091RFCS5V ") == "Fire TV"
        assert CTVProcessor._detect_ctv_platform("  com.test.app  ") == "Android TV"

    def test_detect_g_prefix_not_numeric_is_unknown(self):
        """Test G prefix without numeric suffix returns Unknown."""
        from ctv_processor import CTVProcessor

        assert CTVProcessor._detect_ctv_platform("Google") == "Unknown"
        assert CTVProcessor._detect_ctv_platform("G") == "Unknown"
        assert CTVProcessor._detect_ctv_platform("GabcDEF") == "Unknown"

    def test_detect_9_prefix_non_alphanumeric_is_unknown(self):
        """Test 9 prefix with non-alphanumeric chars returns Unknown."""
        from ctv_processor import CTVProcessor

        assert CTVProcessor._detect_ctv_platform("9abc-def") == "Unknown"
        assert CTVProcessor._detect_ctv_platform("9test.app") == "Unknown"


class TestContentTypeCTV:
    """Tests for CTV_APP ContentType."""

    def test_content_type_ctv_app_exists(self):
        """Test that CTV_APP content type exists."""
        assert hasattr(ContentType, "CTV_APP")
        assert ContentType.CTV_APP.value == "ctv_app"

    def test_work_item_is_ctv_app(self):
        """Test WorkItem.is_ctv_app property."""
        item = WorkItem(
            identifier="Netflix",
            content_type=ContentType.CTV_APP,
            original_value="Netflix",
        )
        assert item.is_ctv_app is True
        assert item.is_website is False
        assert item.is_ios_app is False
        assert item.is_android_app is False

    def test_work_item_from_ctv_app(self):
        """Test WorkItem.from_ctv_app class method."""
        item = WorkItem.from_ctv_app("Disney+", "Disney+ Streaming")
        assert item.identifier == "Disney+"
        assert item.content_type == ContentType.CTV_APP
        assert item.original_value == "Disney+ Streaming"


class TestCTVInputFileDetection:
    """Tests for CTV input file detection."""

    def test_detect_ctv_file_with_app_name_column(self):
        """Test detection with app_name and bundle_id columns (both CTV indicators)."""
        df = pd.DataFrame(
            {
                "app_name": ["Netflix", "Hulu"],
                "bundle_id": ["com.netflix", "com.hulu"],  # Added strong CTV indicator
                "platform": ["Roku", "Fire TV"],
            }
        )
        assert is_ctv_input_file(df) is True

    def test_detect_ctv_file_with_bundle_id_column(self):
        """Test detection with bundle_id column."""
        df = pd.DataFrame(
            {
                "bundle_id": ["com.netflix", "com.hulu"],
                "name": ["Netflix", "Hulu"],
            }
        )
        assert is_ctv_input_file(df) is True

    def test_detect_ctv_file_with_ctv_in_column_name(self):
        """Test detection with 'ctv' in column name."""
        df = pd.DataFrame(
            {
                "ctv_channel": ["ESPN", "CNN"],
                "viewers": [1000000, 500000],
            }
        )
        assert is_ctv_input_file(df) is True

    def test_detect_ctv_file_with_channel_name_column(self):
        """Test detection with channel_name column."""
        df = pd.DataFrame(
            {
                "channel_name": ["FOX News", "MSNBC"],
                "network": ["FOX", "NBC"],
            }
        )
        assert is_ctv_input_file(df) is True

    def test_detect_non_ctv_file_with_domain_column(self):
        """Test non-CTV file with domain column."""
        df = pd.DataFrame(
            {
                "domain": ["example.com", "test.org"],
                "category": ["News", "Sports"],
            }
        )
        assert is_ctv_input_file(df) is False

    def test_detect_non_ctv_file_with_url_column(self):
        """Test non-CTV file with URL column."""
        df = pd.DataFrame(
            {
                "pageURL": ["https://example.com", "https://test.org"],
                "status": ["active", "inactive"],
            }
        )
        assert is_ctv_input_file(df) is False


class TestCTVColumnDetection:
    """Tests for CTV column detection."""

    def test_detect_standard_columns(self):
        """Test detection of standard CTV columns."""
        df = pd.DataFrame(
            {
                "app_name": ["Netflix"],
                "bundle_id": ["com.netflix"],
                "platform": ["Roku"],
                "url": ["https://netflix.com"],
            }
        )
        columns = detect_ctv_columns(df)
        assert columns["app_name"] == "app_name"
        assert columns["bundle_id"] == "bundle_id"
        assert columns["platform"] == "platform"
        assert columns["url"] == "url"

    def test_detect_alternative_column_names(self):
        """Test detection with alternative column names."""
        df = pd.DataFrame(
            {
                "channel_name": ["ESPN"],
                "roku_id": ["12345"],
                "device": ["Roku"],
                "website": ["https://espn.com"],
            }
        )
        columns = detect_ctv_columns(df)
        assert columns["app_name"] == "channel_name"
        assert columns["bundle_id"] == "roku_id"
        assert columns["platform"] == "device"
        assert columns["url"] == "website"

    def test_detect_case_insensitive_columns(self):
        """Test case-insensitive column detection."""
        df = pd.DataFrame(
            {
                "App_Name": ["HBO Max"],
                "BUNDLE_ID": ["com.hbo.max"],
                "Platform": ["Apple TV"],
            }
        )
        columns = detect_ctv_columns(df)
        assert columns["app_name"] == "App_Name"
        assert columns["bundle_id"] == "BUNDLE_ID"
        assert columns["platform"] == "Platform"

    def test_detect_missing_columns(self):
        """Test handling of missing columns."""
        df = pd.DataFrame(
            {
                "app_name": ["Netflix"],
                "random_column": ["value"],
            }
        )
        columns = detect_ctv_columns(df)
        assert columns["app_name"] == "app_name"
        assert columns["bundle_id"] is None
        assert columns["platform"] is None
        assert columns["url"] is None


class TestCTVWorkItemParsing:
    """Tests for parsing CTV work items from DataFrames."""

    def test_parse_basic_work_items(self):
        """Test parsing basic CTV work items."""
        df = pd.DataFrame(
            {
                "app_name": ["Netflix", "Hulu", "Disney+"],
                "platform": ["Roku", "Fire TV", "Apple TV"],
            }
        )
        items = parse_ctv_work_items(df)
        assert len(items) == 3
        assert items[0].app_name == "Netflix"
        assert items[0].platform == "Roku"
        assert items[1].app_name == "Hulu"
        assert items[2].app_name == "Disney+"

    def test_parse_full_work_items(self):
        """Test parsing work items with all fields."""
        df = pd.DataFrame(
            {
                "app_name": ["ESPN"],
                "bundle_id": ["com.espn.score"],
                "platform": ["Roku"],
                "url": ["https://espn.com"],
            }
        )
        items = parse_ctv_work_items(df)
        assert len(items) == 1
        assert items[0].app_name == "ESPN"
        assert items[0].bundle_id == "com.espn.score"
        assert items[0].platform == "Roku"
        assert items[0].url == "https://espn.com"
        assert items[0].original_row is not None

    def test_parse_skips_empty_app_names(self):
        """Test that empty app names are skipped."""
        df = pd.DataFrame(
            {
                "app_name": ["Netflix", "", "   ", "Hulu"],
                "platform": ["Roku", "Fire TV", "Apple TV", "Roku"],
            }
        )
        items = parse_ctv_work_items(df)
        assert len(items) == 2
        assert items[0].app_name == "Netflix"
        assert items[1].app_name == "Hulu"

    def test_parse_with_missing_app_name_column(self):
        """Test parsing when no app_name column exists uses first column."""
        df = pd.DataFrame(
            {
                "name": ["Netflix", "Hulu"],
                "platform": ["Roku", "Fire TV"],
            }
        )
        items = parse_ctv_work_items(df)
        assert len(items) == 2
        assert items[0].app_name == "Netflix"

    def test_parse_strips_whitespace(self):
        """Test that whitespace is stripped from values."""
        df = pd.DataFrame(
            {
                "app_name": ["  Netflix  ", " Hulu "],
                "platform": [" Roku ", "  Fire TV  "],
            }
        )
        items = parse_ctv_work_items(df)
        assert items[0].app_name == "Netflix"
        assert items[0].platform == "Roku"
        assert items[1].app_name == "Hulu"
        assert items[1].platform == "Fire TV"


class TestCTVPromptGeneration:
    """Tests for CTV prompt generation in OpenRouterClient."""

    @pytest.fixture
    def mock_client(self):
        """Create a mock OpenRouterClient for testing."""
        from openrouter_client import OpenRouterClient

        with patch.object(OpenRouterClient, "__init__", lambda x, **kwargs: None):
            client = OpenRouterClient()
            client._taxonomy_cache = {"entertainment": ["Entertainment"]}
            return client

    def test_build_ctv_research_prompt_minimal(self, mock_client):
        """Test research prompt with minimal info."""
        prompt = mock_client._build_ctv_research_prompt(
            app_name="Netflix",
            bundle_id="",
            platform="",
            url="",
        )
        assert "Netflix" in prompt
        assert "CTV APP INFORMATION" in prompt
        assert "Content Overview" in prompt
        assert "Target Audience" in prompt

    def test_build_ctv_research_prompt_full(self, mock_client):
        """Test research prompt with full info."""
        prompt = mock_client._build_ctv_research_prompt(
            app_name="ESPN",
            bundle_id="com.espn.score",
            platform="Roku",
            url="https://espn.com",
        )
        assert "ESPN" in prompt
        assert "com.espn.score" in prompt
        assert "Roku" in prompt
        assert "https://espn.com" in prompt

    def test_build_ctv_classification_prompt(self, mock_client):
        """Test classification prompt generation."""
        # Mock the taxonomy method
        mock_client._get_taxonomy_list_for_prompt = MagicMock(
            return_value="Entertainment\nSports\nNews"
        )

        prompt = mock_client._build_ctv_classification_prompt(
            app_name="Hulu",
            research_content="Hulu is a streaming service...",
            bundle_id="com.hulu.plus",
            platform="Fire TV",
            url="https://hulu.com",
        )
        assert "Hulu" in prompt
        assert "Hulu is a streaming service" in prompt
        assert "QUALITY TIERS FOR CTV APPS" in prompt
        assert "Premium" in prompt
        assert "Standard" in prompt
        assert "Long Tail" in prompt


class TestCTVResponseParsing:
    """Tests for CTV response parsing."""

    @pytest.fixture
    def mock_client(self):
        """Create a mock OpenRouterClient for testing."""
        from openrouter_client import OpenRouterClient

        with patch.object(OpenRouterClient, "__init__", lambda x, **kwargs: None):
            client = OpenRouterClient()
            client._taxonomy_cache = {}
            client._format_vertical_with_taxonomy = MagicMock(
                return_value=["Entertainment", "Television", ""]
            )
            return client

    def test_parse_valid_ctv_response(self, mock_client):
        """Test parsing a valid CTV classification response."""
        response_data = {
            "choices": [
                {
                    "message": {
                        "tool_calls": [
                            {
                                "function": {
                                    "arguments": json.dumps(
                                        {
                                            "quality": "Premium",
                                            "justification": "Major streaming service with original content.",
                                            "vertical": "Entertainment",
                                            "description": "A subscription streaming service.",
                                            "target_audience": "General entertainment seekers",
                                            "content_type": "On-Demand Streaming",
                                            "confidence": "High",
                                            "language": "English",
                                            "political_leaning": "Non-Political",
                                            "audience_size": "XL",
                                        }
                                    )
                                }
                            }
                        ]
                    }
                }
            ]
        }
        result = mock_client._parse_ctv_classification_response(response_data)
        assert result["quality"] == "Premium"
        assert result["justification"] == "Major streaming service with original content."
        assert result["content_type"] == "On-Demand Streaming"
        assert result["confidence"] == "High"

    def test_parse_ctv_response_invalid_quality(self, mock_client):
        """Test parsing response with invalid quality value."""
        response_data = {
            "choices": [
                {
                    "message": {
                        "tool_calls": [
                            {
                                "function": {
                                    "arguments": json.dumps(
                                        {
                                            "quality": "InvalidQuality",
                                            "justification": "Test",
                                            "vertical": "Entertainment",
                                            "description": "Test",
                                            "target_audience": "Test",
                                            "content_type": "On-Demand Streaming",
                                            "confidence": "High",
                                            "language": "English",
                                            "political_leaning": "Non-Political",
                                            "audience_size": "XL",
                                        }
                                    )
                                }
                            }
                        ]
                    }
                }
            ]
        }
        result = mock_client._parse_ctv_classification_response(response_data)
        assert result["quality"] == "Standard"  # Should default to Standard

    def test_parse_ctv_response_invalid_content_type(self, mock_client):
        """Test parsing response with invalid content_type value."""
        response_data = {
            "choices": [
                {
                    "message": {
                        "tool_calls": [
                            {
                                "function": {
                                    "arguments": json.dumps(
                                        {
                                            "quality": "Premium",
                                            "justification": "Test",
                                            "vertical": "Entertainment",
                                            "description": "Test",
                                            "target_audience": "Test",
                                            "content_type": "InvalidContentType",
                                            "confidence": "High",
                                            "language": "English",
                                            "political_leaning": "Non-Political",
                                            "audience_size": "XL",
                                        }
                                    )
                                }
                            }
                        ]
                    }
                }
            ]
        }
        result = mock_client._parse_ctv_classification_response(response_data)
        assert result["content_type"] == "Mixed Content"  # Should default to Mixed Content

    def test_parse_ctv_response_no_tool_calls(self, mock_client):
        """A response without tool_calls or inline JSON raises so the caller
        can retry with a larger budget or record a Failed row — it must not
        be swallowed as a silent Standard default."""
        response_data = {"choices": [{"message": {"content": "Some text response"}}]}
        with pytest.raises(ValueError):
            mock_client._parse_ctv_classification_response(response_data)

    def test_parse_ctv_response_malformed_json(self, mock_client):
        """Malformed tool_calls JSON raises instead of returning a default."""
        response_data = {
            "choices": [
                {"message": {"tool_calls": [{"function": {"arguments": "not valid json {"}}]}}
            ]
        }
        with pytest.raises(ValueError):
            mock_client._parse_ctv_classification_response(response_data)

    def test_parse_ctv_response_missing_keys(self, mock_client):
        """A response with no usable message is surfaced as a parse failure so
        the caller writes a Failed row instead of a silent Standard default."""
        response_data = {"choices": []}
        with pytest.raises(ValueError):
            mock_client._parse_ctv_classification_response(response_data)

    def test_parse_ctv_response_message_none(self, mock_client):
        """Reasoning models on OpenRouter have been observed to return
        choices[0].message=null when their output overflows max_tokens.
        That should escalate to the caller, not be swallowed as Standard."""
        response_data = {"choices": [{"message": None}], "usage": None}
        with pytest.raises(ValueError):
            mock_client._parse_ctv_classification_response(response_data)


class TestCTVClassificationSchema:
    """Tests for CTV classification schema."""

    def test_ctv_schema_structure(self):
        """Test CTV classification schema has correct structure."""
        from openrouter_client import OpenRouterClient

        schema = OpenRouterClient._CTV_CLASSIFICATION_SCHEMA

        assert schema["type"] == "function"
        assert "function" in schema
        assert schema["function"]["name"] == "classify_ctv_app"

        params = schema["function"]["parameters"]
        assert params["type"] == "object"

        properties = params["properties"]
        assert "quality" in properties
        assert "justification" in properties
        assert "vertical" in properties
        assert "description" in properties
        assert "target_audience" in properties
        assert "content_type" in properties
        assert "confidence" in properties
        assert "language" in properties
        assert "political_leaning" in properties
        assert "audience_size" in properties

    def test_ctv_schema_quality_enum(self):
        """Test quality enum values in schema."""
        from openrouter_client import OpenRouterClient

        schema = OpenRouterClient._CTV_CLASSIFICATION_SCHEMA
        quality_enum = schema["function"]["parameters"]["properties"]["quality"]["enum"]
        assert "Premium" in quality_enum
        assert "Standard" in quality_enum
        assert "Long Tail" in quality_enum

    def test_ctv_schema_content_type_enum(self):
        """Test content_type enum values in schema."""
        from openrouter_client import OpenRouterClient

        schema = OpenRouterClient._CTV_CLASSIFICATION_SCHEMA
        content_type_enum = schema["function"]["parameters"]["properties"]["content_type"]["enum"]
        assert "Live TV" in content_type_enum
        assert "On-Demand Streaming" in content_type_enum
        assert "Live Sports" in content_type_enum
        assert "News" in content_type_enum
        assert "Mixed Content" in content_type_enum


class TestCTVProcessorIntegration:
    """Integration tests for CTVProcessor using mocks."""

    @pytest.fixture
    def mock_dependencies(self):
        """Create mock dependencies for CTVProcessor."""
        progress_tracker = MagicMock()
        progress_tracker.mark_domain_processed = AsyncMock()

        openrouter_client = MagicMock()
        openrouter_client.research_ctv_app = AsyncMock()
        openrouter_client.classify_ctv_app = AsyncMock()

        reporter = MagicMock()
        reporter.log_attempt = AsyncMock()

        results_writer = MagicMock()
        results_file = MagicMock()

        return {
            "progress_tracker": progress_tracker,
            "openrouter_client": openrouter_client,
            "reporter": reporter,
            "results_writer": results_writer,
            "results_file": results_file,
        }

    @pytest.mark.asyncio
    async def test_process_ctv_app_success(self, mock_dependencies):
        """Test successful CTV app processing."""
        from ctv_processor import CTVProcessor

        # Setup mocks
        mock_dependencies["openrouter_client"].research_ctv_app.return_value = {
            "success": True,
            "research_content": "Netflix is a major streaming service...",
            "processing_time": 1.0,
            "tokens_used": 500,
            "model": "perplexity/sonar-pro",
        }

        mock_dependencies["openrouter_client"].classify_ctv_app.return_value = {
            "success": True,
            "quality": "Premium",
            "justification": "Major streaming service",
            "vertical": "Entertainment",
            "vertical_tier_1": "Entertainment",
            "vertical_tier_2": "Television",
            "vertical_tier_3": "",
            "description": "Streaming video service",
            "target_audience": "General audience",
            "content_type": "On-Demand Streaming",
            "confidence": "High",
            "language": "English",
            "political_leaning": "Non-Political",
            "audience_size": "XL",
            "processing_time": 0.5,
            "tokens_used": 300,
            "model": "~google/gemini-flash-latest",
        }

        processor = CTVProcessor(**mock_dependencies)
        item = CTVWorkItem(app_name="Netflix", platform="Roku")

        await processor.process_ctv_app(item)

        # Verify research was called
        mock_dependencies["openrouter_client"].research_ctv_app.assert_called_once()

        # Verify classification was called
        mock_dependencies["openrouter_client"].classify_ctv_app.assert_called_once()

        # Verify result was written
        mock_dependencies["results_writer"].writerow.assert_called_once()

        # Verify success was logged
        mock_dependencies["reporter"].log_attempt.assert_called()
        call_args = mock_dependencies["reporter"].log_attempt.call_args
        assert call_args.kwargs["ok"] is True

    @pytest.mark.asyncio
    async def test_process_ctv_app_research_failure(self, mock_dependencies):
        """Test CTV app processing with research failure."""
        from ctv_processor import CTVProcessor

        # Setup research to fail
        mock_dependencies["openrouter_client"].research_ctv_app.return_value = {
            "success": False,
            "error": "API rate limit exceeded",
            "research_content": "",
            "processing_time": 0.5,
            "model": "perplexity/sonar-pro",
        }

        processor = CTVProcessor(**mock_dependencies)
        item = CTVWorkItem(app_name="Netflix", platform="Roku")

        await processor.process_ctv_app(item)

        # Verify research was called
        mock_dependencies["openrouter_client"].research_ctv_app.assert_called_once()

        # Verify classification was NOT called
        mock_dependencies["openrouter_client"].classify_ctv_app.assert_not_called()

        # Verify failure was recorded
        mock_dependencies["results_writer"].writerow.assert_called_once()
        written_record = mock_dependencies["results_writer"].writerow.call_args[0][0]
        assert written_record["Quality"] == "Failed"
        assert "Research failed" in written_record["Justification"]

    @pytest.mark.asyncio
    async def test_process_ctv_app_classification_failure(self, mock_dependencies):
        """Test CTV app processing with classification failure."""
        from ctv_processor import CTVProcessor

        # Setup research to succeed but classification to fail
        mock_dependencies["openrouter_client"].research_ctv_app.return_value = {
            "success": True,
            "research_content": "Netflix is a streaming service...",
            "processing_time": 1.0,
            "tokens_used": 500,
            "model": "perplexity/sonar-pro",
        }

        mock_dependencies["openrouter_client"].classify_ctv_app.return_value = {
            "success": False,
            "error": "Model timeout",
            "processing_time": 30.0,
            "model": "~google/gemini-flash-latest",
        }

        processor = CTVProcessor(**mock_dependencies)
        item = CTVWorkItem(app_name="Netflix", platform="Roku")

        await processor.process_ctv_app(item)

        # Verify both steps were called
        mock_dependencies["openrouter_client"].research_ctv_app.assert_called_once()
        mock_dependencies["openrouter_client"].classify_ctv_app.assert_called_once()

        # Verify failure was recorded with correct error message
        mock_dependencies["results_writer"].writerow.assert_called_once()
        written_record = mock_dependencies["results_writer"].writerow.call_args[0][0]
        assert written_record["Quality"] == "Failed"
        assert "Classification failed" in written_record["Justification"]


class TestStripMarkdown:
    """Tests for the _strip_markdown() method."""

    def test_strip_markdown_empty_string(self):
        """Test handling of empty string."""
        from ctv_processor import CTVProcessor

        assert CTVProcessor._strip_markdown("") == ""
        assert CTVProcessor._strip_markdown(None) is None

    def test_strip_markdown_headers(self):
        """Test removal of Markdown headers."""
        from ctv_processor import CTVProcessor

        assert CTVProcessor._strip_markdown("# Header 1") == "Header 1"
        assert CTVProcessor._strip_markdown("## Header 2") == "Header 2"
        assert CTVProcessor._strip_markdown("### Header 3") == "Header 3"
        assert CTVProcessor._strip_markdown("#### Header 4") == "Header 4"
        # Multi-line with headers
        result = CTVProcessor._strip_markdown("# Title\nSome content\n## Subtitle")
        assert "# " not in result
        assert "## " not in result
        assert "Title" in result
        assert "Subtitle" in result

    def test_strip_markdown_bold(self):
        """Test removal of bold markers."""
        from ctv_processor import CTVProcessor

        assert CTVProcessor._strip_markdown("**bold text**") == "bold text"
        assert CTVProcessor._strip_markdown("__bold text__") == "bold text"
        assert CTVProcessor._strip_markdown("Some **bold** word") == "Some bold word"

    def test_strip_markdown_italic(self):
        """Test removal of italic markers."""
        from ctv_processor import CTVProcessor

        assert CTVProcessor._strip_markdown("*italic text*") == "italic text"
        assert CTVProcessor._strip_markdown("_italic text_") == "italic text"
        assert CTVProcessor._strip_markdown("Some *italic* word") == "Some italic word"

    def test_strip_markdown_inline_code(self):
        """Test removal of inline code backticks."""
        from ctv_processor import CTVProcessor

        assert CTVProcessor._strip_markdown("`code`") == "code"
        assert CTVProcessor._strip_markdown("Use `print()` function") == "Use print() function"

    def test_strip_markdown_links(self):
        """Test removal of link syntax, keeping link text."""
        from ctv_processor import CTVProcessor

        assert CTVProcessor._strip_markdown("[Google](https://google.com)") == "Google"
        assert (
            CTVProcessor._strip_markdown("Visit [our site](https://example.com) today")
            == "Visit our site today"
        )
        assert CTVProcessor._strip_markdown("[Link](url) and [Another](url2)") == "Link and Another"

    def test_strip_markdown_citations(self):
        """Test removal of citation markers."""
        from ctv_processor import CTVProcessor

        assert CTVProcessor._strip_markdown("Text with citation[1]") == "Text with citation"
        assert (
            CTVProcessor._strip_markdown("Multiple[1] citations[2] here[3]")
            == "Multiple citations here"
        )
        assert CTVProcessor._strip_markdown("Reference [12] number") == "Reference  number"

    def test_strip_markdown_multiple_newlines(self):
        """Test collapsing multiple newlines."""
        from ctv_processor import CTVProcessor

        assert CTVProcessor._strip_markdown("Line 1\n\n\n\nLine 2") == "Line 1\n\nLine 2"
        assert CTVProcessor._strip_markdown("A\n\n\n\n\nB\n\n\n\nC") == "A\n\nB\n\nC"

    def test_strip_markdown_combined(self):
        """Test stripping multiple Markdown elements combined."""
        from ctv_processor import CTVProcessor

        input_text = """# Research Summary

**Netflix** is a major streaming platform[1]. It offers *premium content* and has a
[comprehensive library](https://netflix.com) of movies and TV shows.

## Key Features

The service includes `4K streaming` and multiple profiles[2][3].
"""
        result = CTVProcessor._strip_markdown(input_text)

        # Headers removed
        assert "# Research" not in result
        assert "## Key" not in result
        # Bold removed
        assert "**" not in result
        assert "Netflix" in result
        # Italic removed
        assert "*premium" not in result
        assert "premium content" in result
        # Links cleaned
        assert "[comprehensive library]" not in result
        assert "comprehensive library" in result
        # Citations removed
        assert "[1]" not in result
        assert "[2]" not in result
        assert "[3]" not in result
        # Code backticks removed
        assert "`" not in result
        assert "4K streaming" in result

    def test_strip_markdown_plain_text_unchanged(self):
        """Test that plain text without Markdown remains unchanged."""
        from ctv_processor import CTVProcessor

        plain = "This is plain text with no Markdown formatting."
        assert CTVProcessor._strip_markdown(plain) == plain

    def test_strip_markdown_preserves_content(self):
        """Test that actual content is preserved after stripping."""
        from ctv_processor import CTVProcessor

        input_text = "# Netflix Overview\n**Netflix** offers streaming content."
        result = CTVProcessor._strip_markdown(input_text)
        assert "Netflix Overview" in result
        assert "Netflix" in result
        assert "offers streaming content" in result


class TestTruncateResearch:
    """Tests for the _truncate_research() method with sentence boundary detection."""

    def test_truncate_short_text(self):
        """Test that short text is not truncated."""
        from ctv_processor import CTVProcessor

        text = "This is short text."
        assert CTVProcessor._truncate_research(text) == text

    def test_truncate_at_sentence_boundary(self):
        """Test truncation at sentence boundary."""
        from ctv_processor import CTVProcessor

        # Create text with clear sentence boundaries
        text = "First sentence. " * 50 + "Second sentence. " * 50  # > 1000 chars
        result = CTVProcessor._truncate_research(text, max_length=100)
        # Should end with a period (sentence boundary)
        assert result.endswith(".")
        assert "..." not in result
        assert len(result) <= 100

    def test_truncate_finds_last_sentence(self):
        """Test that truncation finds the last sentence boundary."""
        from ctv_processor import CTVProcessor

        text = "Short. Medium sentence here. This is a much longer sentence that goes on and on. Final."
        result = CTVProcessor._truncate_research(text, max_length=60)
        # Should cut at a sentence boundary
        assert result.endswith(".") or result.endswith("...")

    def test_truncate_fallback_to_ellipsis(self):
        """Test fallback to ellipsis when no good sentence boundary found."""
        from ctv_processor import CTVProcessor

        # Text with no sentence boundaries in first part
        text = "A" * 1500  # No periods at all
        result = CTVProcessor._truncate_research(text, max_length=1000)
        assert result.endswith("...")

    def test_truncate_respects_exclamation(self):
        """Test that exclamation marks are recognized as sentence boundaries."""
        from ctv_processor import CTVProcessor

        text = "Wow! " * 100  # > 1000 chars
        result = CTVProcessor._truncate_research(text, max_length=100)
        # Should end with ! (sentence boundary)
        assert "!" in result

    def test_truncate_respects_question_mark(self):
        """Test that question marks are recognized as sentence boundaries."""
        from ctv_processor import CTVProcessor

        text = "How does this work? " * 100  # > 1000 chars
        result = CTVProcessor._truncate_research(text, max_length=100)
        # Should end with ? (sentence boundary)
        assert "?" in result


class TestCTVNewsRouting:
    """Tests for CTV news routing logic.

    These tests verify that the _apply_ctv_routing function correctly:
    1. Detects news apps based on content_type, research phrases, and app names
    2. Routes news apps to appropriate IAB tiers (since IAB lacks Tier-1 "News")
    3. Corrects political_leaning discrepancies for Politics vertical
    """

    @pytest.fixture
    def mock_client(self):
        """Create a mock OpenRouterClient for testing routing logic."""
        from openrouter_client import OpenRouterClient

        with patch.object(OpenRouterClient, "__init__", lambda x, **kwargs: None):
            client = OpenRouterClient()
            client._taxonomy_cache = {}
            return client

    def test_news_content_type_routes_to_politics_civic_affairs(self, mock_client):
        """Test: content_type='News' + generic news research routes to Politics > Civic Affairs.

        When an app has content_type='News' and generic news research content,
        it should be routed to Politics > Civic Affairs with political_leaning
        set to 'Center' if it was 'Non-Political'.
        """
        result = {
            "content_type": "News",
            "vertical": "Entertainment > Television",
            "vertical_tier_1": "Entertainment",
            "vertical_tier_2": "Television",
            "vertical_tier_3": "",
            "political_leaning": "Non-Political",
            "justification": "Local news station with regional coverage.",
        }
        research_content = "This is a local news station providing local news coverage, breaking news, and headlines for the community."

        routed = mock_client._apply_ctv_routing("KARE 11 News", research_content, result)

        assert routed["vertical_tier_1"] == "Politics"
        assert routed["vertical_tier_2"] == "Civic affairs"
        assert routed["vertical_tier_3"] == ""
        assert routed["vertical"] == "Politics > Civic affairs"
        assert routed["political_leaning"] == "Center"
        assert "(post-routed for CTV news)" in routed["justification"]
        assert "Political_Leaning set to Center due to Politics vertical" in routed["justification"]

    def test_election_keywords_route_to_politics_elections(self, mock_client):
        """Test: election keywords route to Politics > Elections with leaning Center.

        When research content contains election-related keywords (election, ballot,
        campaign, voting, etc.), the app should be routed to Politics > Elections.
        """
        result = {
            "content_type": "News",
            "vertical": "Entertainment",
            "vertical_tier_1": "Entertainment",
            "vertical_tier_2": "",
            "vertical_tier_3": "",
            "political_leaning": "Non-Political",
            "justification": "Political news coverage.",
        }
        research_content = "This news app covers election coverage, election results, and presidential election news with ballot tracking and electoral college analysis."

        routed = mock_client._apply_ctv_routing("Election Central", research_content, result)

        assert routed["vertical_tier_1"] == "Politics"
        assert routed["vertical_tier_2"] == "Elections"
        assert routed["vertical_tier_3"] == ""
        assert routed["vertical"] == "Politics > Elections"
        assert routed["political_leaning"] == "Center"
        assert "(post-routed for CTV election news)" in routed["justification"]

    def test_weather_focused_news_routes_to_science_weather(self, mock_client):
        """Test: weather-heavy 'news' routes to Science > Weather.

        Weather apps should be routed to Science > Weather and political_leaning
        should remain 'Non-Political' since weather is not political content.
        """
        result = {
            "content_type": "News",
            "vertical": "Entertainment",
            "vertical_tier_1": "Entertainment",
            "vertical_tier_2": "",
            "vertical_tier_3": "",
            "political_leaning": "Non-Political",
            "justification": "Weather service with forecasts.",
        }
        research_content = "Weather Channel provides weather forecast, weather radar, storm tracker, severe weather alerts, and meteorologist reports. Track temperature and hurricane updates."

        routed = mock_client._apply_ctv_routing("The Weather Channel", research_content, result)

        assert routed["vertical_tier_1"] == "Science"
        assert routed["vertical_tier_2"] == "Weather"
        assert routed["vertical_tier_3"] == ""
        assert routed["vertical"] == "Science > Weather"
        assert routed["political_leaning"] == "Non-Political"  # Weather stays Non-Political
        assert "(post-routed for CTV weather app)" in routed["justification"]

    def test_crime_focused_news_routes_to_crime(self, mock_client):
        """Test: crime-focused news routes to Crime vertical."""
        result = {
            "content_type": "News",
            "vertical": "Entertainment",
            "vertical_tier_1": "Entertainment",
            "vertical_tier_2": "",
            "vertical_tier_3": "",
            "political_leaning": "Non-Political",
            "justification": "Crime reporting.",
        }
        research_content = "True crime news reporting. Covers crime reports, police investigations, court cases, criminal arrests, and crime blotter updates."

        routed = mock_client._apply_ctv_routing("Crime Watch Daily", research_content, result)

        assert routed["vertical_tier_1"] == "Crime"
        assert routed["vertical_tier_2"] == ""
        assert routed["vertical"] == "Crime"
        assert routed["political_leaning"] == "Non-Political"  # Crime stays Non-Political
        assert "(post-routed for CTV crime app)" in routed["justification"]

    def test_disaster_focused_news_routes_to_disasters(self, mock_client):
        """Test: disaster/emergency news routes to Disasters vertical."""
        result = {
            "content_type": "News",
            "vertical": "Entertainment",
            "vertical_tier_1": "Entertainment",
            "vertical_tier_2": "",
            "vertical_tier_3": "",
            "political_leaning": "Non-Political",
            "justification": "Emergency coverage.",
        }
        research_content = "Emergency alert system providing disaster updates, evacuation notices, wildfire tracking, earthquake reports, and emergency management information."

        routed = mock_client._apply_ctv_routing("Emergency Alert", research_content, result)

        assert routed["vertical_tier_1"] == "Disasters"
        assert routed["vertical_tier_2"] == ""
        assert routed["vertical"] == "Disasters"
        assert routed["political_leaning"] == "Non-Political"  # Disasters stays Non-Political
        assert "(post-routed for CTV disaster app)" in routed["justification"]

    def test_major_news_network_name_triggers_routing(self, mock_client):
        """Test: major news network names in app_name trigger routing."""
        result = {
            "content_type": "Live TV",  # Not explicitly "News"
            "vertical": "Entertainment > Television",
            "vertical_tier_1": "Entertainment",
            "vertical_tier_2": "Television",
            "vertical_tier_3": "",
            "political_leaning": "Non-Political",
            "justification": "Cable news network.",
        }
        research_content = "Cable television network providing 24-hour coverage of current events and breaking stories."

        # Test CNN
        routed = mock_client._apply_ctv_routing("CNN", research_content, result)
        assert routed["vertical_tier_1"] == "Politics"
        assert routed["political_leaning"] == "Center"

    def test_entertainment_app_not_routed_as_news(self, mock_client):
        """Test: primarily entertainment apps are NOT routed as news.

        Apps that are streaming services with only incidental news mentions
        should not be routed through the news logic.
        """
        result = {
            "content_type": "On-Demand Streaming",
            "vertical": "Entertainment > Television",
            "vertical_tier_1": "Entertainment",
            "vertical_tier_2": "Television",
            "vertical_tier_3": "",
            "political_leaning": "Non-Political",
            "justification": "Streaming service with original content.",
        }
        research_content = "Netflix is a streaming service offering movies, scripted series, comedy series, drama series, reality shows, and Netflix original programming. The platform offers video on demand and binge watch capabilities. Some news documentaries available."

        routed = mock_client._apply_ctv_routing("Netflix", research_content, result)

        # Should remain Entertainment, not routed to Politics
        assert routed["vertical_tier_1"] == "Entertainment"
        assert routed["vertical_tier_2"] == "Television"
        assert routed["political_leaning"] == "Non-Political"

    def test_non_news_app_unchanged(self, mock_client):
        """Test: non-news apps are not modified by routing."""
        result = {
            "content_type": "Live Sports",
            "vertical": "Sports > Football",
            "vertical_tier_1": "Sports",
            "vertical_tier_2": "Football",
            "vertical_tier_3": "",
            "political_leaning": "Non-Political",
            "justification": "Sports streaming app.",
        }
        research_content = "ESPN provides live sports coverage including football, basketball, baseball, and other athletic events."

        routed = mock_client._apply_ctv_routing("ESPN", research_content, result)

        # Should remain unchanged
        assert routed["vertical_tier_1"] == "Sports"
        assert routed["vertical_tier_2"] == "Football"
        assert routed["political_leaning"] == "Non-Political"

    def test_explicit_political_leaning_preserved(self, mock_client):
        """Test: explicit non-Non-Political leaning is preserved."""
        result = {
            "content_type": "News",
            "vertical": "Entertainment",
            "vertical_tier_1": "Entertainment",
            "vertical_tier_2": "",
            "vertical_tier_3": "",
            "political_leaning": "Right",  # Explicit leaning from model
            "justification": "Conservative news network.",
        }
        research_content = "Conservative news network providing right-leaning news coverage and political commentary."

        routed = mock_client._apply_ctv_routing("Conservative News", research_content, result)

        # Should route to Politics but preserve the explicit "Right" leaning
        assert routed["vertical_tier_1"] == "Politics"
        assert routed["political_leaning"] == "Right"  # Preserved, not overwritten to Center

    def test_is_ctv_news_app_detection(self, mock_client):
        """Test the _is_ctv_news_app detection helper."""
        # Test content_type detection
        result_news = {"content_type": "News"}
        assert mock_client._is_ctv_news_app("TestApp", "Some content", result_news) is True

        # Test news phrases in research
        result_other = {"content_type": "Live TV"}
        assert (
            mock_client._is_ctv_news_app(
                "TestApp", "breaking news and headlines coverage", result_other
            )
            is True
        )

        # Test major network name
        assert mock_client._is_ctv_news_app("CBS News", "general content", result_other) is True

        # Test non-news
        result_sports = {"content_type": "Live Sports"}
        assert (
            mock_client._is_ctv_news_app("ESPN", "sports coverage and games", result_sports)
            is False
        )

    def test_is_primarily_entertainment_helper(self, mock_client):
        """Test the _is_primarily_entertainment helper."""
        # Entertainment-heavy content
        entertainment_content = "Netflix offers movies, scripted series, comedy series, drama series, TV shows, streaming content, video on demand, and original programming."
        assert mock_client._is_primarily_entertainment(entertainment_content) is True

        # News-heavy content
        news_content = "Breaking news, headlines, current events, newscast, news coverage, journalism, and news programming."
        assert mock_client._is_primarily_entertainment(news_content) is False

    def test_general_news_with_weather_mention_not_routed_to_weather(self, mock_client):
        """Test: general news apps that mention weather do NOT route to Science > Weather.

        This is a regression test - previously, any mention of weather terms like
        'forecast', 'hurricane', 'temperature' would cause news apps to be incorrectly
        routed to Science > Weather. Only dedicated weather apps should be routed there.
        """
        result = {
            "content_type": "News",
            "vertical": "Entertainment",
            "vertical_tier_1": "Entertainment",
            "vertical_tier_2": "",
            "vertical_tier_3": "",
            "political_leaning": "Non-Political",
            "justification": "Local news station.",
        }
        # This research content mentions weather but is clearly a general news station
        research_content = """Local news station providing breaking news, headlines, weather forecast,
        traffic updates, local events coverage, sports scores, and community news. The station covers
        hurricane season updates, temperature reports, and storm tracking along with election coverage,
        crime reports, and human interest stories."""

        routed = mock_client._apply_ctv_routing("KARE 11 News", research_content, result)

        # Should route to Politics > Civic Affairs (general news), NOT Science > Weather
        assert routed["vertical_tier_1"] == "Politics"
        assert routed["vertical_tier_2"] == "Civic affairs"
        assert routed["vertical"] == "Politics > Civic affairs"
        assert routed["political_leaning"] == "Center"

    def test_local_news_station_routes_to_civic_affairs(self, mock_client):
        """Test: local news stations route to Politics > Civic Affairs."""
        result = {
            "content_type": "News",
            "vertical": "Entertainment",
            "vertical_tier_1": "Entertainment",
            "vertical_tier_2": "",
            "vertical_tier_3": "",
            "political_leaning": "Non-Political",
            "justification": "Regional news coverage.",
        }
        research_content = """WCCO is a CBS affiliate providing local news, weather, sports,
        and community coverage for the Minneapolis-St. Paul metro area. Features breaking news,
        investigative reports, and daily newscasts."""

        routed = mock_client._apply_ctv_routing("WCCO News", research_content, result)

        # Local news should route to Politics > Civic Affairs
        assert routed["vertical_tier_1"] == "Politics"
        assert routed["vertical_tier_2"] == "Civic affairs"
        assert routed["political_leaning"] == "Center"


class TestCTVConfiguration:
    """Tests for CTV configuration options."""

    def test_ctv_config_exists(self):
        """Test that CTV config options exist."""
        from config import config

        assert hasattr(config, "CTV_RESEARCH_MODEL")
        assert hasattr(config, "CTV_RESEARCH_TEMPERATURE")
        assert hasattr(config, "CTV_RESEARCH_MAX_TOKENS")
        assert hasattr(config, "CTV_CLASSIFICATION_MODEL")
        assert hasattr(config, "CTV_CLASSIFICATION_TEMPERATURE")
        assert hasattr(config, "CTV_CLASSIFICATION_MAX_TOKENS")
        assert hasattr(config, "CTV_MAX_CONCURRENT")

    def test_ctv_csv_fieldnames_exist(self):
        """Test that CTV CSV fieldnames are defined."""
        from config import config

        assert hasattr(config, "CTV_CSV_FIELDNAMES")
        assert "App_Name" in config.CTV_CSV_FIELDNAMES
        assert "Bundle_ID" in config.CTV_CSV_FIELDNAMES
        assert "Platform" in config.CTV_CSV_FIELDNAMES
        assert "Network_Affiliation" in config.CTV_CSV_FIELDNAMES
        assert "Quality" in config.CTV_CSV_FIELDNAMES
        assert "Research_Summary" in config.CTV_CSV_FIELDNAMES


if __name__ == "__main__":
    pytest.main([__file__, "-v"])
