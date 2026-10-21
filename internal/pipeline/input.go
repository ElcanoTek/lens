// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package pipeline

import (
	"fmt"

	"github.com/ElcanoTek/lens/internal/detect"
	"github.com/ElcanoTek/lens/internal/item"
)

// loadInputData loads the website/app work list, detecting content types.
// A CTV list is routed to the CTV workflow in auto mode (isCTVInput) and
// only warned about otherwise.
func (o *Orchestrator) loadInputData() ([]item.WorkItem, error) {
	items, err := o.loadWorkItems()
	if err != nil {
		logger.Error(fmt.Sprintf("Failed to load input data: %v", err))
		return nil, err
	}
	return items, nil
}

func (o *Orchestrator) loadWorkItems() ([]item.WorkItem, error) {
	f, err := detect.LoadInput(o.cfg.InputCSVPath)
	if err != nil {
		return nil, err
	}
	if o.ctvMode {
		logger.Info("CTV mode enabled; treating input as a CTV app list")
		o.isCTVInput = true
		o.ctvItems = detect.ParseCTVInput(f)
		o.progress.SetTotal(len(o.ctvItems))
		logger.Info(fmt.Sprintf("Prepared %d unique CTV apps for processing", len(o.ctvItems)))
		return nil, nil
	}
	if detect.IsCTVInputFile(f) {
		if o.autoMode {
			logger.Info("Auto-detected a CTV app list; routing to the CTV workflow")
			o.isCTVInput = true
			return nil, nil
		}
		logger.Warn("Input appears to be a CTV list. Re-run with --ctv to process CTV apps explicitly.")
	}

	list, err := detect.BuildWorkList(f)
	if err != nil {
		return nil, err
	}
	o.hasIOSApps = list.Has(item.IOSApp)
	o.hasAndroidApps = list.Has(item.AndroidApp)
	o.hasWebsites = list.Has(item.Website)
	o.progress.SetTotal(len(list.Items))
	logger.Info(fmt.Sprintf("Prepared %d unique items for processing", len(list.Items)))
	return list.Items, nil
}

// loadCTVInputData loads and parses the CTV list.
func (o *Orchestrator) loadCTVInputData() ([]item.CTVWorkItem, error) {
	f, err := detect.LoadInput(o.cfg.InputCSVPath)
	if err != nil {
		logger.Error(fmt.Sprintf("Failed to load CTV input data: %v", err))
		return nil, err
	}
	if !detect.IsCTVInputFile(f) {
		logger.Warn("Input file does not appear to be a CTV file. Expected columns like 'app_name', 'bundle_id', 'platform', etc.")
	}
	items := detect.ParseCTVWorkItems(f)
	o.ctvItems = items
	o.hasCTVApps = len(items) > 0
	o.progress.SetTotal(len(items))
	logger.Info(fmt.Sprintf("Prepared %d CTV apps for processing", len(items)))
	return items, nil
}
