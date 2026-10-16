# SPDX-License-Identifier: BUSL-1.1
# Copyright (c) 2026 ElcanoTek, Inc.

"""A small HTML tree on top of the standard library's ``html.parser``.

The scrapers only ever asked BeautifulSoup for a handful of things: find the
first or every element by tag name and attribute (exact value, presence, or a
regex over the value), the page title, the text under an element, the next
sibling element, and to drop ``<script>``/``<style>`` subtrees. This module
does exactly that and nothing more, so the dependency could go.

What it reproduces, because the extractors' output depends on it, is the tree
BeautifulSoup 4.15.0 builds with its ``html.parser`` backend (which Lens always
used). The behaviour was re-derived from that release, not copied:

* **No implicit closing.** An end tag closes the most recent open element of
  that name and everything opened inside it; an end tag with no open element
  of that name is ignored. Void elements (``br``, ``img``, ``meta`` …) close
  at once, and a later stray ``</br>`` for one of them is swallowed.
* **Character references are resolved like bs4's**, not like
  ``html.unescape``: an unknown ``&name;`` stays ``&name`` (semicolon
  dropped), and numeric references follow the HTML5 rules (0, surrogates and
  out-of-range become U+FFFD; 0x80–0x9F map through Windows-1252).
* **Whitespace-only text is collapsed** to a single ``"\\n"`` (if it held a
  newline) or ``" "``, except inside ``<pre>`` and ``<textarea>``.
* **Text kinds.** Strings inside ``script``, ``style``, ``template``, ``rt`` and
  ``rp`` belong to that container, and comments, CDATA, doctypes, declarations
  and processing instructions are their own kinds. ``get_text()`` on an
  ordinary element or the document returns only plain text and CDATA; on one
  of the containers it returns that container's own strings.

Attribute matching is deliberately simpler than bs4's: values are always plain
strings, so ``class`` is not split into a token list. No caller matches on
``class``; add token matching here first if one ever needs to.

``tests/test_html_extractors.py`` holds every extractor to the output bs4
produced on the same fixtures.
"""

from __future__ import annotations

import re
from html.entities import html5
from html.parser import HTMLParser
from typing import Dict, Iterator, List, Optional, Union

# String kinds. Containers use their tag name as the kind.
TEXT = "text"
CDATA = "cdata"
COMMENT = "comment"
DOCTYPE = "doctype"
DECLARATION = "declaration"
PROCESSING_INSTRUCTION = "pi"

_MAIN_CONTENT = frozenset({TEXT, CDATA})
_STRING_CONTAINERS = frozenset({"rt", "rp", "style", "script", "template"})
_PRESERVE_WHITESPACE = frozenset({"pre", "textarea"})
_VOID_ELEMENTS = frozenset(
    {
        "area",
        "base",
        "br",
        "col",
        "embed",
        "hr",
        "img",
        "input",
        "keygen",
        "link",
        "menuitem",
        "meta",
        "param",
        "source",
        "track",
        "wbr",
        # Pre-HTML5 void elements.
        "basefont",
        "bgsound",
        "command",
        "frame",
        "image",
        "isindex",
        "nextid",
        "spacer",
    }
)
_ASCII_SPACES = "\x20\x0a\x09\x0c\x0d"
_ROOT_NAME = "[document]"


def _build_entity_table() -> Dict[str, str]:
    # Named references keyed without their semicolon; the first spelling in
    # sorted order wins (``AMP`` and ``AMP;`` decode the same anyway).
    table: Dict[str, str] = {}
    for name, character in sorted(html5.items()):
        table.setdefault(name[:-1] if name.endswith(";") else name, character)
    return table


_ENTITIES = _build_entity_table()
_DECIMAL_WITH_TRAILER = re.compile("^([0-9]+)(.*)")
_HEX_WITH_TRAILER = re.compile("^([0-9a-f]+)(.*)")


def _numeric_reference(number: int) -> str:
    if number == 0 or number > 0x10FFFF or 0xD800 <= number <= 0xDFFF:
        return "�"
    if 0x80 <= number <= 0x9F:
        try:
            return bytes([number]).decode("cp1252")
        except UnicodeDecodeError:  # 0x81, 0x8D, 0x8F, 0x90, 0x9D: kept as-is
            pass
    return chr(number)


