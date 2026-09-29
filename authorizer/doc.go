// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

// Package authorizer is the vocabulary an authorization endpoint for
// Cella is written against: the actions cellad asks, the resource kind
// each one acts on, and the limits an allow may carry. Import it to
// write the endpoint CELLA_AUTHORIZER_URL points at, in Go, instead of
// keeping a copy of the strings.
//
// The envelope on the wire is latere.ai/x/pkg/authz's, and this package
// declares none of it: cellad POSTs a request with the caller's subject,
// its claims, an action, and a resource, and reads back an allow or a
// deny, all of it that package's types. This package is the Cella half
// of that contract and nothing more. Vocabulary is the whole action
// table as authz.Vocabulary, which the client refuses an unknown action
// against, latere.ai/x/pkg/authz/server validates against, and
// latere.ai/x/pkg/authz/conformance drives its cases from:
//
//	conformance.Run(t, url, token, conformance.WithVocabulary(authorizer.Vocabulary()))
//
// Actions lists every action, thirty-two of them, each one a constant
// here; Kind reports the resource kind an action acts on, and Known
// whether a string is in the vocabulary at all. Nothing here dials: the
// package builds values and decodes them, and the client is the
// caller's.
//
// An endpoint written on latere.ai/x/pkg/authz/server answers every
// action of the table from one Decider. The scaffold routes to a Lister
// only the actions an endpoint names in server.Options.PageActions, and
// Cella names none: its five list actions are a decision like any other,
// an allow whose optional filter narrows the page to owners and labels
// (spec 006). authz.IsList reads the verb and routes nothing, so an
// endpoint that serves the five writes that decision from the same
// Decider as the other twenty-seven.
//
// An allow may carry ceilings. WireLimits is the limits object as the
// answer renders it, so an endpoint writes the type cellad decodes:
//
//	limits := authorizer.WireLimits{MaxSandboxes: &ten}
//
// A member the endpoint does not set is left out, which grants nothing
// and takes nothing away; a member set to zero is sent and reads as
// zero, the same grant. DecodeLimits is the reading half, cellad's own,
// and is here so an endpoint's tests can hold their answers to it: a
// figure below zero is an error, and cellad treats such an answer as no
// decision at all.
//
// A decision names a subject as the issuer and the sub joined,
// "https://login.example.com|alice", and the claims are the token's
// verbatim, which is where an endpoint reads a plan, a team, or a role
// from.
//
// The resource carries the object's own members flat beside its kind and
// id: its name, its owner and its labels, and the members its kind adds,
// such as a sandbox's environment, parent and root. This package declares
// no type for them; an endpoint reads them with authz.Resource's
// accessors. An update carries one more: sandbox.update and secret.update
// add "proposed", the owner, metadata and spec of the object the request
// would write, so an endpoint reads the change and can refuse one it does
// not allow, such as a label it reads tenancy from being dropped. A
// Secret's proposed spec never carries its value, and no request carries a
// Secret's value in any member.
//
// The promise, as for every package at this module's root: additive
// within a module major, and the same on every build. An action string
// never changes and never disappears, a resource kind stays the kind it
// is, and a limits member keeps its wire name and its meaning. A new
// action is a new row in spec 006's table first and a constant here
// second, so an endpoint that decides by the constants keeps compiling
// and an endpoint that decides by a default keeps deciding.
package authorizer
