// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package detect works out what an input file is: which column holds the
// identifiers, whether each one is a website, an iOS app or an Android app,
// whether the whole file is a CTV channel list, and how the rows become work
// items. It is the port of input_detector.py plus the input-shaping helpers
// orchestration.py and web_service.py kept next to it (headerless lists,
// the dashboard's per-type breakdown), operating on tabular.Frame.
//
// The value rules, in priority order:
//
//   - Clear URL/website indicators win first (scheme, URL punctuation, www.,
//     ports, IPs, hyphens, well-known TLD endings), so io.example.com is a
//     website even though it starts with a reverse-DNS prefix.
//   - All digits is an App Store id (iOS).
//   - A structurally valid package name is Android when it starts with a
//     known reverse-DNS prefix, when its leaf label is not a real (IANA) TLD
//     (so it cannot be a domain), or when it nests five or more labels deep.
//   - Anything else is a website.
package detect

import (
	"regexp"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	lens "github.com/ElcanoTek/lens"
	"github.com/ElcanoTek/lens/internal/item"
	"github.com/ElcanoTek/lens/internal/logx"
	"github.com/ElcanoTek/lens/internal/tabular"
)

var log = logx.New("input_detector")

// packageMinLabelsWhenTLDLeaf: domains rarely nest deeper than four labels
// (a.b.c.example.com); Android package names routinely have five or more.
// When a package-shaped string's leaf collides with a real TLD (e.g.
// puzzle.solver.free), this depth is the tie-breaker that keeps it out of
// the website bucket. Measured against 31k real domains, a >=5 threshold
// yields zero false positives.
const packageMinLabelsWhenTLDLeaf = 5

// urlIndicatorChars never appear in Android package names.
const urlIndicatorChars = "/:?#&=@!$'()*+,;[]%"

// primaryWebTLDs are the classic TLDs that almost never end a package name.
var primaryWebTLDs = []string{".com", ".org", ".net", ".edu", ".gov", ".mil", ".int"}

// countryTLDs: when at the end, definitely a website.
var countryTLDs = []string{
	".us", ".uk", ".de", ".fr", ".es", ".it", ".nl", ".au", ".ca", ".jp", ".cn", ".ru", ".br",
	".in", ".kr", ".mx", ".pl", ".se", ".ch", ".at", ".be", ".dk", ".fi", ".no", ".nz", ".za",
	".ie", ".pt", ".cz", ".hu", ".gr", ".ro", ".il", ".sg", ".hk", ".tw", ".th", ".my", ".ph",
	".vn", ".id", ".ar", ".cl",
}

// compoundTLDs: when at the end, definitely a website.
var compoundTLDs = []string{
	".co.uk", ".com.au", ".com.br", ".co.jp", ".co.kr", ".co.nz", ".co.za", ".com.mx",
	".com.ar", ".org.uk", ".net.au",
}

// semiPrimaryTLDs are newer but so commonly used as real domain TLDs (tech
// companies) that a value ending in one is a website even with a package
// prefix: com.company.io is company.io with a "com" subdomain.
var semiPrimaryTLDs = []string{".io", ".co"}

// ambiguousTLDs can be website TLDs or Android package suffixes; when one
// ends the value, the prefix decides.
var ambiguousTLDs = []string{
	".me", ".tv", ".ai", ".app", ".dev", ".tech", ".info", ".biz", ".xyz", ".online", ".site",
	".website", ".gg", ".fm", ".ly", ".to", ".cc", ".ws", ".so", ".ac", ".sh",
}

// androidPackagePrefixes are TLDs used as reverse-DNS package prefixes.
// They make a value an Android package only when it does not end like a
// website.
var androidPackagePrefixes = []string{
	"com.", "net.", "org.", "io.", "co.", "me.", "app.", "dev.", "ai.", "tv.", "in.", "de.",
	"fr.", "uk.", "jp.", "kr.", "cn.", "ru.", "br.", "it.", "es.", "au.", "ca.",
}

var urlSchemes = []string{"http://", "https://", "ftp://", "ftps://", "file://", "mailto:", "tel:", "sms:", "//"}

var (
	ianaOnce sync.Once
	ianaTLDs map[string]struct{}
)

