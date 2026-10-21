// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package openrouter

import "github.com/ElcanoTek/lens/internal/categories/pyjson"

// Function-calling schemas for structured output, copied verbatim from the
// Python release. The tool definitions are ordered objects so the request
// lists properties in the order the prompts were tuned with.

// siteSchema is the classify_website tool.
func siteSchema() pyjson.Object {
	return pyjson.Object{
		{Key: "type", Value: "function"},
		{Key: "function", Value: pyjson.Object{
			{Key: "name", Value: "classify_website"},
			{Key: "description", Value: "Classify a website based on quality tier, category, and other metadata"},
			{Key: "parameters", Value: pyjson.Object{
				{Key: "type", Value: "object"},
				{Key: "properties", Value: pyjson.Object{
					{Key: "quality", Value: pyjson.Object{
						{Key: "type", Value: "string"},
						{Key: "enum", Value: []any{"Premium", "Standard", "Long Tail"}},
						{Key: "description", Value: "The quality tier of the website: Premium (best of the best), Standard (acceptable but not elite), or Long Tail (low quality/suspicious)"},
					}},
					{Key: "justification", Value: pyjson.Object{
						{Key: "type", Value: "string"},
						{Key: "description", Value: "Detailed explanation for the quality rating, considering content quality, editorial standards, user experience, and brand recognition"},
					}},
					{Key: "vertical", Value: pyjson.Object{
						{Key: "type", Value: "string"},
						{Key: "description", Value: "A SINGLE category name from the approved IAB taxonomy. Must be an exact match from the taxonomy list. DO NOT use commas or combine categories. Examples: 'Sports', 'Puzzle Video Games', 'Technology & Computing'"},
					}},
					{Key: "description", Value: pyjson.Object{
						{Key: "type", Value: "string"},
						{Key: "description", Value: "A single sentence describing the site's primary content or purpose"},
					}},
					{Key: "confidence", Value: pyjson.Object{
						{Key: "type", Value: "string"},
						{Key: "enum", Value: []any{"High", "Medium", "Low"}},
						{Key: "description", Value: "Confidence level in the classification: High (clear signals), Medium (some ambiguity), or Low (minimal/conflicting signals)"},
					}},
					{Key: "language", Value: pyjson.Object{
						{Key: "type", Value: "string"},
						{Key: "description", Value: "The primary language of the content (e.g., 'English', 'Spanish', 'French', 'Chinese', 'Multilingual', etc.)"},
					}},
					{Key: "political_leaning", Value: pyjson.Object{
						{Key: "type", Value: "string"},
						{Key: "enum", Value: []any{"Far Left", "Left", "Center-Left", "Center", "Center-Right", "Right", "Far Right", "Non-Political"}},
						{Key: "description", Value: "Political orientation of the content for US political context: Far Left (socialist/progressive advocacy), Left (liberal/progressive), Center-Left (moderate liberal), Center (balanced/neutral), Center-Right (moderate conservative), Right (conservative), Far Right (nationalist/hard conservative), Non-Political (no political content)"},
					}},
					{Key: "audience_size", Value: pyjson.Object{
						{Key: "type", Value: "string"},
						{Key: "description", Value: "Conservatively estimate the site's relative audience size. You have a strong tendency to overestimate. Be skeptical: high quality or brand recognition does not mean high traffic. Most sites are XS or S. Use L/XL only for globally recognized destinations. Use the following t-shirt sizes: XS, S, M, L, XL."},
						{Key: "enum", Value: []any{"XS", "S", "M", "L", "XL"}},
					}},
				}},
				{Key: "required", Value: []any{"quality", "justification", "vertical", "description", "confidence", "language", "political_leaning", "audience_size"}},
			}},
		}},
	}
}

// appSchema is the classify_app tool.
func appSchema() pyjson.Object {
	return pyjson.Object{
		{Key: "type", Value: "function"},
		{Key: "function", Value: pyjson.Object{
			{Key: "name", Value: "classify_app"},
			{Key: "description", Value: "Classify a mobile app based on quality tier, category, and other metadata"},
			{Key: "parameters", Value: pyjson.Object{
				{Key: "type", Value: "object"},
				{Key: "properties", Value: pyjson.Object{
					{Key: "quality", Value: pyjson.Object{
						{Key: "type", Value: "string"},
						{Key: "enum", Value: []any{"Premium", "Standard", "Long Tail"}},
						{Key: "description", Value: "The quality tier of the app: Premium (best of the best), Standard (acceptable but not elite), or Long Tail (low quality/suspicious)"},
					}},
					{Key: "justification", Value: pyjson.Object{
						{Key: "type", Value: "string"},
						{Key: "description", Value: "Detailed explanation for the quality rating, considering app purpose, developer reputation, user reviews, and content quality"},
					}},
					{Key: "vertical", Value: pyjson.Object{
						{Key: "type", Value: "string"},
						{Key: "description", Value: "A SINGLE category name from the approved IAB taxonomy. Must be an exact match from the taxonomy list. DO NOT use commas or combine categories. Examples: 'Video Gaming', 'Puzzle Video Games', 'Social Networking'"},
					}},
					{Key: "description", Value: pyjson.Object{
						{Key: "type", Value: "string"},
						{Key: "description", Value: "A single sentence describing the app's primary purpose or function"},
					}},
					{Key: "confidence", Value: pyjson.Object{
						{Key: "type", Value: "string"},
						{Key: "enum", Value: []any{"High", "Medium", "Low"}},
						{Key: "description", Value: "Confidence level in the classification: High (clear signals), Medium (some ambiguity), or Low (minimal/conflicting signals)"},
					}},
					{Key: "language", Value: pyjson.Object{
						{Key: "type", Value: "string"},
						{Key: "description", Value: "The primary language of the app content and interface (e.g., 'English', 'Spanish', 'French', 'Chinese', 'Multilingual', etc.)"},
					}},
					{Key: "political_leaning", Value: pyjson.Object{
						{Key: "type", Value: "string"},
						{Key: "enum", Value: []any{"Far Left", "Left", "Center-Left", "Center", "Center-Right", "Right", "Far Right", "Non-Political"}},
						{Key: "description", Value: "Political orientation of the app content for US political context: Far Left (socialist/progressive advocacy), Left (liberal/progressive), Center-Left (moderate liberal), Center (balanced/neutral), Center-Right (moderate conservative), Right (conservative), Far Right (nationalist/hard conservative), Non-Political (no political content)"},
					}},
					{Key: "audience_size", Value: pyjson.Object{
						{Key: "type", Value: "string"},
						{Key: "description", Value: "Conservatively estimate the app's relative audience size. You have a strong tendency to overestimate. Be skeptical: high quality or brand recognition does not mean high usage. Most apps are XS or S. Use L/XL only for globally recognized apps. Use the following t-shirt sizes: XS, S, M, L, XL."},
						{Key: "enum", Value: []any{"XS", "S", "M", "L", "XL"}},
					}},
				}},
				{Key: "required", Value: []any{"quality", "justification", "vertical", "description", "confidence", "language", "political_leaning", "audience_size"}},
			}},
		}},
	}
}

