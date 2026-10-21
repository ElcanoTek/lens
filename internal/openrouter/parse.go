// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package openrouter

import (
	"fmt"
	"strings"

	"github.com/ElcanoTek/lens/internal/categories/pyjson"
)

// Valid values the model must choose from; anything else is replaced by the
// documented default (with a warning), never passed through.
var (
	validQualities    = []string{"Premium", "Standard", "Long Tail"}
	validConfidences  = []string{"High", "Medium", "Low"}
	validLeanings     = []string{"Far Left", "Left", "Center-Left", "Center", "Center-Right", "Right", "Far Right", "Non-Political"}
	validContentTypes = []string{"Live TV", "On-Demand Streaming", "Live Sports", "News", "Music/Audio", "Kids Content", "Fitness", "Educational", "Gaming", "Mixed Content"}
)

// extractMessage walks choices[0].message, tolerating null at any layer
// (some reasoning models return choices=null or message=null when their
// internal output overflows max_tokens).
func extractMessage(resp pyjson.Object) pyjson.Object {
	choices, _ := resp.Get("choices")
	list, ok := choices.([]any)
	if !ok || len(list) == 0 {
		return nil
	}
	first, ok := list[0].(pyjson.Object)
	if !ok {
		return nil
	}
	msg, _ := first.Get("message")
	m, _ := msg.(pyjson.Object)
	return m
}

// finishReason is choices[0].finish_reason as Python printed it ("None" when
// absent). "length" means the model ran out of max_tokens mid-response.
func finishReason(resp pyjson.Object) string {
	choices, _ := resp.Get("choices")
	if list, ok := choices.([]any); ok && len(list) > 0 {
		if first, ok := list[0].(pyjson.Object); ok {
			if r, ok := first.Get("finish_reason"); ok {
				if s, ok := r.(string); ok {
					return s
				}
			}
		}
	}
	return "None"
}

// usageTokens is usage.total_tokens, tolerating usage=null.
func usageTokens(resp pyjson.Object) int {
	usage, _ := resp.Get("usage")
	obj, ok := usage.(pyjson.Object)
	if !ok {
		return 0
	}
	switch n, _ := obj.Get("total_tokens"); t := n.(type) {
	case int64:
		return int(t)
	case float64:
		return int(t)
	}
	return 0
}

// messageContent is message.content when it is a string.
func messageContent(msg pyjson.Object) string {
	if msg == nil {
		return ""
	}
	v, _ := msg.Get("content")
	s, _ := v.(string)
	return s
}

// extractFunctionArguments pulls the function-call arguments out of a chat
// message. The standard tool_calls shape comes first; failing that,
// message.content is searched for an inline JSON object, for models (some
// reasoning variants) that emit the structured payload as text, sometimes
// wrapped in markdown fences or prose.
func extractFunctionArguments(msg pyjson.Object, expectedName string) pyjson.Object {
	if calls, _ := msg.Get("tool_calls"); truthy(calls) {
		args, err := toolCallArguments(calls, expectedName)
		if err == nil {
			return args
		}
		log.Warn(fmt.Sprintf("Failed to read tool_calls payload: %v", err))
	}
	content := messageContent(msg)
	start, end := strings.Index(content, "{"), strings.LastIndex(content, "}")
	if start == -1 || end == -1 || end <= start {
		return nil
	}
	parsed, err := pyjson.Decode([]byte(content[start : end+1]))
	if err != nil {
		return nil
	}
	if obj, ok := parsed.(pyjson.Object); ok {
		log.Info("Recovered classification arguments from message content (no tool_calls present)")
		return obj
	}
	return nil
}

func toolCallArguments(calls any, expectedName string) (pyjson.Object, error) {
	list, ok := calls.([]any)
	if !ok || len(list) == 0 {
		return nil, fmt.Errorf("tool_calls is not a list")
	}
	call, ok := list[0].(pyjson.Object)
	if !ok {
		return nil, fmt.Errorf("tool call is not an object")
	}
	fnValue, _ := call.Get("function")
	fn, ok := fnValue.(pyjson.Object)
	if !ok {
		return nil, fmt.Errorf("'function'")
	}
	if name, _ := fn.Get("name"); expectedName != "" && name != nil && name != expectedName {
		log.Warn(fmt.Sprintf("Function call name %s did not match expected %s", reprValue(name), pyRepr(expectedName)))
	}
	raw, ok := fn.Get("arguments")
	if !ok {
		return nil, fmt.Errorf("'arguments'")
	}
	if obj, ok := raw.(pyjson.Object); ok {
		return obj, nil
	}
	text, ok := raw.(string)
	if !ok {
		return nil, fmt.Errorf("the JSON object must be str, not %T", raw)
	}
	parsed, err := pyjson.Decode([]byte(text))
	if err != nil {
		return nil, err
	}
	obj, ok := parsed.(pyjson.Object)
	if !ok {
		// Python went on to call .get on a non-dict and failed the item;
		// falling back to the message content is strictly more useful.
		return nil, fmt.Errorf("arguments are not a JSON object")
	}
	return obj, nil
}

func reprValue(v any) string {
	if s, ok := v.(string); ok {
		return pyRepr(s)
	}
	return pyStr(v)
}

