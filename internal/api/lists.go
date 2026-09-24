// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"slices"
	"strconv"

	"latere.ai/x/pkg/authz"

	"latere.ai/x/cella/manifest"
)

// pageLimit reads the limit of one page of a list: 1 to 200, 50 when the
// request names none, and anything else invalid_field.
func pageLimit(r *http.Request) (int, error) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return 50, nil
	}
	limit, err := strconv.Atoi(raw)
	if err != nil || limit < 1 || limit > 200 {
		return 0, &manifest.Error{Code: "invalid_field", Path: "limit", Detail: "limit must be between 1 and 200"}
	}
	return limit, nil
}

// admits is the filter step of design 008's list rule: a list decision's
// filter over one row. The row's owner is one the filter names, when it names
// any, and the row carries every label the filter names with the value it
// names. A decision with no filter admits every row. The kind's read still
// decides each row the filter admits, so the filter narrows a list and never
// widens it.
func admits(filter *authz.Filter, owner string, labels map[string]string) bool {
	if filter == nil {
		return true
	}
	if len(filter.Owners) > 0 && !slices.Contains(filter.Owners, owner) {
		return false
	}
	for k, v := range filter.Labels {
		if got, ok := labels[k]; !ok || got != v {
			return false
		}
	}
	return true
}

// admitsEnvironment is the filter step for one environment. The default one,
// which CELLA_DEFAULT_ENVIRONMENT names, passes it whatever the filter names:
// every subject may use it, its owner is the control plane's own subject or
// the administrator who applied it, and it carries no caller's labels, so no
// filter over owners and labels names it without naming what the filter hides
// (spec 006). Its environment.read is what decides whether it is listed.
func admitsEnvironment(filter *authz.Filter, defaultEnvironment, name, owner string, labels map[string]string) bool {
	return (defaultEnvironment != "" && name == defaultEnvironment) || admits(filter, owner, labels)
}
