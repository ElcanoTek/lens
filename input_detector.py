# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.

"""Input detection module for elcano-lens.

This module provides functionality to auto-detect:
1. The input column containing URLs/IDs (pageURL, domain, url, etc.)
2. The content type for each row (iOS app, Android app, or Website)
"""

import logging
import re
from pathlib import Path
from typing import List, Optional, Tuple

import pandas as pd

from shared_types import ContentType, CTVWorkItem

logger = logging.getLogger(__name__)

# Authoritative set of registrable top-level domains (IANA). Loaded from the
# bundled ``iana_tlds.txt`` snapshot next to this module. This is the linchpin
# of Android-vs-website detection: a real domain ALWAYS ends in a registered
# TLD, so a dotted identifier whose leaf label is not a TLD cannot be a domain.
# See ``iana_tlds.txt`` for the source URL and one-line refresh command.
_IANA_TLDS_PATH = Path(__file__).with_name("iana_tlds.txt")


def _load_iana_tlds() -> frozenset:
    """Load the bundled IANA TLD list; fall back to the hand-maintained
    ``WEB_TLDS`` if the file is missing so detection never hard-fails."""
    try:
        text = _IANA_TLDS_PATH.read_text(encoding="utf-8")
    except OSError as exc:
        logger.warning(
            "Could not read %s (%s); falling back to built-in TLD list",
            _IANA_TLDS_PATH,
            exc,
        )
        return frozenset(tld.lstrip(".") for tld in WEB_TLDS)

    tlds = set()
    for line in text.splitlines():
        entry = line.strip().lower()
        if entry and not entry.startswith("#"):
            tlds.add(entry)
    return frozenset(tlds)


# Domains rarely nest deeper than four labels (e.g. a.b.c.example.com); Android
# package names routinely have five or more. When a package-shaped string's leaf
# collides with a real TLD (e.g. ``puzzle.solver.free``), this depth is the
# tie-breaker that keeps it out of the website bucket. Measured against 31k real
# domains, a >=5 threshold yields zero false positives.
_PACKAGE_MIN_LABELS_WHEN_TLD_LEAF = 5


# CTV-specific column names that indicate a CTV input file
CTV_COLUMN_INDICATORS = [
    "app_name",
    "appname",
    "app name",
    "bundle_id",
    "bundleid",
    "bundle id",
    "ctv_app",
    "ctvapp",
    "ctv app",
    "channel_name",
    "channelname",
    "channel name",
    "roku_id",
    "rokuid",
    "fire_tv_id",
    "firetvid",
    "apple_tv_id",
    "appletvid",
    "SSP",
    "App, Account or Network Name",
    "Bundle ID",
    "Publisher",
]

# Column name mapping for CTV files (case-insensitive partial matches)
CTV_COLUMN_MAPPINGS = {
    "ssp": "ssp",
    "publisher": "publisher",
    "app": "app_name",  # "App, Account or Network Name" contains "app"
    "account": "app_name",
    "network name": "app_name",
    "bundle": "bundle_id",
}

# CTV platform indicators in column names
CTV_PLATFORM_COLUMNS = [
    "platform",
    "ctv_platform",
    "streaming_platform",
    "device",
    "device_type",
]

# CTV network indicators in column names (deprecated - network column removed)
CTV_NETWORK_COLUMNS = [
    "network",
    "network_affiliation",
    "broadcaster",
    "content_owner",
]


# Common column names that might contain the URL/ID data
CANDIDATE_COLUMNS = [
    "pageURL",
    "pageurl",
    "page_url",
    "domain",
    "Domain",
    "url",
    "URL",
    "Url",
    "website",
    "Website",
    "site",
    "Site",
    "app_id",
    "appId",
    "app",
    "bundle_id",
    "bundleId",
    "package",
    "package_name",
]

# Characters that indicate a URL/website (never found in Android package names)
URL_INDICATOR_CHARS = frozenset("/:?#&=@!$'()*+,;[]%")

# Primary/core TLDs that are ALWAYS websites when they appear at the end
# These are the classic TLDs that almost never appear as Android package suffixes
PRIMARY_WEB_TLDS = (
    ".com",
    ".org",
    ".net",
    ".edu",
    ".gov",
    ".mil",
    ".int",
)