class Text:
    """A string in the tree. ``kind`` says what sort (see module docstring)."""

    __slots__ = ("value", "kind", "parent", "previous_sibling", "next_sibling")

    def __init__(self, value: str, kind: str) -> None:
        self.value = value
        self.kind = kind
        self.parent: Optional[Element] = None
        self.previous_sibling: Optional[Node] = None
        self.next_sibling: Optional[Node] = None


Node = Union["Element", Text]
AttrFilter = Union[str, bool, "re.Pattern[str]"]


def _attr_matches(value: Optional[str], wanted: AttrFilter) -> bool:
    if wanted is True:
        return value is not None
    if value is None:
        return False
    if isinstance(wanted, re.Pattern):
        return wanted.search(value) is not None
    return value == wanted


class Element:
    """An element. Children form a doubly linked list (``first_child``,
    ``next_sibling`` …), so appending, removing and stepping to a sibling are
    all O(1) whatever the page's shape; ``children`` builds a list on demand.
    Every walk below is iterative and linear in the nodes it visits — the
    scaling tests in tests/test_html_dom.py hold each operation to that."""

    __slots__ = (
        "name",
        "attrs",
        "parent",
        "previous_sibling",
        "next_sibling",
        "first_child",
        "last_child",
        "child_count",
    )

    def __init__(self, name: str, attrs: Dict[str, str]) -> None:
        self.name = name
        self.attrs = attrs
        self.parent: Optional[Element] = None
        self.previous_sibling: Optional[Node] = None
        self.next_sibling: Optional[Node] = None
        self.first_child: Optional[Node] = None
        self.last_child: Optional[Node] = None
        self.child_count = 0

    # -- attributes -------------------------------------------------------

    def get(self, key: str, default=None):
        return self.attrs.get(key, default)

    def __getitem__(self, key: str) -> str:
        return self.attrs[key]

    # -- structure --------------------------------------------------------

    def append(self, node: Node) -> None:
        node.parent = self
        node.previous_sibling = self.last_child
        node.next_sibling = None
        if self.last_child is None:
            self.first_child = node
        else:
            self.last_child.next_sibling = node
        self.last_child = node
        self.child_count += 1

    @property
    def children(self) -> List[Node]:
        out: List[Node] = []
        node = self.first_child
        while node is not None:
            out.append(node)
            node = node.next_sibling
        return out

    def descendants(self) -> Iterator[Node]:
        """Every node below this one, in document order, by walking the links
        (no recursion, no stack: pages with thousands of unclosed tags nest
        deeper than the recursion limit)."""
        node = self.first_child
        while node is not None:
            yield node
            if isinstance(node, Element) and node.first_child is not None:
                node = node.first_child
                continue
            while node is not self and node.next_sibling is None:
                node = node.parent
            if node is self:
                return
            node = node.next_sibling

    # -- searching --------------------------------------------------------

    def find_all(
        self,
        name: Union[None, str, List[str], tuple] = None,
        attrs: Optional[Dict[str, AttrFilter]] = None,
        **kwargs: AttrFilter,
    ) -> List["Element"]:
        """Descendant elements matching ``name`` (one name or a list of
        names) and every attribute filter: a string matches exactly, ``True``
        means "present", a compiled regex is searched in the value."""
        return list(self._matches(name, attrs, kwargs))

    __call__ = find_all

    def find(
        self,
        name: Union[None, str, List[str], tuple] = None,
        attrs: Optional[Dict[str, AttrFilter]] = None,
        **kwargs: AttrFilter,
    ) -> Optional["Element"]:
        return next(self._matches(name, attrs, kwargs), None)

    def _matches(self, name, attrs, kwargs) -> Iterator["Element"]:
        names = None if name is None else {name} if isinstance(name, str) else set(name)
        filters = dict(attrs or {})
        filters.update(kwargs)
        for node in self.descendants():
            if not isinstance(node, Element):
                continue
            if names is not None and node.name not in names:
                continue
            if all(_attr_matches(node.attrs.get(k), v) for k, v in filters.items()):
                yield node

    def find_next_sibling(self) -> Optional["Element"]:
        node = self.next_sibling
        while node is not None and not isinstance(node, Element):
            node = node.next_sibling
        return node

    @property
    def title(self) -> Optional["Element"]:
        return self.find("title")

    # -- text -------------------------------------------------------------

    def _text_kinds(self) -> frozenset:
        if self.name in _STRING_CONTAINERS:
            return frozenset({self.name})
        return _MAIN_CONTENT

    def get_text(self, separator: str = "", strip: bool = False) -> str:
        kinds = self._text_kinds()
        parts = []
        for node in self.descendants():
            if isinstance(node, Text) and node.kind in kinds:
                value = node.value.strip() if strip else node.value
                if strip and not value:
                    continue
                parts.append(value)
        return separator.join(parts)

    @property
    def string(self) -> Optional[str]:
        """The only string under this element, following single-child
        elements down; ``None`` when there are zero or several children."""
        node: Optional[Node] = self
        while isinstance(node, Element):
            if node.child_count != 1:
                return None
            node = node.first_child
        return node.value if node is not None else None

    # -- mutation ---------------------------------------------------------

    def decompose(self) -> None:
        """Remove this element and everything inside it from the tree, in
        O(1): it is unlinked from its siblings and its subtree goes with it."""
        parent = self.parent
        if parent is None:
            return
        if self.previous_sibling is None:
            parent.first_child = self.next_sibling
        else:
            self.previous_sibling.next_sibling = self.next_sibling
        if self.next_sibling is None:
            parent.last_child = self.previous_sibling
        else:
            self.next_sibling.previous_sibling = self.previous_sibling
        parent.child_count -= 1
        self.parent = self.previous_sibling = self.next_sibling = None

    # -- serialisation ----------------------------------------------------

    def __str__(self) -> str:
        """Markup for this element. Approximate (attribute order and quoting
        follow the parse, entities are minimally re-escaped); callers only
        search it for substrings. One pass over the subtree's links."""
        out: List[str] = []
        if self.name != _ROOT_NAME:
            out.append(_open_tag(self))
        for node, closing in self._events():
            if closing:
                out.append(f"</{node.name}>")
            elif isinstance(node, Text):
                out.append(_serialise_text(node))
            else:
                out.append(_open_tag(node))
        if self.name != _ROOT_NAME and not _self_closing(self):
            out.append(f"</{self.name}>")
        return "".join(out)

    def _events(self) -> Iterator[tuple]:
        """(node, is_end) for every node below this one: a start for each
        node, and an end after an element's subtree unless it self-closes."""
        node = self.first_child
        while node is not None:
            yield node, False
            if isinstance(node, Element) and node.first_child is not None:
                node = node.first_child
                continue
            while True:
                if isinstance(node, Element) and not _self_closing(node):
                    yield node, True
                if node.next_sibling is not None:
                    node = node.next_sibling
                    break
                node = node.parent
                if node is self:
                    return


