# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.

"""Tests for the input_detector module.

These tests verify that the content type classifier correctly distinguishes
between websites/URLs, Android apps, and iOS apps.
"""

import sys
from pathlib import Path

import pandas as pd

PROJECT_ROOT = Path(__file__).resolve().parents[1]
if str(PROJECT_ROOT) not in sys.path:
    sys.path.insert(0, str(PROJECT_ROOT))

from input_detector import (
    _is_url_or_website,
    _is_valid_android_package,
    detect_content_type,
    detect_input_column,
    detect_type_column,
    parse_content_type_hint,
)
from shared_types import ContentType


class TestWebsiteDetection:
    """Tests for website/URL detection."""

    def test_full_urls_with_protocol(self):
        """URLs with protocol should always be detected as websites."""
        urls = [
            "https://example.com",
            "http://example.com",
            "https://www.example.com",
            "http://www.example.com/path/to/page",
            "https://example.com:8080",
            "https://example.com/path?query=1&foo=bar",
            "https://sub.domain.example.com",
            "ftp://files.example.com",
            "//example.com/resource",
        ]
        for url in urls:
            assert detect_content_type(url) == ContentType.WEBSITE, f"Failed for: {url}"

    def test_domains_with_common_tlds(self):
        """Domains ending with common TLDs should be detected as websites."""
        domains = [
            "example.com",
            "example.org",
            "example.net",
            "example.io",
            "example.co",
            "example.edu",
            "example.gov",
            "example.info",
            "example.biz",
            "example.me",
            "example.tv",
            "example.xyz",
            "example.app",
            "example.dev",
            "example.tech",
            "example.ai",
        ]
        for domain in domains:
            assert detect_content_type(domain) == ContentType.WEBSITE, f"Failed for: {domain}"

    def test_detect_input_column_finds_domain_header(self):
        df = pd.DataFrame({"Domain": ["example.com", "github.com"]})

        assert detect_input_column(df) == "Domain"

    def test_domains_with_country_tlds(self):
        """Domains ending with country TLDs should be detected as websites."""
        domains = [
            "example.uk",
            "example.de",
            "example.fr",
            "example.jp",
            "example.cn",
            "example.br",
            "example.in",
            "example.au",
            "example.ca",
            "example.ru",
        ]
        for domain in domains:
            assert detect_content_type(domain) == ContentType.WEBSITE, f"Failed for: {domain}"

    def test_domains_with_compound_tlds(self):
        """Domains with compound TLDs should be detected as websites."""
        domains = [
            "example.co.uk",
            "example.com.au",
            "example.com.br",
            "example.co.jp",
            "example.co.kr",
            "example.co.nz",
        ]
        for domain in domains:
            assert detect_content_type(domain) == ContentType.WEBSITE, f"Failed for: {domain}"

    def test_subdomains_ending_with_tlds(self):
        """Subdomains ending with TLDs should be detected as websites."""
        domains = [
            "api.example.com",
            "www.example.org",
            "blog.example.net",
            "app.example.io",
            "mail.example.co.uk",
            "cdn.static.example.com",
        ]
        for domain in domains:
            assert detect_content_type(domain) == ContentType.WEBSITE, f"Failed for: {domain}"

    def test_domains_starting_with_tld_like_prefixes(self):
        """Domains that start with TLD-like prefixes but end with TLDs should be websites.

        This is the KEY test case - these were being misclassified as Android apps!
        """
        domains = [
            "io.example.com",  # Starts with "io." but ends with ".com"
            "co.example.com",  # Starts with "co." but ends with ".com"
            "me.example.com",  # Starts with "me." but ends with ".com"
            "dev.example.org",  # Starts with "dev." but ends with ".org"
            "ai.startup.com",  # Starts with "ai." but ends with ".com"
            "app.company.io",  # Starts with "app." but ends with ".io"
            "net.something.com",  # Starts with "net." but ends with ".com"
            "org.website.net",  # Starts with "org." but ends with ".net"
            "com.company.io",  # Starts with "com." but ends with ".io"
        ]
        for domain in domains:
            assert detect_content_type(domain) == ContentType.WEBSITE, f"Failed for: {domain}"

    def test_www_prefix(self):
        """Domains with www prefix should be detected as websites."""
        domains = [
            "www.example.com",
            "www.example.org",
            "www.example.net",
        ]
        for domain in domains:
            assert detect_content_type(domain) == ContentType.WEBSITE, f"Failed for: {domain}"

    def test_urls_with_paths(self):
        """URLs with paths should be detected as websites."""
        urls = [
            "example.com/path",
            "example.com/path/to/page",
            "example.com/page.html",
            "example.com/api/v1/users",
        ]
        for url in urls:
            assert detect_content_type(url) == ContentType.WEBSITE, f"Failed for: {url}"

    def test_urls_with_query_strings(self):
        """URLs with query strings should be detected as websites."""
        urls = [
            "example.com?query=1",
            "example.com/page?id=123&name=test",
            "example.com/search?q=hello+world",
        ]
        for url in urls:
            assert detect_content_type(url) == ContentType.WEBSITE, f"Failed for: {url}"

    def test_urls_with_fragments(self):
        """URLs with fragment identifiers should be detected as websites."""
        urls = [
            "example.com#section",
            "example.com/page#top",
            "example.com/docs#api-reference",
        ]
        for url in urls:
            assert detect_content_type(url) == ContentType.WEBSITE, f"Failed for: {url}"

    def test_urls_with_port_numbers(self):
        """URLs with port numbers should be detected as websites."""
        urls = [
            "example.com:8080",
            "example.com:3000",
            "example.com:443",
            "localhost:8080",
            "localhost:3000/api",
        ]
        for url in urls:
            assert detect_content_type(url) == ContentType.WEBSITE, f"Failed for: {url}"

    def test_ip_addresses(self):
        """IP addresses should be detected as websites."""
        ips = [
            "192.168.1.1",
            "10.0.0.1",
            "127.0.0.1",
            "192.168.1.1:8080",
            "10.0.0.1/api",
        ]
        for ip in ips:
            assert detect_content_type(ip) == ContentType.WEBSITE, f"Failed for: {ip}"

    def test_localhost(self):
        """Localhost URLs should be detected as websites."""
        urls = [
            "localhost",
            "localhost:8080",
            "localhost/api",
            "localhost:3000/path",
        ]
        for url in urls:
            assert detect_content_type(url) == ContentType.WEBSITE, f"Failed for: {url}"

    def test_domains_with_hyphens(self):
        """Domains with hyphens should be detected as websites (hyphens are invalid in Java packages)."""
        domains = [
            "my-website.com",
            "test-site.org",
            "hello-world.net",
            "sub-domain.example.com",
            "api-v2.company.io",
        ]
        for domain in domains:
            assert detect_content_type(domain) == ContentType.WEBSITE, f"Failed for: {domain}"