# Country code TLDs - when at end, definitely a website
COUNTRY_TLDS = (
    ".us",
    ".uk",
    ".de",
    ".fr",
    ".es",
    ".it",
    ".nl",
    ".au",
    ".ca",
    ".jp",
    ".cn",
    ".ru",
    ".br",
    ".in",
    ".kr",
    ".mx",
    ".pl",
    ".se",
    ".ch",
    ".at",
    ".be",
    ".dk",
    ".fi",
    ".no",
    ".nz",
    ".za",
    ".ie",
    ".pt",
    ".cz",
    ".hu",
    ".gr",
    ".ro",
    ".il",
    ".sg",
    ".hk",
    ".tw",
    ".th",
    ".my",
    ".ph",
    ".vn",
    ".id",
    ".ar",
    ".cl",
)

# Compound TLDs - when at end, definitely a website
COMPOUND_TLDS = (
    ".co.uk",
    ".com.au",
    ".com.br",
    ".co.jp",
    ".co.kr",
    ".co.nz",
    ".co.za",
    ".com.mx",
    ".com.ar",
    ".org.uk",
    ".net.au",
)

# Semi-primary TLDs - these are newer but commonly used as actual domain TLDs
# When at end, usually a website unless there's strong evidence otherwise
SEMI_PRIMARY_TLDS = (
    ".io",
    ".co",  # Very commonly used as domain TLDs for tech companies
)

# Ambiguous TLDs - these can be either website TLDs OR Android package suffixes
# When these appear at the END, context matters (prefix, structure, etc.)
AMBIGUOUS_TLDS = (
    ".me",
    ".tv",
    ".ai",
    ".app",
    ".dev",
    ".tech",
    ".info",
    ".biz",
    ".xyz",
    ".online",
    ".site",
    ".website",
    ".gg",
    ".fm",
    ".ly",
    ".to",
    ".cc",
    ".ws",
    ".so",
    ".ac",
    ".sh",
)

# All web TLDs combined for simple checks
WEB_TLDS = PRIMARY_WEB_TLDS + COUNTRY_TLDS + COMPOUND_TLDS + SEMI_PRIMARY_TLDS + AMBIGUOUS_TLDS

# Full authoritative TLD set, loaded once at import (see _load_iana_tlds above).
IANA_TLDS = _load_iana_tlds()

# Android package name prefixes (TLDs used as package prefixes)
# These are ONLY valid as Android packages when NOT ending with a web TLD
ANDROID_PACKAGE_PREFIXES = (
    "com.",
    "net.",
    "org.",
    "io.",
    "co.",
    "me.",
    "app.",
    "dev.",
    "ai.",
    "tv.",
    "in.",
    "de.",
    "fr.",
    "uk.",
    "jp.",
    "kr.",
    "cn.",
    "ru.",
    "br.",
    "it.",
    "es.",
    "au.",
    "ca.",
)


def detect_input_column(df: pd.DataFrame) -> Optional[str]:
    """
    Auto-detect the column containing URL/ID data.

    Args:
        df: Input DataFrame

    Returns:
        Column name if found, None otherwise
    """
    columns = df.columns.tolist()

    # First, try exact matches with candidate columns
    for candidate in CANDIDATE_COLUMNS:
        if candidate in columns:
            logger.info("Detected input column: %s (exact match)", candidate)
            return candidate

    # Then, try case-insensitive matching
    columns_lower = {col.lower(): col for col in columns}
    for candidate in CANDIDATE_COLUMNS:
        if candidate.lower() in columns_lower:
            actual_col = columns_lower[candidate.lower()]
            logger.info("Detected input column: %s (case-insensitive match)", actual_col)
            return actual_col

    # Finally, try partial matching
    for col in columns:
        col_lower = col.lower()
        if any(term in col_lower for term in ["url", "domain", "site", "app", "package"]):
            logger.info("Detected input column: %s (partial match)", col)
            return col

    logger.warning("Could not auto-detect input column from: %s", columns)
    return None


# Loose shape of a web domain/URL for content-based column scoring
_DOMAIN_LIKE_RE = re.compile(
    r"^(?:https?://)?[A-Za-z0-9][A-Za-z0-9._-]*\.[A-Za-z]{2,}(?:[/:?#].*)?$"
)


def _looks_processable(value: str) -> bool:
    """Check if a single value looks like something Lens can classify.

    Stricter than detect_content_type (which defaults to WEBSITE): used to
    score columns by content, so a Notes or Rank column doesn't win.
    """
    v = value.strip()
    if not v:
        return False
    if v.isdigit():
        # App Store IDs are long; short numbers are ranks/counts.
        return len(v) >= 6
    if _DOMAIN_LIKE_RE.match(v):
        return True
    v_lower = v.lower()
    return v_lower.startswith(ANDROID_PACKAGE_PREFIXES) and _is_valid_android_package(v)


