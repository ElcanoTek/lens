#!/usr/bin/env python3
# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.
"""
Management utility for Lens.
"""

import argparse
import asyncio
from pathlib import Path

from config import config
from progress_tracker import ProgressTracker


def setup_environment():
    """Setup the environment and configuration files."""
    print("🔧 Setting up environment...")

    # Copy example files if they don't exist
    if not Path(".env").exists():
        if Path(".env.example").exists():
            print("Creating .env from .env.example")
            with open(".env.example", "r") as src, open(".env", "w") as dst:
                dst.write(src.read())
        else:
            print("Creating basic .env file")
            with open(".env", "w") as f:
                f.write("# Add your API keys here\n")
                f.write("OPENROUTER_API_KEY=\n")

    if not Path("config.json").exists():
        if Path("config.json.example").exists():
            print("Creating config.json from config.json.example")
            with open("config.json.example", "r") as src, open("config.json", "w") as dst:
                dst.write(src.read())

    print("✅ Environment setup complete!")
    print("📝 Please edit .env file with your API keys before running the script.")


def show_progress():
    """Show current progress."""
    tracker = ProgressTracker(config.PROGRESS_FILE_PATH)
    tracker.print_summary()

    # Show recent activity
    processed_domains = tracker.get_processed_domains()
    if processed_domains:
        print("\n📋 Recent activity (last 10 domains):")
        for domain in processed_domains[-10:]:
            domain_data = tracker.progress_data["processed_domains"][domain]
            status = domain_data["status"]
            timestamp = domain_data["timestamp"]
            print(f"   {domain} - {status} ({timestamp})")


def reset_progress():
    """Reset progress tracking."""
    if Path(config.PROGRESS_FILE_PATH).exists():
        response = input(
            f"Are you sure you want to reset progress? This will delete {config.PROGRESS_FILE_PATH} (y/N): "
        )
        if response.lower() == "y":
            Path(config.PROGRESS_FILE_PATH).unlink()
            print("✅ Progress reset successfully")
        else:
            print("❌ Progress reset cancelled")
    else:
        print("ℹ️  No progress file found")


async def reset_failed():
    """Reset failed domains for retry."""
    tracker = ProgressTracker(config.PROGRESS_FILE_PATH)
    failed_domains = tracker.get_failed_domains()

    if not failed_domains:
        print("ℹ️  No failed domains found")
        return

    print(f"Found {len(failed_domains)} failed domains:")
    for domain in failed_domains[:10]:  # Show first 10
        print(f"   {domain}")

    if len(failed_domains) > 10:
        print(f"   ... and {len(failed_domains) - 10} more")

    response = input(f"Reset {len(failed_domains)} failed domains for retry? (y/N): ")
    if response.lower() == "y":
        await tracker.reset_failed_domains()
        print("✅ Failed domains reset successfully")
    else:
        print("❌ Reset cancelled")


def validate_config():
    """Validate configuration and API keys."""
    print("🔍 Validating configuration...")

    # Check required environment variables based on enabled integrations
    missing_vars = []

    if not getattr(config, "OPENROUTER_API_KEY", None):
        missing_vars.append("OPENROUTER_API_KEY")

    if missing_vars:
        print("❌ Missing required environment variables:")
        for var in missing_vars:
            print(f"   {var}")
        print("Please update your .env file with the required credentials")
        return False

    # Check input file
    if not Path(config.INPUT_CSV_PATH).exists():
        print(f"❌ Input file not found: {config.INPUT_CSV_PATH}")
        return False

    print("✅ Configuration validation passed")
    return True


def create_sample_input():
    """Create a sample input CSV file."""
    sample_data = """Domain
example.com
google.com
cnn.com
amazon.com
github.com"""

    with open("sample_input.csv", "w") as f:
        f.write(sample_data)

    print("✅ Created sample_input.csv")
    print("You can use this as a template for your input file")


def show_stats():
    """Show detailed statistics."""
    tracker = ProgressTracker(config.PROGRESS_FILE_PATH)
    summary = tracker.get_summary()

    print("📊 Detailed Statistics:")
    print(f"   Total domains: {summary['total_domains']}")
    print(f"   Processed: {summary['processed']}")
    print(f"   Successful: {summary['successful']}")
    print(f"   Errors: {summary['errors']}")
    print(f"   Remaining: {summary['remaining']}")
    print(f"   Completion: {summary['completion_percentage']:.1f}%")

    if "elapsed_time" in summary:
        print(f"   Elapsed time: {summary['elapsed_time']}")

    if "estimated_time_remaining" in summary:
        print(f"   Estimated remaining: {summary['estimated_time_remaining']}")

    # Show error breakdown if any
    failed_domains = tracker.get_failed_domains()
    if failed_domains:
        print(f"\n❌ Failed domains ({len(failed_domains)}):")
        for domain in failed_domains[:5]:  # Show first 5
            domain_data = tracker.progress_data["processed_domains"][domain]
            error_msg = domain_data.get("error_message", "Unknown error")
            print(f"   {domain}: {error_msg}")

        if len(failed_domains) > 5:
            print(f"   ... and {len(failed_domains) - 5} more")


def main():
    parser = argparse.ArgumentParser(description="Manage a Lens installation")
    parser.add_argument(
        "command",
        choices=[
            "setup",
            "progress",
            "reset",
            "reset-failed",
            "validate",
            "sample",
            "stats",
            "help",
        ],
        help="Command to execute",
    )

    args = parser.parse_args()

    if args.command == "setup":
        setup_environment()
    elif args.command == "progress":
        show_progress()
    elif args.command == "reset":
        reset_progress()
    elif args.command == "reset-failed":
        asyncio.run(reset_failed())
    elif args.command == "validate":
        validate_config()
    elif args.command == "sample":
        create_sample_input()
    elif args.command == "stats":
        show_stats()
    elif args.command == "help":
        print("Available commands:")
        print("  setup       - Setup environment and config files")
        print("  progress    - Show current progress")
        print("  reset       - Reset all progress")
        print("  reset-failed- Reset failed domains for retry")
        print("  validate    - Validate configuration")
        print("  sample      - Create sample input file")
        print("  stats       - Show detailed statistics")


if __name__ == "__main__":
    main()