// ctvSchema is the classify_ctv_app tool.
func ctvSchema() pyjson.Object {
	return pyjson.Object{
		{Key: "type", Value: "function"},
		{Key: "function", Value: pyjson.Object{
			{Key: "name", Value: "classify_ctv_app"},
			{Key: "description", Value: "Classify a CTV (Connected TV) app based on quality tier, category, and other metadata"},
			{Key: "parameters", Value: pyjson.Object{
				{Key: "type", Value: "object"},
				{Key: "properties", Value: pyjson.Object{
					{Key: "quality", Value: pyjson.Object{
						{Key: "type", Value: "string"},
						{Key: "enum", Value: []any{"Premium", "Standard", "Long Tail"}},
						{Key: "description", Value: "The quality tier of the CTV app based on RECOGNIZABILITY: Premium (only major nationally/internationally recognizable brands like NBC, ESPN, Peacock, NFL), Standard (default for most CTV apps including local stations, regional networks, niche content), or Long Tail (rarely used for CTV - almost no CTV apps qualify)"},
					}},
					{Key: "justification", Value: pyjson.Object{
						{Key: "type", Value: "string"},
						{Key: "description", Value: "Detailed explanation for the quality rating, considering content quality, network affiliation, brand recognition, and viewer engagement"},
					}},
					{Key: "vertical", Value: pyjson.Object{
						{Key: "type", Value: "string"},
						{Key: "description", Value: "A SINGLE category name from the approved IAB taxonomy. Must be an exact match from the taxonomy list. DO NOT use commas or combine categories."},
					}},
					{Key: "description", Value: pyjson.Object{
						{Key: "type", Value: "string"},
						{Key: "description", Value: "A single sentence describing the CTV app's primary content or purpose"},
					}},
					{Key: "target_audience", Value: pyjson.Object{
						{Key: "type", Value: "string"},
						{Key: "description", Value: "Description of the target audience (e.g., 'General entertainment seekers', 'Sports enthusiasts', 'News viewers', 'Children and families')"},
					}},
					{Key: "content_type", Value: pyjson.Object{
						{Key: "type", Value: "string"},
						{Key: "enum", Value: []any{"Live TV", "On-Demand Streaming", "Live Sports", "News", "Music/Audio", "Kids Content", "Fitness", "Educational", "Gaming", "Mixed Content"}},
						{Key: "description", Value: "The primary type of content offered by the CTV app"},
					}},
					{Key: "network_affiliation", Value: pyjson.Object{
						{Key: "type", Value: "string"},
						{Key: "description", Value: "Network affiliation if any (e.g., 'NBC', 'CBS', 'ABC', 'Fox', 'Independent', 'None')"},
					}},
					{Key: "confidence", Value: pyjson.Object{
						{Key: "type", Value: "string"},
						{Key: "enum", Value: []any{"High", "Medium", "Low"}},
						{Key: "description", Value: "Confidence level in the classification: High (clear signals), Medium (some ambiguity), or Low (minimal/conflicting signals)"},
					}},
					{Key: "language", Value: pyjson.Object{
						{Key: "type", Value: "string"},
						{Key: "description", Value: "The primary language of the content (e.g., 'English', 'Spanish', 'Multilingual')"},
					}},
					{Key: "political_leaning", Value: pyjson.Object{
						{Key: "type", Value: "string"},
						{Key: "enum", Value: []any{"Far Left", "Left", "Center-Left", "Center", "Center-Right", "Right", "Far Right", "Non-Political"}},
						{Key: "description", Value: "Political orientation of the content"},
					}},
					{Key: "audience_size", Value: pyjson.Object{
						{Key: "type", Value: "string"},
						{Key: "description", Value: "Conservatively estimate the CTV app's relative audience size. You have a strong tendency to overestimate. Be skeptical: high quality or brand recognition does not mean high viewership. Most CTV apps are XS or S. Use L/XL only for globally recognized streaming services. Use the following t-shirt sizes: XS, S, M, L, XL."},
						{Key: "enum", Value: []any{"XS", "S", "M", "L", "XL"}},
					}},
				}},
				{Key: "required", Value: []any{"quality", "justification", "vertical", "description", "target_audience", "content_type", "network_affiliation", "confidence", "language", "political_leaning", "audience_size"}},
			}},
		}},
	}
}