def detect_input_column_by_content(
    df: pd.DataFrame, min_ratio: float = 0.6, sample_size: int = 200
) -> Optional[str]:
    """Fallback for files with unrecognizable headers: score every column by
    how many of its values look like domains/app IDs and pick the best one.
    """
    best_col: Optional[str] = None
    best_ratio = 0.0
    for col in df.columns:
        values = [v.strip() for v in df[col].dropna().astype(str).tolist() if v and v.strip()]
        if not values:
            continue
        sample = values[:sample_size]
        ratio = sum(1 for v in sample if _looks_processable(v)) / len(sample)
        if ratio > best_ratio:
            best_col, best_ratio = col, ratio

    if best_col is not None and best_ratio >= min_ratio:
        logger.info(
            "Detected input column by content: %s (%d%% classifiable values)",
            best_col,
            int(best_ratio * 100),
        )
        return best_col
    return None


def _is_url_or_website(value: str) -> bool:
    """
    Check if a value is clearly a URL or website domain.

    This function checks for definitive URL/website indicators that
    Android package names would NEVER have.

    Args:
        value: The value to check

    Returns:
        True if this is definitely a URL/website, False otherwise
    """
    value_lower = value.lower()

    # 1. Check for URL protocol schemes (definitive URL indicator)
    if any(
        value_lower.startswith(proto)
        for proto in (
            "http://",
            "https://",
            "ftp://",
            "ftps://",
            "file://",
            "mailto:",
            "tel:",
            "sms:",
            "//",
        )
    ):
        return True

    # 2. Check for URL indicator characters (never in Android packages)
    if any(char in value for char in URL_INDICATOR_CHARS):
        return True

    # 3. Check for www prefix (definitive website indicator)
    if value_lower.startswith("www."):
        return True

    # 4. Check for port numbers (e.g., example.com:8080)
    if re.search(r":\d{1,5}(?:/|$)", value):
        return True

    # 5. Check for IP addresses (v4)
    if re.match(r"^\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}(?::\d+)?(?:/.*)?$", value):
        return True

    # 6. Check for localhost
    if value_lower.startswith("localhost"):
        return True

    # 7. Check for hyphens in domain segments - common in domains, rare in packages
    # e.g., "my-website.com" - hyphens are invalid in Java package names
    parts = value.split(".")
    if any("-" in part for part in parts):
        return True

    # 8. Check if it ends with a PRIMARY web TLD (.com, .org, .net, etc.)
    # These are definitive website indicators - ALWAYS a website
    for tld in PRIMARY_WEB_TLDS + COUNTRY_TLDS + COMPOUND_TLDS:
        if value_lower.endswith(tld):
            return True

    # 9. Check for SEMI-PRIMARY TLDs (.io, .co) - commonly used as domain TLDs
    # These are usually websites even if they start with an Android prefix
    # e.g., "com.company.io" is more likely company.io with "com" subdomain
    for tld in SEMI_PRIMARY_TLDS:
        if value_lower.endswith(tld):
            return True

    # 10. For AMBIGUOUS TLDs (.app, .dev, .me, etc.), check the structure
    # If it starts with an Android prefix AND ends with same TLD category,
    # it might be a package. Otherwise, it's likely a website.
    for tld in AMBIGUOUS_TLDS:
        if value_lower.endswith(tld):
            # If it doesn't start with an Android prefix, it's a website
            if not value_lower.startswith(ANDROID_PACKAGE_PREFIXES):
                return True

            # If it starts with an Android prefix AND ends with an ambiguous TLD,
            # we need more context. Check if prefix != TLD (different categories)
            # e.g., "io.example.dev" - prefix is "io", TLD is ".dev" -> website
            # e.g., "dev.example.app" - prefix is "dev", TLD is ".app" -> could be either
            # e.g., "com.example.app" - prefix is "com", TLD is ".app" -> likely Android package
            prefix = None
            for p in ANDROID_PACKAGE_PREFIXES:
                if value_lower.startswith(p):
                    prefix = p.rstrip(".")
                    break

            tld_name = tld.lstrip(".")

            # If prefix is a PRIMARY TLD (com, org, net), treat the ambiguous ending
            # as a package segment, not a website TLD
            # e.g., "com.example.app" -> prefix is "com" (primary TLD), so ".app" is package segment
            if prefix in ("com", "org", "net"):
                # Don't classify as website - let Android detection handle it
                continue

            # If prefix is a secondary Android prefix (co, me, etc.) and ends
            # with a common package suffix (.app, etc.), treat as potential package
            # e.g., "co.company.app" -> likely an Android package
            # e.g., "me.developer.app" -> likely an Android package
            # BUT: "app." and "dev." are also common subdomains, so don't bypass for those
            # e.g., "app.startup.dev" -> likely a website (app subdomain of startup.dev)
            if tld_name in ("app", "tech", "ai") and prefix not in (
                "app",
                "dev",
                "api",
                "cdn",
                "www",
                "m",
            ):
                # These are common Android package suffixes - let Android detection handle
                continue

            # If prefix is an ambiguous TLD itself and different from ending TLD,
            # this is more likely a website
            # e.g., "io.company.dev" - both io and dev are ambiguous, but different -> website
            if prefix != tld_name:
                return True

    return False


