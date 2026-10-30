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
	ka, kb := collationKey(a), collationKey(b)
	if c := slices.CompareFunc(ka.base, kb.base, compareBase); c != 0 {
		return c
	}
	if c := slices.Compare(ka.accents, kb.accents); c != 0 {
		return c
	}
	if c := slices.Compare(ka.cases, kb.cases); c != 0 {
		return c
	}
	if c := compareRest(a, b); c != 0 {
		return c
	}
	return strings.Compare(a, b)
}

type collation struct {
	base    []rune
	accents []int
	cases   []int
}

func collationKey(s string) collation {
	var k collation
	for _, r := range s {
		if !isAlnum(r) {
			continue
		}
		base, accent := fold(unicode.ToLower(r))
		k.base = append(k.base, base)
		k.accents = append(k.accents, accent)
		c := 0
		if unicode.IsUpper(r) {
			c = 1
		}
		k.cases = append(k.cases, c)
	}
	return k
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
	ra, rb := []rune(a), []rune(b)
	for i := 0; i < len(ra) && i < len(rb); i++ {
		pa, pb := !isAlnum(ra[i]), !isAlnum(rb[i])
		switch {
		case pa && !pb:
			return -1
		case !pa && pb:
			return 1
		case pa && pb && ra[i] != rb[i]:
			return cmp.Compare(markWeight(ra[i]), markWeight(rb[i]))
		}
	}
	return cmp.Compare(len(ra), len(rb))
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
