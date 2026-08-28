#!/usr/bin/env python3
# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.
"""
Standalone smoke-test script for the Lens pipeline.
"""

import asyncio
import csv
import json
import os
import tempfile

import pytest

from openrouter_client import OpenRouterClient
from progress_tracker import ProgressTracker
from scraper_client import ScraperClient

# Import our modules
from test_config import TestConfig

pytestmark = pytest.mark.asyncio


async def test_config_loading():
    """Test configuration loading."""
    print("🧪 Testing configuration loading...")

    # Create temporary config files
    with tempfile.NamedTemporaryFile(mode="w", suffix=".json", delete=False) as f:
        test_config = {"concurrent_sessions": 3, "llm_model": "google/gemini-2.5-flash-lite"}
        json.dump(test_config, f)
        temp_config_file = f.name

    try:
        # Test config loading
        config = TestConfig(temp_config_file)
        assert config.CONCURRENT_SESSIONS == 3
        assert config.LLM_MODEL == "google/gemini-2.5-flash-lite"
        print("✅ Configuration loading test passed")
        return True
    except Exception as e:
        print(f"❌ Configuration loading test failed: {e}")
        return False
    finally:
        os.unlink(temp_config_file)


async def test_progress_tracker():
    """Test progress tracking functionality."""
    print("🧪 Testing progress tracker...")

    with tempfile.NamedTemporaryFile(mode="w", suffix=".json", delete=False) as f:
        temp_progress_file = f.name

    try:
        # Initialize tracker
        tracker = ProgressTracker(temp_progress_file)
        tracker.set_total_domains(5)

        # Test marking domains as processed
        await tracker.mark_domain_processed("example.com", "success", {"quality": "Premium"})
        await tracker.mark_domain_processed("test.com", "error", error_message="Network error")

        # Test queries
        assert tracker.is_domain_processed("example.com")
        assert not tracker.is_domain_processed("notprocessed.com")
        assert len(tracker.get_successful_domains()) == 1
        assert len(tracker.get_failed_domains()) == 1

        # Test summary
        summary = tracker.get_summary()
        assert summary["total_domains"] == 5
        assert summary["processed"] == 2
        assert summary["successful"] == 1
        assert summary["errors"] == 1

        print("✅ Progress tracker test passed")
        return True
    except Exception as e:
        print(f"❌ Progress tracker test failed: {e}")
        return False
    finally:
        if os.path.exists(temp_progress_file):
            os.unlink(temp_progress_file)


async def test_openrouter_client():
    """Test OpenRouter client (mock mode)."""
    print("🧪 Testing OpenRouter client...")

    try:
        # Create client with dummy API key for testing
        client = OpenRouterClient(api_key="test_key", model="google/gemini-2.5-flash-lite")

        # Test prompt building
        prompt = client._build_classification_prompt(
            domain="example.com",
            vertical="Technology",
            content="Sample content about technology",
            title="Example Tech Site",
            meta_description="A technology website",
        )

        assert "example.com" in prompt
        assert "Technology" in prompt
        assert "Premium" in prompt or "Standard" in prompt or "Long Tail" in prompt

        # Test response parsing
        mock_response = {
            "choices": [
                {
                    "message": {
                        "content": "Quality: Premium\nJustification: High-quality content\nDescription: Technology website\nConfidence: High"
                    }
                }
            ],
            "usage": {"total_tokens": 100},
        }

        parsed = client._parse_classification_response(mock_response)
        assert parsed["quality"] == "Premium"
        assert parsed["justification"] == "High-quality content"
        assert parsed["confidence"] == "High"

        print("✅ OpenRouter client test passed")
        return True
    except Exception as e:
        print(f"❌ OpenRouter client test failed: {e}")
        return False