def _self_closing(element: Element) -> bool:
    return element.first_child is None and element.name in _VOID_ELEMENTS


def _open_tag(element: Element) -> str:
    attrs = "".join(f" {k}={_quote_attr(v)}" for k, v in element.attrs.items())
    return f"<{element.name}{attrs}{'/' if _self_closing(element) else ''}>"


def _escape(value: str) -> str:
    return value.replace("&", "&amp;").replace("<", "&lt;").replace(">", "&gt;")


def _quote_attr(value: str) -> str:
    value = _escape(value)
    if '"' in value:
        if "'" in value:
            return '"' + value.replace('"', "&quot;") + '"'
        return "'" + value + "'"
    return '"' + value + '"'


def _serialise_text(node: Text) -> str:
    if node.kind == TEXT:
        return _escape(node.value)
    if node.kind == CDATA:
        return f"<![CDATA[{node.value}]]>"
    if node.kind == COMMENT:
        return f"<!--{node.value}-->"
    if node.kind == DOCTYPE:
        return f"<!DOCTYPE {node.value}>"
    if node.kind == DECLARATION:
        return f"<!{node.value}>"
    if node.kind == PROCESSING_INSTRUCTION:
        return f"<?{node.value}>"
    return node.value  # script/style/template/rt/rp contents are not escaped