// tlds is the authoritative set of registrable top-level domains (IANA),
// from the iana_tlds.txt snapshot embedded in the binary. It is the linchpin
// of Android-vs-website detection: a real domain always ends in a delegated
// TLD, so a dotted identifier whose leaf is not a TLD cannot be a domain.
func tlds() map[string]struct{} {
	ianaOnce.Do(func() {
		ianaTLDs = make(map[string]struct{}, 1600)
		for _, line := range strings.Split(string(lens.IANATLDs), "\n") {
			entry := strings.ToLower(tabular.Strip(line))
			if entry != "" && !strings.HasPrefix(entry, "#") {
				ianaTLDs[entry] = struct{}{}
			}
		}
		if len(ianaTLDs) == 0 {
			// The list is embedded, so this only guards a broken build.
			log.Warn("Could not read iana_tlds.txt; falling back to built-in TLD list")
			for _, group := range [][]string{primaryWebTLDs, countryTLDs, compoundTLDs, semiPrimaryTLDs, ambiguousTLDs} {
				for _, t := range group {
					ianaTLDs[strings.TrimPrefix(t, ".")] = struct{}{}
				}
			}
		}
	})
	return ianaTLDs
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func hasAnySuffix(s string, suffixes []string) bool {
	for _, p := range suffixes {
		if strings.HasSuffix(s, p) {
			return true
		}
	}
	return false
}

// isDigits is Python's str.isdigit(): non-empty and every rune a digit.
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !unicode.IsDigit(c) {
			return false
		}
	}
	return true
}

// isAlnum is Python's str.isalnum().
func isAlnum(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if !unicode.IsLetter(c) && !unicode.IsDigit(c) && !unicode.IsNumber(c) {
			return false
		}
	}
	return true
}

var (
	portRE = regexp.MustCompile(`:\p{Nd}{1,5}(?:/|$)`)
	ipv4RE = regexp.MustCompile(`^\p{Nd}{1,3}\.\p{Nd}{1,3}\.\p{Nd}{1,3}\.\p{Nd}{1,3}(?::\p{Nd}+)?(?:/.*)?$`)
	// domainLikeRE is the loose shape of a web domain/URL, for scoring
	// columns by content.
	domainLikeRE = regexp.MustCompile(`^(?:https?://)?[A-Za-z0-9][A-Za-z0-9._-]*\.[A-Za-z]{2,}(?:[/:?#].*)?$`)
	segmentRE    = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
)

// isURLOrWebsite reports definitive URL/website indicators that Android
// package names never have.
func isURLOrWebsite(value string) bool {
	lower := strings.ToLower(value)

	// 1. URL schemes.
	if hasAnyPrefix(lower, urlSchemes) {
		return true
	}
	// 2. URL punctuation.
	if strings.ContainsAny(value, urlIndicatorChars) {
		return true
	}
	// 3. www prefix.
	if strings.HasPrefix(lower, "www.") {
		return true
	}
	// 4. Port numbers (example.com:8080).
	if portRE.MatchString(value) {
		return true
	}
	// 5. IPv4 addresses.
	if ipv4RE.MatchString(value) {
		return true
	}
	// 6. localhost.
	if strings.HasPrefix(lower, "localhost") {
		return true
	}
	// 7. Hyphens are common in domains and invalid in Java package names.
	if strings.Contains(value, "-") {
		return true
	}
	// 8. Primary, country and compound TLD endings are always websites.
	if hasAnySuffix(lower, primaryWebTLDs) || hasAnySuffix(lower, countryTLDs) || hasAnySuffix(lower, compoundTLDs) {
		return true
	}
	// 9. .io/.co endings are websites even with a package prefix.
	if hasAnySuffix(lower, semiPrimaryTLDs) {
		return true
	}
	// 10. Ambiguous endings (.app, .dev, .me, ...): the structure decides.
	for _, tld := range ambiguousTLDs {
		if !strings.HasSuffix(lower, tld) {
			continue
		}
		// Without an Android prefix it's a website.
		if !hasAnyPrefix(lower, androidPackagePrefixes) {
			return true
		}
		prefix := ""
		for _, p := range androidPackagePrefixes {
			if strings.HasPrefix(lower, p) {
				prefix = strings.TrimSuffix(p, ".")
				break
			}
		}
		tldName := strings.TrimPrefix(tld, ".")
		// A primary-TLD prefix (com.example.app) makes the ending a package
		// segment, not a TLD: let Android detection handle it.
		if prefix == "com" || prefix == "org" || prefix == "net" {
			continue
		}
		// A secondary prefix (co., me.) with a common package suffix
		// (co.company.app) is a potential package. "app." and "dev." are
		// also common subdomains (app.startup.dev), so they don't get this
		// bypass.
		if (tldName == "app" || tldName == "tech" || tldName == "ai") && prefix != "app" && prefix != "dev" {
			continue
		}
		// Two different ambiguous labels (io.company.dev): a website.
		if prefix != tldName {
			return true
		}
	}
	return false
}