def _is_valid_android_package(value: str) -> bool:
    """
    Check if a value is a valid Android package name.

    Android package names must:
    - Contain only lowercase letters, numbers, underscores, and dots
    - Have at least 2 segments separated by dots
    - Each segment must start with a letter
    - Not contain consecutive dots
    - Not start or end with a dot

    Args:
        value: The value to check

    Returns:
        True if this looks like a valid Android package name
    """
    # Android packages are case-insensitive but conventionally lowercase
    value_lower = value.lower()

    # Must not contain URL indicator characters
    if any(char in value for char in URL_INDICATOR_CHARS):
        return False

    # Must not contain hyphens (invalid in Java identifiers)
    if "-" in value:
        return False

    # Must have at least one dot
    if "." not in value:
        return False

    # Split into segments
    segments = value_lower.split(".")

    # Must have at least 2 segments (e.g., com.example)
    if len(segments) < 2:
        return False

    # Each segment must be non-empty and start with a letter
    for segment in segments:
        if not segment:
            return False
        if not segment[0].isalpha():
            return False
        # Segment can only contain letters, numbers, and underscores
        if not re.match(r"^[a-z][a-z0-9_]*$", segment):
            return False

    return True


def _leaf_is_registrable_tld(value_lower: str) -> bool:
    """True if the value's final label is a real (IANA) top-level domain.

    A registrable web domain must end in a delegated TLD, so this is what tells
    a website apart from a reverse-DNS Android package whose leaf is an ordinary
    word (``com.example.myapp`` -> leaf ``myapp`` is not a TLD).
    """
    leaf = value_lower.rsplit(".", 1)[-1]
    return leaf in IANA_TLDS


def detect_content_type(value: str) -> ContentType:
    """
    Detect the content type from a single value.

    Detection rules (in priority order):
    1. Check for clear URL/website indicators FIRST (protocol, special chars, TLD endings)
    2. All numeric -> iOS app (App Store ID)
    3. Valid Android package name -> Android app. A known reverse-DNS prefix
       (com., org., ...) is a fast path; otherwise a package-shaped string is an
       Android app when its leaf label is not a real TLD (so it cannot be a
       domain), or when it nests too deep (>=5 labels) to be a plausible domain.
    4. Default -> Website

    The key insight is that:
    - URLs/domains END with TLDs (e.g., example.com, api.example.org)
    - Android packages START with TLDs (e.g., com.example.app, org.company.product)
      and their LEAF label is an ordinary word, not a TLD (e.g. ...example.myapp)

    Args:
        value: The raw input value (app ID, package name, or domain)

    Returns:
        ContentType enum value
    """
    if not value or not isinstance(value, str):
        return ContentType.WEBSITE

    value = value.strip()
    if not value:
        return ContentType.WEBSITE

    # CRITICAL: Check for URL/website indicators FIRST
    # This catches domains like io.example.com that might otherwise
    # be misclassified as Android apps because they start with "io."
    if _is_url_or_website(value):
        return ContentType.WEBSITE

    # Check for iOS app (all numeric - App Store ID)
    if value.isdigit():
        return ContentType.IOS_APP

    value_lower = value.lower()

    # Fast path: valid package that starts with a known reverse-DNS prefix.
    if value_lower.startswith(ANDROID_PACKAGE_PREFIXES) and _is_valid_android_package(value):
        # _is_valid_android_package already guarantees at least 2 segments.
        return ContentType.ANDROID_APP

    # Fallback for packages without a recognized prefix (e.g. "tunein.player",
    # "jigsaw.puzzle.game.banana", "kik.android"). Only structurally-valid
    # package names that survived the website gate above reach here.
    if _is_valid_android_package(value):
        # A real domain must end in a registrable TLD; if the leaf is not one,
        # this cannot be a website -> Android package. (Zero false positives by
        # construction: anything ending in a real TLD is left as a website.)
        if not _leaf_is_registrable_tld(value_lower):
            return ContentType.ANDROID_APP
        # Leaf collides with a real TLD (e.g. ".games", ".news"): ambiguous.
        # Depth is the safe tie-breaker -- domains don't nest this deep.
        if value_lower.count(".") + 1 >= _PACKAGE_MIN_LABELS_WHEN_TLD_LEAF:
            return ContentType.ANDROID_APP

    # Default to website for anything else
    return ContentType.WEBSITE


