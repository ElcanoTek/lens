# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.
"""OpenRouter spend per item and per run, as OpenRouter itself reports it.

Every OpenRouter response carries ``usage.cost`` in US dollars, including
search fees and retries that were billed. Lens records that figure; it never
estimates from token counts and prices.

Two meters live in context variables so concurrent items never mix:

- the *run* meter is installed once by the orchestrator and inherited by every
  task it starts; it is persisted in the progress file, so a resumed run keeps
  counting from its previous total;
- an *item* meter is opened by each processor entry point (``per_item``) and
  read when the item's CSV row is written.

Calls whose response carries no cost (for example TypeSafe's own endpoint)
are counted as unpriced rather than silently treated as free.
"""

import functools
import math
from contextvars import ContextVar
from typing import Any, Dict, Optional


class CostMeter:
    def __init__(self) -> None:
        self.usd = 0.0
        self.calls = 0
        self.unpriced_calls = 0
        self.by_model: Dict[str, float] = {}

    def add(self, cost: Optional[float], model: str = "") -> None:
        self.calls += 1
        if cost is None:
            self.unpriced_calls += 1
            return
        self.usd += cost
        key = model or "unknown"
        self.by_model[key] = self.by_model.get(key, 0.0) + cost

    def snapshot(self) -> Dict[str, Any]:
        return {
            "usd": round(self.usd, 6),
            "calls": self.calls,
            "unpriced_calls": self.unpriced_calls,
            "by_model": {k: round(v, 6) for k, v in sorted(self.by_model.items())},
        }

    @classmethod
    def restore(cls, data: Any) -> "CostMeter":
        """Rebuild a run meter from a saved snapshot; tolerate missing or bad data."""
        meter = cls()
        if not isinstance(data, dict):
            return meter
        usd = parse_cost(data.get("usd"))
        meter.usd = usd or 0.0
        for field in ("calls", "unpriced_calls"):
            value = data.get(field)
            if isinstance(value, int) and value >= 0:
                setattr(meter, field, value)
        by_model = data.get("by_model")
        if isinstance(by_model, dict):
            for model, cost in by_model.items():
                cost = parse_cost(cost)
                if isinstance(model, str) and cost is not None:
                    meter.by_model[model] = cost
        return meter


_run: ContextVar[Optional[CostMeter]] = ContextVar("lens_run_cost", default=None)
_item: ContextVar[Optional[CostMeter]] = ContextVar("lens_item_cost", default=None)


def parse_cost(value: Any) -> Optional[float]:
    """A finite, non-negative dollar amount, or None."""
    if isinstance(value, bool) or not isinstance(value, (int, float, str)):
        return None
    try:
        cost = float(value)
    except ValueError:
        return None
    return cost if math.isfinite(cost) and cost >= 0 else None


def cost_from_usage(usage: Any) -> Optional[float]:
    """``usage.cost`` from an OpenAI SDK object or a plain response dict."""
    if usage is None:
        return None
    if isinstance(usage, dict):
        return parse_cost(usage.get("cost"))
    extra = getattr(usage, "model_extra", None) or {}
    return parse_cost(extra.get("cost", getattr(usage, "cost", None)))


def start_run(meter: CostMeter) -> None:
    _run.set(meter)


def record(cost: Optional[float], model: str = "") -> None:
    """Add one billed call to the active run and item meters."""
    for var in (_run, _item):
        meter = var.get()
        if meter is not None:
            meter.add(cost, model)


def per_item(func):
    """Give each processed item its own meter unless one is already open.

    Nested entry points (for example research inside a website item) keep
    adding to the outer item's meter.
    """

    @functools.wraps(func)
    async def wrapper(*args, **kwargs):
        if _item.get() is not None:
            return await func(*args, **kwargs)
        token = _item.set(CostMeter())
        try:
            return await func(*args, **kwargs)
        finally:
            _item.reset(token)

    return wrapper


def item_cost_usd() -> str:
    """The open item's spend for its CSV row; blank outside an item or if unpriced."""
    meter = _item.get()
    if meter is None or (meter.calls and meter.unpriced_calls == meter.calls):
        return ""
    return f"{meter.usd:.6f}"
