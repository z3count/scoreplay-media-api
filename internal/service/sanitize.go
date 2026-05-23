package service

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/scoreplay/media-api/internal/domain"
	"golang.org/x/text/unicode/norm"
)

// maxTagsPerMedia is the maximum number of tags that can be associated with
// a single media item. This prevents abuse via massive INSERT batches into
// the media_tags junction table.
const maxTagsPerMedia = 50

// Unicode codepoint ranges that are rejected in user-facing text fields.
// These characters are either invisible, can cause display/security issues,
// or indicate malformed input.
//
// Blocked categories:
//
//   - Control characters (U+0000–U+001F, U+007F–U+009F):
//     Break JSON serialization, terminal output, and log aggregation tools.
//     Includes null bytes (U+0000) which truncate strings in C-based systems.
//
//   - Zero-width characters (U+200B–U+200F, U+FEFF):
//     Invisible characters that cause confusing behavior: two strings that
//     look identical to users but don't match in search/comparison.
//     U+200B = zero-width space, U+200C/D = joiners, U+FEFF = BOM.
//
//   - Bidirectional override characters (U+202A–U+202E, U+2066–U+2069):
//     Can make text render right-to-left or override natural direction.
//     Used in "Bidi attacks" where displayed text differs from stored text.
//     See: https://trojansource.codes/
//
//   - Deprecated tag characters (U+E0001–U+E007F):
//     Part of a deprecated Unicode mechanism with no legitimate use.
//
//   - Interlinear annotation anchors (U+FFF9–U+FFFB):
//     Internal Unicode markup with no use in user content.
//
//   - Replacement character (U+FFFD):
//     Indicates malformed UTF-8 was decoded upstream. If present, the input
//     had encoding errors that were silently replaced.

// isUnsafeRune returns true if the rune should be rejected in user input.
func isUnsafeRune(r rune) bool {
	// Control characters (C0: U+0000–U+001F, DEL: U+007F, C1: U+0080–U+009F).
	// Exception: tabs (U+0009) and newlines (U+000A, U+000D) are normalized
	// to spaces rather than rejected — they can appear in copy-pasted text.
	if unicode.IsControl(r) && r != '\t' && r != '\n' && r != '\r' {
		return true
	}

	switch {
	// Zero-width characters.
	case r == '\u200B': // zero-width space
		return true
	case r == '\u200C': // zero-width non-joiner
		return true
	case r == '\u200D': // zero-width joiner
		return true
	case r == '\u200E': // left-to-right mark
		return true
	case r == '\u200F': // right-to-left mark
		return true
	case r == '\uFEFF': // byte order mark (BOM)
		return true

	// Bidirectional overrides (U+202A–U+202E).
	case r >= '\u202A' && r <= '\u202E':
		return true

	// Bidirectional isolates (U+2066–U+2069).
	case r >= '\u2066' && r <= '\u2069':
		return true

	// Interlinear annotation anchors (U+FFF9–U+FFFB).
	case r >= '\uFFF9' && r <= '\uFFFB':
		return true

	// Replacement character — indicates upstream UTF-8 decoding errors.
	case r == '\uFFFD':
		return true

	// Deprecated tag characters (U+E0001–U+E007F).
	case r >= 0xE0001 && r <= 0xE007F:
		return true
	}

	return false
}

// unicodeWhitespace matches Unicode whitespace characters that should be
// normalized to regular ASCII spaces. This includes:
//   - U+00A0 NO-BREAK SPACE (from copy-paste, especially from Word/PDF)
//   - U+2000–U+200A various typographic spaces (en/em/thin/hair spaces)
//   - U+2028 LINE SEPARATOR
//   - U+2029 PARAGRAPH SEPARATOR
//   - U+202F NARROW NO-BREAK SPACE
//   - U+205F MEDIUM MATHEMATICAL SPACE
//   - U+3000 IDEOGRAPHIC SPACE (CJK full-width space)
var unicodeWhitespace = regexp.MustCompile(`[\x{00A0}\x{2000}-\x{200A}\x{2028}\x{2029}\x{202F}\x{205F}\x{3000}]`)

// multipleSpaces matches two or more consecutive ASCII spaces.
var multipleSpaces = regexp.MustCompile(`\s{2,}`)

// SanitizeName validates and normalizes a user-provided name (tag or media).
//
// Processing pipeline:
//  1. Reject invalid UTF-8 sequences.
//  2. NFC normalization — ensures canonical Unicode composition so that
//     "é" (U+0065 U+0301) and "é" (U+00E9) are stored identically.
//     This prevents duplicate tags that look the same but differ at byte level.
//  3. Replace Unicode whitespace variants with standard ASCII space.
//  4. Replace tabs/newlines with spaces (from copy-pasted text).
//  5. Collapse multiple consecutive spaces into one.
//  6. Trim leading/trailing whitespace.
//  7. Reject if any unsafe runes remain (control chars, zero-width, bidi, etc.).
//  8. Reject if empty after processing.
//  9. Reject if exceeds max length.
//
// What is ALLOWED (all scripts are welcome):
//   - Latin with accents: Mbappé, Müller, Señor
//   - CJK ideographs: 田中将大, 손흥민, 大谷翔平
//   - Arabic, Hebrew, Devanagari, Thai: محمد صلاح
//   - Emoji: ⚽ 🏆 🇫🇷
//   - Numbers, punctuation: Ligue 1, U-21, Player's Highlight
//
// What is REJECTED (invisible or dangerous):
//   - Control characters (break logs/JSON)
//   - Zero-width characters (invisible, cause search mismatches)
//   - Bidi overrides (text renders differently than stored — security risk)
//   - Replacement char U+FFFD (indicates upstream encoding errors)
func SanitizeName(s string) (string, error) {
	// Step 1: Reject invalid UTF-8.
	if !utf8.ValidString(s) {
		return "", fmt.Errorf("%w: name contains invalid UTF-8 sequences", domain.ErrValidation)
	}

	// Step 2: NFC normalization.
	s = norm.NFC.String(s)

	// Step 3: Replace Unicode whitespace with regular space.
	s = unicodeWhitespace.ReplaceAllString(s, " ")

	// Step 4: Replace tabs/newlines with spaces.
	s = strings.NewReplacer("\t", " ", "\n", " ", "\r", " ").Replace(s)

	// Step 5: Collapse multiple spaces.
	s = multipleSpaces.ReplaceAllString(s, " ")

	// Step 6: Trim.
	s = strings.TrimSpace(s)

	// Step 7: Check for unsafe runes.
	for _, r := range s {
		if isUnsafeRune(r) {
			return "", fmt.Errorf("%w: name contains disallowed character U+%04X", domain.ErrValidation, r)
		}
	}

	// Step 8: Non-empty check.
	if s == "" {
		return "", fmt.Errorf("%w: name is required", domain.ErrValidation)
	}

	// Step 9: Length check.
	if len(s) > maxTagNameLength {
		return "", fmt.Errorf("%w: name must not exceed %d characters", domain.ErrValidation, maxTagNameLength)
	}

	return s, nil
}
