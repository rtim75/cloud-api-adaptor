// (C) Copyright Confidential Containers Contributors
// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"encoding/json"
	"fmt"

	"cloud.google.com/go/auth"
	"cloud.google.com/go/auth/credentials"
)

func loadCredentials(raw string) (*auth.Credentials, error) {
	var config struct {
		Type credentials.CredType `json:"type"`
	}
	if err := json.Unmarshal([]byte(raw), &config); err != nil {
		return nil, fmt.Errorf("parsing GCP credential type: %w", err)
	}
	// GCP_CREDENTIALS is administrator-controlled configuration. In particular,
	// external_account endpoints and credential sources must come from a trusted
	// source; allowing its type does not validate those values.
	switch config.Type {
	case credentials.ServiceAccount, credentials.ExternalAccount:
	default:
		return nil, fmt.Errorf("unsupported GCP credential type: expected service_account or external_account")
	}
	return credentials.NewCredentialsFromJSON(config.Type, []byte(raw), &credentials.DetectOptions{
		Scopes: []string{computeScope},
	})
}