class _TreeBuilder(HTMLParser):
    def __init__(self) -> None:
        # Character references are resolved by the handlers below, the same
        # way bs4 does, rather than by html.parser's html.unescape().
        super().__init__(convert_charrefs=False)
        # Every attribute and helper here is prefixed ``tree_`` because
        # HTMLParser keeps private state on self (``_pending`` among it), and
        # a name clash silently reorders text.
        self.tree_root = Element(_ROOT_NAME, {})
        self._tree_stack: List[Element] = [self.tree_root]
        self._tree_containers: List[Element] = []
        self._tree_preserving: List[Element] = []
        self._tree_text: List[str] = []
        self._tree_open: Dict[str, int] = {}  # open elements by name, for end tags
        # Void elements closed on their start tag, counted by name: a
        # matching end tag later is redundant and swallowed (once per such
        # element). A counter, not a list, so each end tag is O(1).
        self._tree_closed_voids: Dict[str, int] = {}

    # -- strings ----------------------------------------------------------

    def _tree_flush(self, kind: str = TEXT) -> None:
        if not self._tree_text:
            return
        data = "".join(self._tree_text)
        self._tree_text = []
        if not self._tree_preserving and all(ch in _ASCII_SPACES for ch in data):
            data = "\n" if "\n" in data else " "
        if kind == TEXT and self._tree_containers:
            kind = self._tree_containers[-1].name
        self._tree_stack[-1].append(Text(data, kind))

    def handle_data(self, data: str) -> None:
        self._tree_text.append(data)

    def handle_charref(self, name: str) -> None:
        base, trailer = 10, _DECIMAL_WITH_TRAILER
        if name[:1] in ("x", "X"):
            name, base, trailer = name[1:], 16, _HEX_WITH_TRAILER
        number: Optional[int] = None
        extra = ""
        try:
            number = int(name, base)
        except ValueError:
            match = trailer.search(name)
            if match is not None:
                number = int(match.group(1), base)
                extra = match.group(2)
        if number is None:
            self.handle_data(name)
            return
        self.handle_data(_numeric_reference(number))
        self.handle_data(extra)

    def handle_entityref(self, name: str) -> None:
        self.handle_data(_ENTITIES.get(name, "&" + name))

    def _tree_special(self, data: str, kind: str) -> None:
        self._tree_flush()
        self.handle_data(data)
        self._tree_flush(kind)

    def handle_comment(self, data: str) -> None:
        self._tree_special(data, COMMENT)

    def handle_decl(self, decl: str) -> None:
        self._tree_special(decl[len("DOCTYPE ") :], DOCTYPE)

    def unknown_decl(self, data: str) -> None:
        if data.upper().startswith("CDATA["):
            self._tree_special(data[len("CDATA[") :], CDATA)
        else:
            self._tree_special(data, DECLARATION)

    def handle_pi(self, data: str) -> None:
        self._tree_special(data, PROCESSING_INSTRUCTION)

    # -- elements ---------------------------------------------------------

    def handle_starttag(self, tag, attrs, closes_void: bool = True) -> None:
        self._tree_flush()
        attributes: Dict[str, str] = {}
        for key, value in attrs:
            attributes[key] = "" if value is None else value  # a repeat wins
        element = Element(tag, attributes)
        self._tree_stack[-1].append(element)
        self._tree_stack.append(element)
        self._tree_open[tag] = self._tree_open.get(tag, 0) + 1
        if tag in _STRING_CONTAINERS:
            self._tree_containers.append(element)
        if tag in _PRESERVE_WHITESPACE:
            self._tree_preserving.append(element)
        if closes_void and tag in _VOID_ELEMENTS:
            self.handle_endtag(tag, check_already_closed=False)
            self._tree_closed_voids[tag] = self._tree_closed_voids.get(tag, 0) + 1

    def handle_startendtag(self, tag, attrs) -> None:
        self.handle_starttag(tag, attrs, closes_void=False)
        self.handle_endtag(tag, check_already_closed=False)

    def handle_endtag(self, tag, check_already_closed: bool = True) -> None:
        if check_already_closed and self._tree_closed_voids.get(tag):
            # bs4 swallows this end tag without ending the current string, so
            # "a<br>b</br>c" keeps "bc" as one string; kept for parity.
            self._tree_closed_voids[tag] -= 1
            return
        self._tree_flush()
        if not self._tree_open.get(tag):
            return
        while True:
            element = self._tree_pop()
            if element.name == tag:
                return

    def _tree_pop(self) -> Element:
        element = self._tree_stack.pop()
        self._tree_open[element.name] -= 1
        if self._tree_containers and self._tree_containers[-1] is element:
            self._tree_containers.pop()
        if self._tree_preserving and self._tree_preserving[-1] is element:
            self._tree_preserving.pop()
        return element

    def finish(self) -> Element:
        self.close()
        self._tree_flush()
        while len(self._tree_stack) > 1:
            self._tree_pop()
        return self.tree_root


def parse_html(markup: str) -> Element:
    """Parse ``markup`` into a tree whose root element is the document."""
    builder = _TreeBuilder()
    builder.feed(markup)
    return builder.finish()
