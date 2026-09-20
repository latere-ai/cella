// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strconv"
	"strings"
	"time"

	"latere.ai/x/cella/internal/admission"
)

// Defaults for the admission variables of spec 007.
const (
	// DefaultAdmissionTimeout bounds one admission call. Admission has no
	// retry, so this is the whole of the deadline and not half of it.
	DefaultAdmissionTimeout = admission.DefaultTimeout
	// MinAdmissionTimeout and MaxAdmissionTimeout bound what an operator
	// may set it to: a deadline under the first refuses every call that
	// crosses a network, and one past the second holds an apply open
	// longer than the caller waits.
	MinAdmissionTimeout = admission.MinTimeout
	MaxAdmissionTimeout = admission.MaxTimeout
)

// Admission is the configuration of spec 007's step: the endpoint stage 3
// of a resolve calls, and the fallback an installation without one runs.
type Admission struct {
	// URL is CELLA_ADMISSION_URL and Token the bearer CELLA_ADMISSION_TOKEN
	// it requires. An unset URL selects the built-in identity step; a URL
	// without a token is a start-up failure.
	URL   string
	Token string
	// Timeout is CELLA_ADMISSION_TIMEOUT, the deadline of one call.
	Timeout time.Duration
	// DefaultImage is CELLA_DEFAULT_IMAGE, the reference a manifest that
	// names none takes where the environment runs images. It is unset by
	// default: an installation with an admission step lets that step
	// supply the image, which is what an image catalogue is.
	DefaultImage string
}

// Enabled reports whether a webhook is configured, which is what the
// start-up line says and what selects the client over the identity step.
func (a Admission) Enabled() bool { return a.URL != "" }

// Mode is the word the start-up line carries.
func (a Admission) Mode() string {
	if a.Enabled() {
		return "webhook"
	}
	return "builtin"
}

// loadAdmission reads spec 007's variables. The URL rules are the
// authorizer's, because the two endpoints carry the same kind of bearer
// over the same kind of hop.
func loadAdmission(getenv Getenv, problems *[]string) Admission {
	a := Admission{
		URL:          strings.TrimSpace(getenv("CELLA_ADMISSION_URL")),
		Token:        strings.TrimSpace(getenv("CELLA_ADMISSION_TOKEN")),
		DefaultImage: strings.TrimSpace(getenv("CELLA_DEFAULT_IMAGE")),
	}
	if a.URL != "" {
		if u, ok := endpoint(a.URL); !ok {
			*problems = append(*problems, "CELLA_ADMISSION_URL is "+strconv.Quote(a.URL)+", not an absolute http:// or https:// URL with a host")
		} else if !isHTTPS(u) && !isLoopback(u) {
			*problems = append(*problems, "CELLA_ADMISSION_URL is http:// on a host other than loopback; a manifest and the bearer that admits it do not travel in the clear")
		}
		if a.Token == "" {
			*problems = append(*problems, "CELLA_ADMISSION_TOKEN is unset while CELLA_ADMISSION_URL is set; the endpoint requires a bearer")
		}
	}
	a.Timeout = duration(getenv, "CELLA_ADMISSION_TIMEOUT", DefaultAdmissionTimeout, problems)
	if a.Timeout < MinAdmissionTimeout || a.Timeout > MaxAdmissionTimeout {
		*problems = append(*problems, "CELLA_ADMISSION_TIMEOUT is "+a.Timeout.String()+"; between "+MinAdmissionTimeout.String()+" and "+MaxAdmissionTimeout.String())
		a.Timeout = DefaultAdmissionTimeout
	}
	return a
}
