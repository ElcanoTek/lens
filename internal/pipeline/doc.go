// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package pipeline is Lens's processing core: the `lens run` and
// `lens manage` commands, the orchestrator and the per-content-type
// processors (ports of main.py, manage.py, orchestration.py,
// domain_processing.py, app_processor.py and ctv_processor.py).
//
// Data flow:
//
//	input CSV/XLSX ──detect──► work items
//	  websites   : scrape (direct; auto mode then deep, then research) → classify → row
//	  iOS/Android: store metadata → classify → row
//	  CTV        : research → classify → post-process → row
//	each row ──► output CSV (append) + progress.json (saved per item)
//
// Auto mode's ladder is direct → deep (headless Chrome, single session,
// skipped without Podman and for anti-bot-blocked domains) → research
// (web-search model, never contacts the site). Earlier-pass failures are
// recorded as retry_pending while a later pass can still run, and the output
// is de-duplicated at the end so each identifier keeps its best row.
//
// The on-disk formats are the Python release's: the CSV uses Python's
// csv-module quoting and "\r\n" line ends, cells render values the way
// Python's str() did, progress entries' "result" payloads carry the row with
// the same keys, and the custom-category definitions are bound to the output
// through the "<output>.categories.json" sidecar and the progress file.
//
// The processors depend on small interfaces (Scraper, SiteClassifier,
// StoreClient, CategoryClassifier, ...) so tests drive them with fakes;
// adapters.go and stores_adapter.go wrap the real clients.
package pipeline