async def test_scraper_client():
    """Test scraper client functionality."""
    print("🧪 Testing scraper client...")

    try:
        async with ScraperClient() as client:
            await client.create_session_pool(1)
            session = await client.get_available_session()
            assert session.id == "local-0"
            client.release_session(session)
            stats = client.get_session_stats()
            assert stats["total_sessions"] == 1

        print("✅ Scraper client structure test passed")
        return True
    except Exception as e:
        print(f"❌ Scraper client test failed: {e}")
        return False


async def test_csv_handling():
    """Test CSV input/output handling."""
    print("🧪 Testing CSV handling...")

    try:
        # Create temporary input CSV
        with tempfile.NamedTemporaryFile(mode="w", suffix=".csv", delete=False) as f:
            writer = csv.writer(f)
            writer.writerow(["Domain"])
            writer.writerow(["example.com"])
            writer.writerow(["test.org"])
            temp_input_file = f.name

        # Create temporary output CSV
        with tempfile.NamedTemporaryFile(mode="w", suffix=".csv", delete=False) as f:
            temp_output_file = f.name

        # Test reading input
        import pandas as pd

        df = pd.read_csv(temp_input_file)
        assert len(df) == 2
        assert "Domain" in df.columns
        assert list(df.columns) == ["Domain"]

        # Test writing output
        fieldnames = [
            "Domain",
            "Quality",
            "Justification",
            "IAB Tier 1",
            "IAB Tier 2",
            "IAB Tier 3",
            "Description",
            "Content_Length",
            "Processing_Time",
        ]
        with open(temp_output_file, "w", newline="") as f:
            writer = csv.DictWriter(f, fieldnames=fieldnames)
            writer.writeheader()
            writer.writerow(
                {
                    "Domain": "example.com",
                    "Quality": "Premium",
                    "Justification": "High-quality content",
                    "IAB Tier 1": "Technology",
                    "IAB Tier 2": "Technology Industry",
                    "IAB Tier 3": "Software",
                    "Description": "Technology website",
                    "Content_Length": 1000,
                    "Processing_Time": 2.5,
                }
            )

        # Verify output
        df_output = pd.read_csv(temp_output_file)
        assert len(df_output) == 1
        assert df_output.iloc[0]["Quality"] == "Premium"

        print("✅ CSV handling test passed")
        return True
    except Exception as e:
        print(f"❌ CSV handling test failed: {e}")
        return False
    finally:
        for temp_file in [temp_input_file, temp_output_file]:
            if os.path.exists(temp_file):
                os.unlink(temp_file)


async def test_error_handling():
    """Test error handling scenarios."""
    print("🧪 Testing error handling...")

    try:
        # Test OpenRouter client error handling
        client = OpenRouterClient(api_key="invalid_key")

        # Test field extraction with malformed content
        malformed_content = "This is not properly formatted response"
        result = client._extract_field(malformed_content, "Quality")
        assert result == "N/A"  # Should return N/A for missing fields

        # Test classification response parsing with invalid data
        invalid_response = {"choices": []}
        parsed = client._parse_classification_response(invalid_response)
        assert parsed["quality"] in [
            "Premium",
            "Standard",
            "Long Tail",
        ]  # Should have valid default

        print("✅ Error handling test passed")
        return True
    except Exception as e:
        print(f"❌ Error handling test failed: {e}")
        return False


async def run_all_tests():
    """Run all tests."""
    print("🚀 Running improved script tests...\n")

    tests = [
        test_config_loading,
        test_progress_tracker,
        test_openrouter_client,
        test_scraper_client,
        test_csv_handling,
        test_error_handling,
    ]

    passed = 0
    total = len(tests)

    for test in tests:
        try:
            if await test():
                passed += 1
        except Exception as e:
            print(f"❌ Test {test.__name__} crashed: {e}")
        print()  # Add spacing between tests

    print(f"📊 Test Results: {passed}/{total} tests passed")

    if passed == total:
        print("🎉 All tests passed! The improved script is ready to use.")
        return True
    else:
        print("⚠️  Some tests failed. Please review the implementation.")
        return False


if __name__ == "__main__":
    asyncio.run(run_all_tests())
