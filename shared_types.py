# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.

from dataclasses import dataclass, field
from enum import Enum
from typing import Dict, Optional

# Failure messages that mean "an anti-bot system blocked the request", not
# "this backend couldn't render the page". Shared by orchestration (to skip
# pointless deep-crawl retries) and domain_processing (to derive the
# Bot_Protection column).
ANTIBOT_ERROR_MARKERS = (
    "anti-bot",
    "document_antibot",
    "block page detected",
    "http status 401",
    "http status 403",
    "http status 429",
)


def is_antibot_error(message: Optional[str]) -> bool:
    """True when a failure message carries an anti-bot/block signature."""
    blob = (message or "").lower()
    return any(marker in blob for marker in ANTIBOT_ERROR_MARKERS)


class ContentType(Enum):
    """Enumeration of supported content types."""

    IOS_APP = "ios_app"
    ANDROID_APP = "android_app"
    WEBSITE = "website"
    CTV_APP = "ctv_app"


@dataclass(frozen=True)
class DomainWorkItem:
    """Simple container for a domain and its associated metadata."""

    domain: str


@dataclass(frozen=True)
class WorkItem:
    """
    Unified work item that can represent a website, iOS app, or Android app.

    Attributes:
        identifier: The unique identifier (domain, app ID, or package name)
        content_type: The type of content (website, iOS app, or Android app)
        original_value: The original value from the input file
    """

    identifier: str
    content_type: ContentType
    original_value: Optional[str] = None

    @property
    def is_website(self) -> bool:
        """Check if this item is a website."""
        return self.content_type == ContentType.WEBSITE

    @property
    def is_ios_app(self) -> bool:
        """Check if this item is an iOS app."""
        return self.content_type == ContentType.IOS_APP

    @property
    def is_android_app(self) -> bool:
        """Check if this item is an Android app."""
        return self.content_type == ContentType.ANDROID_APP

    @property
    def is_ctv_app(self) -> bool:
        """Check if this item is a CTV app."""
        return self.content_type == ContentType.CTV_APP

    @property
    def is_app(self) -> bool:
        """Check if this item is any type of app."""
        return self.content_type in (ContentType.IOS_APP, ContentType.ANDROID_APP)

    @classmethod
    def from_domain(cls, domain: str) -> "WorkItem":
        """Create a WorkItem from a domain (website)."""
        return cls(
            identifier=domain,
            content_type=ContentType.WEBSITE,
            original_value=domain,
        )

    @classmethod
    def from_ios_app_id(cls, app_id: str) -> "WorkItem":
        """Create a WorkItem from an iOS app ID."""
        return cls(
            identifier=app_id,
            content_type=ContentType.IOS_APP,
            original_value=app_id,
        )

    @classmethod
    def from_android_package(cls, package_name: str) -> "WorkItem":
        """Create a WorkItem from an Android package name."""
        return cls(
            identifier=package_name,
            content_type=ContentType.ANDROID_APP,
            original_value=package_name,
        )

    @classmethod
    def from_ctv_app(
        cls, app_name: str, bundle_id: str = "", original_value: str = None
    ) -> "WorkItem":
        """Create a WorkItem from a CTV app name and bundle ID."""
        return cls(
            identifier=app_name,
            content_type=ContentType.CTV_APP,
            original_value=original_value or bundle_id or app_name,
        )


@dataclass
class CTVWorkItem:
    """
    Work item representing a CTV (Connected TV) app for processing.

    Attributes:
        app_name: The name of the CTV application
        bundle_id: Optional bundle identifier for the app
        ssp: Supply-side platform source
        publisher: Publisher/network name
        platform: The CTV platform (e.g., Roku, Fire TV, Apple TV, etc.)
        url: Optional URL associated with the app
        original_row: Optional dictionary containing the original CSV row data
    """

    app_name: str
    bundle_id: Optional[str] = None
    ssp: str = ""
    publisher: str = ""
    platform: Optional[str] = None
    url: Optional[str] = None
    original_row: Optional[Dict[str, str]] = field(default=None)

    @property
    def identifier(self) -> str:
        """Return the primary identifier for this CTV app."""
        return self.bundle_id or self.app_name

    @property
    def display_name(self) -> str:
        """Return a display-friendly name for this CTV app."""
        return self.app_name

    def to_work_item(self) -> WorkItem:
        """Convert to a generic WorkItem."""
        return WorkItem(
            identifier=self.app_name,
            content_type=ContentType.CTV_APP,
            original_value=self.bundle_id or self.app_name,
        )