class TestAndroidAppDetection:
    """Tests for Android app (package name) detection."""

    def test_standard_android_packages(self):
        """Standard Android package names should be detected as Android apps."""
        packages = [
            "com.facebook.katana",
            "com.instagram.android",
            "com.twitter.android",
            "com.google.android.apps.maps",
            "com.spotify.music",
            "com.netflix.mediaclient",
            "org.mozilla.firefox",
            "net.oneplus.launcher",
        ]
        for package in packages:
            assert detect_content_type(package) == ContentType.ANDROID_APP, f"Failed for: {package}"

    def test_android_packages_with_underscores(self):
        """Android packages with underscores should be detected correctly."""
        packages = [
            "com.example.my_app",
            "com.company.app_name",
            "org.project.module_one",
        ]
        for package in packages:
            assert detect_content_type(package) == ContentType.ANDROID_APP, f"Failed for: {package}"

    def test_android_packages_with_numbers(self):
        """Android packages with numbers should be detected correctly."""
        packages = [
            "com.example.app2",
            "com.company.v2app",
            "org.project.module1",
        ]
        for package in packages:
            assert detect_content_type(package) == ContentType.ANDROID_APP, f"Failed for: {package}"

    def test_android_packages_with_various_prefixes(self):
        """Android packages with various known prefixes should be detected."""
        packages = [
            "com.example.app",
            "net.company.product",
            "org.foundation.project",
            "io.startup.mobile",
            "co.company.app",
            "me.developer.app",
            "app.company.product",
            "dev.studio.game",
            "ai.company.assistant",
            "tv.company.streaming",
        ]
        for package in packages:
            assert detect_content_type(package) == ContentType.ANDROID_APP, f"Failed for: {package}"

    def test_packages_without_known_prefix_non_tld_leaf(self):
        """Real packages that do NOT start with a known reverse-DNS prefix.

        Their leaf label is an ordinary word (not a TLD), so they cannot be
        domains. Regression cases pulled from the apps master list that used to
        be misclassified as websites.
        """
        packages = [
            "tunein.player",
            "mobi.ifunny",
            "jigsaw.puzzle.game.banana",
            "kidultlovin.word.zen",
            "beetles.puzzle.solitaire",
        ]
        for package in packages:
            assert detect_content_type(package) == ContentType.ANDROID_APP, f"Failed for: {package}"

    def test_deeply_nested_packages_with_tld_leaf(self):
        """Packages whose leaf collides with a real TLD (.free/.games/.news) but
        that nest too deep (>=5 labels) to be a plausible domain -> Android."""
        packages = [
            "easy.sudoku.puzzle.solver.free",
            "ball.sort.puzzle.color.sorting.bubble.games",
            "classic.relax.happy.color.by.number.game",
            "park.master.car.parking.games",
        ]
        for package in packages:
            assert detect_content_type(package) == ContentType.ANDROID_APP, f"Failed for: {package}"


