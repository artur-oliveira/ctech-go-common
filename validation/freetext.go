package validation

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Freetext failure reasons; the returned error wraps one, so use errors.Is.
var (
	ErrFreetextLineBreaks = errors.New("must not contain line breaks")
	ErrFreetextTooShort   = errors.New("too short")
	ErrFreetextTooLong    = errors.New("too long")
	ErrFreetextRepeated   = errors.New("looks like repeated characters")
	ErrFreetextUnreadable = errors.New("must contain readable text")
)

// FreetextRule bounds one free-text field. Min/Max are inclusive, in runes
// after trimming.
type FreetextRule struct {
	Min int
	Max int
	// AllowNewlines permits \n/\r. Leave false for anything that lands on one
	// logical line, above all a raw MIME header (CRLF/header injection).
	AllowNewlines bool
}

const (
	// minLetterRatio: floor for letters / non-space runes; below it the input is
	// keyboard-mashing or punctuation spam. A bot filter, not a quality judge.
	minLetterRatio = 0.3
	// maxRepeat: identical consecutive runes tolerated before rejecting as junk.
	maxRepeat = 4
)

// Freetext trims s, checks its length against rule and rejects low-signal junk.
// It returns the trimmed string on success.
func Freetext(s string, rule FreetextRule) (string, error) {
	t := strings.TrimSpace(s)
	if !rule.AllowNewlines && strings.ContainsAny(t, "\r\n") {
		return "", ErrFreetextLineBreaks
	}
	n := utf8.RuneCountInString(t)
	if n < rule.Min {
		return "", fmt.Errorf("%w: at least %d characters", ErrFreetextTooShort, rule.Min)
	}
	if n > rule.Max {
		return "", fmt.Errorf("%w: at most %d characters", ErrFreetextTooLong, rule.Max)
	}
	if hasLongRepeat(t) {
		return "", ErrFreetextRepeated
	}
	if !hasEnoughLetters(t) {
		return "", ErrFreetextUnreadable
	}
	return t, nil
}

func hasLongRepeat(s string) bool {
	run, prev := 0, rune(-1)
	for _, r := range s {
		if r == prev {
			run++
		} else {
			run, prev = 1, r
		}
		if run > maxRepeat {
			return true
		}
	}
	return false
}

func hasEnoughLetters(s string) bool {
	var letters, nonSpace int
	for _, r := range s {
		if unicode.IsSpace(r) {
			continue
		}
		nonSpace++
		if unicode.IsLetter(r) {
			letters++
		}
	}
	return nonSpace > 0 && float64(letters)/float64(nonSpace) >= minLetterRatio
}
