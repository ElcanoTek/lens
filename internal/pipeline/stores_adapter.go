// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package pipeline

import (
	"context"

	"github.com/ElcanoTek/lens/internal/config"
	"github.com/ElcanoTek/lens/internal/stores"
)

// The store clients always report a rating and a count on success (0.0 / 0
// when the store showed none), so the Rating/Rating_Count cells are never
// blank for a successful app, as in the Python release.

type iosAdapter struct{ c *stores.IOSClient }

func defaultNewIOS(_ context.Context, cfg *config.Config) (StoreClient, error) {
	return NewIOSAdapter(stores.NewIOSClient(stores.IOSOptionsFromConfig(cfg))), nil
}

// NewIOSAdapter wraps an iTunes Search API client as a StoreClient.
func NewIOSAdapter(c *stores.IOSClient) StoreClient { return iosAdapter{c: c} }

func (a iosAdapter) FetchAppMetadata(ctx context.Context, id string) (AppMetadata, error) {
	r := a.c.FetchAppMetadata(ctx, id)
	if r == nil {
		return AppMetadata{Error: "Unknown error"}, nil
	}
	rating, count := r.Rating, r.RatingCount
	return AppMetadata{
		Success:       r.Success,
		Error:         r.Error,
		AppName:       r.AppName,
		Developer:     r.Developer,
		PrimaryGenre:  r.PrimaryGenre,
		Description:   r.Description,
		Rating:        &rating,
		RatingCount:   &count,
		ContentForLLM: r.ContentForLLM,
		FetchedAt:     r.FetchedAt,
	}, nil
}

func (a iosAdapter) Close() error {
	a.c.Close()
	return nil
}

type androidAdapter struct{ c *stores.AndroidClient }

func defaultNewAndroid(_ context.Context, cfg *config.Config) (StoreClient, error) {
	o := stores.AndroidOptionsFromConfig(cfg)
	// orchestration.py passed user_agent=config.USER_AGENT to the scraper.
	if cfg.UserAgent != "" {
		o.UserAgent = cfg.UserAgent
	}
	return NewAndroidAdapter(stores.NewAndroidClient(o)), nil
}

// NewAndroidAdapter wraps a Play Store scraper as a StoreClient.
func NewAndroidAdapter(c *stores.AndroidClient) StoreClient { return androidAdapter{c: c} }

func (a androidAdapter) FetchAppMetadata(ctx context.Context, id string) (AppMetadata, error) {
	r := a.c.FetchAppMetadata(ctx, id)
	if r == nil {
		return AppMetadata{Error: "Unknown error"}, nil
	}
	rating, count := r.Rating, r.RatingCount
	return AppMetadata{
		Success:       r.Success,
		Error:         r.Error,
		AppName:       r.AppName,
		Developer:     r.Developer,
		Category:      r.Category,
		Description:   r.Description,
		Rating:        &rating,
		RatingCount:   &count,
		Downloads:     r.Downloads,
		ContentForLLM: r.ContentForLLM,
		FetchedAt:     r.FetchedAt,
	}, nil
}

func (a androidAdapter) Close() error {
	a.c.Close()
	return nil
}