// isValidAndroidPackage reports whether value has the shape of an Android
// package name: two or more dot-separated segments, each starting with a
// letter and holding only letters, digits and underscores (case-insensitive),
// and no URL punctuation or hyphens.
func isValidAndroidPackage(value string) bool {
	if strings.ContainsAny(value, urlIndicatorChars) || strings.Contains(value, "-") || !strings.Contains(value, ".") {
		return false
	}
	segments := strings.Split(strings.ToLower(value), ".")
	if len(segments) < 2 {
		return false
	}
	for _, seg := range segments {
		if seg == "" {
			return false
		}
		if first, _ := utf8.DecodeRuneInString(seg); !unicode.IsLetter(first) {
			return false
		}
		if !segmentRE.MatchString(seg) {
			return false
		}
	}
	return true
}

// leafIsRegistrableTLD reports whether the value's final label is a real
// (IANA) top-level domain: what tells a website apart from a reverse-DNS
// package whose leaf is an ordinary word (com.example.myapp).
func leafIsRegistrableTLD(lower string) bool {
	leaf := lower[strings.LastIndexByte(lower, '.')+1:]
	_, ok := tlds()[leaf]
	return ok
}

// ContentType detects the content type of one identifier; see the package
// documentation for the rules. Blank values are websites.
func ContentType(value string) item.ContentType {
	value = tabular.Strip(value)
	if value == "" {
		return item.Website
	}
	// URL/website indicators first: this catches io.example.com, which
	// would otherwise look like a package because it starts with "io.".
	if isURLOrWebsite(value) {
		return item.Website
	}
	if isDigits(value) {
		return item.IOSApp
	}
	lower := strings.ToLower(value)
	// Fast path: a valid package with a known reverse-DNS prefix.
	if hasAnyPrefix(lower, androidPackagePrefixes) && isValidAndroidPackage(value) {
		return item.AndroidApp
	}
	// Packages without a recognised prefix (tunein.player, kik.android,
	// jigsaw.puzzle.game.banana) that survived the website gate above.
	if isValidAndroidPackage(value) {
		// A real domain must end in a registrable TLD; if the leaf is not
		// one this cannot be a website. Zero false positives by construction.
		if !leafIsRegistrableTLD(lower) {
			return item.AndroidApp
		}
		// The leaf collides with a real TLD (.games, .news): depth is the
		// safe tie-breaker, as domains don't nest this deep.
		if strings.Count(lower, ".")+1 >= packageMinLabelsWhenTLDLeaf {
			return item.AndroidApp
		}
	}
	return item.Website
}

// LooksProcessable reports whether a single value looks like something Lens
// can classify. Stricter than ContentType (which defaults to website): it is
// used to score columns by content, so a Notes or Rank column doesn't win,
// and to tell a lost first data row from a real header.
func LooksProcessable(value string) bool {
	v := tabular.Strip(value)
	if v == "" {
		return false
	}
	if isDigits(v) {
		// App Store ids are long; short numbers are ranks/counts.
		return utf8.RuneCountInString(v) >= 6
	}
	if domainLikeRE.MatchString(v) {
		return true
	}
	return hasAnyPrefix(strings.ToLower(v), androidPackagePrefixes) && isValidAndroidPackage(v)
}

// SpreadsheetCell is the text of one cell without float artifacts or blank
// markers: missing cells (tabular.IsNA) and the words nan/none/<na>/nat are
// "", and a whole number written as "54092.0" (how Excel and pandas exports
// write integer ids) loses its ".0", which would otherwise send a Roku id or
// App Store id down the wrong path.
func SpreadsheetCell(raw string) string {
	if tabular.IsNA(raw) {
		return ""
	}
	text := tabular.Strip(raw)
	switch strings.ToLower(text) {
	case "nan", "none", "<na>", "nat":
		return ""
	}
	if head, ok := strings.CutSuffix(text, ".0"); ok && isDigits(head) {
		return head
	}
	return text
}
