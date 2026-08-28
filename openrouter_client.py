# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.

"""OpenRouter client for AI-powered site classification using the OpenAI SDK."""

import asyncio
import csv
import json
import time
import re
from pathlib import Path
from typing import Dict, Any, Optional, List, Tuple
import logging

from openai import AsyncOpenAI

logger = logging.getLogger(__name__)


class OpenRouterClient:
    """Async client for interacting with OpenRouter API."""

    _taxonomy_cache: Optional[Dict[str, List[str]]] = None

    # Function calling schema for structured output
    _CLASSIFICATION_SCHEMA = {
        "type": "function",
        "function": {
            "name": "classify_website",
            "description": "Classify a website based on quality tier, category, and other metadata",
            "parameters": {
                "type": "object",
                "properties": {
                    "quality": {
                        "type": "string",
                        "enum": ["Premium", "Standard", "Long Tail"],
                        "description": "The quality tier of the website: Premium (best of the best), Standard (acceptable but not elite), or Long Tail (low quality/suspicious)"
                    },
                    "justification": {
                        "type": "string",
                        "description": "Detailed explanation for the quality rating, considering content quality, editorial standards, user experience, and brand recognition"
                    },
                    "vertical": {
                        "type": "string",
                        "description": "A SINGLE category name from the approved IAB taxonomy. Must be an exact match from the taxonomy list. DO NOT use commas or combine categories. Examples: 'Sports', 'Puzzle Video Games', 'Technology & Computing'"
                    },
                    "description": {
                        "type": "string",
                        "description": "A single sentence describing the site's primary content or purpose"
                    },
                    "confidence": {
                        "type": "string",
                        "enum": ["High", "Medium", "Low"],
                        "description": "Confidence level in the classification: High (clear signals), Medium (some ambiguity), or Low (minimal/conflicting signals)"
                    },
                    "language": {
                        "type": "string",
                        "description": "The primary language of the content (e.g., 'English', 'Spanish', 'French', 'Chinese', 'Multilingual', etc.)"
                    },
                    "political_leaning": {
                        "type": "string",
                        "enum": ["Far Left", "Left", "Center-Left", "Center", "Center-Right", "Right", "Far Right", "Non-Political"],
                        "description": "Political orientation of the content for US political context: Far Left (socialist/progressive advocacy), Left (liberal/progressive), Center-Left (moderate liberal), Center (balanced/neutral), Center-Right (moderate conservative), Right (conservative), Far Right (nationalist/hard conservative), Non-Political (no political content)"
                    },
                    "audience_size": {
                        "type": "string",
                        "description": "Conservatively estimate the site's relative audience size. You have a strong tendency to overestimate. Be skeptical: high quality or brand recognition does not mean high traffic. Most sites are XS or S. Use L/XL only for globally recognized destinations. Use the following t-shirt sizes: XS, S, M, L, XL.",
                        "enum": ["XS", "S", "M", "L", "XL"]
                    }
                },
                "required": ["quality", "justification", "vertical", "description", "confidence", "language", "political_leaning", "audience_size"]
            }
        }
    }

    def __init__(
        self,
        api_key: Optional[str],
        model: str = "~google/gemini-flash-latest",
        temperature: float = 0.1,
        max_tokens: int = 1500,
        *,
        max_retries: int = 2,
        timeout: Optional[float] = None,
        base_url: str = "https://openrouter.ai/api/v1",
        fallback_model: Optional[str] = None,
    ):
        if api_key is None:
            raise ValueError("OpenRouter API key is required for classification")

        self.api_key = api_key
        self.model = model
        self.primary_model = model
        self.fallback_model = fallback_model
        self._fallback_active = False
        self._fallback_lock = asyncio.Lock()
        self.temperature = temperature
        self.max_tokens = max_tokens
        self.base_url = base_url
        self._client: Optional[AsyncOpenAI] = None
        self.request_count = 0
        self.total_tokens_used = 0
        self.max_retries = max(1, int(max_retries))
        self.timeout = timeout

    async def __aenter__(self):
        """Async context manager entry."""
        self._client = AsyncOpenAI(
            api_key=self.api_key,
            base_url=self.base_url,
            default_headers=self._get_headers(),
            max_retries=self.max_retries,
            timeout=self.timeout,
        )
        return self

    async def __aexit__(self, exc_type, exc_val, exc_tb):
        """Async context manager exit."""
        if self._client:
            await self._client.close()
            self._client = None

    def _get_headers(self) -> Dict[str, str]:
        """Get headers for API requests."""
        return {
            "Authorization": f"Bearer {self.api_key}",
            "HTTP-Referer": "https://github.com/ElcanoTek/lens",
            "X-Title": "Lens",
            "Content-Type": "application/json"
        }

    @staticmethod
    def _extract_message(response_data: Dict[str, Any]) -> Optional[Dict[str, Any]]:
        """Safely walk response_data["choices"][0]["message"], tolerating None at
        any layer. Returns the message dict, or None if the response is shaped
        unexpectedly (e.g. choices=null or message=null, which some reasoning
        models return when their internal output overflows max_tokens)."""
        if not isinstance(response_data, dict):
            return None
        choices = response_data.get("choices")
        if not choices:
            return None
        first = choices[0] if isinstance(choices, list) else None
        if not isinstance(first, dict):
            return None
        message = first.get("message")
        return message if isinstance(message, dict) else None

    @staticmethod
    def _finish_reason(response_data: Dict[str, Any]) -> Optional[str]:
        """Return choices[0].finish_reason, tolerating malformed shapes.

        "length" means the model ran out of max_tokens mid-response — with
        reasoning models this typically happens while they are still
        "thinking", so no tool call was ever emitted."""
        if not isinstance(response_data, dict):
            return None
        choices = response_data.get("choices")
        first = choices[0] if isinstance(choices, list) and choices else None
        if not isinstance(first, dict):
            return None
        reason = first.get("finish_reason")
        return reason if isinstance(reason, str) else None

    @staticmethod
    def _extract_function_arguments(
        message: Dict[str, Any], expected_name: Optional[str] = None
    ) -> Optional[Dict[str, Any]]:
        """Pull the function-call arguments out of a chat message.

        First tries the standard tool_calls shape. Falls back to parsing
        message["content"] as JSON for models (some reasoning variants on
        OpenRouter) that emit the structured payload inline instead of as a
        tool call. Returns None if nothing parseable is found.
        """
        tool_calls = message.get("tool_calls") if message else None
        if tool_calls:
            try:
                call = tool_calls[0]
                if expected_name and call.get("function", {}).get("name") not in (expected_name, None):
                    logger.warning(
                        "Function call name %r did not match expected %r",
                        call.get("function", {}).get("name"), expected_name,
                    )
                arguments_raw = call["function"]["arguments"]
                if isinstance(arguments_raw, dict):
                    return arguments_raw
                return json.loads(arguments_raw)
            except (KeyError, IndexError, TypeError, json.JSONDecodeError) as exc:
                logger.warning("Failed to read tool_calls payload: %s", exc)

        content = (message or {}).get("content") or ""
        if not isinstance(content, str):
            return None
        # Find the first balanced JSON object in the content; some models wrap
        # the JSON in markdown fences or prose.
        start = content.find("{")
        end = content.rfind("}")
        if start == -1 or end == -1 or end <= start:
            return None
        try:
            parsed = json.loads(content[start : end + 1])
            if isinstance(parsed, dict):
                logger.info("Recovered classification arguments from message content (no tool_calls present)")
                return parsed
        except json.JSONDecodeError:
            return None
        return None

    @staticmethod
    def _usage_tokens(response_data: Dict[str, Any]) -> int:
        """Return total tokens from response usage, tolerating usage=null."""
        usage = response_data.get("usage") if isinstance(response_data, dict) else None
        if not isinstance(usage, dict):
            return 0
        return usage.get("total_tokens", 0) or 0

    @staticmethod
    def _extract_body_error(response: Any) -> Optional[str]:
        """OpenRouter sometimes wraps upstream provider failures inside an
        HTTP-200 response body with a top-level "error" object (and every other
        field null). The OpenAI SDK happily returns that as a ChatCompletion,
        so we have to dig it out ourselves. Returns a formatted message string
        if a body-embedded error is present, else None.
        """
        if response is None or not hasattr(response, "model_dump"):
            return None
        try:
            dumped = response.model_dump()
        except Exception:
            return None
        if not isinstance(dumped, dict):
            return None
        err = dumped.get("error")
        if not err:
            return None
        if isinstance(err, dict):
            msg = err.get("message") or "unknown error"
            code = err.get("code")
            return f"OpenRouter body error (code={code}): {msg}" if code is not None else f"OpenRouter body error: {msg}"
        return f"OpenRouter body error: {err}"

    @staticmethod
    def _is_model_unavailable_error(error_str: str) -> bool:
        """Detect errors that indicate the configured model is unavailable
        (deprecated, removed, or unknown to the provider) OR that it permanently
        rejected our request payload. Both should trigger a fallback swap
        rather than a normal retry — the primary won't accept this workload."""
        lowered = error_str.lower()
        availability_signals = ("deprecated", "not found", "no endpoints", "no allowed providers")
        if any(sig in lowered for sig in availability_signals):
            return True
        # 404 specifically against the chat completions endpoint = unknown model
        if "404" in lowered and "not found" in lowered:
            return True
        # OpenRouter wraps upstream provider rejections as a 502-coded body
        # error with the message "Invalid arguments passed to the model."
        # The HTTP layer returns 200 OK so retries don't help — only a model
        # swap will.
        if "openrouter body error" in lowered and (
            "invalid arguments" in lowered
            or "code=502" in lowered
            or "code=400" in lowered
        ):
            return True
        return False

    async def _activate_fallback(self, reason: str) -> bool:
        """Swap the active model to the configured fallback (idempotent).

        Returns True if the swap happened on this call, False if no fallback is
        configured or it was already active.
        """
        if not self.fallback_model:
            return False
        async with self._fallback_lock:
            if self._fallback_active:
                return False
            logger.warning(
                "Activating OpenRouter fallback model %r (primary %r failed: %s)",
                self.fallback_model, self.primary_model, reason,
            )
            self.model = self.fallback_model
            self._fallback_active = True
            return True

    async def _call_api_with_retry(
        self,
        api_call_func,
        *args,
        max_retries: int = 3,
        base_delay: float = 2.0,
        operation_name: str = "API call",
        **kwargs
    ):
        """
        Execute an async API call with exponential backoff retry logic.

        Args:
            api_call_func: The async function to call
            *args: Positional arguments for the function
            max_retries: Maximum number of retry attempts (default: 3)
            base_delay: Base delay in seconds for exponential backoff (default: 2.0)
            operation_name: Name of the operation for logging
            **kwargs: Keyword arguments for the function

        Returns:
            The result of the successful API call

        Raises:
            The last exception if all retries fail
        """
        last_exception = None

        for attempt in range(max_retries):
            try:
                result = await api_call_func(*args, **kwargs)
                body_error = self._extract_body_error(result)
                if body_error:
                    # Surface OpenRouter's body-embedded provider error as an
                    # exception so the rest of this loop (retry / fallback)
                    # treats it like any other failure.
                    raise RuntimeError(body_error)
                return result
            except Exception as e:
                last_exception = e
                error_str = str(e)[:150]

                # If the model itself is unavailable (deprecated/unknown), swap to
                # the fallback immediately and retry with the new model.
                if self._is_model_unavailable_error(error_str) and not self._fallback_active:
                    if await self._activate_fallback(error_str):
                        logger.warning(
                            "%s retrying with fallback model after primary unavailable: %s",
                            operation_name, error_str,
                        )
                        continue

                # Check if this is a retryable error
                retryable_keywords = [
                    "connection", "timeout", "rate limit", "429", "503", "502",
                    "500", "network", "reset", "refused", "unavailable", "overloaded"
                ]
                is_retryable = any(kw in error_str.lower() for kw in retryable_keywords)

                if attempt < max_retries - 1 and is_retryable:
                    delay = base_delay * (2 ** attempt)  # Exponential backoff
                    logger.warning(
                        "%s attempt %d/%d failed: %s. Retrying in %.1fs...",
                        operation_name, attempt + 1, max_retries, error_str, delay
                    )
                    await asyncio.sleep(delay)
                elif attempt < max_retries - 1:
                    # Non-retryable error, but still retry once more with shorter delay
                    delay = base_delay
                    logger.warning(
                        "%s attempt %d/%d failed (non-retryable): %s. Trying once more in %.1fs...",
                        operation_name, attempt + 1, max_retries, error_str, delay
                    )
                    await asyncio.sleep(delay)
                else:
                    logger.error(
                        "%s failed after %d attempts: %s",
                        operation_name, max_retries, error_str
                    )

        if last_exception is None:
            raise RuntimeError(
                f"{operation_name} failed without executing any attempts "
                f"(max_retries={max_retries})"
            )
        raise last_exception

    async def _request_and_parse(
        self,
        make_api_call_factory,
        parse_response,
        operation_name: str,
        *,
        initial_max_tokens: Optional[int] = None,
        truncation_retries: int = 2,
    ):
        """Call the API and parse the structured reply, retrying with a doubled
        max_tokens budget when the reply is truncated or unparseable.

        Reasoning models can spend the whole token budget "thinking" and get
        cut off (finish_reason="length") before emitting the tool call, so a
        larger budget on retry is the reliable fix. If every attempt is
        unparseable the last parse error propagates, making the caller record
        a Failed row instead of a silent default classification.

        Returns (classification, response_data) from the first parseable reply.
        """
        budget = int(initial_max_tokens or self.max_tokens)
        last_exception: Optional[Exception] = None
        for attempt in range(truncation_retries + 1):
            response = await self._call_api_with_retry(
                make_api_call_factory(budget),
                max_retries=self.max_retries,
                base_delay=2.0,
                operation_name=operation_name,
            )
            response_data = response.model_dump()
            self.request_count += 1
            if response.usage and response.usage.total_tokens:
                self.total_tokens_used += response.usage.total_tokens
            try:
                return parse_response(response_data), response_data
            except ValueError as exc:
                last_exception = exc
                finish_reason = self._finish_reason(response_data)
                if attempt < truncation_retries:
                    budget *= 2
                    logger.warning(
                        "%s returned an unparseable response (finish_reason=%s): %s; "
                        "retrying with max_tokens=%d",
                        operation_name, finish_reason, exc, budget,
                    )
                else:
                    logger.error(
                        "%s produced no parseable response after %d attempts "
                        "(last finish_reason=%s)",
                        operation_name, truncation_retries + 1, finish_reason,
                    )
        raise last_exception

    async def classify_site(
        self,
        domain: str,
        content: str = "",
        title: str = "",
        meta_description: str = "",
        content_source: str = "scrape",
    ) -> Dict[str, Any]:
        """Classify a website using the configured Gemini model.

        *content_source* is "scrape" when *content* is the site's own page
        text, or "research" when it is an external research summary about the
        site (used for sites that block every crawler).
        """
        start_time = time.time()

        try:
            prompt = self._build_classification_prompt(
                domain, content, title, meta_description,
                content_source=content_source,
            )

            if not self._client:
                raise RuntimeError("OpenRouter client has not been initialised")

            # Define the API call as a coroutine factory for the retry wrapper;
            # max_tokens is parameterized so truncated replies retry larger.
            def make_api_call_factory(budget: int):
                async def make_api_call():
                    return await self._client.chat.completions.create(
                        model=self.model,
                        messages=[
                            {
                                "role": "system",
                                "content": "You are a professional website quality analyst. Provide accurate, consistent classifications based on the given criteria.",
                            },
                            {
                                "role": "user",
                                "content": prompt,
                            },
                        ],
                        tools=[self._CLASSIFICATION_SCHEMA],
                        tool_choice={"type": "function", "function": {"name": "classify_website"}},
                        temperature=self.temperature,
                        max_tokens=budget,
                        top_p=0.9,
                        frequency_penalty=0.1,
                        presence_penalty=0.1,
                    )
                return make_api_call

            classification, response_data = await self._request_and_parse(
                make_api_call_factory,
                self._parse_classification_response,
                operation_name=f"Site classification ({domain})",
            )

            processing_time = time.time() - start_time

            return {
                "success": True,
                "quality": classification.get("quality", "Unknown"),
                "justification": classification.get("justification", "No justification provided"),
                "vertical": classification.get("vertical", "Unknown"),
                "vertical_tier_1": classification.get("vertical_tier_1", ""),
                "vertical_tier_2": classification.get("vertical_tier_2", ""),
                "vertical_tier_3": classification.get("vertical_tier_3", ""),
                "description": classification.get("description", "No description provided"),
                "confidence": classification.get("confidence", "Medium"),
                "language": classification.get("language", "Unknown"),
                "political_leaning": classification.get("political_leaning", "Non-Political"),
                "audience_size": classification.get("audience_size", "S"),
                "processing_time": processing_time,
                "tokens_used": self._usage_tokens(response_data),
                "source": "openrouter"
            }

        except Exception as exc:
            processing_time = time.time() - start_time
            logger.error("Failed to classify %s via OpenRouter: %s", domain, exc)
            return {
                "success": False,
                "error": str(exc),
                "processing_time": processing_time,
                "quality": "Error",
                "justification": f"Classification failed: {exc}",
                "vertical": "Unknown",
                "vertical_tier_1": "",
                "vertical_tier_2": "",
                "vertical_tier_3": "",
                "description": "N/A",
                "language": "Unknown",
                "political_leaning": "Non-Political",
                "audience_size": "Unknown",
                "source": "openrouter"
            }

    def _build_classification_prompt(
        self,
        domain: str,
        content: str,
        title: str,
        meta_description: str,
        content_source: str = "scrape",
    ) -> str:
        """Build the classification prompt for the LLM."""

        # Research summaries are denser than page text; give them more room.
        max_content_length = 3000 if content_source == "research" else 2000
        if len(content) > max_content_length:
            content = content[:max_content_length] + "..."

        # Load taxonomy dynamically from TSV file
        taxonomy_list = self._get_taxonomy_list_for_prompt()

        if content_source == "research":
            info_block = f"""WEBSITE INFORMATION:
Domain: {domain}
NOTE: This site could not be crawled directly (it blocks automated access), so no on-page content is available. The text below is a research summary about the site compiled from public web sources. Base your classification on this research combined with your own knowledge of the domain. Blocking crawlers is common among major premium publishers and says nothing negative about quality. If the research indicates the domain is parked, defunct, or has no meaningful content, classify it as Long Tail with Low confidence.
Research Summary: {content}"""
        else:
            info_block = f"""WEBSITE INFORMATION:
Domain: {domain}
Title: {title}
Meta Description: {meta_description}
Content Sample: {content}"""

        prompt = f"""You are an expert website quality analyst specializing in advertising quality, editorial credibility, and user experience evaluation.
Your task is to classify websites into quality tiers for advertising purposes and assign the most accurate category from the approved taxonomy based on site content.

QUALITY TIERS:
- Premium: The "best of the best" available programmatically. Represented by trusted publishers, major media networks, and highly respected independent brands. High editorial standards and reputable, original content. Large, loyal, and engaged audiences.
  Examples: nytimes.com, espn.com, bbc.com, forbes.com, nationalgeographic.com, wired.com, vogue.com, mayoclinic.org, wsj.com, cnn.com, theguardian.com, bloomberg.com, techcrunch.com, travelandleisure.com, healthline.com, people.com, iheart.com

- Standard: Web properties that are acceptable in programmatic but not considered elite. Legitimate, well-constructed content serving a clear purpose. May have less editorial oversight, sometimes including user-generated content. Niche blogs, small publications, or moderately authoritative sites. Functional but unremarkable user experience.
  Examples: local news sites, regional sports blogs, established hobby forums, niche industry publications, mid-tier lifestyle blogs, community news sites, specialized how-to sites, smaller e-commerce stores, professional association websites, educational institution blogs

- Long Tail: Low Quality. Suspicious or low-value properties primarily built for monetization. Excessive ad density and poor UX. Clickbait headlines, recycled or AI-generated articles. Multiple pop-ups, redirects, or disruptive design patterns. Outdated layouts or compromised user trust.
  Examples: content farms, clickbait aggregators, sites with excessive pop-ups, auto-generated content sites, scraper sites that republish content from other sources, sites with misleading headlines, pages dominated by ads with minimal content, suspicious download sites, low-effort affiliate marketing sites, parked domains with generic content

{info_block}

ANALYSIS REQUIREMENTS:
1. Evaluate content quality, editorial standards, and user experience
2. Look for grammar, originality, readability, design, navigation, and mobile usability.
3. Consider domain authority and brand recognition
4. Well-known brands, major publishers, and long-established domains generally rank higher.
5. Assess advertising suitability and brand safety
6. Check for harmful, illegal, or adult content.
7. Identify whether the content is MFA (made-for-advertising).
8. Classify the site's primary advertising vertical using the provided category list.
9. Choose the most relevant category label.
10. If multiple categories apply, select the dominant one that best reflects the site's core focus.
11. Provide confidence level (High / Medium / Low)
12. Detect the primary language of the content (e.g., English, Spanish, French, Chinese, Multilingual, etc.)
13. Assess political leaning using the 8-point scale: Far Left, Left, Center-Left, Center, Center-Right, Right, Far Right, or Non-Political
    - Far Left: Socialist/progressive advocacy, anti-capitalist themes
    - Left: Liberal/progressive perspectives, social justice focus
    - Center-Left: Moderate liberal viewpoints, pragmatic progressivism
    - Center: Balanced coverage, neutral/non-partisan approach
    - Center-Right: Moderate conservative perspectives, traditional values with pragmatism
    - Right: Conservative viewpoints, free market advocacy, traditional values
    - Far Right: Nationalist themes, hard conservative positions
    - Non-Political: No political content or bias detectable
14. Estimate audience size using t-shirt sizing (XS, S, M, L, XL)

AUDIENCE SIZE ESTIMATION GUIDE:
Your goal is to provide a conservative, ordinally correct estimate of audience size. You have a known bias to overestimate traffic, especially for well-known brands or high-quality content. Correct for this bias.

- Be Conservative: When in doubt, choose the smaller size. The vast majority of websites are small.
- Focus on Ordinal Rank: It is more important that you correctly rank sites relative to each other than to guess the absolute size perfectly.
- High Quality ≠ High Traffic: A beautiful, well-written corporate blog for a niche B2B product may have very few daily visitors (XS).
- Brand Recognition ≠ High Traffic: A famous brand's secondary site (e.g., a specific product page or a regional subdomain) may have low traffic (S or M), even if the main domain is huge.

T-Shirt Size Key (Daily Visitors):
- XS (0 - 100): The default. Niche blogs, personal projects, very small local businesses, inactive sites.
- S (100 - 1,000): Small but active. Established blogs, small e-commerce stores, local news.
- M (1,000 - 10,000): Medium-sized. Popular niche communities, smaller national news, well-known blogs.
- L (10,000 - 100,000): Large. Authoritative national publications, major e-commerce sites, popular tools.
- XL (100,000+): Massive, global scale. Household name media, top-tier social networks, major search engines. Use this size very sparingly.

PRIORITY CATEGORIZATION TOPICS:
When analyzing content, prioritize accurate categorization within these high-value topics:
- Entertainment (Movies, Music, Television, Celebrity Content)
- Sports (All sports categories including major leagues, college sports, fantasy sports)
- Family Focused (Parenting, Children's Content, Family Activities, Education)
- Health (Medical Health, Healthy Living, Wellness, Fitness, Nutrition)
- Lifestyle and Fashion (Style & Fashion, Beauty, Personal Care, Home & Garden)
- News (Current Events, Politics, Commentary, Civic Affairs)
- Technology (Technology & Computing, Consumer Electronics, Software, AI)
- Travel (All travel types, destinations, and travel planning)
- Business and Finance (Business, Economy, Personal Finance, Careers, Industries)

If the site clearly fits one of these priority topics, ensure it is categorized with the most specific and relevant IAB category from the approved list that matches the priority topic area. These topics represent high-value advertising verticals and should be identified with precision.

If the site does NOT clearly fit into any priority topic, proceed with standard IAB categorization using the full approved category list.

CATEGORY SELECTION - READ CAREFULLY:
1. **SELECT ONE CATEGORY ONLY** from the approved list in the APPENDIX below
2. **USE THE EXACT NAME** as shown in the list (copy it exactly, including & symbols and capitalization)
3. **NO COMMAS ALLOWED** - Never combine categories or use punctuation like commas or parentheses
4. **CHOOSE THE MOST SPECIFIC** - If there's a specific subcategory, use that instead of the broad parent
5. **FOR MIXED CONTENT** - Pick the PRIMARY focus, not multiple categories

VALID EXAMPLES:
✓ "Baseball"
✓ "Puzzle Video Games"
✓ "Technology & Computing"
✓ "Social Networking"

INVALID EXAMPLES (WILL CAUSE ERRORS):
✗ "Sports (Baseball,Football)"  ← NO COMMAS!
✗ "Entertainment - Games (Puzzle,Coloring)" ← NO COMMAS OR DASHES!
✗ "Puzzle" ← Not specific enough, use "Puzzle Video Games" or "Board Games and Puzzles"

CONFIDENCE LEVEL GUIDELINES:
High: Clear signals from domain, title, and content. Unambiguous category fit. Strong alignment with priority topics or well-defined IAB category.
Medium: Some ambiguity in focus or quality. Content spans multiple categories but primary focus is identifiable. Reasonable category match.
Low: Minimal signals, poor clarity, or conflicting indicators. Difficult to determine primary focus. Generic or placeholder content.

OUTPUT FORMAT (respond with exactly this structure):
Quality: <Premium|Standard|Long Tail>
Justification: <detailed reason for the quality rating>
Vertical: <SINGLE category name from the approved list - NO COMMAS>
Description: <one sentence describing the site's primary content/purpose>
Confidence: <High|Medium|Low>
Language: <primary language of content>
Political_Leaning: <Far Left|Left|Center-Left|Center|Center-Right|Right|Far Right|Non-Political>
Audience_Size: <XS|S|M|L|XL>

IMPORTANT: The Vertical field must contain ONLY ONE category name with NO commas or parentheses.

APPENDIX: APPROVED CATEGORY LIST
{taxonomy_list}

Analyze the website and provide your classification:"""

        return prompt

    # Sentinel the research model is told to emit when it finds nothing; its
    # presence keeps the item Failed instead of fabricating a classification.
    RESEARCH_INSUFFICIENT = "INSUFFICIENT INFORMATION"

    async def research_website(
        self,
        domain: str,
        research_model: str = "perplexity/sonar-pro",
        temperature: float = 0.2,
        max_tokens: int = 1500,
    ) -> Dict[str, Any]:
        """Research a website via a web-search-augmented model.

        Last rung of the auto-mode ladder: for sites every crawler failed on
        (typically hardened anti-bot publishers), gather public information
        about the domain so it can still be classified. The target site is
        never contacted from this host, so this pass adds zero ban risk.

        Returns a dict with success, research_content (empty when the model
        found nothing meaningful), and timing/token metadata.
        """
        start_time = time.time()

        try:
            prompt = self._build_website_research_prompt(domain)

            if not self._client:
                raise RuntimeError("OpenRouter client has not been initialised")

            async def make_api_call():
                return await self._client.chat.completions.create(
                    model=research_model,
                    messages=[
                        {
                            "role": "system",
                            "content": (
                                "You are a digital-media research analyst. Provide "
                                "accurate, sourced information about websites, their "
                                "publishers, content, reputation, and audience."
                            ),
                        },
                        {"role": "user", "content": prompt},
                    ],
                    temperature=temperature,
                    max_tokens=max_tokens,
                )

            response = await self._call_api_with_retry(
                make_api_call,
                max_retries=self.max_retries,
                base_delay=2.0,
                operation_name=f"Website research ({domain})",
            )

            response_data = response.model_dump()
            message = self._extract_message(response_data) or {}
            research_content = message.get("content") or ""

            processing_time = time.time() - start_time
            self.request_count += 1
            self.total_tokens_used += self._usage_tokens(response_data)

            insufficient = (
                self.RESEARCH_INSUFFICIENT in research_content.upper()
                or len(research_content.strip()) < 100
            )
            if insufficient:
                logger.info(
                    "Research found no meaningful information for %s", domain
                )
                research_content = ""

            return {
                "success": True,
                "research_content": research_content,
                "processing_time": processing_time,
                "tokens_used": self._usage_tokens(response_data),
                "model": research_model,
            }

        except Exception as exc:
            processing_time = time.time() - start_time
            logger.error("Failed to research website %s: %s", domain, exc)
            return {
                "success": False,
                "error": str(exc),
                "research_content": "",
                "processing_time": processing_time,
                "model": research_model,
            }

    def _build_website_research_prompt(self, domain: str) -> str:
        """Build the research prompt for a website that could not be scraped."""
        return f"""Research the website "{domain}" and summarize what is publicly known about it, for the purpose of classifying its advertising quality. Cover:

1. **Identity**: What is this website? Who owns or publishes it? (If the input includes a path, e.g. "example.com/travel", describe that section of the site.)
2. **Content**: Primary topics and content focus. What kind of articles/media does it publish?
3. **Editorial reputation**: Credibility, editorial standards, awards, or known controversies.
4. **Audience**: Approximate reach/traffic, geography, and the primary language of its content.
5. **Political leaning**: Any documented political orientation of its content (or none).
6. **Brand safety**: Any advertiser concerns (adult content, misinformation, piracy, excessive ads, made-for-advertising behavior).
7. **Status**: Is the site active today, or is the domain parked, defunct, or redirecting elsewhere?

Be factual and concise (under 400 words). If you cannot find any meaningful information about this domain, respond with exactly: {self.RESEARCH_INSUFFICIENT}"""

    def _parse_classification_response(
        self, response_data: Dict[str, Any], expected_name: str = "classify_website"
    ) -> Dict[str, str]:
        """Parse the classification response from the LLM using function calling.

        Raises ValueError whenever no classification can be recovered — a
        missing message, no tool_calls, and no inline JSON payload all count.
        Callers retry truncated replies with a larger token budget (see
        _request_and_parse) and otherwise mark the item as Failed; nothing
        here writes a silent default classification.
        """
        message = self._extract_message(response_data)
        if message is None:
            logger.error(
                "Classification response missing message; raw response: %s",
                json.dumps(response_data, default=str)[:1500],
            )
            raise ValueError("Response missing choices[0].message")

        try:
            arguments = self._extract_function_arguments(message, expected_name=expected_name)
            if arguments is not None:
                logger.debug(f"Extracted function call arguments: {arguments}")

                # Extract values from structured response
                quality = arguments.get("quality", "Standard")
                justification = arguments.get("justification", "No justification provided")
                vertical_raw = arguments.get("vertical", "Unknown")
                description = arguments.get("description", "No description provided")
                confidence = arguments.get("confidence", "Medium")
                language = arguments.get("language", "English")
                political_leaning = arguments.get("political_leaning", "Non-Political")
                audience_size = arguments.get("audience_size", "S")

                # Format vertical with taxonomy hierarchy
                tiers = self._format_vertical_with_taxonomy(vertical_raw)
                logger.debug(f"Vertical: '{vertical_raw}' -> Tiers: {tiers}")

                result = {
                    "quality": quality,
                    "justification": justification,
                    "vertical": vertical_raw,
                    "vertical_tier_1": tiers[0] if len(tiers) > 0 else "",
                    "vertical_tier_2": tiers[1] if len(tiers) > 1 else "",
                    "vertical_tier_3": tiers[2] if len(tiers) > 2 else "",
                    "description": description,
                    "confidence": confidence,
                    "language": language,
                    "political_leaning": political_leaning,
                    "audience_size": audience_size,
                }

                # Validate quality
                valid_qualities = ["Premium", "Standard", "Long Tail"]
                if result["quality"] not in valid_qualities:
                    logger.warning(f"Invalid quality value '{result['quality']}', defaulting to 'Standard'")
                    result["quality"] = "Standard"

                # Format vertical with hierarchy if we have tiers
                if result["vertical_tier_1"]:
                    result["vertical"] = " > ".join(
                        tier for tier in (
                            result["vertical_tier_1"],
                            result["vertical_tier_2"],
                            result["vertical_tier_3"],
                        )
                        if tier
                    )
                elif not result["vertical"] or result["vertical"].lower() in ["unknown", "n/a"]:
                    result["vertical"] = "Unknown"

                # Validate confidence
                valid_confidences = ["High", "Medium", "Low"]
                if result["confidence"] not in valid_confidences:
                    logger.warning(f"Invalid confidence value '{result['confidence']}', defaulting to 'Medium'")
                    result["confidence"] = "Medium"

                # Validate political_leaning
                valid_political_leanings = ["Far Left", "Left", "Center-Left", "Center", "Center-Right", "Right", "Far Right", "Non-Political"]
                if result["political_leaning"] not in valid_political_leanings:
                    logger.warning(f"Invalid political_leaning value '{result['political_leaning']}', defaulting to 'Non-Political'")
                    result["political_leaning"] = "Non-Political"

                # Ensure no empty values (except tier fields and fields with sensible defaults)
                skip_fields = {"vertical_tier_1", "vertical_tier_2", "vertical_tier_3", "language", "political_leaning", "audience_size"}
                for key, value in result.items():
                    if key in skip_fields:
                        if key.startswith("vertical_tier") and value in {None, "N/A", ""}:
                            result[key] = ""
                        continue
                    if not value or value in ["N/A", ""]:
                        result[key] = "Not provided"

                return result
            else:
                logger.error(
                    "No tool_calls or parseable content in classification response; raw response: %s",
                    json.dumps(response_data, default=str)[:1500],
                )
                raise ValueError("Response does not contain function call or parseable content")

        except (KeyError, IndexError, TypeError, ValueError, json.JSONDecodeError) as exc:
            logger.error(f"Failed to parse classification response: {exc}")
            raise ValueError(f"Unparseable classification response: {exc}") from exc

    def _format_vertical_with_taxonomy(self, vertical: str) -> List[str]:
        """Resolve the provided vertical label into taxonomy tiers."""
        if not vertical or vertical.strip().lower() in {"unknown", "n/a", "not provided"}:
            return []
        
        # Check if vertical contains field names (indicates parsing error)
        if any(field.lower() in vertical.lower() for field in ["description:", "confidence:", "quality:", "justification:"]):
            logger.warning(f"Vertical field contains other field names: '{vertical}', returning empty")
            return []

        taxonomy = self._load_taxonomy()
        first_line = vertical.splitlines()[0].strip()
        if not first_line:
            return []
        
        # If the LLM provided a hierarchy with separators, try to use it directly first
        if any(sep in first_line for sep in ['>', '|', '/']):
            # Split by common separators and clean up
            tiers = [token.strip() for token in re.split(r"[>\|/]+", first_line) if token.strip()]
            if len(tiers) > 1:
                # LLM provided a hierarchy, use it
                logger.debug(f"Using LLM-provided hierarchy: {tiers[:3]}")
                return tiers[:3]

        candidates = [first_line]

        for separator_pattern in (r"[>\|/,;]+", r"[()]"):
            for token in re.split(separator_pattern, first_line):
                token = token.strip()
                if token and token not in candidates:
                    candidates.append(token)

        def find_match(label: str) -> Optional[List[str]]:
            normalized = self._normalize_taxonomy_label(label)
            if not normalized:
                return None

            if taxonomy and normalized in taxonomy:
                return taxonomy[normalized]

            if taxonomy:
                best_key = None
                for key in taxonomy:
                    if normalized in key or key in normalized:
                        if best_key is None or len(key) > len(best_key):
                            best_key = key
                if best_key:
                    return taxonomy[best_key]

            return None

        for candidate in candidates:
            match = find_match(candidate)
            if match:
                return match[:3]

        fallback = [token.strip() for token in re.split(r"[>\|/,;]+", first_line) if token.strip()]
        return fallback[:3]

    @classmethod
    def _load_taxonomy(cls) -> Dict[str, List[str]]:
        """Load and cache the taxonomy data from the TSV file."""
        if cls._taxonomy_cache is not None:
            return cls._taxonomy_cache

        taxonomy_path = Path(__file__).resolve().parent / "content_taxonomy.tsv"
        if not taxonomy_path.exists():
            logger.warning("Taxonomy file %s not found", taxonomy_path)
            cls._taxonomy_cache = {}
            return cls._taxonomy_cache

        taxonomy_map: Dict[str, List[str]] = {}

        try:
            with taxonomy_path.open(encoding="utf-8") as taxonomy_file:
                next(taxonomy_file)
                header = next(taxonomy_file).strip().split("\t")
                reader = csv.DictReader(taxonomy_file, fieldnames=header, delimiter="\t")

                for row in reader:
                    if None in row:
                        row.pop(None)

                    name = (row.get("Name") or "").strip()
                    if not name:
                        continue

                    tiers: List[str] = []
                    for column in ("Tier 1", "Tier 2", "Tier 3", "Tier 4"):
                        value = (row.get(column) or "").strip()
                        if value and value not in tiers:
                            tiers.append(value)

                    if name and name not in tiers:
                        tiers.append(name)

                    for index, tier_name in enumerate(tiers, start=1):
                        normalized = cls._normalize_taxonomy_label(tier_name)
                        if not normalized:
                            continue

                        path = tiers[:index]
                        existing = taxonomy_map.get(normalized)
                        if not existing or len(path) > len(existing):
                            taxonomy_map[normalized] = path

        except (OSError, StopIteration) as exc:
            logger.error("Failed to load taxonomy file: %s", exc)
            taxonomy_map = {}

        cls._taxonomy_cache = taxonomy_map
        return cls._taxonomy_cache

    @staticmethod
    def _normalize_taxonomy_label(label: str) -> str:
        """Normalize taxonomy labels for matching."""
        normalized = label.lower().replace("&", "and")
        normalized = re.sub(r"[^a-z0-9]+", " ", normalized)
        normalized = re.sub(r"\s+", " ", normalized)
        return normalized.strip()

    @classmethod
    def _get_taxonomy_list_for_prompt(cls) -> str:
        """Generate a formatted list of valid categories from the taxonomy file for the LLM prompt."""
        taxonomy_path = Path(__file__).resolve().parent / "content_taxonomy.tsv"
        if not taxonomy_path.exists():
            logger.warning("Taxonomy file %s not found, using fallback categories", taxonomy_path)
            return "Sports\nNews\nEntertainment\nTechnology & Computing\n"

        # Build hierarchical structure from TSV
        categories = []
        tier1_groups = {}

        try:
            with taxonomy_path.open(encoding="utf-8") as f:
                # Skip first line (description), read header
                next(f)
                header_line = next(f).strip().split("\t")
                reader = csv.DictReader(f, fieldnames=header_line, delimiter="\t")

                for row in reader:
                    tier1 = (row.get("Tier 1") or "").strip()
                    tier2 = (row.get("Tier 2") or "").strip()
                    tier3 = (row.get("Tier 3") or "").strip()

                    if not tier1:
                        continue

                    if tier1 not in tier1_groups:
                        tier1_groups[tier1] = {}

                    if tier2:
                        if tier2 not in tier1_groups[tier1]:
                            tier1_groups[tier1][tier2] = []
                        if tier3 and tier3 not in tier1_groups[tier1][tier2]:
                            tier1_groups[tier1][tier2].append(tier3)

        except (OSError, StopIteration, csv.Error) as exc:
            logger.error("Failed to read taxonomy file: %s", exc)
            return "Sports\nNews\nEntertainment\nTechnology & Computing\n"

        # Format as hierarchical list
        output_lines = []
        for tier1 in sorted(tier1_groups.keys()):
            output_lines.append(f"• {tier1}")
            tier2_dict = tier1_groups[tier1]
            for tier2 in sorted(tier2_dict.keys()):
                output_lines.append(f"  - {tier2}")
                tier3_list = tier2_dict[tier2]
                for tier3 in sorted(tier3_list):
                    output_lines.append(f"    • {tier3}")

        return "\n".join(output_lines)

    # Function calling schema for app classification
    _APP_CLASSIFICATION_SCHEMA = {
        "type": "function",
        "function": {
            "name": "classify_app",
            "description": "Classify a mobile app based on quality tier, category, and other metadata",
            "parameters": {
                "type": "object",
                "properties": {
                    "quality": {
                        "type": "string",
                        "enum": ["Premium", "Standard", "Long Tail"],
                        "description": "The quality tier of the app: Premium (best of the best), Standard (acceptable but not elite), or Long Tail (low quality/suspicious)"
                    },
                    "justification": {
                        "type": "string",
                        "description": "Detailed explanation for the quality rating, considering app purpose, developer reputation, user reviews, and content quality"
                    },
                    "vertical": {
                        "type": "string",
                        "description": "A SINGLE category name from the approved IAB taxonomy. Must be an exact match from the taxonomy list. DO NOT use commas or combine categories. Examples: 'Video Gaming', 'Puzzle Video Games', 'Social Networking'"
                    },
                    "description": {
                        "type": "string",
                        "description": "A single sentence describing the app's primary purpose or function"
                    },
                    "confidence": {
                        "type": "string",
                        "enum": ["High", "Medium", "Low"],
                        "description": "Confidence level in the classification: High (clear signals), Medium (some ambiguity), or Low (minimal/conflicting signals)"
                    },
                    "language": {
                        "type": "string",
                        "description": "The primary language of the app content and interface (e.g., 'English', 'Spanish', 'French', 'Chinese', 'Multilingual', etc.)"
                    },
                    "political_leaning": {
                        "type": "string",
                        "enum": ["Far Left", "Left", "Center-Left", "Center", "Center-Right", "Right", "Far Right", "Non-Political"],
                        "description": "Political orientation of the app content for US political context: Far Left (socialist/progressive advocacy), Left (liberal/progressive), Center-Left (moderate liberal), Center (balanced/neutral), Center-Right (moderate conservative), Right (conservative), Far Right (nationalist/hard conservative), Non-Political (no political content)"
                    },
                    "audience_size": {
                        "type": "string",
                        "description": "Conservatively estimate the app's relative audience size. You have a strong tendency to overestimate. Be skeptical: high quality or brand recognition does not mean high usage. Most apps are XS or S. Use L/XL only for globally recognized apps. Use the following t-shirt sizes: XS, S, M, L, XL.",
                        "enum": ["XS", "S", "M", "L", "XL"]
                    }
                },
                "required": ["quality", "justification", "vertical", "description", "confidence", "language", "political_leaning", "audience_size"]
            }
        }
    }

    async def classify_app(
        self,
        app_id: str,
        app_name: str = "",
        developer: str = "",
        store_category: str = "",
        description: str = "",
        rating: float = 0.0,
        rating_count: int = 0,
        downloads: str = "",
        content_for_llm: str = "",
        platform: str = "ios",
    ) -> Dict[str, Any]:
        """
        Classify a mobile app using the configured LLM model.

        Args:
            app_id: The app's unique identifier (App Store ID or package name)
            app_name: The app's display name
            developer: The developer/publisher name
            store_category: The app's category in the store
            description: The app's description
            rating: The app's rating (0-5)
            rating_count: Number of ratings/reviews
            downloads: Download count (Android only)
            content_for_llm: Pre-formatted content for LLM
            platform: Platform type ("ios" or "android")

        Returns:
            Dictionary containing classification results
        """
        start_time = time.time()

        try:
            prompt = self._build_app_classification_prompt(
                app_id=app_id,
                app_name=app_name,
                developer=developer,
                store_category=store_category,
                description=description,
                rating=rating,
                rating_count=rating_count,
                downloads=downloads,
                content_for_llm=content_for_llm,
                platform=platform,
            )

            if not self._client:
                raise RuntimeError("OpenRouter client has not been initialised")

            # Define the API call as a coroutine factory for the retry wrapper;
            # max_tokens is parameterized so truncated replies retry larger.
            def make_api_call_factory(budget: int):
                async def make_api_call():
                    return await self._client.chat.completions.create(
                        model=self.model,
                        messages=[
                            {
                                "role": "system",
                                "content": "You are a professional mobile app quality analyst. Provide accurate, consistent classifications based on the given criteria.",
                            },
                            {
                                "role": "user",
                                "content": prompt,
                            },
                        ],
                        tools=[self._APP_CLASSIFICATION_SCHEMA],
                        tool_choice={"type": "function", "function": {"name": "classify_app"}},
                        temperature=self.temperature,
                        max_tokens=budget,
                        top_p=0.9,
                        frequency_penalty=0.1,
                        presence_penalty=0.1,
                    )
                return make_api_call

            classification, response_data = await self._request_and_parse(
                make_api_call_factory,
                lambda data: self._parse_classification_response(
                    data, expected_name="classify_app"
                ),
                operation_name=f"App classification ({app_id})",
            )

            processing_time = time.time() - start_time

            return {
                "success": True,
                "quality": classification.get("quality", "Unknown"),
                "justification": classification.get("justification", "No justification provided"),
                "vertical": classification.get("vertical", "Unknown"),
                "vertical_tier_1": classification.get("vertical_tier_1", ""),
                "vertical_tier_2": classification.get("vertical_tier_2", ""),
                "vertical_tier_3": classification.get("vertical_tier_3", ""),
                "description": classification.get("description", "No description provided"),
                "confidence": classification.get("confidence", "Medium"),
                "language": classification.get("language", "Unknown"),
                "political_leaning": classification.get("political_leaning", "Non-Political"),
                "audience_size": classification.get("audience_size", "S"),
                "processing_time": processing_time,
                "tokens_used": self._usage_tokens(response_data),
                "source": "openrouter"
            }

        except Exception as exc:
            processing_time = time.time() - start_time
            logger.error("Failed to classify app %s via OpenRouter: %s", app_id, exc)
            return {
                "success": False,
                "error": str(exc),
                "processing_time": processing_time,
                "quality": "Error",
                "justification": f"Classification failed: {exc}",
                "vertical": "Unknown",
                "vertical_tier_1": "",
                "vertical_tier_2": "",
                "vertical_tier_3": "",
                "description": "N/A",
                "language": "Unknown",
                "political_leaning": "Non-Political",
                "audience_size": "Unknown",
                "source": "openrouter"
            }

    def _build_app_classification_prompt(
        self,
        app_id: str,
        app_name: str,
        developer: str,
        store_category: str,
        description: str,
        rating: float,
        rating_count: int,
        downloads: str,
        content_for_llm: str,
        platform: str,
    ) -> str:
        """Build the classification prompt for app classification."""

        # Truncate description if too long
        max_content_length = 2000
        if len(description) > max_content_length:
            description = description[:max_content_length] + "..."

        platform_name = "iOS App Store" if platform == "ios" else "Google Play Store"

        # Load taxonomy dynamically from TSV file
        taxonomy_list = self._get_taxonomy_list_for_prompt()

        prompt = f"""You are an expert mobile app quality analyst specializing in advertising quality, developer credibility, and user experience evaluation.
Your task is to classify mobile apps into quality tiers for advertising purposes and assign the most accurate category from the approved taxonomy based on app content.

IMPORTANT: App store metrics (ratings, reviews, downloads) can be manipulated by developers through fake reviews, incentivized ratings, and misleading categories.
Your analysis should focus on the actual app description, stated purpose, and developer reputation rather than solely relying on store metrics.

QUALITY TIERS:
- Premium: High-quality apps from reputable developers or well-known brands. Clear purpose, professional presentation, genuine utility or entertainment value. Strong developer track record.
  Examples: Apps from major tech companies (Google, Microsoft, Meta, X), established game studios (EA, Supercell), major media brands (Netflix, Spotify, Disney+), trusted financial institutions, or highly-rated independent apps with genuine user engagement.

- Standard: Legitimate apps that serve a clear purpose but may not be from top-tier developers. Functional, acceptable for advertising, but not elite. May have some rough edges or less sophisticated design.
  Examples: Smaller utility apps, regional service apps, niche hobby apps, indie games, educational apps from smaller publishers, lifestyle apps with genuine functionality.

- Long Tail: Low-quality or suspicious apps. May include: excessive in-app advertising, misleading descriptions, copycat apps, apps with privacy concerns, apps primarily designed for monetization rather than user value, abandoned or poorly maintained apps.
  Examples: Ad-heavy clones of popular apps, apps with misleading names/icons, apps with suspicious permissions, apps with fake or purchased reviews, abandoned apps with no recent updates.

APP INFORMATION:
Platform: {platform_name}
App ID: {app_id}
App Name: {app_name}
Developer: {developer}
Store Category: {store_category}
Rating: {rating:.1f}/5.0 ({rating_count:,} reviews){"" if not downloads else f'''
Downloads: {downloads}'''}

Description:
{description if description else "(No description available)"}

{f"Additional Context:{chr(10)}{content_for_llm}" if content_for_llm and content_for_llm != description else ""}

ANALYSIS REQUIREMENTS:
1. Evaluate the app's stated purpose and whether it provides genuine value to users
2. Assess the developer's reputation and track record (if identifiable)
3. Look for red flags: excessive monetization mentions, vague descriptions, copycat indicators
4. Consider whether the app would be brand-safe for advertising
5. Be skeptical of high ratings with low review counts (potential manipulation)
6. Classify the app's primary advertising vertical using the provided category list
7. Provide confidence level (High / Medium / Low)
8. Detect the primary language of the app content and interface (e.g., English, Spanish, French, Chinese, Multilingual, etc.)
9. Assess political leaning using the 8-point scale: Far Left, Left, Center-Left, Center, Center-Right, Right, Far Right, or Non-Political
   - Far Left: Socialist/progressive advocacy, anti-capitalist themes
   - Left: Liberal/progressive perspectives, social justice focus
   - Center-Left: Moderate liberal viewpoints, pragmatic progressivism
   - Center: Balanced coverage, neutral/non-partisan approach
   - Center-Right: Moderate conservative perspectives, traditional values with pragmatism
   - Right: Conservative viewpoints, free market advocacy, traditional values
   - Far Right: Nationalist themes, hard conservative positions
   - Non-Political: No political content or bias detectable
10. Estimate audience size using t-shirt sizing (XS, S, M, L, XL)

AUDIENCE SIZE ESTIMATION GUIDE:
Your goal is to provide a conservative, ordinally correct estimate of audience size. You have a known bias to overestimate usage, especially for well-known brands or high-quality apps. Correct for this bias.

- Be Conservative: When in doubt, choose the smaller size. The vast majority of apps are small.
- Focus on Ordinal Rank: It is more important that you correctly rank apps relative to each other than to guess the absolute size perfectly.
- High Quality ≠ High Usage: A beautifully designed niche productivity app may have very few daily users (XS).
- Brand Recognition ≠ High Usage: A famous brand's secondary app (e.g., a companion app or regional variant) may have low usage (S or M), even if the main app is huge.

T-Shirt Size Key (Daily Active Users):
- XS (0 - 100): The default. Niche utilities, personal projects, very small local business apps, inactive apps.
- S (100 - 1,000): Small but active. Established niche apps, small business apps, local service apps.
- M (1,000 - 10,000): Medium-sized. Popular niche apps, regional apps, well-known indie apps.
- L (10,000 - 100,000): Large. Major utility apps, popular games, well-known service apps.
- XL (100,000+): Massive, global scale. Household name apps, top-tier social networks, major platform apps. Use this size very sparingly.

PRIORITY CATEGORIZATION TOPICS:
- Entertainment (Movies, Music, Television, Games)
- Sports (All sports categories including major leagues, fantasy sports)
- Family Focused (Parenting, Children's Content, Educational)
- Health (Medical, Fitness, Wellness, Nutrition)
- Lifestyle and Fashion (Style, Beauty, Personal Care)
- News (Current Events, Politics)
- Technology (Computing, Software, AI)
- Travel (All travel types and planning)
- Business and Finance (Business tools, Personal Finance)
- Shopping (E-commerce, Retail)
- Social (Social networking, Communication)

CATEGORY SELECTION - READ CAREFULLY:
1. **SELECT ONE CATEGORY ONLY** from the approved list in the APPENDIX below
2. **USE THE EXACT NAME** as shown in the list (copy it exactly, including & symbols and capitalization)
3. **NO COMMAS ALLOWED** - Never combine categories or use punctuation like commas or parentheses
4. **CHOOSE THE MOST SPECIFIC** - If there's a specific subcategory, use that instead of the broad parent
5. **FOR MIXED CONTENT** - Pick the PRIMARY function, not multiple categories

VALID EXAMPLES:
✓ "Puzzle Video Games"
✓ "Social Networking"
✓ "Fitness and Exercise"
✓ "Maps & Navigation"

INVALID EXAMPLES (WILL CAUSE ERRORS):
✗ "Games (Puzzle,Casual)"  ← NO COMMAS!
✗ "Entertainment - Games (Puzzle,Coloring)" ← NO COMMAS OR DASHES!
✗ "Video Games" ← Not specific enough, use "Video Gaming" or a specific genre

APPENDIX: APPROVED CATEGORY LIST
{taxonomy_list}

Analyze the app and provide your classification:"""

        return prompt

    async def test_connection(self) -> bool:
        """Test the connection to OpenRouter API.

        If the primary model is unavailable and a fallback is configured, swap
        to the fallback and retry once before reporting failure.
        """
        if not self._client:
            logger.error("OpenRouter API connection test failed: client not initialised")
            return False

        async def _attempt() -> bool:
            test_prompt = "Respond with 'OK' if you can read this message."
            response = await self._client.chat.completions.create(
                model=self.model,
                messages=[
                    {
                        "role": "system",
                        "content": "You are a professional website quality analyst.",
                    },
                    {"role": "user", "content": test_prompt},
                ],
                temperature=0.0,
                max_tokens=16,
            )

            body_error = self._extract_body_error(response)
            if body_error:
                raise RuntimeError(body_error)

            response_data = response.model_dump()
            choices = response_data.get("choices") if isinstance(response_data, dict) else None
            if choices:
                logger.info(
                    "OpenRouter API connection test successful (model=%s)", self.model,
                )
                return True

            logger.error("OpenRouter API connection test failed: Invalid response format")
            return False

        try:
            return await _attempt()
        except Exception as exc:
            logger.error(
                "OpenRouter API connection test failed for model %r: %s",
                self.model, exc,
            )
            if self._is_model_unavailable_error(str(exc)) and not self._fallback_active:
                if await self._activate_fallback(str(exc)):
                    try:
                        return await _attempt()
                    except Exception as fallback_exc:
                        logger.error(
                            "OpenRouter API fallback connection test failed for model %r: %s",
                            self.model, fallback_exc,
                        )
            return False

    def get_usage_stats(self) -> Dict[str, Any]:
        """Get usage statistics for the client."""
        return {
            "requests_made": self.request_count,
            "total_tokens_used": self.total_tokens_used,
            "model": self.model,
            "average_tokens_per_request": (
                self.total_tokens_used / self.request_count
                if self.request_count > 0 else 0
            )
        }

    async def batch_classify(self, sites: list, max_concurrent: int = 5) -> list:
        """Classify multiple sites concurrently with rate limiting."""
        semaphore = asyncio.Semaphore(max_concurrent)

        async def classify_with_semaphore(site_data):
            async with semaphore:
                return await self.classify_site(**site_data)

        tasks = [classify_with_semaphore(site) for site in sites]
        return await asyncio.gather(*tasks, return_exceptions=True)

    def get_mode(self) -> str:
        """Return the current classification mode."""
        return "openrouter"

    # CTV (Connected TV) Classification Schema for structured classification output
    _CTV_CLASSIFICATION_SCHEMA = {
        "type": "function",
        "function": {
            "name": "classify_ctv_app",
            "description": "Classify a CTV (Connected TV) app based on quality tier, category, and other metadata",
            "parameters": {
                "type": "object",
                "properties": {
                    "quality": {
                        "type": "string",
                        "enum": ["Premium", "Standard", "Long Tail"],
                        "description": "The quality tier of the CTV app based on RECOGNIZABILITY: Premium (only major nationally/internationally recognizable brands like NBC, ESPN, Peacock, NFL), Standard (default for most CTV apps including local stations, regional networks, niche content), or Long Tail (rarely used for CTV - almost no CTV apps qualify)"
                    },
                    "justification": {
                        "type": "string",
                        "description": "Detailed explanation for the quality rating, considering content quality, network affiliation, brand recognition, and viewer engagement"
                    },
                    "vertical": {
                        "type": "string",
                        "description": "A SINGLE category name from the approved IAB taxonomy. Must be an exact match from the taxonomy list. DO NOT use commas or combine categories."
                    },
                    "description": {
                        "type": "string",
                        "description": "A single sentence describing the CTV app's primary content or purpose"
                    },
                    "target_audience": {
                        "type": "string",
                        "description": "Description of the target audience (e.g., 'General entertainment seekers', 'Sports enthusiasts', 'News viewers', 'Children and families')"
                    },
                    "content_type": {
                        "type": "string",
                        "enum": ["Live TV", "On-Demand Streaming", "Live Sports", "News", "Music/Audio", "Kids Content", "Fitness", "Educational", "Gaming", "Mixed Content"],
                        "description": "The primary type of content offered by the CTV app"
                    },
                    "network_affiliation": {
                        "type": "string",
                        "description": "Network affiliation if any (e.g., 'NBC', 'CBS', 'ABC', 'Fox', 'Independent', 'None')"
                    },
                    "confidence": {
                        "type": "string",
                        "enum": ["High", "Medium", "Low"],
                        "description": "Confidence level in the classification: High (clear signals), Medium (some ambiguity), or Low (minimal/conflicting signals)"
                    },
                    "language": {
                        "type": "string",
                        "description": "The primary language of the content (e.g., 'English', 'Spanish', 'Multilingual')"
                    },
                    "political_leaning": {
                        "type": "string",
                        "enum": ["Far Left", "Left", "Center-Left", "Center", "Center-Right", "Right", "Far Right", "Non-Political"],
                        "description": "Political orientation of the content"
                    },
                    "audience_size": {
                        "type": "string",
                        "description": "Conservatively estimate the CTV app's relative audience size. You have a strong tendency to overestimate. Be skeptical: high quality or brand recognition does not mean high viewership. Most CTV apps are XS or S. Use L/XL only for globally recognized streaming services. Use the following t-shirt sizes: XS, S, M, L, XL.",
                        "enum": ["XS", "S", "M", "L", "XL"]
                    }
                },
                "required": ["quality", "justification", "vertical", "description", "target_audience", "content_type", "network_affiliation", "confidence", "language", "political_leaning", "audience_size"]
            }
        }
    }

    async def research_ctv_app(
        self,
        app_name: str,
        bundle_id: str = "",
        platform: str = "",
        url: str = "",
        publisher: str = "",
        research_model: str = "perplexity/sonar-pro",
        temperature: float = 0.3,
        max_tokens: int = 2000,
    ) -> Dict[str, Any]:
        """
        Research a CTV app using Perplexity Sonar Pro for in-depth information gathering.

        This is the first step of the two-step CTV classification pipeline.
        It gathers detailed information about the CTV app including content,
        target audience, network affiliation, and more.

        Args:
            app_name: The CTV app's display name
            bundle_id: Optional bundle identifier
            platform: The CTV platform (Roku, Fire TV, Apple TV, etc.)
            url: Optional URL associated with the app
            publisher: Publisher name
            research_model: Model to use for research (default: perplexity/sonar-pro)
            temperature: Temperature for research model
            max_tokens: Maximum tokens for research response

        Returns:
            Dictionary containing research results
        """
        start_time = time.time()

        try:
            # Build the research prompt
            prompt = self._build_ctv_research_prompt(
                app_name=app_name,
                bundle_id=bundle_id,
                platform=platform,
                url=url,
                publisher=publisher,
            )

            if not self._client:
                raise RuntimeError("OpenRouter client has not been initialised")

            # Define the API call as a coroutine for retry wrapper
            async def make_api_call():
                return await self._client.chat.completions.create(
                    model=research_model,
                    messages=[
                        {
                            "role": "system",
                            "content": "You are a CTV (Connected TV) industry research analyst. Provide detailed, accurate information about CTV applications, their content offerings, network affiliations, and audience reach.",
                        },
                        {
                            "role": "user",
                            "content": prompt,
                        },
                    ],
                    temperature=temperature,
                    max_tokens=max_tokens,
                )

            # Call with retry logic
            response = await self._call_api_with_retry(
                make_api_call,
                max_retries=self.max_retries,
                base_delay=2.0,
                operation_name=f"CTV research ({app_name})",
            )

            response_data = response.model_dump()
            _research_msg = self._extract_message(response_data) or {}
            research_content = _research_msg.get("content") if isinstance(_research_msg, dict) else ""
            research_content = research_content or ""

            processing_time = time.time() - start_time

            self.request_count += 1
            if response.usage and response.usage.total_tokens:
                self.total_tokens_used += response.usage.total_tokens

            return {
                "success": True,
                "research_content": research_content,
                "processing_time": processing_time,
                "tokens_used": self._usage_tokens(response_data),
                "model": research_model,
            }

        except Exception as exc:
            processing_time = time.time() - start_time
            logger.error("Failed to research CTV app %s: %s", app_name, exc)
            return {
                "success": False,
                "error": str(exc),
                "research_content": "",
                "processing_time": processing_time,
                "model": research_model,
            }

    def _build_ctv_research_prompt(
        self,
        app_name: str,
        bundle_id: str,
        platform: str,
        url: str,
        publisher: str = "",
    ) -> str:
        """Build the research prompt for CTV app analysis."""
        prompt = f"""Research the following CTV (Connected TV) application and provide detailed information:

CTV APP INFORMATION:
App Name: {app_name}
{f"Bundle ID: {bundle_id}" if bundle_id else ""}
{f"Platform: {platform}" if platform else ""}
{f"Publisher/Network: {publisher}" if publisher else ""}
{f"Associated URL: {url}" if url else ""}

Please research and provide information on the following aspects:

1. **Content Overview**: What type of content does this CTV app offer? (streaming video, live TV, news, sports, music, etc.)

2. **Network/Publisher**: Who owns or operates this app? Is it affiliated with a major media network or studio?

3. **Target Audience**: Who is the primary target audience? (demographics, interests, viewing habits)

4. **Content Quality**: What is the production quality of the content? Is it original content, licensed content, or user-generated?

5. **Monetization Model**: How does the app monetize? (subscription, ad-supported, FAST channel, hybrid)

6. **Availability**: Which CTV platforms is this app available on? (Roku, Fire TV, Apple TV, Samsung TV+, etc.)

7. **Popularity & Reach**: What is the estimated viewership or user base? Any notable metrics or achievements?

8. **Brand Safety**: Are there any content concerns for advertisers? (adult content, controversial topics, etc.)

9. **Recent Developments**: Any recent news, updates, or changes to the app or its content offerings?

Provide a comprehensive research summary that would help classify this CTV app for advertising purposes."""

        return prompt

    async def classify_ctv_app(
        self,
        app_name: str,
        research_content: str,
        bundle_id: str = "",
        platform: str = "",
        url: str = "",
        publisher: str = "",
        classification_model: Optional[str] = None,
        temperature: float = 0.1,
        max_tokens: int = 1500,
    ) -> Dict[str, Any]:
        """
        Classify a CTV app using the research content from the first step.

        This is the second step of the two-step CTV classification pipeline.
        It uses a fast, cost-effective model to structure the research into
        a consistent classification format.

        Args:
            app_name: The CTV app's display name
            research_content: Research content from the first step
            bundle_id: Optional bundle identifier
            platform: The CTV platform
            url: Optional URL
            publisher: Publisher name
            classification_model: Deprecated. Retained for backwards compatibility,
                but the active model on this client (including any fallback swap)
                is always used for the API call.
            temperature: Temperature for classification model
            max_tokens: Maximum tokens for classification response

        Returns:
            Dictionary containing classification results
        """
        start_time = time.time()

        try:
            prompt = self._build_ctv_classification_prompt(
                app_name=app_name,
                research_content=research_content,
                bundle_id=bundle_id,
                platform=platform,
                url=url,
                publisher=publisher,
            )

            if not self._client:
                raise RuntimeError("OpenRouter client has not been initialised")

            # Define the API call as a coroutine factory for the retry wrapper.
            # Read self.model dynamically so a fallback swap mid-retry takes
            # effect; max_tokens is parameterized so truncated replies retry
            # larger.
            def make_api_call_factory(budget: int):
                async def make_api_call():
                    return await self._client.chat.completions.create(
                        model=self.model,
                        messages=[
                            {
                                "role": "system",
                                "content": "You are a CTV (Connected TV) advertising quality analyst. Classify CTV apps for advertising suitability based on the research provided.",
                            },
                            {
                                "role": "user",
                                "content": prompt,
                            },
                        ],
                        tools=[self._CTV_CLASSIFICATION_SCHEMA],
                        tool_choice={"type": "function", "function": {"name": "classify_ctv_app"}},
                        temperature=temperature,
                        max_tokens=budget,
                    )
                return make_api_call

            classification, response_data = await self._request_and_parse(
                make_api_call_factory,
                self._parse_ctv_classification_response,
                operation_name=f"CTV classification ({app_name})",
                initial_max_tokens=max_tokens,
            )

            # Apply CTV news routing to ensure consistent IAB tier mapping
            classification = self._apply_ctv_routing(
                app_name=app_name,
                research_content=research_content,
                result=classification,
            )

            processing_time = time.time() - start_time

            return {
                "success": True,
                "quality": classification.get("quality", "Unknown"),
                "justification": classification.get("justification", "No justification provided"),
                "vertical": classification.get("vertical", "Unknown"),
                "vertical_tier_1": classification.get("vertical_tier_1", ""),
                "vertical_tier_2": classification.get("vertical_tier_2", ""),
                "vertical_tier_3": classification.get("vertical_tier_3", ""),
                "description": classification.get("description", "No description provided"),
                "target_audience": classification.get("target_audience", "General audience"),
                "content_type": classification.get("content_type", "Mixed Content"),
                "network_affiliation": classification.get("network_affiliation", ""),
                "confidence": classification.get("confidence", "Medium"),
                "language": classification.get("language", "Unknown"),
                "political_leaning": classification.get("political_leaning", "Non-Political"),
                "audience_size": classification.get("audience_size", "S"),
                "processing_time": processing_time,
                "tokens_used": self._usage_tokens(response_data),
                "model": self.model,
            }

        except Exception as exc:
            processing_time = time.time() - start_time
            logger.error("Failed to classify CTV app %s: %s", app_name, exc)
            return {
                "success": False,
                "error": str(exc),
                "processing_time": processing_time,
                "quality": "Error",
                "justification": f"Classification failed: {exc}",
                "vertical": "Unknown",
                "vertical_tier_1": "",
                "vertical_tier_2": "",
                "vertical_tier_3": "",
                "description": "N/A",
                "target_audience": "Unknown",
                "content_type": "Unknown",
                "network_affiliation": "",
                "confidence": "Low",
                "language": "Unknown",
                "political_leaning": "Non-Political",
                "audience_size": "Unknown",
                "model": self.model,
            }

    def _build_ctv_classification_prompt(
        self,
        app_name: str,
        research_content: str,
        bundle_id: str,
        platform: str,
        url: str,
        publisher: str = "",
    ) -> str:
        """Build the classification prompt for CTV app categorization."""
        taxonomy_list = self._get_taxonomy_list_for_prompt()

        prompt = f"""Based on the research provided, classify this CTV (Connected TV) application for advertising purposes.

CTV APP INFORMATION:
App Name: {app_name}
{f"Bundle ID: {bundle_id}" if bundle_id else ""}
{f"Platform: {platform}" if platform else ""}
{f"Publisher: {publisher}" if publisher else ""}
{f"URL: {url}" if url else ""}

RESEARCH FINDINGS:
{research_content}

QUALITY TIERS FOR CTV APPS (RECOGNIZABILITY IS THE PRIMARY FACTOR):

- Premium: ONLY for major, nationally/internationally recognizable brands. This is a high bar.
  * Major broadcast networks: NBC, CBS, ABC, FOX, PBS
  * Major cable networks: ESPN, CNN, MSNBC, Fox News, HGTV, Discovery
  * Major streaming platforms: Peacock, Paramount+, Pluto TV, Tubi
  * Major sports leagues: NFL, NBA, MLB, NHL
  * Examples: ESPN, NBC, CNN, Peacock, NFL

- Standard: THE DEFAULT for most CTV apps. This includes:
  * Local news stations (even if affiliated with major networks like "10 Tampa Bay" or "KGW Portland")
  * Regional sports networks
  * Niche content apps
  * Most CTV apps are advertising-focused and should be Standard
  * Examples: KARE 11, WFLA News, regional sports apps, specialized content channels

- Long Tail: RARELY used for CTV. Almost no CTV apps qualify since they require significant effort to publish (unlike websites which can be created easily). Only use for truly low-quality or suspicious apps.

CLASSIFICATION REQUIREMENTS:
1. Assign a quality tier based on brand recognition, content quality, and reach
2. Select ONE IAB category that best describes the content from the list below
3. Identify the content type (e.g., Live TV, News, Sports, On-Demand Streaming)
4. Describe the target audience
5. Note any network affiliation
6. Identify primary language
7. Assess political leaning if applicable
8. Estimate audience size using t-shirt sizing (XS, S, M, L, XL)

AUDIENCE SIZE ESTIMATION GUIDE:
Your goal is to provide a conservative, ordinally correct estimate of audience size. You have a known bias to overestimate viewership, especially for well-known brands or high-quality content. Correct for this bias.

- Be Conservative: When in doubt, choose the smaller size. The vast majority of CTV apps are small.
- Focus on Ordinal Rank: It is more important that you correctly rank apps relative to each other than to guess the absolute size perfectly.
- High Quality ≠ High Viewership: A well-produced niche streaming app may have very few daily viewers (XS).
- Brand Recognition ≠ High Viewership: A famous network's regional app or secondary channel may have low viewership (S or M), even if the main network is huge.

T-Shirt Size Key (Daily Viewers):
- XS (0 - 100): The default. Niche streaming apps, regional content, inactive apps.
- S (100 - 1,000): Small but active. Local news apps, regional sports, specialized content.
- M (1,000 - 10,000): Medium-sized. Popular regional apps, well-known niche content.
- L (10,000 - 100,000): Large. Major regional networks, popular cable network apps.
- XL (100,000+): Massive, national/global scale. Major broadcast networks, top-tier streaming platforms. Use this size very sparingly.

APPENDIX: APPROVED CATEGORY LIST
{taxonomy_list}

Analyze the CTV app and provide your classification using the classify_ctv_app function."""

        return prompt

    def _parse_ctv_classification_response(self, response_data: Dict[str, Any]) -> Dict[str, str]:
        """Parse the CTV classification response from the LLM.

        Raises ValueError if the response has no usable message at all so the
        caller writes a Failed row rather than silently defaulting to Standard.
        See _parse_classification_response for the same rationale.
        """
        message = self._extract_message(response_data)
        if message is None:
            logger.error(
                "CTV classification response missing message; raw response: %s",
                json.dumps(response_data, default=str)[:1500],
            )
            raise ValueError("Response missing choices[0].message")

        try:
            arguments = self._extract_function_arguments(message, expected_name="classify_ctv_app")
            if arguments is not None:
                logger.debug(f"Extracted CTV classification arguments: {arguments}")

                # Extract values from structured response
                quality = arguments.get("quality", "Standard")
                justification = arguments.get("justification", "No justification provided")
                vertical_raw = arguments.get("vertical", "Unknown")
                description = arguments.get("description", "No description provided")
                target_audience = arguments.get("target_audience", "General audience")
                content_type = arguments.get("content_type", "Mixed Content")
                network_affiliation = arguments.get("network_affiliation", "")
                confidence = arguments.get("confidence", "Medium")
                language = arguments.get("language", "English")
                political_leaning = arguments.get("political_leaning", "Non-Political")
                audience_size = arguments.get("audience_size", "S")

                # Format vertical with taxonomy hierarchy
                tiers = self._format_vertical_with_taxonomy(vertical_raw)

                result = {
                    "quality": quality,
                    "justification": justification,
                    "vertical": vertical_raw,
                    "vertical_tier_1": tiers[0] if len(tiers) > 0 else "",
                    "vertical_tier_2": tiers[1] if len(tiers) > 1 else "",
                    "vertical_tier_3": tiers[2] if len(tiers) > 2 else "",
                    "description": description,
                    "target_audience": target_audience,
                    "content_type": content_type,
                    "network_affiliation": network_affiliation,
                    "confidence": confidence,
                    "language": language,
                    "political_leaning": political_leaning,
                    "audience_size": audience_size,
                }

                # Validate quality
                valid_qualities = ["Premium", "Standard", "Long Tail"]
                if result["quality"] not in valid_qualities:
                    logger.warning(f"Invalid quality value '{result['quality']}', defaulting to 'Standard'")
                    result["quality"] = "Standard"

                # Format vertical with hierarchy if we have tiers
                if result["vertical_tier_1"]:
                    result["vertical"] = " > ".join(
                        tier for tier in (
                            result["vertical_tier_1"],
                            result["vertical_tier_2"],
                            result["vertical_tier_3"],
                        )
                        if tier
                    )

                # Validate confidence
                valid_confidences = ["High", "Medium", "Low"]
                if result["confidence"] not in valid_confidences:
                    logger.warning(f"Invalid confidence value '{result['confidence']}', defaulting to 'Medium'")
                    result["confidence"] = "Medium"

                # Validate content_type
                valid_content_types = [
                    "Live TV", "On-Demand Streaming", "Live Sports", "News",
                    "Music/Audio", "Kids Content", "Fitness", "Educational",
                    "Gaming", "Mixed Content"
                ]
                if result["content_type"] not in valid_content_types:
                    logger.warning(f"Invalid content_type value '{result['content_type']}', defaulting to 'Mixed Content'")
                    result["content_type"] = "Mixed Content"

                # Validate political_leaning
                valid_political_leanings = [
                    "Far Left", "Left", "Center-Left", "Center",
                    "Center-Right", "Right", "Far Right", "Non-Political"
                ]
                if result["political_leaning"] not in valid_political_leanings:
                    logger.warning(f"Invalid political_leaning value '{result['political_leaning']}', defaulting to 'Non-Political'")
                    result["political_leaning"] = "Non-Political"

                return result
            else:
                logger.error(
                    "No tool_calls or parseable content in CTV response; raw response: %s",
                    json.dumps(response_data, default=str)[:1500],
                )
                raise ValueError("Response does not contain function call or parseable content")

        except (KeyError, IndexError, TypeError, ValueError, json.JSONDecodeError) as exc:
            logger.error(f"Failed to parse CTV classification response: {exc}")
            raise ValueError(f"Unparseable CTV classification response: {exc}") from exc

    def _apply_ctv_routing(
        self,
        app_name: str,
        research_content: str,
        result: Dict[str, Any],
    ) -> Dict[str, Any]:
        """
        Apply deterministic post-processing routing for CTV news apps.

        The IAB Content Taxonomy does not have a Tier-1 "News" category, so general
        news apps must be mapped to appropriate IAB tiers. This function detects
        news-related content and routes to the most appropriate taxonomy path.

        Routing rules:
        - General/mixed news → Politics > Civic Affairs
        - Election-focused news → Politics > Elections
        - Weather-focused news → Science > Weather
        - Crime-focused news → Crime
        - Disaster/emergency news → Disasters

        Additionally corrects Political_Leaning discrepancies:
        - If vertical_tier_1 == "Politics" AND political_leaning == "Non-Political",
          sets political_leaning to "Center" (news is political content but may be neutral).

        Args:
            app_name: The CTV app's display name
            research_content: Research content from the first step
            result: The classification result dictionary to modify

        Returns:
            Modified result dictionary with routing applied
        """
        # Make a copy to avoid modifying the original
        result = dict(result)

        # Check if this is a news app
        is_news_app = self._is_ctv_news_app(app_name, research_content, result)

        if not is_news_app:
            return result

        # Determine the specific news category based on content signals
        research_lower = research_content.lower()
        app_name_lower = app_name.lower()

        # Check for weather-focused apps ONLY if weather is the PRIMARY purpose
        # This should only match dedicated weather apps, not news apps that mention weather
        weather_app_names = [
            "weather channel", "accuweather", "weather underground", "weather.com",
            "weather bug", "weatherbug", "weather network", "weather nation",
            "local weather", "storm tracker", "radar", "wunderground"
        ]
        is_weather_app_name = any(name in app_name_lower for name in weather_app_names)

        # Only route to weather if app name explicitly indicates weather app
        # General news apps that mention weather should NOT be routed here
        if is_weather_app_name:
            # Verify it's not actually a general news app with "weather" in name
            general_news_signals = [
                "news", "breaking news", "headlines", "current events", "local news",
                "national news", "world news", "politics", "election", "newscast"
            ]
            news_signal_count = sum(1 for sig in general_news_signals if sig in research_lower)

            # Only route to weather if news signals are minimal (< 3)
            if news_signal_count < 3:
                result["vertical_tier_1"] = "Science"
                result["vertical_tier_2"] = "Weather"
                result["vertical_tier_3"] = ""
                result["vertical"] = "Science > Weather"
                result["justification"] = result.get("justification", "") + " (post-routed for CTV weather app)"
                # Weather apps can remain Non-Political
                return result

        # Check for crime-focused apps ONLY if crime is the PRIMARY purpose
        # This should only match dedicated crime/true crime apps
        crime_app_names = [
            "crime watch", "true crime", "crime tv", "court tv", "crime stories",
            "crime investigation", "unsolved mysteries", "dateline", "48 hours"
        ]
        is_crime_app_name = any(name in app_name_lower for name in crime_app_names)

        if is_crime_app_name:
            general_news_signals = [
                "news", "breaking news", "headlines", "current events", "local news",
                "national news", "world news", "politics", "election", "newscast"
            ]
            news_signal_count = sum(1 for sig in general_news_signals if sig in research_lower)

            # Only route to crime if news signals are minimal (< 3)
            if news_signal_count < 3:
                result["vertical_tier_1"] = "Crime"
                result["vertical_tier_2"] = ""
                result["vertical_tier_3"] = ""
                result["vertical"] = "Crime"
                result["justification"] = result.get("justification", "") + " (post-routed for CTV crime app)"
                # Crime apps can remain Non-Political
                return result

        # Check for disaster/emergency-focused apps ONLY if disaster is the PRIMARY purpose
        # This should only match dedicated emergency/disaster apps
        disaster_app_names = [
            "emergency alert", "fema", "red cross", "disaster", "emergency management",
            "emergency preparedness", "alert", "warning system"
        ]
        is_disaster_app_name = any(name in app_name_lower for name in disaster_app_names)

        if is_disaster_app_name:
            general_news_signals = [
                "news", "breaking news", "headlines", "current events", "local news",
                "national news", "world news", "politics", "election", "newscast"
            ]
            news_signal_count = sum(1 for sig in general_news_signals if sig in research_lower)

            # Only route to disasters if news signals are minimal (< 3)
            if news_signal_count < 3:
                result["vertical_tier_1"] = "Disasters"
                result["vertical_tier_2"] = ""
                result["vertical_tier_3"] = ""
                result["vertical"] = "Disasters"
                result["justification"] = result.get("justification", "") + " (post-routed for CTV disaster app)"
                # Disaster apps can remain Non-Political
                return result

        # Check for election-focused news - only if elections are prominently featured
        # Count strong election signals (these indicate election is a major focus)
        strong_election_signals = [
            "election coverage", "election results", "election night", "vote count",
            "ballot", "electoral college", "midterm election", "presidential election",
            "senate race", "house race", "gubernatorial", "primary election"
        ]
        election_signal_count = sum(1 for sig in strong_election_signals if sig in research_lower)

        # Also check for election-focused app names
        election_app_names = ["election", "vote", "ballot", "c-span", "cspan"]
        is_election_app_name = any(name in app_name_lower for name in election_app_names)

        # Route to Elections only if strong election signals OR election-focused app name
        if election_signal_count >= 2 or is_election_app_name:
            result["vertical_tier_1"] = "Politics"
            result["vertical_tier_2"] = "Elections"
            result["vertical_tier_3"] = ""
            result["vertical"] = "Politics > Elections"
            result["justification"] = result.get("justification", "") + " (post-routed for CTV election news)"
            # Apply political_leaning correction
            if result.get("political_leaning") == "Non-Political":
                result["political_leaning"] = "Center"
                result["justification"] = result.get("justification", "") + " Political_Leaning set to Center due to Politics vertical."
            return result

        # Default: General/mixed news → Politics > Civic Affairs
        result["vertical_tier_1"] = "Politics"
        result["vertical_tier_2"] = "Civic affairs"
        result["vertical_tier_3"] = ""
        result["vertical"] = "Politics > Civic affairs"
        result["justification"] = result.get("justification", "") + " (post-routed for CTV news)"

        # Apply political_leaning correction for Politics vertical
        if result.get("political_leaning") == "Non-Political":
            result["political_leaning"] = "Center"
            result["justification"] = result.get("justification", "") + " Political_Leaning set to Center due to Politics vertical."

        return result

    def _is_ctv_news_app(
        self,
        app_name: str,
        research_content: str,
        result: Dict[str, Any],
    ) -> bool:
        """
        Determine if a CTV app should be treated as a news app for routing purposes.

        Detection signals:
        1. content_type == "News" (explicit schema value)
        2. Strong news phrases in research_content
        3. News terms in app_name (major news networks)

        Exclusion: Apps that are primarily entertainment with only incidental news
        content should NOT be routed as news.

        Args:
            app_name: The CTV app's display name
            research_content: Research content from the first step
            result: The classification result dictionary

        Returns:
            True if the app should be treated as news, False otherwise
        """
        # Signal 1: Explicit content_type == "News"
        if result.get("content_type") == "News":
            # Check for entertainment override
            if not self._is_primarily_entertainment(research_content):
                return True

        research_lower = research_content.lower()
        app_name_lower = app_name.lower()

        # Signal 2: Strong news phrases in research_content
        strong_news_phrases = [
            "news", "breaking news", "live news", "local news", "national news",
            "headlines", "current events", "news channel", "news network",
            "24/7 news", "weather forecast", "election coverage", "news station",
            "news broadcast", "news programming", "newscast"
        ]
        news_phrase_count = sum(1 for phrase in strong_news_phrases if phrase in research_lower)

        # Signal 3: Major news network names in app_name
        major_news_networks = [
            "news", "nbc news", "abc news", "cbs news", "fox news", "cnn",
            "bbc", "al jazeera", "msnbc", "cnbc", "c-span", "pbs newshour",
            "reuters", "associated press", "ap news", "npr", "bloomberg news",
            "sky news", "newsmax", "oan", "newsy", "local", "weather"
        ]
        has_news_name = any(network in app_name_lower for network in major_news_networks)

        # Require sufficient signals
        if news_phrase_count >= 2 or has_news_name:
            # Check for entertainment override
            if not self._is_primarily_entertainment(research_content):
                return True

        return False

    def _is_primarily_entertainment(self, research_content: str) -> bool:
        """
        Check if the app is primarily entertainment content.

        This is used to avoid routing entertainment apps that happen to mention
        news as a secondary feature (e.g., streaming services with a news section).

        Args:
            research_content: Research content from the first step

        Returns:
            True if primarily entertainment, False otherwise
        """
        research_lower = research_content.lower()

        entertainment_signals = [
            "movies", "scripted series", "television series", "streaming service",
            "original programming", "entertainment platform", "tv shows",
            "drama series", "comedy series", "reality shows", "on-demand movies",
            "movie streaming", "binge watch", "netflix original", "amazon original",
            "hulu original", "streaming content", "video on demand"
        ]

        news_signals = [
            "news", "headlines", "breaking news", "current events", "journalism",
            "newscast", "news programming", "news coverage"
        ]

        entertainment_count = sum(1 for signal in entertainment_signals if signal in research_lower)
        news_count = sum(1 for signal in news_signals if signal in research_lower)

        # Only exclude if entertainment significantly outweighs news
        return entertainment_count > news_count * 2
