package service

import (
	"errors"
	"strings"
	"testing"

	"github.com/scoreplay/media-api/internal/domain"
)

// ==========================================================================
// SanitizeName — Accepted inputs
// ==========================================================================

func TestSanitizeName_AcceptsBasicLatin(t *testing.T) {
	tests := []struct{ input, expected string }{
		{"Ligue 1", "Ligue 1"},
		{"Player's Highlight", "Player's Highlight"},
		{"U-21 Championship", "U-21 Championship"},
		{"Goal (2024)", "Goal (2024)"},
		{"Hello, World!", "Hello, World!"},
	}
	for _, tt := range tests {
		got, err := SanitizeName(tt.input)
		if err != nil {
			t.Errorf("SanitizeName(%q): unexpected error: %v", tt.input, err)
		}
		if got != tt.expected {
			t.Errorf("SanitizeName(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestSanitizeName_AcceptsLatinAccents(t *testing.T) {
	tests := []struct{ input, expected string }{
		{"Mbappé", "Mbappé"},
		{"Müller", "Müller"},
		{"Señor", "Señor"},
		{"Zlatan Ibrahimović", "Zlatan Ibrahimović"},
		{"Çalhanoğlu", "Çalhanoğlu"},
	}
	for _, tt := range tests {
		got, err := SanitizeName(tt.input)
		if err != nil {
			t.Errorf("SanitizeName(%q): unexpected error: %v", tt.input, err)
		}
		if got != tt.expected {
			t.Errorf("SanitizeName(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestSanitizeName_AcceptsCJK(t *testing.T) {
	tests := []struct{ input, expected string }{
		{"田中将大", "田中将大"},           // Japanese Kanji
		{"손흥민", "손흥민"},             // Korean Hangul
		{"大谷翔平", "大谷翔平"},           // Japanese
		{"カタカナ", "カタカナ"},           // Japanese Katakana
		{"ひらがな", "ひらがな"},           // Japanese Hiragana
		{"中国足球", "中国足球"},           // Chinese
	}
	for _, tt := range tests {
		got, err := SanitizeName(tt.input)
		if err != nil {
			t.Errorf("SanitizeName(%q): unexpected error: %v", tt.input, err)
		}
		if got != tt.expected {
			t.Errorf("SanitizeName(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestSanitizeName_AcceptsArabicHebrewDevanagari(t *testing.T) {
	tests := []struct{ input, expected string }{
		{"محمد صلاح", "محمد صلاح"},       // Arabic
		{"שלום", "שלום"},                   // Hebrew
		{"विराट कोहली", "विराट कोहली"}, // Hindi/Devanagari
		{"ไทย", "ไทย"},                     // Thai
	}
	for _, tt := range tests {
		got, err := SanitizeName(tt.input)
		if err != nil {
			t.Errorf("SanitizeName(%q): unexpected error: %v", tt.input, err)
		}
		if got != tt.expected {
			t.Errorf("SanitizeName(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestSanitizeName_AcceptsEmoji(t *testing.T) {
	tests := []struct{ input, expected string }{
		{"⚽ Goal", "⚽ Goal"},
		{"🏆 Champion", "🏆 Champion"},
		{"🇫🇷 France", "🇫🇷 France"},
		{"Fire 🔥", "Fire 🔥"},
	}
	for _, tt := range tests {
		got, err := SanitizeName(tt.input)
		if err != nil {
			t.Errorf("SanitizeName(%q): unexpected error: %v", tt.input, err)
		}
		if got != tt.expected {
			t.Errorf("SanitizeName(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

// ==========================================================================
// SanitizeName — Normalization
// ==========================================================================

func TestSanitizeName_NFCNormalization(t *testing.T) {
	// NFD form: "e" + combining acute accent (U+0301).
	nfd := "e\u0301" // é in NFD
	got, err := SanitizeName(nfd)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Should be normalized to NFC: "é" (U+00E9).
	if got != "é" {
		t.Errorf("expected NFC 'é' (U+00E9), got %q (bytes: %x)", got, []byte(got))
	}
}

func TestSanitizeName_NFCNormalizationPreventsDuplicates(t *testing.T) {
	// Two forms of "café" that should produce the same output.
	nfc := "caf\u00E9"     // composed form
	nfd := "cafe\u0301"    // decomposed form
	gotNFC, _ := SanitizeName(nfc)
	gotNFD, _ := SanitizeName(nfd)
	if gotNFC != gotNFD {
		t.Errorf("NFC and NFD forms should normalize to same string: %q vs %q", gotNFC, gotNFD)
	}
}

func TestSanitizeName_TrimsWhitespace(t *testing.T) {
	got, err := SanitizeName("  Mbappé  ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "Mbappé" {
		t.Errorf("expected trimmed 'Mbappé', got %q", got)
	}
}

func TestSanitizeName_CollapsesMultipleSpaces(t *testing.T) {
	got, err := SanitizeName("Goal   celebration   2024")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "Goal celebration 2024" {
		t.Errorf("expected collapsed spaces, got %q", got)
	}
}

func TestSanitizeName_NormalizesUnicodeWhitespace(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"no-break space", "Goal\u00A0celebration"},
		{"en space", "Goal\u2002celebration"},
		{"em space", "Goal\u2003celebration"},
		{"thin space", "Goal\u2009celebration"},
		{"ideographic space", "Goal\u3000celebration"},
		{"narrow no-break space", "Goal\u202Fcelebration"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := SanitizeName(tt.input)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != "Goal celebration" {
				t.Errorf("expected 'Goal celebration', got %q", got)
			}
		})
	}
}

func TestSanitizeName_NormalizesTabsAndNewlines(t *testing.T) {
	got, err := SanitizeName("Line1\tLine2\nLine3\rLine4")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "Line1 Line2 Line3 Line4" {
		t.Errorf("expected normalized whitespace, got %q", got)
	}
}

// ==========================================================================
// SanitizeName — Rejected inputs
// ==========================================================================

func TestSanitizeName_RejectsEmpty(t *testing.T) {
	tests := []string{"", "   ", "\t\n", "\u00A0"}
	for _, input := range tests {
		_, err := SanitizeName(input)
		if !errors.Is(err, domain.ErrValidation) {
			t.Errorf("SanitizeName(%q): expected ErrValidation, got: %v", input, err)
		}
	}
}

func TestSanitizeName_RejectsTooLong(t *testing.T) {
	long := strings.Repeat("a", 256)
	_, err := SanitizeName(long)
	if !errors.Is(err, domain.ErrValidation) {
		t.Errorf("expected ErrValidation for 256-char name, got: %v", err)
	}
}

func TestSanitizeName_AcceptsMaxLength(t *testing.T) {
	long := strings.Repeat("a", 255)
	got, err := SanitizeName(long)
	if err != nil {
		t.Fatalf("255 chars should be accepted: %v", err)
	}
	if len(got) != 255 {
		t.Errorf("expected 255 chars, got %d", len(got))
	}
}

func TestSanitizeName_RejectsControlCharacters(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"null byte", "hello\x00world"},
		{"bell", "hello\x07world"},
		{"backspace", "hello\x08world"},
		{"escape", "hello\x1Bworld"},
		{"C1 control", "hello\u0080world"},
		{"C1 control 9F", "hello\u009Fworld"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := SanitizeName(tt.input)
			if !errors.Is(err, domain.ErrValidation) {
				t.Errorf("expected ErrValidation, got: %v", err)
			}
		})
	}
}

func TestSanitizeName_RejectsZeroWidthCharacters(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"zero-width space", "Mbapp\u200Bé"},
		{"zero-width non-joiner", "Mbapp\u200Cé"},
		{"zero-width joiner", "Mbapp\u200Dé"},
		{"left-to-right mark", "Mbapp\u200Eé"},
		{"right-to-left mark", "Mbapp\u200Fé"},
		{"BOM", "\uFEFFMbappé"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := SanitizeName(tt.input)
			if !errors.Is(err, domain.ErrValidation) {
				t.Errorf("expected ErrValidation, got: %v", err)
			}
		})
	}
}

func TestSanitizeName_RejectsBidiOverrides(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{"LRE", "test\u202Avalue"},
		{"RLE", "test\u202Bvalue"},
		{"PDF", "test\u202Cvalue"},
		{"LRO", "test\u202Dvalue"},
		{"RLO", "test\u202Evalue"},
		{"LRI", "test\u2066value"},
		{"RLI", "test\u2067value"},
		{"FSI", "test\u2068value"},
		{"PDI", "test\u2069value"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := SanitizeName(tt.input)
			if !errors.Is(err, domain.ErrValidation) {
				t.Errorf("expected ErrValidation, got: %v", err)
			}
		})
	}
}

func TestSanitizeName_RejectsReplacementCharacter(t *testing.T) {
	_, err := SanitizeName("hello\uFFFDworld")
	if !errors.Is(err, domain.ErrValidation) {
		t.Errorf("expected ErrValidation for U+FFFD, got: %v", err)
	}
}

func TestSanitizeName_RejectsInterlinearAnnotations(t *testing.T) {
	tests := []string{
		"test\uFFF9value",
		"test\uFFFAvalue",
		"test\uFFFBvalue",
	}
	for _, input := range tests {
		_, err := SanitizeName(input)
		if !errors.Is(err, domain.ErrValidation) {
			t.Errorf("SanitizeName(%q): expected ErrValidation, got: %v", input, err)
		}
	}
}

func TestSanitizeName_RejectsDeprecatedTagCharacters(t *testing.T) {
	// U+E0001 (LANGUAGE TAG) and U+E0020 (TAG SPACE).
	input := "test" + string(rune(0xE0001)) + "value"
	_, err := SanitizeName(input)
	if !errors.Is(err, domain.ErrValidation) {
		t.Errorf("expected ErrValidation for tag character, got: %v", err)
	}
}

func TestSanitizeName_RejectsInvalidUTF8(t *testing.T) {
	// \xff is not valid UTF-8.
	_, err := SanitizeName("hello\xffworld")
	if !errors.Is(err, domain.ErrValidation) {
		t.Errorf("expected ErrValidation for invalid UTF-8, got: %v", err)
	}
}

// ==========================================================================
// SanitizeName — Mixed/edge cases
// ==========================================================================

func TestSanitizeName_MixedScriptsAccepted(t *testing.T) {
	tests := []struct{ input, expected string }{
		{"Ligue 1 - Mbappé ⚽", "Ligue 1 - Mbappé ⚽"},
		{"J-League 田中将大", "J-League 田中将大"},
		{"K-League 손흥민 🇰🇷", "K-League 손흥민 🇰🇷"},
	}
	for _, tt := range tests {
		got, err := SanitizeName(tt.input)
		if err != nil {
			t.Errorf("SanitizeName(%q): unexpected error: %v", tt.input, err)
		}
		if got != tt.expected {
			t.Errorf("SanitizeName(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestSanitizeName_SingleCharacter(t *testing.T) {
	got, err := SanitizeName("A")
	if err != nil {
		t.Fatalf("single character should be accepted: %v", err)
	}
	if got != "A" {
		t.Errorf("expected 'A', got %q", got)
	}
}

func TestSanitizeName_OnlyWhitespaceAfterNormalization(t *testing.T) {
	// Input is only Unicode whitespace — should normalize to spaces, then trim to empty.
	_, err := SanitizeName("\u00A0\u2003\u3000")
	if !errors.Is(err, domain.ErrValidation) {
		t.Errorf("expected ErrValidation for all-whitespace input, got: %v", err)
	}
}

func TestSanitizeName_ComplexNormalization(t *testing.T) {
	// Tab + multiple spaces + Unicode whitespace + trailing spaces.
	input := "  Goal\t\u00A0  celebration   "
	got, err := SanitizeName(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "Goal celebration" {
		t.Errorf("expected 'Goal celebration', got %q", got)
	}
}