// argString returns args[key]: def when the key is missing, "" for null,
// and Python's str() of non-string values.
func argString(args pyjson.Object, key, def string) string {
	v, ok := args.Get(key)
	if !ok {
		return def
	}
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	}
	return pyStr(v)
}

func oneOf(value string, valid []string, def, field string) string {
	if containsString(valid, value) {
		return value
	}
	log.Warn(fmt.Sprintf("Invalid %s value '%s', defaulting to '%s'", field, value, def))
	return def
}

func applyTiers(c *Classification) {
	tiers := VerticalTiers(c.Vertical)
	if len(tiers) > 0 {
		c.VerticalTier1 = tiers[0]
	}
	if len(tiers) > 1 {
		c.VerticalTier2 = tiers[1]
	}
	if len(tiers) > 2 {
		c.VerticalTier3 = tiers[2]
	}
	if c.VerticalTier1 != "" {
		var parts []string
		for _, t := range []string{c.VerticalTier1, c.VerticalTier2, c.VerticalTier3} {
			if t != "" {
				parts = append(parts, t)
			}
		}
		c.Vertical = strings.Join(parts, " > ")
	}
}

func missingMessage(resp pyjson.Object, what string) error {
	log.Error(fmt.Sprintf("%s response missing message; raw response: %s", what, truncatedDump(resp)))
	return &parseError{msg: "Response missing choices[0].message"}
}

// parseClassification parses a website or app classification reply. It
// fails (and the caller retries with a larger budget, then records the item
// as Failed) whenever no classification can be recovered; nothing here
// writes a silent default classification.
func parseClassification(resp pyjson.Object, expectedName string) (Classification, error) {
	msg := extractMessage(resp)
	if msg == nil {
		return Classification{}, missingMessage(resp, "Classification")
	}
	args := extractFunctionArguments(msg, expectedName)
	if args == nil {
		log.Error(fmt.Sprintf("No tool_calls or parseable content in classification response; raw response: %s", truncatedDump(resp)))
		log.Error("Failed to parse classification response: Response does not contain function call or parseable content")
		return Classification{}, &parseError{msg: "Unparseable classification response: Response does not contain function call or parseable content"}
	}
	c := Classification{
		Quality:          argString(args, "quality", "Standard"),
		Justification:    argString(args, "justification", "No justification provided"),
		Vertical:         argString(args, "vertical", "Unknown"),
		Description:      argString(args, "description", "No description provided"),
		Confidence:       argString(args, "confidence", "Medium"),
		Language:         argString(args, "language", "English"),
		PoliticalLeaning: argString(args, "political_leaning", "Non-Political"),
		AudienceSize:     argString(args, "audience_size", "S"),
	}
	c.Quality = oneOf(c.Quality, validQualities, "Standard", "quality")
	applyTiers(&c)
	if c.VerticalTier1 == "" {
		if v := strings.ToLower(c.Vertical); c.Vertical == "" || v == "unknown" || v == "n/a" {
			c.Vertical = "Unknown"
		}
	}
	c.Confidence = oneOf(c.Confidence, validConfidences, "Medium", "confidence")
	c.PoliticalLeaning = oneOf(c.PoliticalLeaning, validLeanings, "Non-Political", "political_leaning")
	// No empty values, except fields with sensible defaults of their own.
	for _, field := range []*string{&c.Quality, &c.Justification, &c.Vertical, &c.Description, &c.Confidence} {
		if *field == "" || *field == "N/A" {
			*field = "Not provided"
		}
	}
	return c, nil
}

// parseCTVClassification parses a classify_ctv_app reply; see
// parseClassification for the failure rationale.
func parseCTVClassification(resp pyjson.Object) (Classification, error) {
	msg := extractMessage(resp)
	if msg == nil {
		return Classification{}, missingMessage(resp, "CTV classification")
	}
	args := extractFunctionArguments(msg, "classify_ctv_app")
	if args == nil {
		log.Error(fmt.Sprintf("No tool_calls or parseable content in CTV response; raw response: %s", truncatedDump(resp)))
		log.Error("Failed to parse CTV classification response: Response does not contain function call or parseable content")
		return Classification{}, &parseError{msg: "Unparseable CTV classification response: Response does not contain function call or parseable content"}
	}
	c := Classification{
		Quality:            argString(args, "quality", "Standard"),
		Justification:      argString(args, "justification", "No justification provided"),
		Vertical:           argString(args, "vertical", "Unknown"),
		Description:        argString(args, "description", "No description provided"),
		TargetAudience:     argString(args, "target_audience", "General audience"),
		ContentType:        argString(args, "content_type", "Mixed Content"),
		NetworkAffiliation: argString(args, "network_affiliation", ""),
		Confidence:         argString(args, "confidence", "Medium"),
		Language:           argString(args, "language", "English"),
		PoliticalLeaning:   argString(args, "political_leaning", "Non-Political"),
		AudienceSize:       argString(args, "audience_size", "S"),
	}
	c.Quality = oneOf(c.Quality, validQualities, "Standard", "quality")
	applyTiers(&c)
	c.Confidence = oneOf(c.Confidence, validConfidences, "Medium", "confidence")
	c.ContentType = oneOf(c.ContentType, validContentTypes, "Mixed Content", "content_type")
	c.PoliticalLeaning = oneOf(c.PoliticalLeaning, validLeanings, "Non-Political", "political_leaning")
	return c, nil
}
