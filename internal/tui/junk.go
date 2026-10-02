package tui

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// A junk reasoning block is text a model never wrote. The gateway in front of
// xdev can serve a leg whose decode fell apart, so the "reasoning" the client
// receives is symbol soup: the thinking box paints mojibake, and the run ends
// with no answer because the leg burned its whole output cap on the garbage.
//
// onegw already fails such a leg over before its first byte
// (translat.JunkGuard, 2026-09-21); this is the render-side half, for the
// streams that reach xdev anyway — a direct provider connection, an older
// gateway, or a corrupt leg a sibling could not replace. Two rules:
//
//  1. never paint the soup (gate the box, keeping its header and a notice),
//  2. judge it with the SAME measured rule onegw uses, so the two ends agree
//     on what "junk" means.
//
// The thresholds were fitted against the session corpus (45,017 reasoning
// blocks from 435 sessions): the rule fires on 5 of 25,000 sampled blocks
// >= 400 bytes (0.02%), all five genuine junk, while English, Mandarin,
// Vietnamese, Go source, commit-hash analysis and markdown tables stay clean.
// A deliberate ceiling, pinned by tests: junk whose LETTERS still sit in
// word-like runs while its tokens mix scripts (the pasted report's
// `*** Begin 拳击,   Boxing.""</-Re*=bC([R3]****** Репозиторий` half) measures
// like bilingual reasoning, and no cheap lexical test separates the two.
const (
	// junkMinBytes is the floor below which a block is never judged: a short
	// reasoning preamble carries no signal, and junk burns a whole cap.
	junkMinBytes = 600
	// junkTokenRatio is the alphanumeric share below which a token is soup
	// rather than a word (`,,,`, `####`, `!m"-WR*`).
	junkTokenRatio = 0.6
)

// junkThinking reports whether a reasoning body is symbol soup rather than
// language. Three independently-sufficient shapes, all measured:
//
//	(a) wordR < 0.92 && badR >= 0.25   letters left their words AND most
//	                                   tokens are punctuation (vvvv/comma storms)
//	(b) badR >= 0.40 && symPer100 >= 10 symbol runs dominate the bytes
//	(c) meanTokenLetters < 2.6 && symPer100 >= 10
//	                                   fragmentation (mixed-script soup)
func junkThinking(s string) bool {
	if len(s) < junkMinBytes {
		return false
	}
	wordR, badR, symPer100 := junkThinkingStats(s)
	if (wordR < 0.92 && badR >= 0.25) || (badR >= 0.40 && symPer100 >= 10) {
		return true
	}
	return meanTokenLetters(s) < 2.6 && symPer100 >= 10
}

// junkThinkingStats measures the verdict's inputs over s:
//
//	wordR     letters inside a multi-letter run / all letters. Prose keeps its
//	          letters in words (~1.0); a broken decode scatters them.
//	badR      fraction of tokens whose alphanumerics are a minority of the
//	          token length. A hash, version or identifier counts as clean.
//	symPer100 runs of two or more non-word non-space characters, per 100 bytes.
func junkThinkingStats(s string) (wordR, badR, symPer100 float64) {
	var letters, inWords, symBytes int
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		switch {
		case isWordRune(r):
			run := 0
			j := i
			for j < len(s) {
				r2, s2 := utf8.DecodeRuneInString(s[j:])
				if !isWordRune(r2) {
					break
				}
				run++
				j += s2
			}
			letters += run
			if run >= 2 {
				inWords += run
			}
			i = j
		case isSymbolRune(r):
			run := 0
			j := i
			for j < len(s) {
				r2, s2 := utf8.DecodeRuneInString(s[j:])
				if !isSymbolRune(r2) {
					break
				}
				run++
				j += s2
			}
			if run >= 2 {
				symBytes += run
			}
			i = j
		default:
			i += size
		}
	}

	var tokens, badTokens int
	for _, tok := range strings.Fields(s) {
		if utf8.RuneCountInString(tok) < 4 {
			continue
		}
		alnum, runes := 0, 0
		for _, r := range tok {
			runes++
			if unicode.IsLetter(r) || unicode.IsDigit(r) {
				alnum++
			}
		}
		tokens++
		// A token with no alphanumerics at all (`,,,,`, `####`, `!!`) is the
		// purest soup there is. Skipping those was the bug that let the comma
		// storms through: a punctuation-only stream left tokens == 0, which
		// then read as "no signal".
		if alnum == 0 || float64(alnum)/float64(runes) < junkTokenRatio {
			badTokens++
		}
	}

	wordR = 1
	if letters > 0 {
		wordR = float64(inWords) / float64(letters)
	}
	badR = 1
	if tokens > 0 {
		badR = float64(badTokens) / float64(tokens)
	}
	symPer100 = float64(symBytes) / float64(max(1, len(s))) * 100
	return wordR, badR, symPer100
}

// isWordRune reports whether r counts as word content (a letter or digit of
// any script).
func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// isSymbolRune reports whether r can be part of a symbol run: neither word
// content, nor whitespace, nor an identifier joiner, nor a combining mark
// (which rides along with the letter it modifies).
func isSymbolRune(r rune) bool {
	if r == '_' || isWordRune(r) || unicode.IsSpace(r) {
		return false
	}
	return !unicode.Is(unicode.Mn, r) && !unicode.Is(unicode.Mc, r)
}

// meanTokenLetters is the average letter count of the space-separated
// fragments in s. Junk fragments are two or three letters glued to symbols
// ('aszil,我们都是从这里开始', `$""O7ade!*ll*`); language keeps its letters in
// words.
func meanTokenLetters(s string) float64 {
	var letters, toks int
	for _, tok := range strings.Fields(s) {
		n := 0
		for _, r := range tok {
			if unicode.IsLetter(r) {
				n++
			}
		}
		if n == 0 {
			continue
		}
		toks++
		letters += n
	}
	if toks == 0 {
		return 0
	}
	return float64(letters) / float64(toks)
}

// junkReasoningNotice is what the thinking box shows instead of a soup body:
// one dim row in the frame's own style, naming what happened without
// pretending the reasoning was usable.
const junkReasoningNotice = "reasoning discarded · the model produced unreadable output"