def analyze_batch(values: List[str]) -> Tuple[dict, List[Tuple[str, ContentType]]]:
    """
    Analyze a batch of values and return statistics and per-value types.

    Args:
        values: List of input values

    Returns:
        Tuple of (statistics dict, list of (value, type) tuples)
    """
    results = []
    stats = {
        ContentType.IOS_APP: 0,
        ContentType.ANDROID_APP: 0,
        ContentType.WEBSITE: 0,
        ContentType.CTV_APP: 0,
    }

    for value in values:
        content_type = detect_content_type(str(value) if value else "")
        results.append((value, content_type))
        stats[content_type] += 1

    logger.info(
        "Batch analysis: %d iOS apps, %d Android apps, %d websites, %d CTV apps",
        stats[ContentType.IOS_APP],
        stats[ContentType.ANDROID_APP],
        stats[ContentType.WEBSITE],
        stats[ContentType.CTV_APP],
    )

    return stats, results


def detect_batch_type(values: List[str]) -> Optional[ContentType]:
    """
    Detect if a batch is predominantly one type.

    Returns the content type if >90% of values are that type,
    otherwise returns None (mixed batch).

    Args:
        values: List of input values

    Returns:
        ContentType if homogeneous, None if mixed
    """
    if not values:
        return None

    stats, _ = analyze_batch(values)
    total = len(values)

    for content_type, count in stats.items():
        if count / total >= 0.9:
            logger.info(
                "Batch detected as predominantly %s (%d%%)",
                content_type.value,
                int(count / total * 100),
            )
            return content_type

    logger.info("Mixed batch detected - will process each item by type")
    return None


# Column names that may carry an explicit per-row content type. When present and
# trustworthy, this beats value-based guessing (e.g. an Android package whose
# leaf collides with a real TLD). CTV is intentionally excluded here: CTV inputs
# are routed as a whole file by is_ctv_input_file(), not per row.
TYPE_COLUMN_CANDIDATES = (
    "type",
    "content_type",
    "content type",
    "contenttype",
    "media_type",
    "media type",
    "platform",
)

# Normalized cell value -> ContentType. Only the three types the app/website
# pipeline can act on per row are mapped; anything else (incl. "ctv") is left to
# value-based detection so a hint never produces an unprocessable work item.
_CONTENT_TYPE_HINTS = {
    "ios": ContentType.IOS_APP,
    "ios_app": ContentType.IOS_APP,
    "iosapp": ContentType.IOS_APP,
    "iphone": ContentType.IOS_APP,
    "ipad": ContentType.IOS_APP,
    "apple": ContentType.IOS_APP,
    "app store": ContentType.IOS_APP,
    "appstore": ContentType.IOS_APP,
    "itunes": ContentType.IOS_APP,
    "android": ContentType.ANDROID_APP,
    "android_app": ContentType.ANDROID_APP,
    "androidapp": ContentType.ANDROID_APP,
    "google play": ContentType.ANDROID_APP,
    "googleplay": ContentType.ANDROID_APP,
    "play store": ContentType.ANDROID_APP,
    "playstore": ContentType.ANDROID_APP,
    "gplay": ContentType.ANDROID_APP,
    "website": ContentType.WEBSITE,
    "web": ContentType.WEBSITE,
    "web site": ContentType.WEBSITE,
    "site": ContentType.WEBSITE,
    "domain": ContentType.WEBSITE,
    "url": ContentType.WEBSITE,
    "webpage": ContentType.WEBSITE,
}


def parse_content_type_hint(value: object) -> Optional[ContentType]:
    """Parse a single type-column cell into a ContentType, or None if it is
    empty/unrecognized (caller should fall back to value-based detection)."""
    if value is None:
        return None
    text = str(value).strip().lower()
    if not text:
        return None
    return _CONTENT_TYPE_HINTS.get(text)


