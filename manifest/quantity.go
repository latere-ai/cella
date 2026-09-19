// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package manifest

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	v1 "latere.ai/x/cella/manifest/v1"
)

// MilliScale is the number of powers of ten between a base unit and the
// milli-unit ParseQuantity returns. CPU quantities are cores, memory and disk
// quantities are bytes.
const MilliScale = 3

var decimalSuffix = map[byte]int{'n': -9, 'u': -6, 'm': -3, 'k': 3, 'M': 6, 'G': 9, 'T': 12, 'P': 15, 'E': 18}

// ParseQuantity parses the Kubernetes quantity subset the manifest contract
// admits and returns the value in milli-units, so 2 cores is 2000 and 1Ki is
// 1048576000. The subset is a decimal mantissa with an optional fraction, an
// optional decimal exponent (1e3), a decimal SI suffix (n, u, m, k, M, G, T,
// P, E) or a binary SI suffix (Ki, Mi, Gi, Ti, Pi, Ei). A value with
// precision finer than a milli-unit, and one whose milli-unit form does not
// fit an int64, are both errors: neither can be enforced by a cgroup or a
// volume size. The package parses quantities itself so validating a manifest
// pulls no Kubernetes API machinery.
func ParseQuantity(q v1.Quantity) (int64, error) {
	s := string(q)
	if s == "" {
		return 0, errors.New("quantity is empty")
	}
	negative := false
	switch s[0] {
	case '+':
		s = s[1:]
	case '-':
		negative, s = true, s[1:]
	}
	dec, bin, err := splitSuffix(&s)
	if err != nil {
		return 0, err
	}
	digits, fraction, err := mantissa(s)
	if err != nil {
		return 0, err
	}
	value, err := strconv.ParseInt(digits, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("quantity %q is out of range", string(q))
	}
	// The binary factor is applied first so a fractional binary quantity such
	// as 0.0005Ki, which is whole in milli-units, is not refused as too fine.
	if value, err = shiftLeft(value, 10*bin); err != nil {
		return 0, fmt.Errorf("quantity %q is out of range", string(q))
	}
	switch exponent := dec - fraction + MilliScale; {
	case exponent > 0:
		if value, err = mulPow10(value, exponent); err != nil {
			return 0, fmt.Errorf("quantity %q is out of range", string(q))
		}
	case exponent < 0:
		if value, err = divPow10(value, -exponent); err != nil {
			return 0, fmt.Errorf("quantity %q is finer than a milli-unit", string(q))
		}
	}
	if negative {
		value = -value
	}
	return value, nil
}

// splitSuffix strips the suffix from s and returns its decimal exponent and
// its binary index, where 1 is Ki and 6 is Ei.
func splitSuffix(s *string) (dec, bin int, err error) {
	v := *s
	if len(v) >= 2 && v[len(v)-1] == 'i' {
		i := strings.IndexByte("KMGTPE", v[len(v)-2])
		if i < 0 {
			return 0, 0, fmt.Errorf("binary suffix %q is not one of Ki, Mi, Gi, Ti, Pi, Ei", v[len(v)-2:])
		}
		*s = v[:len(v)-2]
		return 0, i + 1, nil
	}
	// An e or E with digits after it is an exponent; E at the end is the exa
	// suffix. Both forms end the mantissa.
	if i := strings.IndexAny(v, "eE"); i >= 0 && i < len(v)-1 {
		exponent, err := strconv.Atoi(v[i+1:])
		if err != nil {
			return 0, 0, fmt.Errorf("exponent %q is not a number", v[i+1:])
		}
		*s = v[:i]
		return exponent, 0, nil
	}
	if v == "" {
		return 0, 0, errors.New("quantity has no digits")
	}
	if exponent, ok := decimalSuffix[v[len(v)-1]]; ok {
		*s = v[:len(v)-1]
		return exponent, 0, nil
	}
	return 0, 0, nil
}

// mantissa returns the digits of v without its decimal point and the number of
// fraction digits.
func mantissa(v string) (digits string, fraction int, err error) {
	whole, frac, dot := strings.Cut(v, ".")
	if dot && frac == "" {
		return "", 0, fmt.Errorf("quantity %q has no fraction digits", v)
	}
	digits = whole + frac
	if digits == "" {
		return "", 0, errors.New("quantity has no digits")
	}
	for i := range len(digits) {
		if digits[i] < '0' || digits[i] > '9' {
			return "", 0, fmt.Errorf("quantity %q is not a number", v)
		}
	}
	return digits, len(frac), nil
}

var errOverflow = errors.New("value is out of range")

func shiftLeft(v int64, bits int) (int64, error) {
	if bits == 0 || v == 0 {
		return v, nil
	}
	if bits >= 63 || v > math.MaxInt64>>bits {
		return 0, errOverflow
	}
	return v << bits, nil
}

func mulPow10(v int64, exponent int) (int64, error) {
	for range exponent {
		if v > math.MaxInt64/10 {
			return 0, errOverflow
		}
		v *= 10
	}
	return v, nil
}

func divPow10(v int64, exponent int) (int64, error) {
	for range exponent {
		if v%10 != 0 {
			return 0, errors.New("value is finer than a milli-unit")
		}
		v /= 10
	}
	return v, nil
}