class TestAndroidAppNotDetected:
    """Tests for values that should NOT be detected as Android apps."""

    def test_two_part_domains_not_android(self):
        """Two-part domains ending with TLDs should not be detected as Android."""
        domains = [
            "example.com",
            "company.io",
            "startup.ai",
            "mysite.dev",
            "website.app",
        ]
        for domain in domains:
            assert detect_content_type(domain) == ContentType.WEBSITE, (
                f"Wrongly classified as Android: {domain}"
            )

    def test_domains_with_android_like_structure_but_tld_ending(self):
        """Domains that look like packages but end with TLDs should be websites."""
        domains = [
            "api.example.com",  # Could look like package but ends with .com
            "mobile.company.io",  # Could look like package but ends with .io
            "app.startup.dev",  # Could look like package but ends with .dev
            "client.service.app",  # Could look like package but ends with .app
        ]
        for domain in domains:
            assert detect_content_type(domain) == ContentType.WEBSITE, (
                f"Wrongly classified as Android: {domain}"
            )

    def test_shallow_tld_leaf_ambiguous_defaults_to_website(self):
        """Two-label strings whose leaf is itself a real TLD are genuinely
        ambiguous (package vs domain). We default to the safe website reading."""
        ambiguous = [
            "kik.android",  # .android is a real gTLD
            "flipboard.social",  # .social is a real gTLD
        ]
        for value in ambiguous:
            assert detect_content_type(value) == ContentType.WEBSITE, (
                f"Ambiguous shallow value should default to website: {value}"
            )

    def test_new_gtld_and_cctld_domains_stay_websites(self):
        """Shallow domains on newer gTLDs / ccTLDs (not in the hand-maintained
        list) must resolve as websites via the authoritative IANA TLD check,
        not get swept into Android by the prefix-less fallback."""
        domains = [
            "brand.shop",
            "example.store",
            "mysite.blog",
            "fullmeasure.news",
            "eltiempo.pe",
            "ojo.pe",
            "conectate.com.do",
            "pulse.com.gh",
        ]
        for domain in domains:
            assert detect_content_type(domain) == ContentType.WEBSITE, (
                f"Wrongly classified as Android: {domain}"
            )