def detect_type_column(
    df: pd.DataFrame,
    input_column: Optional[str] = None,
    min_ratio: float = 0.5,
) -> Optional[str]:
    """Find a column that holds an explicit per-row content type.

    A column qualifies only if its name is a known type-column candidate AND at
    least ``min_ratio`` of its non-empty values parse to a known ContentType.
    The value check is what makes this safe: a generic "Type" column full of
    unrelated labels (e.g. content genres) is rejected rather than trusted. The
    detected input column is never treated as a type column.
    """
    input_lower = input_column.lower().strip() if input_column else None
    for col in df.columns:
        if not isinstance(col, str):
            continue
        name = col.lower().strip()
        if name not in TYPE_COLUMN_CANDIDATES:
            continue
        if input_lower is not None and name == input_lower:
            continue

        values = [v for v in df[col].dropna().astype(str).tolist() if v.strip()]
        if not values:
            continue
        recognized = sum(1 for v in values if parse_content_type_hint(v) is not None)
        if recognized / len(values) >= min_ratio:
            logger.info(
                "Detected explicit type column: %s (%d%% of values recognized)",
                col,
                int(recognized / len(values) * 100),
            )
            return col

    return None


def is_ctv_input_file(df: pd.DataFrame) -> bool:
    """
    Detect if a DataFrame is from a CTV input file based on column headers.

    CTV input files typically have columns like:
    - app_name, bundle_id, platform, network
    - Or variations like ctv_app, channel_name, roku_id, etc.

    Args:
        df: Input DataFrame to analyze

    Returns:
        True if this appears to be a CTV input file
    """
    columns_lower = [col.lower().strip() for col in df.columns]

    # Strong CTV-specific indicators that are unlikely to appear in generic app/website lists
    # Note: 'app name' and 'publisher' are too generic - they also appear in app lists
    strong_ctv_indicators = [
        "bundle_id",
        "bundleid",
        "bundle id",
        "ssp",
        "ctv_app",
        "ctvapp",
        "ctv app",
        "channel_name",
        "channelname",
        "channel name",
        "roku_id",
        "rokuid",
        "fire_tv_id",
        "firetvid",
        "apple_tv_id",
        "appletvid",
        "app, account or network name",  # Specific to CTV_Master.csv format
    ]

    # Count strong CTV indicators
    strong_indicator_count = 0
    for indicator in strong_ctv_indicators:
        if any(indicator in col for col in columns_lower):
            strong_indicator_count += 1

    # If we find at least one strong CTV indicator, it's likely a CTV file
    if strong_indicator_count >= 1:
        logger.info(
            "Detected CTV input file (found %d strong CTV indicators)", strong_indicator_count
        )
        return True

    # Also check if file has "ctv" or "connected tv" in any column name
    if any("ctv" in col or "connected" in col for col in columns_lower):
        logger.info("Detected CTV input file (found 'ctv' or 'connected' in column names)")
        return True

    # Special case: Check for the exact CTV_Master.csv format (SSP + Bundle ID + App Name column)
    has_ssp = any("ssp" in col for col in columns_lower)
    has_bundle_id = any("bundle" in col and "id" in col for col in columns_lower)
    has_app_network_name = any("app" in col and "network" in col for col in columns_lower)

    if has_ssp and has_bundle_id and has_app_network_name:
        logger.info("Detected CTV input file (SSP + Bundle ID + App/Network Name columns)")
        return True

    return False


