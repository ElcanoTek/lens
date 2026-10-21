// SPDX-License-Identifier: BUSL-1.1
// Copyright (c) 2026 ElcanoTek, Inc.

// Package auth is Lens's browser authentication, in one of two modes picked
// by LENS_AUTH_MODE:
//
//   - "elcano" (default, legacy): Lens owns no login. It verifies the shared
//     elcano_auth cookie (Ed25519, AUTH_SIGNING_PUBKEY) and redirects
//     signed-out browsers to AUTH_LOGIN_URL with ?return_to=.
//   - "central": Lens is a confidential client of central Auth
//     (AUTH_ISSUER_URL): authorization code + PKCE + nonce, a signed
//     id_token, and Lens-only sessions in a local SQLite database
//     (LENS_ACCESS_DB) together with the access allowlist, back-channel
//     logout replay records and Auth's access-provisioning versions.
//
// NewProviderFromEnv (or a ProviderCache) builds the Provider; Handlers are
// the /login, /auth/login, /auth/callback, /auth/backchannel-logout, /logout
// and /signed-out routes. AccessMain is `lens access` and
// MigrateAccessDBMain is `lens migrate-access-db` (run by bootstrap.sh and
// update.sh).
//
// The access database schema and every stored value are identical to the
// Python release's central_auth.py, so deployed hosts upgrade in place.
// Every error message here is free of credential material.
package auth