class TestiOSAppDetection:
    """Tests for iOS app (App Store ID) detection."""

    def test_numeric_ids(self):
        """All-numeric values should be detected as iOS apps."""
        ids = [
            "123456789",
            "1",
            "999999999999",
            "284882215",  # Facebook
            "389801252",  # Instagram
        ]
        for app_id in ids:
            assert detect_content_type(app_id) == ContentType.IOS_APP, f"Failed for: {app_id}"

    def test_numeric_with_leading_zeros(self):
        """Numeric values with leading zeros should be detected as iOS apps."""
        ids = [
            "00123456",
            "0001",
        ]
        for app_id in ids:
            assert detect_content_type(app_id) == ContentType.IOS_APP, f"Failed for: {app_id}"


class TestEdgeCases:
    """Tests for edge cases and boundary conditions."""

    def test_empty_values(self):
        """Empty values should default to website."""
        assert detect_content_type("") == ContentType.WEBSITE
        assert detect_content_type("   ") == ContentType.WEBSITE
        assert detect_content_type(None) == ContentType.WEBSITE

    def test_whitespace_trimming(self):
        """Values with leading/trailing whitespace should be trimmed."""
        assert detect_content_type("  example.com  ") == ContentType.WEBSITE
        assert detect_content_type("  com.example.app  ") == ContentType.ANDROID_APP
        assert detect_content_type("  123456  ") == ContentType.IOS_APP

    def test_case_insensitivity(self):
        """Detection should be case-insensitive."""
        assert detect_content_type("EXAMPLE.COM") == ContentType.WEBSITE
        assert detect_content_type("COM.EXAMPLE.APP") == ContentType.ANDROID_APP
        assert detect_content_type("Example.Com") == ContentType.WEBSITE

    def test_special_url_characters(self):
        """URLs with special characters should be detected as websites."""
        urls = [
            "example.com/path?a=1&b=2",
            "example.com#section",
            "example.com/page.html",
            "user@example.com",  # Email-like
        ]
        for url in urls:
            assert detect_content_type(url) == ContentType.WEBSITE, f"Failed for: {url}"

    def test_invalid_package_characters(self):
        """Values with invalid package characters should not be Android."""
        values = [
            "com-example-app",  # Hyphens not allowed in packages
            "com.example/app",  # Slashes not allowed
            "com.example:app",  # Colons not allowed
            "com.example?app",  # Question marks not allowed
        ]
        for value in values:
            assert detect_content_type(value) == ContentType.WEBSITE, f"Wrongly classified: {value}"


class TestHelperFunctions:
    """Tests for helper functions."""

    def test_is_url_or_website_protocols(self):
        """Test _is_url_or_website with various protocols."""
        assert _is_url_or_website("https://example.com") is True
        assert _is_url_or_website("http://example.com") is True
        assert _is_url_or_website("ftp://files.example.com") is True
        assert _is_url_or_website("//example.com") is True

    def test_is_url_or_website_tld_endings(self):
        """Test _is_url_or_website with TLD endings."""
        assert _is_url_or_website("example.com") is True
        assert _is_url_or_website("example.org") is True
        assert _is_url_or_website("example.net") is True
        assert _is_url_or_website("example.io") is True

    def test_is_url_or_website_special_chars(self):
        """Test _is_url_or_website with URL special characters."""
        assert _is_url_or_website("example.com/path") is True
        assert _is_url_or_website("example.com?query=1") is True
        assert _is_url_or_website("example.com#fragment") is True

    def test_is_valid_android_package_valid(self):
        """Test _is_valid_android_package with valid packages."""
        assert _is_valid_android_package("com.example.app") is True
        assert _is_valid_android_package("org.mozilla.firefox") is True
        assert _is_valid_android_package("com.company.my_app") is True

    def test_is_valid_android_package_invalid(self):
        """Test _is_valid_android_package with invalid packages."""
        assert _is_valid_android_package("example.com") is True  # Valid format, just short
        assert _is_valid_android_package("com-example-app") is False  # Hyphens
        assert _is_valid_android_package("com.example/app") is False  # Slash
        assert _is_valid_android_package("com") is False  # Single segment
        assert _is_valid_android_package("") is False  # Empty