def detect_ctv_columns(df: pd.DataFrame) -> dict:
    """
    Detect which columns in a DataFrame correspond to CTV fields.

    Args:
        df: Input DataFrame

    Returns:
        Dictionary mapping field names to column names:
        {
            "app_name": "App Name",  # or None if not found
            "bundle_id": "Bundle_ID",
            "platform": "Platform",
            "network": "Network",
            "url": "URL",
        }
    """
    columns = df.columns.tolist()
    columns_lower = {col.lower().strip(): col for col in columns}

    result = {
        "app_name": None,
        "bundle_id": None,
        "ssp": None,
        "publisher": None,
        "platform": None,
        "url": None,
    }

    # Detect app_name column
    # First try exact matches, then try partial matches (for columns like "App, Account or Network Name")
    app_name_candidates = [
        "app_name",
        "appname",
        "app name",
        "app",
        "name",
        "channel_name",
        "channelname",
        "channel name",
        "ctv_app",
        "ctvapp",
        "ctv app",
        "app, account or network name",
    ]  # Added explicit match for CTV_Master.csv

    # Try exact match first
    for candidate in app_name_candidates:
        if candidate in columns_lower:
            result["app_name"] = columns_lower[candidate]
            break

    # If no exact match, try partial match (column name contains candidate)
    if not result["app_name"]:
        for col_lower, col_original in columns_lower.items():
            # Check if column contains 'app' and 'name' (but not 'app_id' or similar)
            if "app" in col_lower and "name" in col_lower and "id" not in col_lower:
                result["app_name"] = col_original
                break

    # Detect bundle_id column
    for candidate in [
        "bundle_id",
        "bundleid",
        "bundle id",
        "bundle",
        "app_id",
        "appid",
        "app id",
        "roku_id",
        "rokuid",
        "fire_tv_id",
        "firetvid",
        "apple_tv_id",
        "appletvid",
    ]:
        if candidate in columns_lower:
            result["bundle_id"] = columns_lower[candidate]
            break

    # Detect platform column
    for candidate in CTV_PLATFORM_COLUMNS:
        if candidate in columns_lower:
            result["platform"] = columns_lower[candidate]
            break

    # Detect SSP column
    for candidate in ["ssp", "supply_side_platform", "supply side platform"]:
        if candidate in columns_lower:
            result["ssp"] = columns_lower[candidate]
            break

    # Detect Publisher column
    for candidate in ["publisher", "pub", "publisher_name", "publisher name"]:
        if candidate in columns_lower:
            result["publisher"] = columns_lower[candidate]
            break

    # Detect URL column
    for candidate in ["url", "website", "site", "link", "homepage"]:
        if candidate in columns_lower:
            result["url"] = columns_lower[candidate]
            break

    logger.debug("Detected CTV columns: %s", result)
    return result


def parse_ctv_work_items(df: pd.DataFrame) -> List[CTVWorkItem]:
    """
    Parse a DataFrame into a list of CTVWorkItem objects.

    Args:
        df: Input DataFrame containing CTV app data

    Returns:
        List of CTVWorkItem objects
    """
    columns = detect_ctv_columns(df)

    if not columns["app_name"]:
        logger.warning("No app_name column found in CTV input file")
        # Try to use the first column as app_name
        if len(df.columns) > 0:
            columns["app_name"] = df.columns[0]
            logger.info("Using first column '%s' as app_name", columns["app_name"])
        else:
            return []

    work_items = []

    for _idx, row in df.iterrows():
        app_name = str(row.get(columns["app_name"], "")).strip()
        if not app_name:
            continue

        bundle_id = None
        if columns["bundle_id"]:
            bundle_id = str(row.get(columns["bundle_id"], "")).strip() or None

        platform = None
        if columns["platform"]:
            platform = str(row.get(columns["platform"], "")).strip() or None

        url = None
        if columns["url"]:
            url = str(row.get(columns["url"], "")).strip() or None

        ssp = ""
        if columns["ssp"]:
            ssp = str(row.get(columns["ssp"], "")).strip()
            if ssp.lower() == "nan":
                ssp = ""

        publisher = ""
        if columns["publisher"]:
            publisher = str(row.get(columns["publisher"], "")).strip()
            if publisher.lower() == "nan":
                publisher = ""

        # Store original row data for reference
        original_row = {str(k): str(v) for k, v in row.to_dict().items() if pd.notna(v)}

        work_items.append(
            CTVWorkItem(
                app_name=app_name,
                bundle_id=bundle_id,
                ssp=ssp,
                publisher=publisher,
                platform=platform,
                url=url,
                original_row=original_row,
            )
        )

    logger.info("Parsed %d CTV work items from input file", len(work_items))
    return work_items


