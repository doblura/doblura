// SPDX-License-Identifier: AGPL-3.0-or-later
// Copyright (C) 2026 Antoni Romera

package controller

import "crypto"

// cryptoSHA256 is extracted into a constant so githubapp.go does not have to
// import the crypto package just for this.
const cryptoSHA256 = crypto.SHA256
