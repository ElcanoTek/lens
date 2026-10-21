// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

package pipeline

import (
	"context"
	"errors"

	"github.com/ElcanoTek/lens/internal/config"
)

// TODO(adapter gap): replace with internal/stores once it lands.
func defaultNewIOS(_ context.Context, _ *config.Config) (StoreClient, error) {
	return nil, errors.New("iOS store client is not available in this build")
}

func defaultNewAndroid(_ context.Context, _ *config.Config) (StoreClient, error) {
	return nil, errors.New("Android store client is not available in this build")
}