class InputDetector:
    """
    Main class for input detection and analysis.

    This class wraps the detection functionality and maintains state
    for a processing session.
    """

    def __init__(self, df: pd.DataFrame):
        """
        Initialize the InputDetector with a DataFrame.

        Args:
            df: Input DataFrame containing the data to analyze
        """
        self.df = df
        self.input_column: Optional[str] = None
        self.batch_type: Optional[ContentType] = None
        self.item_types: List[Tuple[str, ContentType]] = []
        self._analyzed = False

    def analyze(self) -> bool:
        """
        Perform full analysis of the input data.

        Returns:
            True if analysis succeeded, False otherwise
        """
        # Detect input column by header name, then by content as a fallback
        self.input_column = detect_input_column(self.df)
        if not self.input_column:
            self.input_column = detect_input_column_by_content(self.df)
        if not self.input_column:
            logger.error("Could not detect input column")
            return False

        # Get values from the detected column
        values = self.df[self.input_column].dropna().astype(str).tolist()
        if not values:
            logger.error("No valid values in input column")
            return False

        # Analyze batch type
        self.batch_type = detect_batch_type(values)

        # Get per-item types
        _, self.item_types = analyze_batch(values)

        self._analyzed = True
        logger.info(
            "Analysis complete: column=%s, batch_type=%s, items=%d",
            self.input_column,
            self.batch_type.value if self.batch_type else "mixed",
            len(self.item_types),
        )

        return True

    def get_items_by_type(self, content_type: ContentType) -> List[str]:
        """
        Get all items of a specific type.

        Args:
            content_type: The content type to filter by

        Returns:
            List of values matching the specified type
        """
        if not self._analyzed:
            raise RuntimeError("Must call analyze() first")

        return [value for value, ct in self.item_types if ct == content_type]

    def get_type_for_value(self, value: str) -> ContentType:
        """
        Get the content type for a specific value.

        Args:
            value: The value to check

        Returns:
            ContentType for the value
        """
        return detect_content_type(value)

    @property
    def has_ios_apps(self) -> bool:
        """Check if the batch contains iOS apps."""
        if not self._analyzed:
            return False
        return any(ct == ContentType.IOS_APP for _, ct in self.item_types)

    @property
    def has_android_apps(self) -> bool:
        """Check if the batch contains Android apps."""
        if not self._analyzed:
            return False
        return any(ct == ContentType.ANDROID_APP for _, ct in self.item_types)

    @property
    def has_websites(self) -> bool:
        """Check if the batch contains websites."""
        if not self._analyzed:
            return False
        return any(ct == ContentType.WEBSITE for _, ct in self.item_types)

    @property
    def has_ctv_apps(self) -> bool:
        """Check if the batch contains CTV apps."""
        if not self._analyzed:
            return False
        return any(ct == ContentType.CTV_APP for _, ct in self.item_types)

    @property
    def is_mixed(self) -> bool:
        """Check if the batch contains mixed content types."""
        if not self._analyzed:
            return False
        return self.batch_type is None


def _find_ctv_column(df: pd.DataFrame, key: str) -> Optional[str]:
    """
    Find a CTV column in the DataFrame by key.

    Args:
        df: Input DataFrame
        key: The key to search for (e.g., 'ssp', 'app', 'bundle')

    Returns:
        The actual column name if found, None otherwise
    """
    for col in df.columns:
        col_lower = col.lower()
        if key in col_lower:
            return col
    return None


def parse_ctv_input(df: pd.DataFrame) -> List[CTVWorkItem]:
    """
    Parse a CTV input DataFrame into a list of CTVWorkItem objects.

    Args:
        df: Input DataFrame in CTV format

    Returns:
        List of CTVWorkItem objects
    """
    # Find the relevant columns
    ssp_col = _find_ctv_column(df, "ssp")
    publisher_col = _find_ctv_column(df, "publisher")
    app_name_col = (
        _find_ctv_column(df, "app")
        or _find_ctv_column(df, "account")
        or _find_ctv_column(df, "network")
    )
    bundle_id_col = _find_ctv_column(df, "bundle")

    if not app_name_col:
        logger.error("Could not find app name column in CTV input")
        return []

    logger.info(
        "CTV column mapping: ssp=%s, publisher=%s, app_name=%s, bundle_id=%s",
        ssp_col,
        publisher_col,
        app_name_col,
        bundle_id_col,
    )

    # Create work items - group by app name to avoid duplicates
    app_items: dict[str, CTVWorkItem] = {}

    for _, row in df.iterrows():
        app_name = str(row.get(app_name_col, "")).strip() if app_name_col else ""

        # Skip empty app names
        if not app_name or app_name.lower() in ("nan", "", "none"):
            continue

        # Skip if we already have this app
        if app_name in app_items:
            continue

        ssp = str(row.get(ssp_col, "")).strip() if ssp_col else ""
        publisher = str(row.get(publisher_col, "")).strip() if publisher_col else ""
        bundle_id = str(row.get(bundle_id_col, "")).strip() if bundle_id_col else ""

        # Clean up nan values
        if ssp.lower() == "nan":
            ssp = ""
        if publisher.lower() == "nan":
            publisher = ""
        if bundle_id.lower() == "nan":
            bundle_id = ""

        app_items[app_name] = CTVWorkItem(
            app_name=app_name,
            bundle_id=bundle_id,
            ssp=ssp,
            publisher=publisher,
        )

    items = list(app_items.values())
    logger.info("Parsed %d unique CTV apps from input", len(items))
    return items
