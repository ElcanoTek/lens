// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package openrouter

import "strings"

// The IAB Content Taxonomy has no Tier-1 "News" category, so general news
// apps must be mapped onto real IAB tiers. ApplyCTVRouting does that
// deterministically after the model's classification:
//
//   - general/mixed news      -> Politics > Civic affairs
//   - election-focused news   -> Politics > Elections
//   - dedicated weather apps  -> Science > Weather
//   - dedicated crime apps    -> Crime
//   - dedicated disaster apps -> Disasters
//
// News routed under Politics with political leaning "Non-Political" gets
// "Center" (news is political content, but may be neutral); an explicit
// leaning from the model is kept.

// generalNewsSignals keep a general news app that merely has "weather",
// "crime" or "alert" in its name out of the specialised verticals.
var generalNewsSignals = []string{
	"news", "breaking news", "headlines", "current events", "local news",
	"national news", "world news", "politics", "election", "newscast",
}

var (
	// Only dedicated weather apps, not news apps that mention weather.
	weatherAppNames = []string{
		"weather channel", "accuweather", "weather underground", "weather.com", "weather bug",
		"weatherbug", "weather network", "weather nation", "local weather", "storm tracker",
		"radar", "wunderground",
	}
	// Only dedicated crime / true-crime apps.
	crimeAppNames = []string{
		"crime watch", "true crime", "crime tv", "court tv", "crime stories",
		"crime investigation", "unsolved mysteries", "dateline", "48 hours",
	}
	// Only dedicated emergency / disaster apps.
	disasterAppNames = []string{
		"emergency alert", "fema", "red cross", "disaster", "emergency management",
		"emergency preparedness", "alert", "warning system",
	}
	// Strong signals that elections are a major focus.
	strongElectionSignals = []string{
		"election coverage", "election results", "election night", "vote count", "ballot",
		"electoral college", "midterm election", "presidential election", "senate race",
		"house race", "gubernatorial", "primary election",
	}
	electionAppNames = []string{"election", "vote", "ballot", "c-span", "cspan"}

	strongNewsPhrases = []string{
		"news", "breaking news", "live news", "local news", "national news", "headlines",
		"current events", "news channel", "news network", "24/7 news", "weather forecast",
		"election coverage", "news station", "news broadcast", "news programming", "newscast",
	}
	majorNewsNetworks = []string{
		"news", "nbc news", "abc news", "cbs news", "fox news", "cnn", "bbc", "al jazeera",
		"msnbc", "cnbc", "c-span", "pbs newshour", "reuters", "associated press", "ap news",
		"npr", "bloomberg news", "sky news", "newsmax", "oan", "newsy", "local", "weather",
	}

	entertainmentSignals = []string{
		"movies", "scripted series", "television series", "streaming service", "original programming",
		"entertainment platform", "tv shows", "drama series", "comedy series", "reality shows",
		"on-demand movies", "movie streaming", "binge watch", "netflix original", "amazon original",
		"hulu original", "streaming content", "video on demand",
	}
	entertainmentNewsSignals = []string{
		"news", "headlines", "breaking news", "current events", "journalism", "newscast",
		"news programming", "news coverage",
	}
)

func countIn(text string, signals []string) int {
	n := 0
	for _, s := range signals {
		if strings.Contains(text, s) {
			n++
		}
	}
	return n
}

func anyIn(text string, names []string) bool { return countIn(text, names) > 0 }

// IsPrimarilyEntertainment reports whether research describes mainly
// entertainment, so a streaming service with a news section is not routed
// as news: entertainment signals must outnumber news signals more than
// two to one.
func IsPrimarilyEntertainment(research string) bool {
	lowered := strings.ToLower(research)
	return countIn(lowered, entertainmentSignals) > countIn(lowered, entertainmentNewsSignals)*2
}

// IsCTVNewsApp reports whether a classified CTV app counts as news for
// routing: content type "News", or at least two strong news phrases in the
// research, or a news network / news term in the app name; in every case
// unless the research is primarily entertainment.
func IsCTVNewsApp(appName, research, contentType string) bool {
	if contentType == "News" && !IsPrimarilyEntertainment(research) {
		return true
	}
	lowered := strings.ToLower(research)
	name := strings.ToLower(appName)
	if countIn(lowered, strongNewsPhrases) >= 2 || anyIn(name, majorNewsNetworks) {
		return !IsPrimarilyEntertainment(research)
	}
	return false
}

func route(c Classification, tier1, tier2, note string) Classification {
	c.VerticalTier1, c.VerticalTier2, c.VerticalTier3 = tier1, tier2, ""
	c.Vertical = tier1
	if tier2 != "" {
		c.Vertical = tier1 + " > " + tier2
	}
	c.Justification += " (" + note + ")"
	return c
}

func centerIfNonPolitical(c Classification) Classification {
	if c.PoliticalLeaning == "Non-Political" {
		c.PoliticalLeaning = "Center"
		c.Justification += " Political_Leaning set to Center due to Politics vertical."
	}
	return c
}

// ApplyCTVRouting applies the deterministic news routing described above to
// a CTV classification and returns the (copied) result. Non-news apps come
// back unchanged.
func ApplyCTVRouting(appName, research string, c Classification) Classification {
	if !IsCTVNewsApp(appName, research, c.ContentType) {
		return c
	}
	lowered := strings.ToLower(research)
	name := strings.ToLower(appName)
	// A dedicated weather/crime/disaster app is routed there only when the
	// research carries few general-news signals (< 3); these stay
	// Non-Political.
	fewNewsSignals := countIn(lowered, generalNewsSignals) < 3
	switch {
	case anyIn(name, weatherAppNames) && fewNewsSignals:
		return route(c, "Science", "Weather", "post-routed for CTV weather app")
	case anyIn(name, crimeAppNames) && fewNewsSignals:
		return route(c, "Crime", "", "post-routed for CTV crime app")
	case anyIn(name, disasterAppNames) && fewNewsSignals:
		return route(c, "Disasters", "", "post-routed for CTV disaster app")
	}
	if countIn(lowered, strongElectionSignals) >= 2 || anyIn(name, electionAppNames) {
		return centerIfNonPolitical(route(c, "Politics", "Elections", "post-routed for CTV election news"))
	}
	return centerIfNonPolitical(route(c, "Politics", "Civic affairs", "post-routed for CTV news"))
}
