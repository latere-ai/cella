// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package conformance

// row is one line of the error table: the status a code answers with and the
// fixed user sentence it carries.
type row struct {
	Status  int
	Message string
}

// errorTable is the error table of design 008, which is the contract and not
// a copy of any handler: one code, one status, one sentence. A server that
// answers a code with another status or another sentence fails the case that
// provoked it.
var errorTable = map[string]row{
	"unsupported_media_type": {415, "Send the manifest as JSON or YAML."},
	"not_acceptable":         {406, "This endpoint answers in JSON or YAML."},
	"multi_document":         {400, "Send one manifest per request."},
	"unsupported_version":    {400, "This server serves cella.latere.ai/v1beta1."},
	"unsupported_kind":       {400, "This server does not serve that kind."},
	"unknown_field":          {400, "The manifest has a field this schema does not know."},
	"missing_field":          {400, "A required field is missing."},
	"invalid_field":          {400, "A field has a value it cannot take."},
	"reserved_prefix":        {400, "That name is reserved for the control plane."},
	"exclusive_fields":       {400, "Two fields that cannot be set together are set."},
	"path_conflict":          {400, "Two mounts share a path."},
	"bad_request":            {400, "The request could not be read."},
	"body_too_large":         {413, "The request body is larger than this server accepts."},
	"unauthenticated":        {401, "Sign in and send a valid token."},
	"forbidden":              {403, "You do not have permission to do this."},
	"not_found":              {404, "There is no such object."},
	"immutable_field":        {409, "This field cannot be changed after the object is created."},
	"boundary_widened":       {409, "A sandbox cannot widen its own boundary."},
	"name_taken":             {409, "You already have an object with this name."},
	"version_conflict":       {409, "The object changed since you read it; read it again and retry."},
	"phase_conflict":         {409, "The sandbox is not in a state that allows this."},
	"volume_busy":            {409, "The volume is attached in a way that excludes this."},
	"secret_host_conflict":   {409, "Two mounted secrets apply to the same host."},
	"secret_out_of_scope":    {422, "The secret does not cover the host it is used for."},
	"ceiling_exceeded":       {422, "The value is above what this server allows."},
	"capability_unsupported": {422, "The environment cannot provide this."},
	"environment_mismatch":   {422, "The worker does not match the environment it registered for."},
	"admission_refused":      {422, "The request was refused by this server's policy."},
	"quota_exceeded":         {422, "You have reached your sandbox limit."},
	"boundary_exceeded":      {422, "A child sandbox cannot exceed its parent's boundary."},
	"spawn_budget_exhausted": {422, "The sandbox has no spawn budget left."},
	"cursor_expired":         {410, "The feed no longer holds the records after that position; read it again from the newest."},
	"rate_limited":           {429, "Too many requests; wait and retry."},
	"admission_unavailable":  {503, "The policy service is unavailable; retry shortly."},
	"authorizer_unavailable": {503, "The permission service is unavailable; retry shortly."},
	"driver_unavailable":     {503, "The environment is unavailable; retry shortly."},
	"upstream_unavailable":   {502, "Nothing is listening on that port."},
}
