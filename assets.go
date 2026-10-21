// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package lens holds the files every Lens binary carries with it: the
// dashboard's static assets and templates, the IAB content taxonomy and the
// IANA TLD list. Embedding them means a deployed binary can never run against
// a taxonomy or a main.js from a different release.
package lens

import "embed"

// Static is the dashboard's static tree (served under /static).
//
//go:embed all:static
var Static embed.FS

// Templates holds the dashboard's HTML templates.
//
//go:embed templates
var Templates embed.FS

// ContentTaxonomy is the IAB content taxonomy (TSV) the classifier may use.
//
//go:embed content_taxonomy.tsv
var ContentTaxonomy []byte

// IANATLDs is the IANA root-zone TLD list used by input detection.
//
//go:embed iana_tlds.txt
var IANATLDs []byte