class TestRealWorldExamples:
    """Tests with real-world examples that might cause confusion."""

    def test_problematic_domains_not_misclassified(self):
        """Real domains that start with TLD-like prefixes should be websites."""
        domains = [
            # Real domains that start with common prefixes
            "io.github.com",
            "co.pinterest.com",
            "me.medium.com",
            "dev.to",  # Developer blogging platform
            "ai.google.com",
            "app.slack.com",
            # Subdomains of popular services
            "api.twitter.com",
            "cdn.example.com",
            "static.example.org",
        ]
        for domain in domains:
            result = detect_content_type(domain)
            assert result == ContentType.WEBSITE, f"'{domain}' misclassified as {result.value}"

    def test_real_android_packages(self):
        """Real Android package names should be classified correctly."""
        packages = [
            "com.whatsapp",
            "com.facebook.orca",
            "com.snapchat.android",
            "com.zhiliaoapp.musically",  # TikTok
            "com.amazon.mShop.android.shopping",
            "com.google.android.youtube",
            "com.microsoft.teams",
            "com.discord",
            "org.telegram.messenger",
            "com.Slack",
        ]
        for package in packages:
            result = detect_content_type(package)
            assert result == ContentType.ANDROID_APP, f"'{package}' misclassified as {result.value}"

    def test_real_ios_app_ids(self):
        """Real iOS App Store IDs should be classified correctly."""
        app_ids = [
            "284882215",  # Facebook
            "389801252",  # Instagram
            "333903271",  # Twitter/X
            "310633997",  # WhatsApp
            "835599320",  # TikTok
        ]
        for app_id in app_ids:
            result = detect_content_type(app_id)
            assert result == ContentType.IOS_APP, f"'{app_id}' misclassified as {result.value}"


class TestContentTypeHint:
    """Tests for the explicit per-row type-column override."""

    def test_parse_hint_recognized_values(self):
        assert parse_content_type_hint("IOS") == ContentType.IOS_APP
        assert parse_content_type_hint("iPhone") == ContentType.IOS_APP
        assert parse_content_type_hint("ANDROID") == ContentType.ANDROID_APP
        assert parse_content_type_hint("Google Play") == ContentType.ANDROID_APP
        assert parse_content_type_hint("  Website ") == ContentType.WEBSITE
        assert parse_content_type_hint("url") == ContentType.WEBSITE

    def test_parse_hint_unrecognized_or_empty(self):
        assert parse_content_type_hint("") is None
        assert parse_content_type_hint(None) is None
        assert parse_content_type_hint("news") is None
        # CTV is intentionally not mapped here (routed as a whole file elsewhere)
        assert parse_content_type_hint("CTV") is None

    def test_detect_type_column_from_master_schema(self):
        df = pd.DataFrame(
            {
                "Domain": ["333903271", "com.foo.bar", "example.com"],
                "Type": ["IOS", "ANDROID", "WEBSITE"],
            }
        )
        assert detect_type_column(df, "Domain") == "Type"

    def test_detect_type_column_rejects_unrelated_type_column(self):
        # A "Type" column whose values are not content types must be ignored.
        df = pd.DataFrame(
            {
                "Domain": ["a.com", "b.com"],
                "Type": ["News", "Sports"],
            }
        )
        assert detect_type_column(df, "Domain") is None

    def test_detect_type_column_never_returns_input_column(self):
        df = pd.DataFrame({"type": ["website", "website"]})
        # The only column is also the input column -> not usable as a hint source
        assert detect_type_column(df, "type") is None

    def test_type_hint_overrides_ambiguous_value_detection(self):
        # Leaf collides with a real TLD and is shallow -> value detection would
        # default to website, but an explicit ANDROID hint must win.
        assert detect_content_type("kik.android") == ContentType.WEBSITE
        assert parse_content_type_hint("android") == ContentType.ANDROID_APP
