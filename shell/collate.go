// SPDX-FileCopyrightText: 2026 Latere AI
// SPDX-License-Identifier: Apache-2.0

package shell

import (
	"cmp"
	"io/fs"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// collate compares two strings as glibc's en_US.UTF-8 orders them, the
// locale a sandbox image sets, near enough for names and lines: letters
// and digits first, with case and accents set aside, digits before
// letters; then accents, unaccented first; then case, lower first; then
// the whole strings again, with the punctuation and blanks the first levels
// skipped, which sort before letters and digits; and last the bytes.
func collate(a, b string) int {
	for _, level := range []collationLevel{levelBase, levelAccent, levelCase} {
		if c := compareLevel(a, b, level); c != 0 {
			return c
		}
	}
	if c := compareRest(a, b); c != 0 {
		return c
	}
	return strings.Compare(a, b)
}

// collationLevel is one of the first three levels collate compares the
// letters and digits of two strings at.
type collationLevel int

const (
	levelBase collationLevel = iota
	levelAccent
	levelCase
)

// compareLevel compares the letters and digits of a and b at one level,
// one by one, a string whose letters and digits end first before the
// other. It reads the strings in place, as sort and ls compare names
// many times each.
func compareLevel(a, b string, level collationLevel) int {
	i, j := 0, 0
	for {
		ra, ni := nextAlnum(a, i)
		rb, nj := nextAlnum(b, j)
		switch {
		case ni < 0 && nj < 0:
			return 0
		case ni < 0:
			return -1
		case nj < 0:
			return 1
		}
		if c := compareAt(ra, rb, level); c != 0 {
			return c
		}
		i, j = ni, nj
	}
}

// nextAlnum is the first letter or digit of s at or after i and the index
// after it, -1 when there is none.
func nextAlnum(s string, i int) (rune, int) {
	for i < len(s) {
		r, size := utf8.DecodeRuneInString(s[i:])
		i += size
		if isAlnum(r) {
			return r, i
		}
	}
	return 0, -1
}

// compareAt compares two letters or digits at one level: their base
// letters, then their accents, unaccented first, then their case, lower
// first.
func compareAt(a, b rune, level collationLevel) int {
	baseA, accentA := fold(unicode.ToLower(a))
	baseB, accentB := fold(unicode.ToLower(b))
	switch level {
	case levelBase:
		return compareBase(baseA, baseB)
	case levelAccent:
		return cmp.Compare(accentA, accentB)
	}
	return cmp.Compare(caseOf(a), caseOf(b))
}

func caseOf(r rune) int {
	if unicode.IsUpper(r) {
		return 1
	}
	return 0
}

// compareBase orders digits before letters, each by value.
func compareBase(a, b rune) int {
	da, db := unicode.IsDigit(a), unicode.IsDigit(b)
	switch {
	case da && !db:
		return -1
	case !da && db:
		return 1
	}
	return cmp.Compare(a, b)
}

// compareRest orders two strings the first three levels found equal,
// character by character: a punctuation mark or a blank before a letter or
// a digit, two marks by code point, and a string that ends first before
// the longer one. The letters and digits of such strings are the same, so
// two of them at one place are equal.
func compareRest(a, b string) int {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		ra, sa := utf8.DecodeRuneInString(a[i:])
		rb, sb := utf8.DecodeRuneInString(b[j:])
		i, j = i+sa, j+sb
		pa, pb := !isAlnum(ra), !isAlnum(rb)
		switch {
		case pa && !pb:
			return -1
		case !pa && pb:
			return 1
		case pa && pb && ra != rb:
			return cmp.Compare(markWeight(ra), markWeight(rb))
		}
	}
	switch {
	case i < len(a):
		return 1
	case j < len(b):
		return -1
	}
	return 0
}

// markWeight orders the marks: a byte that is no character, as a key cut
// inside one holds, before every mark, the rest by code point.
func markWeight(r rune) rune {
	if r == utf8.RuneError {
		return -1
	}
	return r
}

func isAlnum(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// accented maps the Latin letters with diacritics to their base letter.
var accented = map[rune]rune{
	'à': 'a', 'á': 'a', 'â': 'a', 'ã': 'a', 'ä': 'a', 'å': 'a', 'ā': 'a',
	'ç': 'c', 'č': 'c', 'ć': 'c',
	'è': 'e', 'é': 'e', 'ê': 'e', 'ë': 'e', 'ē': 'e', 'ę': 'e', 'ě': 'e',
	'ì': 'i', 'í': 'i', 'î': 'i', 'ï': 'i', 'ī': 'i',
	'ñ': 'n', 'ń': 'n', 'ň': 'n',
	'ò': 'o', 'ó': 'o', 'ô': 'o', 'õ': 'o', 'ö': 'o', 'ø': 'o', 'ō': 'o',
	'ù': 'u', 'ú': 'u', 'û': 'u', 'ü': 'u', 'ū': 'u', 'ů': 'u',
	'ý': 'y', 'ÿ': 'y', 'ś': 's', 'š': 's', 'ž': 'z', 'ź': 'z', 'ż': 'z', 'ł': 'l', 'ř': 'r',
}

func fold(r rune) (rune, int) {
	if b, ok := accented[r]; ok {
		return b, int(r)
	}
	return r, 0
}

// compareNames orders names as ls and the others list them: by collation,
// or by bytes under the C locale.
func compareNames(a, b string, byteOrder bool) int {
	if byteOrder {
		return strings.Compare(a, b)
	}
	return collate(a, b)
}

// sortInfos sorts directory entries by name.
func sortInfos(infos []fs.FileInfo, byteOrder bool) {
	slices.SortStableFunc(infos, func(a, b fs.FileInfo) int { return compareNames(a.Name(), b.Name(), byteOrder) })
}
