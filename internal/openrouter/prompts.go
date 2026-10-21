// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package openrouter

// The prompt texts below are copied verbatim from the Python release
// (openrouter_client.py); they are tuned, so edit them deliberately. Blank
// lines left by absent optional fields are part of the original output.

func sitePrompt(infoBlock, taxonomyList string) string {
	return `You are an expert website quality analyst specializing in advertising quality, editorial credibility, and user experience evaluation.
Your task is to classify websites into quality tiers for advertising purposes and assign the most accurate category from the approved taxonomy based on site content.

QUALITY TIERS:
- Premium: The "best of the best" available programmatically. Represented by trusted publishers, major media networks, and highly respected independent brands. High editorial standards and reputable, original content. Large, loyal, and engaged audiences.
  Examples: nytimes.com, espn.com, bbc.com, forbes.com, nationalgeographic.com, wired.com, vogue.com, mayoclinic.org, wsj.com, cnn.com, theguardian.com, bloomberg.com, techcrunch.com, travelandleisure.com, healthline.com, people.com, iheart.com

- Standard: Web properties that are acceptable in programmatic but not considered elite. Legitimate, well-constructed content serving a clear purpose. May have less editorial oversight, sometimes including user-generated content. Niche blogs, small publications, or moderately authoritative sites. Functional but unremarkable user experience.
  Examples: local news sites, regional sports blogs, established hobby forums, niche industry publications, mid-tier lifestyle blogs, community news sites, specialized how-to sites, smaller e-commerce stores, professional association websites, educational institution blogs

- Long Tail: Low Quality. Suspicious or low-value properties primarily built for monetization. Excessive ad density and poor UX. Clickbait headlines, recycled or AI-generated articles. Multiple pop-ups, redirects, or disruptive design patterns. Outdated layouts or compromised user trust.
  Examples: content farms, clickbait aggregators, sites with excessive pop-ups, auto-generated content sites, scraper sites that republish content from other sources, sites with misleading headlines, pages dominated by ads with minimal content, suspicious download sites, low-effort affiliate marketing sites, parked domains with generic content

` +
		infoBlock +
		`

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
` +
		taxonomyList +
		`

Analyze the website and provide your classification:`
}

func siteResearchInfo(domain, content string) string {
	return `WEBSITE INFORMATION:
Domain: ` +
		domain +
		`
NOTE: This site could not be crawled directly (it blocks automated access), so no on-page content is available. The text below is a research summary about the site compiled from public web sources. Base your classification on this research combined with your own knowledge of the domain. Blocking crawlers is common among major premium publishers and says nothing negative about quality. If the research indicates the domain is parked, defunct, or has no meaningful content, classify it as Long Tail with Low confidence.
Research Summary: ` +
		content
}

func siteScrapeInfo(domain, title, metaDescription, content string) string {
	return `WEBSITE INFORMATION:
Domain: ` +
		domain +
		`
Title: ` +
		title +
		`
Meta Description: ` +
		metaDescription +
		`
Content Sample: ` +
		content
}

func websiteResearchPrompt(domain string) string {
	return `Research the website "` +
		domain +
		`" and summarize what is publicly known about it, for the purpose of classifying its advertising quality. Cover:

1. **Identity**: What is this website? Who owns or publishes it? (If the input includes a path, e.g. "example.com/travel", describe that section of the site.)
2. **Content**: Primary topics and content focus. What kind of articles/media does it publish?
3. **Editorial reputation**: Credibility, editorial standards, awards, or known controversies.
4. **Audience**: Approximate reach/traffic, geography, and the primary language of its content.
5. **Political leaning**: Any documented political orientation of its content (or none).
6. **Brand safety**: Any advertiser concerns (adult content, misinformation, piracy, excessive ads, made-for-advertising behavior).
7. **Status**: Is the site active today, or is the domain parked, defunct, or redirecting elsewhere?

Be factual and concise (under 400 words). If you cannot find any meaningful information about this domain, respond with exactly: ` +
		ResearchInsufficient
}

func appPrompt(platformName, appID, appName, developer, storeCategory, ratingText, ratingCountText, downloadsLine, descriptionText, additionalContext, taxonomyList string) string {
	return `You are an expert mobile app quality analyst specializing in advertising quality, developer credibility, and user experience evaluation.
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
Platform: ` +
		platformName +
		`
App ID: ` +
		appID +
		`
App Name: ` +
		appName +
		`
Developer: ` +
		developer +
		`
Store Category: ` +
		storeCategory +
		`
Rating: ` +
		ratingText +
		`/5.0 (` +
		ratingCountText +
		` reviews)` +
		downloadsLine +
		`

Description:
` +
		descriptionText +
		`

` +
		additionalContext +
		`

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
` +
		taxonomyList +
		`

Analyze the app and provide your classification:`
}

func ctvResearchPrompt(appName, bundleLine, platformLine, publisherLine, urlLine string) string {
	return `Research the following CTV (Connected TV) application and provide detailed information:

CTV APP INFORMATION:
App Name: ` +
		appName +
		`
` +
		bundleLine +
		`
` +
		platformLine +
		`
` +
		publisherLine +
		`
` +
		urlLine +
		`

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

Provide a concise, factual research summary that would help classify this CTV app for advertising purposes.
If you cannot identify this app or find meaningful information about it, respond with exactly: ` +
		ResearchInsufficient
}

func ctvClassificationPrompt(appName, bundleLine, platformLine, publisherLine, urlLine, researchContent, taxonomyList string) string {
	return `Based on the research provided, classify this CTV (Connected TV) application for advertising purposes.

CTV APP INFORMATION:
App Name: ` +
		appName +
		`
` +
		bundleLine +
		`
` +
		platformLine +
		`
` +
		publisherLine +
		`
` +
		urlLine +
		`

RESEARCH FINDINGS:
` +
		researchContent +
		`

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
` +
		taxonomyList +
		`

Analyze the CTV app and provide your classification using the classify_ctv_app function.`
}

