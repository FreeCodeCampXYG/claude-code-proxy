package ppt

import (
	"strings"
	"testing"
)

func validDeck() Deck {
	return Deck{SchemaVersion: SchemaVersion, Title: "测试", Slides: []Slide{{ID: "slide-1", Layout: "bullets", Title: "第一页", Bullets: []string{"要点"}}}}
}

func TestDecodeAndRender(t *testing.T) {
	deck, err := Decode([]byte(`{"schema_version":"ppt-outline/v1","title":"测试","slides":[{"id":"slide-1","layout":"bullets","title":"第一页","bullets":["要点"]}]}`))
	if err != nil { t.Fatal(err) }
	markdown := Markdown(deck)
	html, err := HTML(deck)
	if err != nil || !strings.Contains(markdown, "第一页") || !strings.Contains(html, "第一页") { t.Fatalf("unexpected render markdown=%q html=%q err=%v", markdown, html, err) }
}

func TestDecodeRejectsUnknownFieldsAndInvalidLayouts(t *testing.T) {
	for _, source := range []string{
		`{"schema_version":"ppt-outline/v1","title":"测试","unknown":true,"slides":[{"id":"slide-1","layout":"bullets","title":"第一页"}]}`,
		`{"schema_version":"ppt-outline/v1","title":"测试","slides":[{"id":"slide-1","layout":"freeform","title":"第一页"}]}`,
		`{"schema_version":"ppt-outline/v1","title":"测试","slides":[{"id":"slide-1","layout":"bullets","title":"第一页"}]} trailing`,
	} { if _, err := Decode([]byte(source)); err == nil { t.Fatalf("Decode(%s) unexpectedly succeeded", source) } }
}

func TestValidateRejectsDuplicateSlideIDs(t *testing.T) {
	deck := validDeck(); deck.Slides = append(deck.Slides, deck.Slides[0])
	if err := Validate(deck); err == nil { t.Fatal("Validate accepted duplicate ID") }
}

func TestValidateRejectsInvalidDeckMetadata(t *testing.T) {
	deck := validDeck(); deck.Audience = strings.Repeat("x", 161)
	if err := Validate(deck); err == nil { t.Fatal("Validate accepted oversized audience") }
	deck = validDeck(); deck.Theme.Name = "bad\x00theme"
	if err := Validate(deck); err == nil { t.Fatal("Validate accepted NUL theme metadata") }
}
