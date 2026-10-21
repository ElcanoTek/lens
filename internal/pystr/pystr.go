// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package pystr reproduces the few Python str/unicodedata operations whose
// exact results Lens persists or compares across releases: NFC
// normalisation, str.strip() and str.casefold(), on tables generated from
// Python's own unicodedata (see unicode_tables.go). Allowlist emails and
// custom-category uniqueness both depend on them matching the Python release.
package pystr

import (
	"sort"
	"strings"
	"unicode/utf8"
)

type runeRange struct {
	lo, hi rune
	val    uint8
}

func lookupRange(table []runeRange, r rune) uint8 {
	i := sort.Search(len(table), func(i int) bool { return table[i].hi >= r })
	if i < len(table) && table[i].lo <= r {
		return table[i].val
	}
	return 0
}

// Hangul syllable composition constants (Unicode §3.12).
const (
	hangulSBase  = 0xAC00
	hangulLBase  = 0x1100
	hangulVBase  = 0x1161
	hangulTBase  = 0x11A7
	hangulLCount = 19
	hangulVCount = 21
	hangulTCount = 28
	hangulNCount = hangulVCount * hangulTCount
	hangulSCount = hangulLCount * hangulNCount
)

// NFC is unicodedata.normalize("NFC", s) on the generated tables: full
// canonical decomposition, canonical ordering, then canonical composition.
func NFC(s string) string {
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			ascii = false
			break
		}
	}
	if ascii {
		return s
	}
	decomposed := make([]rune, 0, len(s))
	for _, r := range s {
		switch {
		case r >= hangulSBase && r < hangulSBase+hangulSCount:
			index := r - hangulSBase
			decomposed = append(decomposed, hangulLBase+index/hangulNCount, hangulVBase+(index%hangulNCount)/hangulTCount)
			if t := index % hangulTCount; t != 0 {
				decomposed = append(decomposed, hangulTBase+t)
			}
		default:
			if d, ok := nfdTable[r]; ok {
				decomposed = append(decomposed, []rune(d)...)
			} else {
				decomposed = append(decomposed, r)
			}
		}
	}
	// Canonical ordering: stable-sort every run of non-starters by class.
	for i := 0; i < len(decomposed); {
		if lookupRange(cccRanges, decomposed[i]) == 0 {
			i++
			continue
		}
		j := i
		for j < len(decomposed) && lookupRange(cccRanges, decomposed[j]) != 0 {
			j++
		}
		run := decomposed[i:j]
		sort.SliceStable(run, func(a, b int) bool {
			return lookupRange(cccRanges, run[a]) < lookupRange(cccRanges, run[b])
		})
		i = j
	}
	// Canonical composition.
	out := make([]rune, 0, len(decomposed))
	starter := -1
	var lastClass uint8
	for _, r := range decomposed {
		class := lookupRange(cccRanges, r)
		if starter >= 0 {
			s := out[starter]
			blocked := len(out)-1 != starter && (lastClass == 0 || lastClass >= class)
			if !blocked {
				if composed, ok := compose(s, r); ok {
					out[starter] = composed
					continue
				}
			}
		}
		if class == 0 {
			starter = len(out)
		}
		lastClass = class
		out = append(out, r)
	}
	return string(out)
}

func compose(a, b rune) (rune, bool) {
	// Hangul L+V and LV+T are algorithmic.
	if a >= hangulLBase && a < hangulLBase+hangulLCount && b >= hangulVBase && b < hangulVBase+hangulVCount {
		return hangulSBase + ((a-hangulLBase)*hangulVCount+(b-hangulVBase))*hangulTCount, true
	}
	if a >= hangulSBase && a < hangulSBase+hangulSCount && (a-hangulSBase)%hangulTCount == 0 &&
		b > hangulTBase && b < hangulTBase+hangulTCount {
		return a + (b - hangulTBase), true
	}
	c, ok := nfcPairs[uint64(a)<<21|uint64(b)] //nolint:gosec // G115: runes decoded from a string are in [0, 0x10FFFF]
	return c, ok
}

// Strip is Python's str.strip() with no arguments.
func Strip(s string) string {
	return strings.TrimFunc(s, func(r rune) bool { return pySpace[r] })
}

// CaseFold is Python's str.casefold().
func CaseFold(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if f, ok := caseFoldTable[r]; ok {
			b.WriteString(f)
		} else {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// IsControl reports whether r is in a Unicode general category C* (control,
// format, surrogate, private use or unassigned), like unicodedata.category.
func IsControl(r rune) bool { return lookupRange(controlRanges, r) != 0 }
