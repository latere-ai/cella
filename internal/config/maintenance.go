// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"errors"
	"sort"
	"strings"
)

// MaintenanceConfig is what cellad unseal and cellad rotate read: the
// database, over its direct endpoint since a migration holds a session lock a
// transaction pooler loses, and the key what it holds is sealed under now,
// with the key a rotation moves it to.
type MaintenanceConfig struct {
	DBURL         string
	SecretKey     []byte
	NextSecretKey []byte
}

// LoadMaintenance reads the database and the key, and the next key where
// rotate is true. Each is required: a run against no database or without the
// key has nothing it could do.
func LoadMaintenance(getenv Getenv, rotate bool) (MaintenanceConfig, error) {
	var problems []string
	c := MaintenanceConfig{
		DBURL:     databaseURL(getenv("CELLA_DB_URL"), "CELLA_DB_URL", &problems),
		SecretKey: secretKey(getenv, &problems),
	}
	if strings.TrimSpace(getenv("CELLA_DB_URL")) == "" {
		problems = append(problems, "CELLA_DB_URL is unset, and it names the database the run rewrites")
	}
	if strings.TrimSpace(getenv("CELLA_SECRET_KEY")) == "" {
		problems = append(problems, "CELLA_SECRET_KEY is unset, and it is the key the database is sealed under")
	}
	if rotate {
		c.NextSecretKey = keyNamed(getenv, "CELLA_SECRET_KEY_NEXT", &problems)
		if strings.TrimSpace(getenv("CELLA_SECRET_KEY_NEXT")) == "" {
			problems = append(problems, "CELLA_SECRET_KEY_NEXT is unset, and it is the key the rotation moves the database to")
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return MaintenanceConfig{}, errors.New("configuration: " + strings.Join(problems, "; "))
	}
	return c, nil
}
